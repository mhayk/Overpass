-- planner-service inputs: the candidate set it allocates over, and the request
-- facts it allocates by.
--
-- 00004 gave the planner everything it WRITES — plans, acquisitions, the
-- non-overlap invariant. It gave it nothing it READS. A planning round needs two
-- things that arrive on two different streams:
--
--   the candidates — (satellite, window, geometry, mode) tuples, from
--   feasibility.opportunities.computed.v1;
--
--   the value — bid, tier, deadline, from tasking.request.received.v1. The
--   opportunities event does not carry them, and correctly so: feasibility
--   computes geometry, not commerce.
--
-- Both are projected into this schema rather than read from tasking's or
-- feasibility's. ADR-0015 has the argument; the short version is that a planner
-- SELECTing across schemas would put the ownership rule and the round's
-- transaction boundary in the same place, and lose both.

-- +goose Up

-- ---------------------------------------------------------------------------
-- The value half.
-- ---------------------------------------------------------------------------
-- A projection of tasking.request.received.v1, holding exactly the fields the
-- allocation objective and the commitment check consume. Not a copy of the
-- request: a copy of the request would drift, and every column not listed here
-- is one the planner has been shown not to need.
CREATE TABLE planning.request_snapshots (
    request_id     uuid        PRIMARY KEY,

    -- Needed because planning.acquisitions.customer_id is NOT NULL and the
    -- opportunities event carries no customer. Without this column the planner
    -- literally cannot write a winning acquisition.
    customer_id    text        NOT NULL REFERENCES reference.customers (customer_id),

    priority_tier  text        NOT NULL
                               CHECK (priority_tier IN ('GOVERNMENT', 'CIVIL_PROTECTION',
                                                        'COMMERCIAL', 'BEST_EFFORT')),
    bid_credits    bigint      NOT NULL CHECK (bid_credits BETWEEN 0 AND 100000000),

    -- The deadline, and NOT a restatement of anything feasibility already
    -- checked. Feasibility clamps its search horizon to the request window, so
    -- every candidate STARTS in time by construction — but an acquisition
    -- occupies acquisition_duration_s from that start, and one that begins
    -- before upper(request_window) can still finish after it. The contract is
    -- explicit that such an acquisition has no value and is infeasible rather
    -- than late. That check has no other home: feasibility does not know the
    -- start the planner will choose.
    --
    -- Named request_window for the reason tasking.tasking_requests documents:
    -- WINDOW is a reserved word and `window tstzrange` is a syntax error.
    request_window tstzrange   NOT NULL
                               CONSTRAINT request_snapshots_window_bounded
                               CHECK (NOT isempty(request_window)
                                      AND lower(request_window) IS NOT NULL
                                      AND upper(request_window) IS NOT NULL),

    -- The origin of the fairness ageing factor (M2-09). A request that keeps
    -- losing gains weight with age, and age is measured from acceptance, not
    -- from when the planner first heard about it — otherwise a slow consumer
    -- would silently reset a customer's accrued fairness.
    submitted_at   timestamptz NOT NULL,

    -- Provenance. Every row traces to the event that produced it, which is what
    -- makes "the planner valued this wrongly" a diagnosable claim rather than an
    -- accusation. occurred_at is the ordering key a future update would compare
    -- against; today the only concurrency it faces is redelivery, which the
    -- primary key already absorbs.
    source_event_id uuid       NOT NULL,
    occurred_at     timestamptz NOT NULL,

    created_at     timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- The candidate half.
-- ---------------------------------------------------------------------------
-- Opportunities the planner has been told about, held until a round consumes
-- them. Shaped after feasibility.opportunities, because both are storing the
-- same contract object — but owned here, so a round reads its own schema.
CREATE TABLE planning.candidate_opportunities (
    opportunity_id         uuid        PRIMARY KEY,

    -- DELIBERATELY NOT A FOREIGN KEY to planning.request_snapshots.
    --
    -- This is the load-bearing line of the migration, and it is the one most
    -- likely to be "fixed" by someone tidying up. The two events arrive on
    -- different streams through different consumers, and the spec states plainly
    -- that global ordering is not assumed. So a request's opportunities can
    -- arrive BEFORE the request itself. A foreign key would reject that insert,
    -- the consumer would nack, and the batch would redeliver until the other
    -- stream happened to catch up — turning a benign ordering fact into a
    -- retry storm.
    --
    -- The join is therefore made at read time, and a candidate with no snapshot
    -- yet is HELD, not dropped: it has no value and no deadline, so allocating
    -- it would be guessing, but a later round will find it once the snapshot
    -- lands. Out-of-order tolerance is a state-machine guard here, not a
    -- constraint.
    request_id             uuid        NOT NULL,

    satellite_id           text        NOT NULL REFERENCES reference.satellites (satellite_id),
    mode                   text        NOT NULL CHECK (mode IN ('SPOTLIGHT', 'STRIPMAP', 'SCAN')),

    -- The interval in which the acquisition may START — not its duration. The
    -- gap between the two is the slack the planner spends absorbing slew, and
    -- collapsing them into one fixed interval would delete the system's main
    -- scheduling freedom. Same reasoning, same wording, as feasibility.
    access_window          tstzrange   NOT NULL
                                       CONSTRAINT candidate_opportunities_window_bounded
                                       CHECK (NOT isempty(access_window)
                                              AND lower(access_window) IS NOT NULL
                                              AND upper(access_window) IS NOT NULL),
    acquisition_duration_s numeric     NOT NULL CHECK (acquisition_duration_s > 0),

    -- Nullable, because the contract does not require it — orbit_number is
    -- absent from the required list of an opportunity item. That is not
    -- hygiene, it is a live constraint on M2-03: the duty-cycle budget is
    -- enforced per orbit, so a candidate with no orbit number cannot be charged
    -- against any budget. Declaring this NOT NULL would make a contract-valid
    -- event unstorable, which is the wrong direction to resolve the tension in.
    -- M2-03 has to decide whether such a candidate is skipped or charged to a
    -- synthetic bucket, and it should decide that in the open.
    orbit_number           integer     CHECK (orbit_number IS NULL OR orbit_number >= 0),

    -- AccessGeometry, verbatim. The slew model (M2-02) reads look_side, squint
    -- and roll out of here, so this blob is an INPUT to scheduling rather than
    -- decoration — note that of those three only look_side and squint are
    -- required by the contract.
    geometry               jsonb       NOT NULL CHECK (jsonb_typeof(geometry) = 'object'),

    -- Carried because planning.acquisitions.footprint is NOT NULL: a winning
    -- candidate must be able to become an acquisition without a second lookup
    -- into feasibility's schema.
    footprint              geometry(Polygon, 4326) NOT NULL,

    duty_cycle_cost_s      numeric     NOT NULL CHECK (duty_cycle_cost_s > 0),
    quality_score          numeric     NOT NULL CHECK (quality_score BETWEEN 0 AND 1),

    computed_at            timestamptz NOT NULL,
    source_event_id        uuid        NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now()
);

-- The round's read path: candidates for one satellite whose access window meets
-- a horizon bucket. A range query, so GiST rather than btree — the same shape
-- feasibility indexes for the same reason.
CREATE INDEX candidate_opportunities_satellite_window
    ON planning.candidate_opportunities USING gist (satellite_id, access_window);

-- The join to request_snapshots, and the sweep for held candidates.
CREATE INDEX candidate_opportunities_request
    ON planning.candidate_opportunities (request_id);

-- No GIN on geometry and no GiST on footprint here, unlike feasibility. The
-- planner never queries by a geometry term or a spatial predicate; it loads
-- both columns for candidates it has already selected by satellite and time.
-- An index nothing queries is write amplification with a reassuring name.

-- +goose Down

DROP TABLE planning.candidate_opportunities;
DROP TABLE planning.request_snapshots;

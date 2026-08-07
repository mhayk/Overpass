-- planner-service: collection plans, acquisitions, and the central invariant.
--
-- This file is the one a reviewer should read first. Everything else in the
-- repository exists to make the constraint at the bottom of planning.acquisitions
-- meaningful, and ADR-0003 and ADR-0004 both rest on it holding.

-- +goose Up

CREATE TABLE planning.collection_plans (
    plan_id            uuid        PRIMARY KEY,
    round_id           uuid        NOT NULL,
    satellite_id       text        NOT NULL REFERENCES reference.satellites (satellite_id),

    -- One plan covers one satellite over one horizon bucket.
    bucket             tstzrange   NOT NULL
                                   CONSTRAINT collection_plans_bucket_bounded
                                   CHECK (NOT isempty(bucket)
                                          AND lower(bucket) IS NOT NULL
                                          AND upper(bucket) IS NOT NULL),

    -- Supersession, per ADR-0012. A bucket can be planned more than once
    -- because rounds fire on either a cadence timer or an opportunity
    -- debounce, so plan_version increments per (satellite_id, bucket) and
    -- supersedes_plan_id points at the plan replaced.
    plan_version       integer     NOT NULL CHECK (plan_version >= 1),
    supersedes_plan_id uuid        REFERENCES planning.collection_plans (plan_id),

    -- A plan cannot supersede itself. Cheap to state, and the kind of thing a
    -- generated INSERT gets wrong exactly once.
    CONSTRAINT collection_plans_no_self_supersede
        CHECK (supersedes_plan_id IS NULL OR supersedes_plan_id <> plan_id),

    policy             text        NOT NULL CHECK (length(policy) > 0),
    metrics            jsonb       NOT NULL DEFAULT '{}'::jsonb
                                   CHECK (jsonb_typeof(metrics) = 'object'),
    committed_at       timestamptz NOT NULL,

    -- Optimistic concurrency control on the row itself.
    --
    -- Named row_version, NOT version, because plan_version already exists two
    -- lines up and means something completely different: one is the domain's
    -- supersession counter, this one is a mutex. Two columns called
    -- version-something on the same table, with different semantics, is a bug
    -- waiting for a tired afternoon.
    row_version        integer     NOT NULL DEFAULT 1 CHECK (row_version >= 1),

    -- The version sequence per bucket is dense and unique. Two plans claiming
    -- v3 of the same bucket means two planner rounds raced, which the advisory
    -- lock in M2-01 is supposed to prevent — this is the backstop that says so
    -- out loud instead of silently keeping both.
    CONSTRAINT collection_plans_unique_version UNIQUE (satellite_id, bucket, plan_version)
);

CREATE INDEX collection_plans_satellite_bucket
    ON planning.collection_plans USING gist (satellite_id, bucket);
CREATE INDEX collection_plans_round      ON planning.collection_plans (round_id);
CREATE INDEX collection_plans_supersedes ON planning.collection_plans (supersedes_plan_id)
    WHERE supersedes_plan_id IS NOT NULL;

CREATE TABLE planning.acquisitions (
    acquisition_id            uuid        PRIMARY KEY,

    -- RESTRICT, not CASCADE. Deleting a plan that has acquisitions should be a
    -- loud error, because ADR-0012 decided acquisitions are retained rather
    -- than deleted, and a stray CASCADE is how that decision would be undone
    -- by accident.
    plan_id                   uuid        NOT NULL
                                          REFERENCES planning.collection_plans (plan_id)
                                          ON DELETE RESTRICT,

    request_id                uuid        NOT NULL,
    opportunity_id            uuid        NOT NULL,
    customer_id               text        NOT NULL REFERENCES reference.customers (customer_id),
    satellite_id              text        NOT NULL REFERENCES reference.satellites (satellite_id),
    mode                      text        NOT NULL CHECK (mode IN ('SPOTLIGHT', 'STRIPMAP', 'SCAN')),

    -- Named acq_window rather than window: WINDOW is a reserved word in
    -- PostgreSQL and will not parse unquoted. Same reasoning as
    -- tasking.tasking_requests.request_window.
    --
    -- This range holds the ACQUISITION ONLY. The slew is not inside it. That
    -- split is stated in planning.plan.committed.v1 and is deliberate: overlap
    -- is enforced here by the database, slew separation is enforced by the
    -- planner and asserted by property-based tests (M2-12).
    acq_window                tstzrange   NOT NULL
                                          CONSTRAINT acquisitions_window_bounded
                                          CHECK (NOT isempty(acq_window)
                                                 AND lower(acq_window) IS NOT NULL
                                                 AND upper(acq_window) IS NOT NULL),

    geometry                  jsonb       NOT NULL CHECK (jsonb_typeof(geometry) = 'object'),
    footprint                 geometry(Polygon, 4326) NOT NULL,

    -- Attitude manoeuvre plus settling after the preceding acquisition in this
    -- plan. Absent for the first acquisition.
    slew_time_from_previous_s numeric     CHECK (slew_time_from_previous_s IS NULL
                                                 OR slew_time_from_previous_s >= 0),
    gap_from_previous_s       numeric     CHECK (gap_from_previous_s IS NULL
                                                 OR gap_from_previous_s >= 0),

    -- The slew invariant, to the extent a single row can carry it: the recorded
    -- gap must be at least the recorded slew.
    --
    -- Be precise about what this does and does not buy. It enforces that the
    -- two numbers on this row are mutually consistent. It does NOT enforce that
    -- either was computed correctly, nor that the gap matches the actual
    -- distance to the preceding acquisition — both are cross-row facts that an
    -- exclusion constraint cannot express, because slew_time(a, b) depends on
    -- the geometry of both acquisitions and not on their intervals. Those
    -- remain the planner's job and M2-12's.
    CONSTRAINT acquisitions_gap_covers_slew
        CHECK (slew_time_from_previous_s IS NULL
               OR gap_from_previous_s IS NULL
               OR gap_from_previous_s >= slew_time_from_previous_s),

    duty_cycle_cost_s         numeric     NOT NULL CHECK (duty_cycle_cost_s > 0),
    awarded_value_credits     bigint      NOT NULL CHECK (awarded_value_credits BETWEEN 0 AND 100000000),
    clearing_price_credits    bigint      CHECK (clearing_price_credits IS NULL
                                                 OR clearing_price_credits BETWEEN 0 AND 100000000),

    -- NOT NULL is part of the invariant, not hygiene.
    --
    -- The exclusion constraint below is PARTIAL. A partial-index predicate that
    -- evaluates to NULL excludes the row from the index entirely, so a row with
    -- status = NULL would escape the constraint completely: it would overlap
    -- live acquisitions freely, and two such rows would overlap each other.
    -- Verified against PostgreSQL 16.4 before this migration was written. The
    -- hole is reachable by any INSERT that omits the column, which is why the
    -- NOT NULL is here and why db/tests asserts it directly rather than
    -- trusting this comment.
    status                    text        NOT NULL
                                          CHECK (status IN ('ACTIVE', 'SUPERSEDED', 'EXECUTED')),

    superseded_at             timestamptz,
    CONSTRAINT acquisitions_superseded_at_agrees
        CHECK ((status = 'SUPERSEDED') = (superseded_at IS NOT NULL)),

    created_at                timestamptz NOT NULL DEFAULT now(),

    -- ---------------------------------------------------------------------
    -- THE INVARIANT.
    --
    -- No two live acquisitions on one satellite may overlap in time, enforced
    -- by the database so that no code path, migration, or manual INSERT can
    -- violate it. This is the claim ADR-0003 and ADR-0004 both rest on.
    --
    -- Three parts, each load-bearing and each decided in ADR-0012:
    --
    --   btree_gist supplies the equality operator class for satellite_id;
    --   plain GiST has none for scalars, so without the extension this
    --   constraint cannot be created at all.
    --
    --   WHERE (status <> 'SUPERSEDED') — partial, over the live set. A total
    --   constraint would fire against the very plan being replaced, because
    --   re-planning inserts v2's acquisitions while v1's still occupy
    --   overlapping times. EXECUTED is inside the live set on purpose: an
    --   acquisition that already flew consumed real sensor time and must keep
    --   blocking overlap.
    --
    --   DEFERRABLE INITIALLY DEFERRED — checked at COMMIT, not per statement.
    --   With an immediate constraint, insert-then-demote is rejected while
    --   demote-then-insert commits, so the plan-commit transaction would
    --   acquire a statement-ordering requirement enforced by nothing. Deferring
    --   moves WHEN the check runs, not WHETHER: a genuinely conflicting plan is
    --   still rejected, at COMMIT. Verified, both ways.
    -- ---------------------------------------------------------------------
    CONSTRAINT acquisitions_no_overlap_live
        EXCLUDE USING gist (satellite_id WITH =, acq_window WITH &&)
        WHERE (status <> 'SUPERSEDED')
        DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX acquisitions_plan       ON planning.acquisitions (plan_id);
CREATE INDEX acquisitions_request    ON planning.acquisitions (request_id);
CREATE INDEX acquisitions_footprint  ON planning.acquisitions USING gist (footprint);
CREATE INDEX acquisitions_geometry_gin ON planning.acquisitions USING gin (geometry);

-- The read path for "what is this satellite doing", which only ever wants live
-- rows. Partial for the same reason the outbox index is: history should not
-- inflate the index that serves the present.
CREATE INDEX acquisitions_live_satellite_window
    ON planning.acquisitions USING gist (satellite_id, acq_window)
    WHERE status <> 'SUPERSEDED';

CREATE TABLE planning.processed_events (
    consumer     text        NOT NULL CHECK (length(consumer) > 0),
    event_id     uuid        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

CREATE TABLE planning.outbox (
    id             bigserial   PRIMARY KEY,
    event_id       uuid        NOT NULL UNIQUE,
    event_type     text        NOT NULL CHECK (length(event_type) > 0),
    schema_version text        NOT NULL,
    subject        text        NOT NULL CHECK (length(subject) > 0),
    payload        jsonb       NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    headers        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    occurred_at    timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz,
    attempts       integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error     text
);

CREATE INDEX outbox_unpublished ON planning.outbox (id) WHERE published_at IS NULL;

-- +goose Down

DROP TABLE planning.outbox;
DROP TABLE planning.processed_events;
DROP TABLE planning.acquisitions;
DROP TABLE planning.collection_plans;

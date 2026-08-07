-- feasibility-service: computed opportunities, its idempotent-consumer ledger,
-- and its own outbox.
--
-- An Opportunity is a CANDIDATE — a (satellite, window, geometry, mode) tuple a
-- real sensor could fly. Nothing here is a commitment; winning allocation is
-- what turns one into an acquisition, and that lives in the planning schema.

-- +goose Up

CREATE TABLE feasibility.opportunities (
    opportunity_id         uuid        PRIMARY KEY,
    request_id             uuid        NOT NULL,
    satellite_id           text        NOT NULL REFERENCES reference.satellites (satellite_id),
    mode                   text        NOT NULL CHECK (mode IN ('SPOTLIGHT', 'STRIPMAP', 'SCAN')),

    -- The interval within which the acquisition may START, which is NOT the
    -- same as how long it takes. Keeping them separate is what gives the
    -- planner slack to shift an acquisition and absorb a slew; collapsing them
    -- into one fixed interval would throw away the system's main scheduling
    -- freedom.
    access_window          tstzrange   NOT NULL
                                       CONSTRAINT opportunities_window_bounded
                                       CHECK (NOT isempty(access_window)
                                              AND lower(access_window) IS NOT NULL
                                              AND upper(access_window) IS NOT NULL),
    acquisition_duration_s numeric     NOT NULL CHECK (acquisition_duration_s > 0),

    orbit_number           integer     CHECK (orbit_number IS NULL OR orbit_number >= 0),

    -- AccessGeometry: incidence angle, look side, squint, slant range,
    -- elevation. A documented JSONB case — this is the model most likely to
    -- gain terms as the SAR geometry is refined through M1 and M2, and each
    -- new term would otherwise be a migration.
    geometry               jsonb       NOT NULL CHECK (jsonb_typeof(geometry) = 'object'),

    -- PostGIS, for the same reason as tasking_requests.target: the containment
    -- check against the target and the M4 coverage view are spatial queries.
    footprint              geometry(Polygon, 4326) NOT NULL,

    duty_cycle_cost_s      numeric     NOT NULL CHECK (duty_cycle_cost_s > 0),

    -- Normalised geometric quality. A tie-break input to allocation and NOT a
    -- value — value comes from the bid and the tier. Keeping the two apart is
    -- what keeps the auction mechanism honest.
    quality_score          numeric     NOT NULL CHECK (quality_score BETWEEN 0 AND 1),

    computed_at            timestamptz NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now()
);

-- The planner batches by (satellite_id, bucket_start), so this is its read
-- path, and it is a range query — hence GiST rather than btree on the window.
CREATE INDEX opportunities_satellite_window
    ON feasibility.opportunities USING gist (satellite_id, access_window);

CREATE INDEX opportunities_request     ON feasibility.opportunities (request_id);
CREATE INDEX opportunities_footprint   ON feasibility.opportunities USING gist (footprint);
CREATE INDEX opportunities_geometry_gin ON feasibility.opportunities USING gin (geometry);

-- The idempotent-consumer ledger. Identical in shape in every consuming schema,
-- duplicated for the same reason the outbox is.
--
-- Keyed by (consumer, event_id) rather than event_id alone: one service can run
-- several durable consumers, and a redelivery to one of them must not look
-- already-processed to another.
CREATE TABLE feasibility.processed_events (
    consumer     text        NOT NULL CHECK (length(consumer) > 0),
    event_id     uuid        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

CREATE TABLE feasibility.outbox (
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

CREATE INDEX outbox_unpublished ON feasibility.outbox (id) WHERE published_at IS NULL;

-- +goose Down

DROP TABLE feasibility.outbox;
DROP TABLE feasibility.processed_events;
DROP TABLE feasibility.opportunities;

-- plan-gateway read models.
--
-- Denormalised on purpose. These tables exist to answer one screen's question
-- in one query, and every one of them is derivable from the event log — which
-- is what makes a rebuild a routine operation rather than an incident.
--
-- Nothing here is authoritative. The planner owns the plan; this is a
-- projection of it, and every row carries the position it was built from so a
-- reader can see how far behind it is.

-- +goose Up

-- Where the projector has got to, per stream.
--
-- Per stream rather than one global cursor: the three streams are independent
-- and a rebuild of one must not rewind the others. The sequence is JetStream's,
-- and `occurred_at` is what staleness is measured against — a sequence number
-- means nothing to a human looking at a stale globe.
CREATE TABLE readmodel.stream_cursors (
    stream        text        PRIMARY KEY,
    last_sequence bigint      NOT NULL DEFAULT 0 CHECK (last_sequence >= 0),
    last_event_at timestamptz,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE readmodel.request_views (
    request_id        uuid        PRIMARY KEY,
    customer_id       text        NOT NULL,
    target_name       text        NOT NULL,
    state             text        NOT NULL,
    request_window    tstzrange   NOT NULL,
    target            geometry(Geometry, 4326),
    opportunity_count integer     NOT NULL DEFAULT 0 CHECK (opportunity_count >= 0),

    -- The unfulfilment explanation, as structured data rather than a message.
    -- Strings cannot be aggregated or charted, and "requests unfulfilled by
    -- reason" is a domain metric.
    unfulfilment      jsonb,

    -- The instant of the event that last moved this row, NOT now(). Ordering
    -- guards compare against it, and a wall-clock stamp would make a replay
    -- produce different guards than the original run.
    last_event_at     timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX request_views_customer  ON readmodel.request_views (customer_id);
CREATE INDEX request_views_state     ON readmodel.request_views (state);
CREATE INDEX request_views_target    ON readmodel.request_views USING gist (target);

CREATE TABLE readmodel.plan_views (
    plan_id            uuid        PRIMARY KEY,
    satellite_id       text        NOT NULL,
    bucket             tstzrange   NOT NULL,
    plan_version       integer     NOT NULL CHECK (plan_version >= 1),
    supersedes_plan_id uuid,
    superseded         boolean     NOT NULL DEFAULT false,
    policy             text        NOT NULL,
    metrics            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    committed_at       timestamptz NOT NULL,
    last_event_at      timestamptz NOT NULL,

    -- One version of one bucket, once. A replay that folded the same
    -- plan.committed event twice would otherwise produce two rows and a read
    -- that returns both.
    CONSTRAINT plan_views_unique_version UNIQUE (satellite_id, bucket, plan_version)
);

CREATE INDEX plan_views_satellite_bucket ON readmodel.plan_views USING gist (satellite_id, bucket);
CREATE INDEX plan_views_current          ON readmodel.plan_views (satellite_id, bucket)
    WHERE NOT superseded;

CREATE TABLE readmodel.acquisition_views (
    acquisition_id            uuid        PRIMARY KEY,
    plan_id                   uuid        NOT NULL REFERENCES readmodel.plan_views (plan_id) ON DELETE CASCADE,
    request_id                uuid        NOT NULL,
    opportunity_id            uuid,
    customer_id               text        NOT NULL,
    satellite_id              text        NOT NULL,
    mode                      text        NOT NULL,
    acq_window                tstzrange   NOT NULL,
    status                    text        NOT NULL,
    footprint                 geometry(Polygon, 4326) NOT NULL,
    slew_time_from_previous_s numeric,
    gap_from_previous_s       numeric,
    awarded_value_credits     bigint      NOT NULL DEFAULT 0
);

-- CASCADE here and RESTRICT in planning.acquisitions, deliberately. The write
-- model must never lose an acquisition to a stray delete; a read model that
-- drops a plan should drop its acquisitions with it, because a projection is
-- rebuildable and an orphan row in one is just wrong data on a screen.

CREATE INDEX acquisition_views_window    ON readmodel.acquisition_views USING gist (satellite_id, acq_window);
CREATE INDEX acquisition_views_request   ON readmodel.acquisition_views (request_id);
CREATE INDEX acquisition_views_footprint ON readmodel.acquisition_views USING gist (footprint);

CREATE TABLE readmodel.opportunity_views (
    opportunity_id         uuid        PRIMARY KEY,
    request_id             uuid        NOT NULL,
    satellite_id           text        NOT NULL,
    mode                   text        NOT NULL,
    access_window          tstzrange   NOT NULL,
    acquisition_duration_s numeric     NOT NULL,
    orbit_number           integer,
    quality_score          numeric     NOT NULL CHECK (quality_score BETWEEN 0 AND 1),
    footprint              geometry(Polygon, 4326) NOT NULL,
    won                    boolean     NOT NULL DEFAULT false
);

CREATE INDEX opportunity_views_request ON readmodel.opportunity_views (request_id);

-- +goose Down

DROP TABLE readmodel.opportunity_views;
DROP TABLE readmodel.acquisition_views;
DROP TABLE readmodel.plan_views;
DROP TABLE readmodel.request_views;
DROP TABLE readmodel.stream_cursors;

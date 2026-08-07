-- Shared reference data: customers, the constellation, and TLE sets.
--
-- Read by every service, written only by the seeder. It lives in its own schema
-- rather than in whichever service happened to need it first, because a table
-- three services read has no single owner and pretending otherwise invites the
-- ownership rule to be quietly broken.
--
-- Column types follow contracts/common/primitives.v1.schema.json. Where the
-- schema states a bound, it is a CHECK here as well: the JSON Schema is the
-- authority at the service boundary, and the database is the authority for
-- anything that reaches it by another path.

-- +goose Up

CREATE TABLE reference.customers (
    customer_id  text        PRIMARY KEY
                             CONSTRAINT customers_id_format
                             CHECK (customer_id ~ '^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$'),
    display_name text        NOT NULL CHECK (length(display_name) > 0),
    created_at   timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE reference.customers IS
    'Tasking customers. priority_tier is deliberately NOT here: the contract carries it per request, because the same customer can submit at different tiers.';

CREATE TABLE reference.satellites (
    satellite_id         text        PRIMARY KEY
                                     CONSTRAINT satellites_id_format
                                     CHECK (satellite_id ~ '^[A-Z0-9][A-Z0-9_-]{0,31}$'),
    norad_id             integer     NOT NULL UNIQUE
                                     CHECK (norad_id BETWEEN 1 AND 999999),
    display_name         text        NOT NULL CHECK (length(display_name) > 0),

    -- One of the four JSONB cases ADR-0004 permits: per-mode sensor parameters
    -- genuinely differ by mode and will keep changing as the geometry model is
    -- refined. Shape is sar.v1.schema.json#/$defs/SensorModeParameters, keyed
    -- by ImagingMode, validated at the service boundary by that same schema.
    sensor_modes         jsonb       NOT NULL CHECK (jsonb_typeof(sensor_modes) = 'object'),

    -- Per-orbit imaging-seconds budget. The knapsack dimension of the
    -- allocation problem (M2-03); surfaced on plan metrics as
    -- duty_cycle_budget_s.
    duty_cycle_budget_s  numeric     NOT NULL CHECK (duty_cycle_budget_s > 0),

    created_at           timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX satellites_sensor_modes_gin ON reference.satellites USING gin (sensor_modes);

CREATE TABLE reference.tle_sets (
    tle_set_id   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    satellite_id text        NOT NULL REFERENCES reference.satellites (satellite_id),
    norad_id     integer     NOT NULL CHECK (norad_id BETWEEN 1 AND 999999),

    -- The two lines are stored as columns, not inside the JSONB blob, even
    -- though ADR-0004 lists "raw TLE" among the permitted JSONB cases. A TLE
    -- line is a fixed 69-character record, not semi-structured data, and
    -- epoch-ordered lookup by satellite is the hottest query the feasibility
    -- service makes. ADR-0004's own rule — anything in a WHERE clause on a hot
    -- path is a candidate for promotion to a real column — points this way.
    line1        text        NOT NULL CHECK (length(line1) = 69),
    line2        text        NOT NULL CHECK (length(line2) = 69),

    -- Epoch parsed out of the TLE. This is what tle_age_hours and the
    -- FRESH/AGING/STALE classification are computed FROM; the classification
    -- itself is not stored, because it changes with the passage of time and a
    -- stored copy would be wrong the moment it was written.
    epoch        timestamptz NOT NULL,

    -- Fetch provenance: source URL, HTTP fetch time, catalogue metadata. This
    -- is the genuinely semi-structured part of a TLE record and the part
    -- ADR-0004's "raw TLE" case is really about.
    source       jsonb       NOT NULL DEFAULT '{}'::jsonb,

    fetched_at   timestamptz NOT NULL DEFAULT now(),

    -- Re-fetching an unchanged TLE must be a no-op, not a duplicate row.
    CONSTRAINT tle_sets_unique_epoch UNIQUE (satellite_id, epoch)
);

-- The feasibility service's hot path: newest TLE at or before a given instant,
-- for one satellite. DESC because it always wants the latest.
CREATE INDEX tle_sets_satellite_epoch ON reference.tle_sets (satellite_id, epoch DESC);

-- +goose Down

DROP TABLE reference.tle_sets;
DROP TABLE reference.satellites;
DROP TABLE reference.customers;

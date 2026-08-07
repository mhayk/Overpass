-- Runs once, on an empty data directory only.
--
-- Extensions and schema namespaces only. Table definitions belong in versioned
-- migrations under db/migrations (issue #13) rather than here: this file runs
-- exactly once ever, so anything in it can never be changed on an existing
-- database, which makes it the wrong place for anything that will evolve.

-- Target and footprint geometry, with GiST indexes for containment and
-- intersection queries. See ADR-0004.
CREATE EXTENSION IF NOT EXISTS postgis;

-- gen_random_uuid(), used for identifiers generated database-side.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- btree_gist is what makes the central invariant enforceable. The exclusion
-- constraint on acquisitions needs to combine an equality operator on
-- satellite_id with an overlap operator on a tstzrange:
--
--   EXCLUDE USING gist (satellite_id WITH =, window WITH &&)
--
-- Plain GiST has no equality operator class for scalars; btree_gist supplies
-- it. Without this extension the constraint cannot be created at all, and the
-- non-overlap guarantee falls back to application logic — exactly what ADR-0003
-- and ADR-0004 exist to avoid.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- Query statistics, for finding what actually got slow under load testing
-- rather than guessing. Requires shared_preload_libraries; harmless if absent.
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- Schema per service. Ownership is explicit and a service cannot accidentally
-- write another's tables, without paying for a second database instance.
CREATE SCHEMA IF NOT EXISTS tasking;
CREATE SCHEMA IF NOT EXISTS feasibility;
CREATE SCHEMA IF NOT EXISTS planning;
CREATE SCHEMA IF NOT EXISTS readmodel;
-- Shared reference data: satellites, tle_sets, customers. Read by everyone,
-- written by the seeder.
CREATE SCHEMA IF NOT EXISTS reference;

COMMENT ON SCHEMA tasking     IS 'tasking-api write model: requests, idempotency_keys, outbox';
COMMENT ON SCHEMA feasibility IS 'feasibility-service: opportunities, processed_events';
COMMENT ON SCHEMA planning    IS 'planner-service: collection_plans, acquisitions, outbox';
COMMENT ON SCHEMA readmodel   IS 'plan-gateway: materialised views, eventually consistent';
COMMENT ON SCHEMA reference   IS 'shared reference data: satellites, tle_sets, customers';

-- tasking-api write model: requests, HTTP idempotency keys, and the outbox.
--
-- Two non-negotiables from CLAUDE.md are structural here rather than
-- conventional: every publish goes through the outbox, and every inbound
-- submission is idempotent. Both get a table, and both are useless unless the
-- service writes them in the same transaction as the business row — which is
-- the point of them being in this schema rather than somewhere shared.

-- +goose Up

CREATE TABLE tasking.tasking_requests (
    request_id     uuid        PRIMARY KEY,
    customer_id    text        NOT NULL REFERENCES reference.customers (customer_id),
    target_name    text        NOT NULL CHECK (length(target_name) > 0),

    -- PostGIS, not JSONB. ADR-0004 lists footprint among the permitted JSONB
    -- cases, but its own driver #3 is spatial query support, and target
    -- containment against an opportunity footprint is exactly that query.
    -- Geometry rather than Point because TargetGeometry is a Point or a
    -- Polygon; SRID 4326 is WGS84, which is what the contracts specify.
    target         geometry(Geometry, 4326) NOT NULL
                                CONSTRAINT tasking_requests_target_kind
                                CHECK (ST_GeometryType(target) IN ('ST_Point', 'ST_Polygon')),

    -- The customer's acceptable collection window. Bounded on both sides: an
    -- unbounded request has no deadline, and DEADLINE_PASSED would be
    -- unreachable for it.
    --
    -- Named request_window, not window, because WINDOW is a reserved word in
    -- PostgreSQL — `window tstzrange` is a syntax error, not a warning. The
    -- alternative is double-quoting the identifier in every query that ever
    -- touches it, and an identifier that must be quoted forever is a trap for
    -- whoever writes the next query. The contract field stays `window`; the
    -- mapping is one line in the repository layer.
    request_window tstzrange   NOT NULL
                                CONSTRAINT tasking_requests_window_bounded
                                CHECK (NOT isempty(request_window)
                                       AND lower(request_window) IS NOT NULL
                                       AND upper(request_window) IS NOT NULL),

    priority_tier  text        NOT NULL
                                CHECK (priority_tier IN ('GOVERNMENT', 'CIVIL_PROTECTION',
                                                         'COMMERCIAL', 'BEST_EFFORT')),
    bid_credits    bigint      NOT NULL CHECK (bid_credits BETWEEN 0 AND 100000000),

    -- At least one mode, every entry a valid ImagingMode. An empty array would
    -- mean "no mode is acceptable", which is a request that can never be
    -- fulfilled and should have been rejected at the boundary.
    requested_modes text[]     NOT NULL
                                CONSTRAINT tasking_requests_modes_valid
                                CHECK (array_length(requested_modes, 1) >= 1
                                       AND requested_modes <@ ARRAY['SPOTLIGHT', 'STRIPMAP', 'SCAN']::text[]),

    -- AcquisitionConstraints: every field optional, customer-supplied narrowing
    -- of the acceptable geometry. Semi-structured and volatile — a JSONB case.
    constraints    jsonb       NOT NULL DEFAULT '{}'::jsonb
                                CHECK (jsonb_typeof(constraints) = 'object'),

    -- The lifecycle from the OpenAPI description. Transitions are the service's
    -- job (M1-07); the database's job is that no other value can ever appear.
    state          text        NOT NULL DEFAULT 'RECEIVED'
                                CHECK (state IN ('RECEIVED', 'AWAITING_PLANNING', 'PLANNED',
                                                 'ACQUIRED', 'INFEASIBLE', 'REJECTED',
                                                 'EXPIRED', 'CANCELLED')),

    submitted_at   timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX tasking_requests_target_gist ON tasking.tasking_requests USING gist (target);
CREATE INDEX tasking_requests_window_gist ON tasking.tasking_requests USING gist (request_window);
CREATE INDEX tasking_requests_constraints_gin ON tasking.tasking_requests USING gin (constraints);

-- Cursor pagination on list, per M0-05. The cursor is (submitted_at,
-- request_id) so the sort is total and a page boundary cannot repeat or skip a
-- row when two requests share a timestamp.
CREATE INDEX tasking_requests_customer_cursor
    ON tasking.tasking_requests (customer_id, submitted_at DESC, request_id DESC);

-- The planner and the expiry sweep both scan by state.
CREATE INDEX tasking_requests_state ON tasking.tasking_requests (state)
    WHERE state IN ('RECEIVED', 'AWAITING_PLANNING', 'PLANNED');

CREATE TABLE tasking.idempotency_keys (
    -- Scoped per customer, not global. A global key space lets one customer's
    -- chosen key collide with another's and silently replay someone else's
    -- response, which is a data leak wearing an idempotency hat.
    customer_id     text        NOT NULL REFERENCES reference.customers (customer_id),
    idempotency_key text        NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),

    -- Same key + different body is 409, not a replay. That requires remembering
    -- what the first body was; a digest is enough and avoids storing the
    -- payload twice.
    request_digest  bytea       NOT NULL CHECK (length(request_digest) = 32),

    request_id      uuid        REFERENCES tasking.tasking_requests (request_id),

    -- The recorded response, replayed verbatim on a repeat. Nullable because
    -- the row is claimed before the response exists — that claim is what makes
    -- two concurrent identical submissions serialise instead of both inserting.
    response_status integer     CHECK (response_status BETWEEN 100 AND 599),
    response_body   jsonb,

    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,

    PRIMARY KEY (customer_id, idempotency_key)
);

CREATE INDEX idempotency_keys_expiry ON tasking.idempotency_keys (expires_at);

-- The transactional outbox. Identical in shape in every publishing schema, and
-- deliberately duplicated rather than shared: a single outbox table would be a
-- cross-service write point and a contention hotspot, and it would break the
-- rule that a service writes only its own schema.
CREATE TABLE tasking.outbox (
    -- bigserial, not uuid: the relay publishes in insertion order and a
    -- monotonic key is what makes "everything after id N" a cheap query.
    id             bigserial   PRIMARY KEY,

    event_id       uuid        NOT NULL UNIQUE,
    event_type     text        NOT NULL CHECK (length(event_type) > 0),
    schema_version text        NOT NULL,
    subject        text        NOT NULL CHECK (length(subject) > 0),

    payload        jsonb       NOT NULL CHECK (jsonb_typeof(payload) = 'object'),

    -- W3C traceparent lives here. Carrying it through the outbox is what makes
    -- the trace survive the async hop; dropping it is how a distributed trace
    -- silently becomes two unrelated traces.
    headers        jsonb       NOT NULL DEFAULT '{}'::jsonb,

    occurred_at    timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz,
    attempts       integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error     text
);

-- The relay's only hot query. Partial, so the index holds unpublished rows
-- only and does not grow with the history of everything ever sent.
CREATE INDEX outbox_unpublished ON tasking.outbox (id) WHERE published_at IS NULL;

-- +goose Down

DROP TABLE tasking.outbox;
DROP TABLE tasking.idempotency_keys;
DROP TABLE tasking.tasking_requests;

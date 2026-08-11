-- The execution simulator's own schema (#62).
--
-- Schema per service, as every other consumer here has: the simulator owns its
-- idempotency ledger and its outbox and nothing else. It reads
-- reference.tle_sets, which is reference data every service reads, and it reads
-- nothing belonging to tasking, feasibility, planning or readmodel.
--
-- NO TABLE OF EXECUTIONS, and that is deliberate. The simulator is a producer:
-- it decides what happened and publishes it. The record of what happened
-- belongs to the read model, which folds acquisition.executed.v1 like every
-- other event. A private executions table would be a second answer to "did this
-- acquisition succeed", and the first thing to go stale.

-- +goose Up

CREATE SCHEMA IF NOT EXISTS simulator;

-- The idempotent-consumer ledger. Identical in shape to every other consuming
-- schema, duplicated for the same reason the outbox is: one service's ledger is
-- not another's, and sharing one would make a redelivery to one consumer look
-- already-processed to the rest.
--
-- Keyed by (consumer, event_id), so a service running several durable consumers
-- keeps them independent.
CREATE TABLE simulator.processed_events (
    consumer     text        NOT NULL CHECK (length(consumer) > 0),
    event_id     uuid        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

CREATE TABLE simulator.outbox (
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

-- Partial, because the relay only ever asks for what has not been published and
-- the published rows are the overwhelming majority within minutes.
CREATE INDEX outbox_unpublished_simulator
    ON simulator.outbox (id) WHERE published_at IS NULL;

-- +goose Down

DROP TABLE simulator.outbox;
DROP TABLE simulator.processed_events;
DROP SCHEMA simulator;

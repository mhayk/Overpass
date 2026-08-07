-- plan-gateway: the idempotent-consumer ledger only.
--
-- The read-model projections themselves belong to M1-14 and are deliberately
-- not here. Guessing their shape now would mean inventing the read API before
-- #78 has frozen it in a contract, which is the exact failure ADR-0013 exists
-- to prevent.
--
-- What IS here is the ledger, because "every consumer is idempotent" is a
-- non-negotiable in CLAUDE.md and plan-gateway is a consumer from its first
-- line of code. A service that cannot record what it has already handled is
-- not idempotent, and adding the table later means shipping a window in which
-- it was not.

-- +goose Up

CREATE TABLE readmodel.processed_events (
    consumer     text        NOT NULL CHECK (length(consumer) > 0),
    event_id     uuid        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

-- +goose Down

DROP TABLE readmodel.processed_events;

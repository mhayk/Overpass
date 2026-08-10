-- A request that feasibility definitively refused had nowhere to be recorded.
--
-- feasibility.failed.v1 is published, reaches the FEASIBILITY stream, and is
-- consumed by a projector that folds five subjects and never included it. The
-- request stayed at RECEIVED forever: no error, no dead letter, no metric
-- moving, and a UI showing a request that never resolves — indistinguishable
-- from a pipeline that lost it. See #207.
--
-- Stored as jsonb beside `unfulfilment` and for the same reason: the served
-- object is the event's object, so the reason a customer reads is literally the
-- one feasibility emitted. A column per field would be a second schema to keep
-- in step with the first, and #201 is what that costs.
--
-- SEPARATE FROM `unfulfilment`, NOT A SHARED "refusal" COLUMN. The two are
-- different answers. An unfulfilment means the request competed and lost — it
-- ages, gains fairness weight, and comes back. An infeasibility means there was
-- nothing to compete for. One column would force every reader to branch on the
-- reason code to know which question it was answering, and the first reader to
-- forget would tell a customer to bid more on a target no satellite can see.

-- +goose Up

ALTER TABLE readmodel.request_views
    ADD COLUMN infeasibility jsonb;

COMMENT ON COLUMN readmodel.request_views.infeasibility IS
    'feasibility.failed.v1 data, verbatim, for NON-RETRYABLE failures only. '
    'A retryable failure is a statement about our ability to answer rather '
    'than about the physics; it returns to the stream with backoff and must '
    'not be shown to a customer as a verdict.';

-- Partial, because the interesting query is "which requests are infeasible and
-- why", and the overwhelming majority of rows will never have one. Indexing the
-- nulls would be paying for the common case to serve the rare one.
CREATE INDEX request_views_infeasible
    ON readmodel.request_views (request_id)
    WHERE infeasibility IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS readmodel.request_views_infeasible;
ALTER TABLE readmodel.request_views
    DROP COLUMN IF EXISTS infeasibility;

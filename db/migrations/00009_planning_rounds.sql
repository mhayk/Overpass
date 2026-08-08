-- planner-service: the round ledger.
--
-- 00004 gave the planner what it writes and 00006 what it reads. Neither gave a
-- round anywhere to exist. `planning.collection_plans.round_id` has pointed at
-- nothing since M1-01 — not a dangling reference, since it carries no foreign
-- key, but a column naming a concept the schema had not modelled.
--
-- Two things force the table now, and the second is the one that matters.
--
-- FIRST, the round is an audit record in its own right.
-- planning.round.triggered.v1 describes itself as "the audit record of a
-- scheduling decision boundary — it says what was on the table before anything
-- was decided, which is what makes a committed plan reviewable rather than
-- merely observable". A record that lives only as a published event is one the
-- planner cannot read back.
--
-- SECOND, and this is the load-bearing reason: ADR-0014 fires a round when
-- candidates for a bucket have stopped arriving for a quiet period, or when the
-- bucket has been dirty for a ceiling, whichever comes first. Both halves need
-- to know WHEN THIS BUCKET WAS LAST PLANNED, and a plan cannot answer it. A
-- round is allowed to trigger without committing a plan — M2-01 opens rounds,
-- M2-04 commits them — so between those two issues a bucket that has been
-- planned repeatedly would look untouched, and the ceiling would refire it
-- forever.
--
-- That is not a transitional concern that disappears when M2-04 lands. ADR-0014
-- is explicit that the dirty flag is cleared by a round RUNNING, never by
-- candidates being allocated, precisely so a permanently held candidate cannot
-- keep its bucket dirty and hot-loop the advisory lock. Deriving dirtiness from
-- committed plans would reintroduce exactly that loop for any round that
-- commits nothing.
--
-- So: dirtiness is derived, not stored. A bucket is dirty when a candidate for
-- it arrived after the most recent round over it. There is no `is_dirty` column
-- and no timer state on disk, which means a planner restart loses nothing and
-- two planners cannot disagree about what is pending.

-- +goose Up

CREATE TABLE planning.rounds (
    -- Also the idempotency key. A round is emitted through the outbox inside
    -- the transaction that records it, so a retried trigger collides here
    -- rather than publishing a second round.v1 for the same decision boundary.
    round_id     uuid        PRIMARY KEY,

    satellite_id text        NOT NULL REFERENCES reference.satellites (satellite_id),

    -- Round identity is (satellite_id, lower(bucket)) — the pair the contract
    -- names, and the advisory-lock key. Stored as the full range rather than
    -- just the start so it matches planning.collection_plans.bucket exactly;
    -- two tables describing the same bucket with different types is a join
    -- waiting to be written wrongly.
    bucket       tstzrange   NOT NULL
                             CONSTRAINT rounds_bucket_bounded
                             CHECK (NOT isempty(bucket)
                                    AND lower(bucket) IS NOT NULL
                                    AND upper(bucket) IS NOT NULL),

    -- DELIBERATELY NOT UNIQUE on (satellite_id, bucket). A bucket is planned
    -- many times over its life — that is what supersession is — and a unique
    -- constraint here would make re-planning impossible. Concurrency is the
    -- advisory lock's job, not this table's, and
    -- collection_plans_unique_version is the backstop that says so if the lock
    -- ever fails.

    -- Mirrors the enum in
    -- contracts/events/planning.round.triggered.v1.schema.json. Two places now
    -- state the same closed set, and they can drift: adding a trigger to the
    -- contract without touching this CHECK produces a valid event the database
    -- refuses to record, and the failure appears at INSERT rather than at
    -- validation. The contract is the authority per CLAUDE.md; this is a
    -- narrower restatement of it and must never be the wider one.
    trigger      text        NOT NULL
                             CHECK (trigger IN ('CADENCE', 'OPPORTUNITY_DEBOUNCE',
                                                'MANUAL', 'REPLAN')),

    -- The policy this round ran under, so a committed plan is attributable to a
    -- strategy after the fact. ADR-0007 turns "which algorithm?" into a
    -- measurement, and this column is where the measurement is recorded per
    -- round rather than inferred.
    --
    -- Not a CHECK against the four policy names, unlike trigger above. The
    -- policy set is expected to change while the trigger set is not, and a
    -- benchmark harness that has to migrate the schema to try a variant is a
    -- harness nobody runs.
    policy       text        NOT NULL CHECK (length(policy) > 0),

    candidate_opportunity_count integer NOT NULL CHECK (candidate_opportunity_count >= 0),

    -- The conservation ledger, and the reason this column is not derivable.
    --
    -- The contract states that every id here "must appear either as an
    -- acquisition in the committed plan or as a planning.request.unfulfilled.v1",
    -- with a contract test asserting it. Checking that after the fact requires
    -- knowing who was actually entered in the round, and the candidate set
    -- changes between rounds — reconstructing it later from
    -- candidate_opportunities would answer a different question.
    --
    -- HELD CANDIDATES ARE ABSENT, per ADR-0014. A candidate whose request
    -- snapshot has not landed has no bid, no tier and no deadline; it can become
    -- neither an acquisition nor an unfulfilment, so listing it would fail the
    -- conservation test, and inventing a reason code for it would tell a
    -- customer they lost a competition they were never entered in.
    --
    -- An array rather than a join table: this is a frozen snapshot of one
    -- round's input, written once and read whole, never queried by element. The
    -- contract caps it at 5000. If M2-15 later needs "which rounds did request X
    -- compete in?", that is a GIN index on this column, not a new table.
    candidate_request_ids uuid[] NOT NULL,

    -- Carried on the event, so recorded here: the budget the round allocated
    -- against. Stored rather than recomputed because the satellite's configured
    -- budget can change, and a round must stay explicable against the number it
    -- actually used.
    duty_cycle_budget_s numeric NOT NULL CHECK (duty_cycle_budget_s >= 0),

    -- The plan this round is replacing, set exactly when trigger is REPLAN.
    --
    -- A REAL foreign key here, unlike the deliberate absence of one in
    -- planning.candidate_opportunities, and the difference is worth stating
    -- because the two look inconsistent side by side. There, the referenced row
    -- may legitimately not exist yet: the events race, so a foreign key would
    -- turn benign out-of-order arrival into a retry storm. Here, the referenced
    -- plan is the live plan being replaced — the round read it moments earlier,
    -- under the advisory lock, to decide it was a REPLAN at all. It cannot be
    -- absent without the round being wrong.
    supersedes_plan_id uuid REFERENCES planning.collection_plans (plan_id),

    CONSTRAINT rounds_supersedes_iff_replan
        CHECK ((trigger = 'REPLAN') = (supersedes_plan_id IS NOT NULL)),

    -- When the round opened. This is the value the debounce reads back, so it
    -- is the planner's clock at trigger time and not a default: a round whose
    -- recorded time drifted from the event it published would make the ceiling
    -- fire against the wrong instant.
    triggered_at timestamptz NOT NULL,

    created_at   timestamptz NOT NULL DEFAULT now()
);

-- The index the trigger loop lives on.
--
-- Both halves of ADR-0014's firing rule reduce to one question per bucket —
-- what is the most recent triggered_at for this (satellite, bucket start)? —
-- and that question is asked for every dirty bucket on every tick. DESC so the
-- answer is the first row of the scan rather than a sort.
--
-- On lower(bucket) rather than on bucket, because the lookup is by the round
-- identity the contract defines, which is the start instant, not by range
-- overlap.
CREATE INDEX rounds_satellite_bucket_start
    ON planning.rounds (satellite_id, lower(bucket), triggered_at DESC);

-- Answers "what happened in round X" from a plan, which is the direction a
-- reviewer reads: collection_plans.round_id carries no foreign key — a plan and
-- its round are written in one transaction, but the round is also allowed to
-- exist without a plan, so the constraint would be true in one direction only.
CREATE INDEX rounds_triggered_at ON planning.rounds (triggered_at DESC);

-- +goose Down

DROP TABLE planning.rounds;

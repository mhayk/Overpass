# Chaos scenarios — design (#50, M3-03)

Approved 2026-08-09.

**Status:** scenario 1 has landed (both halves). Scenarios 2, 3 and 4 are not
written yet, and `docs/performance.md` names them as gaps rather than leaving
them implied. #50 stays open until they are.

## Goal

Kill things at the worst possible moment and require the invariants to hold.
Not "the system survives" — a specific, named failure mode per test, each one
asserting the mechanism the architecture claims protects it.

## What is already proven, and by what

Two of the six acceptance criteria are met by tests written for earlier issues.
Restating them here rather than re-testing them, because a second test of the
same claim is maintenance without coverage:

| Criterion | Test |
| --- | --- |
| Outbox relay killed mid-publish, exactly once on restart | `TestTheRelayPublishesEveryPendingEventExactlyOnceAcrossARestart` (#31) — 1200 rows so the kill lands inside the drain, and it says so when it did not |
| Consumer killed mid-stream loses nothing | `TestAProjectorKilledMidStreamLosesNothing` (#37) |

The gap in the second one is the phrase "mid-**transaction**". Killing between
messages proves durability of the consumer; killing inside the fold's
transaction is what proves there is no PARTIAL state — the ledger claim and the
projection commit together or not at all.

## The four new scenarios

### 1. The advisory lock is released when its holder dies

The one the issue calls out. ADR-0003 chose `pg_advisory_xact_lock` over an
application-level lease *because* Postgres releases it on connection loss, so
there is no expiry logic to get wrong. That is a claim about Postgres, and the
repository's rule is that claims about tool behaviour are executed, not read.

Two tests, because there are two claims:

- **The mechanism.** Connection A takes the lock inside a transaction.
  Connection B kills A's backend (`pg_terminate_backend`). Connection C must
  then take the same lock immediately. Run with a short `lock_timeout` so a
  leaked lock fails the test in seconds instead of hanging the suite.
- **The system.** The planner is killed with `SIGKILL` *while holding the lock*,
  detected by polling `pg_locks` for the bucket's key rather than by sleeping.
  Then: the lock is gone, no round row was committed for that key (the whole
  transaction rolled back), and a second planner plans the same bucket to
  exactly one round with no double allocation.

`pg_locks` polling is what makes the kill deterministic. A timed kill would
sometimes land outside the round and quietly test nothing.

### 2. A consumer killed mid-transaction leaves no partial state

The projector is killed repeatedly while folding a burst. After each kill, the
invariant is checked directly: for every event id, a ledger claim exists if and
only if the projection row exists. Neither half may survive alone — that is
what "one transaction" means, and it is the assertion the mid-stream test does
not make.

### 3. Postgres pool exhausted degrades to 503

Every pool connection is held by an idle transaction from the test, then the
ingress is driven. The claim is that it answers `503` — a documented, retryable
refusal — rather than hanging, 500ing, or writing half a request. Then the
transactions are released and the ingress must recover without a restart.

### 4. NATS restarted under load

The broker is stopped and started while a publisher is running. Consumers must
reconnect and no message may be lost. This needs the container's host port to
survive a restart, so the harness pins it: Docker re-assigns published ports on
start, and without pinning the services would reconnect to a port nobody is
listening on — which would make this test a test of the test.

## Write-up

`docs/performance.md` gains a failure-modes section: one row per scenario, what
was killed, what held, and the mechanism that made it hold. M3-09 owns the rest
of that document; this adds only the section #50 requires.

## Out of scope

Fault injection inside service code (`FAILPOINTS` and friends). Every scenario
here is driven from outside the process — killing, filling, restarting — which
is the only kind of chaos a deployment can also produce.

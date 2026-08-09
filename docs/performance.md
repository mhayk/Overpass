# Performance and failure modes

This document has two halves. The numbers half — throughput, latency, the
breakpoint — is M3-07 through M3-09 and is not written yet. The failure-modes
half is below, and it exists because knowing how a system fails is worth more
than a clean graph of it working.

Every row is a test that kills something on purpose and requires an invariant to
survive. Nothing here is a description of intended behaviour: each line names
the test that produces the evidence, and each test has been watched failing on
the condition it exists to catch.

## Failure modes

| What is killed | What must hold | Mechanism that holds it | Test |
| --- | --- | --- | --- |
| The connection holding a round's advisory lock | Another round takes the same lock immediately | `pg_advisory_xact_lock` is transaction-scoped; Postgres releases it when the backend dies | `TestAnAdvisoryLockDiesWithTheConnectionThatHeldIt` |
| Nothing — a second session simply contends | The second session blocks | The lock is exclusive, which is the other half of the same claim | `TestTheAdvisoryLockActuallyExcludes` |
| The planner, `SIGKILL`, while holding the lock mid-round | No half-planned bucket; the next planner plans it exactly once | One transaction per round: the lock, the reads, the round row and the outbox insert commit together or not at all | `TestAPlannerKilledHoldingTheLockLeavesNoHalfPlannedBucket` |
| The holder of a lock a planner is waiting on | The waiting planner takes over and completes the round | Blocking acquisition, plus release-on-disconnect — no lease, no expiry, no coordination | `TestAPlannerWaitingOnADeadHoldersLockTakesOver` |
| The outbox relay, mid-publish | Every pending event is published exactly once after restart | Mark-after-publish, plus the broker's `Nats-Msg-Id` dedup window | `TestTheRelayPublishesEveryPendingEventExactlyOnceAcrossARestart` |
| A projector, mid-stream | Nothing is lost; events that arrived while it was down are still delivered | Durable pull consumers with explicit ack | `TestAProjectorKilledMidStreamLosesNothing` |
| Duplicates injected at every hop at once | The end state is identical to one clean pass | Idempotency key at ingress, `Nats-Msg-Id` at the broker, `processed_events` at each consumer | `TestDuplicatesInjectedAtEveryHopLeaveTheEndStateIdentical` |
| A consumer's terminal failure | The payload survives the drop and can be replayed | Dead letter published before the Term; Nak if the publish fails (ADR-0017) | `TestADeadLetterIsReplayedBackOntoItsOriginalSubject` |

## The advisory lock, in detail

ADR-0003 chose `pg_advisory_xact_lock` over an application-level lease because
Postgres releases it when the connection dies — no expiry to tune, no renewal to
forget, no lease that outlives its holder. That is a claim about Postgres, and
the claim is now executed rather than believed: the lock is taken, the backend
is terminated, and another session takes the same lock inside two seconds. With
the termination removed, the test fails in exactly two seconds on `lock_timeout`.

The failure this prevents is the worst one available to this design. A leaked
lock is a satellite that can never be planned again — not an error, not an
alert, just a bucket that stays dirty forever while everything else looks
healthy.

Two implementation details worth recording, both found by running the thing
rather than reading about it:

- **`pg_locks` reports the lock halves as OIDs, which are unsigned**, while
  `pg_advisory_xact_lock` takes signed 32-bit keys. The planner's satellite key
  is an FNV-32a hash, so it is negative about half the time, and querying
  `pg_locks` with the signed value fails to encode rather than returning zero
  rows. A test that got this wrong would silently watch the wrong lock.
- **The kill has to be aimed, not timed.** The test polls `pg_locks` every
  millisecond and kills the instant the lock appears. A sleep would land either
  side of a round that takes a few tens of milliseconds, and the test would
  usually exercise a restart while claiming to exercise a kill. When the window
  is missed anyway, the test says so in its output rather than passing quietly.

## Not yet covered

Named here so the gaps are visible rather than implied. All three are part of
M3-03 and are not done:

- NATS restarted under load — consumers reconnect, no message loss. Needs the
  test broker's published port pinned across a restart, or the services
  reconnect to a port nobody is listening on.
- Postgres connection pool exhausted — ingress degrades to `503` rather than
  hanging or writing partial state.
- A consumer killed *inside* its transaction — the mid-stream test proves
  durability, not the absence of partial state. The assertion that matters is
  that a ledger claim exists if and only if its projection row does.

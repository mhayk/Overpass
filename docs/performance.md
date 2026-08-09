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
| A consumer, `SIGKILL`, three times through a 200-event backlog | No delivery is half-applied: a ledger claim exists if and only if its projection does | The claim and the state change are one transaction | `TestAConsumerKilledMidTransactionLeavesNoPartialState` |
| The database, made unreachable under an exhausted pool | Ingress answers 503 promptly, writes nothing, and recovers unaided | A bounded deadline on the submit path; 503 is already the mapping for a submission that could not be stored | `TestIngressRefusesRatherThanHangsWhenTheDatabaseIsUnreachable` |
| The broker, restarted mid-backlog | The consumer reconnects unaided and nothing is lost | Durable pull consumers, file storage, explicit ack | `TestABrokerRestartUnderLoadLosesNothing` |

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
- **The lock is released when Postgres NOTICES the client is gone**, on the
  backend's next socket operation — not at the moment of the kill. Asserting it
  synchronously passed on a laptop and failed in CI with the lock still held
  1.13 seconds after the process died. The test now waits, with a bound tight
  enough that "eventually" still means something an operator could rely on.
- **The kill has to be aimed, not timed.** The test polls `pg_locks` every
  millisecond and kills the instant the lock appears. A sleep would land either
  side of a round that takes a few tens of milliseconds, and the test would
  usually exercise a restart while claiming to exercise a kill. When the window
  is missed anyway, the test says so in its output rather than passing quietly.

## What each scenario had to be taught

None of these worked the first time, and the reasons are the useful part.

**The kills have to land on a working consumer.** The mid-transaction test began
with a 24-event backlog and drained it before the first kill — it passed, having
exercised a restart, and said so in its own log. It runs 200 events now and
kills at a third, a half and two thirds of the way through. Every chaos test
here reports when its window was missed, because a scenario that quietly did not
happen is worse than one that failed.

**Ingress needed a change, not just a test.** Under an exhausted pool the submit
path had no deadline: it waited for a connection that was not coming, and the
symptom was a service that looked alive and answered nothing. 503 was already
the mapping for a submission that could not be stored — what was missing was
anything that made the attempt fail at all. `SUBMIT_TIMEOUT` (5s by default)
supplies it, and the unit test that proves it hangs without one is
`TestASubmissionThatCannotReachTheDatabaseIsRefusedNotHeld`.

**A bucket in the past is not a bucket.** The planner test derived its bucket
by truncating `now + 2h` to a six-hour boundary, which lands in the past for
most of the day — at 21:30 it yields 18:00. An elapsed bucket cannot be flown,
so the planner correctly ignored it and the test waited 150 seconds for a round
that was never going to happen. It passed every afternoon and failed at night,
which is the worst kind of green: sixteen consecutive passes proved only that
sixteen runs happened before six o'clock.

**The test broker's port has to be pinned.** Docker assigns a new host port on
every start, so a restarted broker would have been unreachable at the address
the services already knew — the test would have measured its own setup. It is
pinned to a chosen free port, published on all interfaces: binding it to
127.0.0.1 inside the Docker VM made it unreachable from the test process, which
is how that line was chosen rather than guessed.

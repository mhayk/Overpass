# Performance and failure modes

This document has two halves. The numbers half is below; the failure-modes half
follows it, and it exists because knowing how a system fails is worth more than
a clean graph of it working.

## The environment, stated

An SLO with no stated environment is a number with no meaning, so:

| | |
| --- | --- |
| CPU | Intel Core i9-14900K, 32 logical cores |
| Memory | 31 GiB |
| Kernel | 6.18.33 (WSL2) |
| Docker | 29.6.1 |
| Postgres | 16.4 (postgis/postgis:16-3.4) |
| Load generator | k6 1.4.0, same host as the stack |

**k6 runs on the same machine as the system under test.** That is not ideal —
the generator competes with the services for the same 32 cores — and it is
stated because it bounds what these numbers mean. At 1000 rps k6 used a handful
of VUs and the box was nowhere near saturated, so the effect is small here; it
would not be on a smaller machine.

## Ingress: `POST /v1/tasking-requests`

Three rungs, run in sequence, 30s each. `loadtest/k6/ingress.js`, thresholds as
gates.

| Offered rate | p50 | p90 | p95 | p99 | max | errors |
| --- | --- | --- | --- | --- | --- | --- |
| 10 rps | 2.14ms | 3.90ms | 5.79ms | — | 10.45ms | 0 |
| 100 rps | 1.90ms | 3.01ms | 4.65ms | — | 39.98ms | 0 |
| 1000 rps | 2.25ms | 6.74ms | 9.79ms | 18.26ms | 49.06ms | 0 |

33,302 requests, zero failures, zero dropped iterations.

| Criterion | Target | Measured | |
| --- | --- | --- | --- |
| 1000 rps p95 | < 40ms | **9.79ms** | ✅ |
| 1000 rps p99 | < 120ms | **18.26ms** | ✅ |
| 1000 rps errors | zero | **0 / 33,302** | ✅ |
| 100 rps p95 | < 15ms | **4.65ms** | ✅ |

```
ingress latency vs offered rate          ── p50   ┄┄ p95
 ms
 10 ┤                                                        ┄┄┄┄ 9.79
    ┤                                                   ┄┄┄┄┄
  8 ┤                                              ┄┄┄┄┄
    ┤                                         ┄┄┄┄┄
  6 ┤ 5.79┄┄┄                            ┄┄┄┄┄
    ┤        ┄┄┄┄┄┄┄┄                ┄┄┄┄
  4 ┤              ┄┄┄┄ 4.65 ┄┄┄┄┄┄┄┄
    ┤
  2 ┤ ──── 2.14 ──────── 1.90 ──────────────────── 2.25
    ┤
  0 ┼───────────────┬─────────────────────────────┬──────────────
      10 rps        100 rps                       1000 rps
                         (log scale, 100× range)
```

**The curve is flat, and that is the interesting part.** M3-07 predicted it
would show "the synchronous ingress path degrading while the async path stays
flat". The median does not degrade at all between 10 and 1000 rps — 2.14ms,
1.90ms, 2.25ms — and only the tail grows, p95 roughly doubling from 4.65ms to
9.79ms.

That is ADR-0003 working, measured rather than argued. Ingress does validation,
one insert, one outbox insert, and returns 202. It is O(1) work with no
dependency on constellation size, queue depth, or what the planner is doing, so
there is nothing in it to degrade until the connection pool or the CPU runs
out — and at 1000 rps on this box neither is close. The prediction was wrong in
the right direction.

## End to end: submit to a customer-visible answer

`loadtest/k6/pipeline.js` submits, then polls plan-gateway until the request
reaches a state a customer can act on. The read model is the customer-visible
edge, so it is the edge that is timed — "the event was published" is not
something anyone can see.

Measured on a **cold stack with an empty queue** at 0.1 rps:

| | |
| --- | --- |
| Requests | 6, all resolved |
| Median | 30.3s |
| Min | 25.2s |
| Max | 45.3s |
| p95 | 42.8s |
| p99 | **44.8s** |

| Criterion | Target | Measured | |
| --- | --- | --- | --- |
| End to end p99 at 200 rps | < 5s | **44.8s at 0.1 rps** | ❌ |

**This acceptance criterion is not met, and it is not close.** It is recorded
here rather than quietly redefined, because the reason is the most useful thing
this exercise produced.

### Where the 30 seconds goes

Decomposed from database timestamps and service logs:

| Stage | Cost | Note |
| --- | --- | --- |
| Ingress accept | ~2ms | measured above |
| Feasibility sweep | **3.9s median, 4.7s p95** | SGP4 across nine satellites, per request, single worker |
| Planner round | **< 1ms** | `triggered_at` to `committed_at` rounds to 0.00s |
| Plan committed → visible | **14–54s** | in bursts, tracking projector fetch cadence |

The planner — the component this project exists to show off, the one doing the
actual scheduling — is **not** the bottleneck. Allocation of a real round
completes in under a millisecond. The measured cost is queue time and poll
cadence:

- **One feasibility worker, ~3.9s per request.** That is `0.25 requests per
  second` of sustained capacity for the whole pipeline.
- **Projection hops wait on `FETCH_WAIT`,** 5s by default, across three
  streams. A request's state is derived from events on TASKING, FEASIBILITY and
  PLANNING in order, so a single request can wait three fetch intervals.

### Ingress outruns the pipeline by 4000:1

The two numbers above are the finding:

```
ingress accepts      1000 requests/second
pipeline drains         0.25 requests/second
                     ---------------------
ratio                   4000 : 1
```

This was observed rather than calculated. A 30-second run of the ingress
scenario left **33,337 requests in RECEIVED** and a backlog that would take
roughly **37 hours** to drain at the measured rate.

That is the outbox and JetStream doing exactly what ADR-0003 says they are for:
ingress availability is decoupled from downstream capacity, and a burst is
absorbed rather than refused. It is also the honest limit of that design —
**decoupling converts a throughput problem into a latency problem, and does not
make it go away.** A customer whose request is behind 33,000 others gets a 202
in two milliseconds and an answer a day and a half later.

### What would close the gap

Not tuning. In rough order of effect:

1. **Run more than one feasibility worker.** The work is embarrassingly
   parallel — each request is an independent SGP4 sweep — and the service is
   already an idempotent consumer on a durable queue, which is exactly the
   precondition for scaling out horizontally. This is the single change that
   moves 0.25 rps.
2. **Reduce per-request SGP4 cost.** Nine satellites × the sampling interval
   over a 24h horizon, per request. ADR-0016 fixed the sampling policy; nothing
   has yet profiled the propagation itself.
3. **Shorten the projection hops.** `FETCH_WAIT` at 5s across three streams is
   up to 15s of pure waiting for one request. This is a configuration change
   and the cheapest of the three, but it only addresses the tail after the
   queue is fixed.

None of this is done, and none of it is in M3's scope. It is written down so the
next person does not have to rediscover it.

## Breakpoint: ramping ingress until something gives

`loadtest/k6/breakpoint.js`. Not a gate — it has no thresholds on purpose,
because its output is a number and a failure mode, and a threshold would turn
"find the limit" into "assert the limit has not moved".

Ramped 500 → 10,000 rps offered over two minutes, then to zero, then a steady
50 rps to watch it come back.

| | |
| --- | --- |
| Peak offered | 10,000 rps |
| Peak **achieved** | **1,872 rps** |
| Requests | 308,829 |
| HTTP failures | **0** |
| 503s | **0** |
| Latency under load | median 2.18s, p95 3.27s, **max 3.50s** |
| Dropped iterations | 267,670 |

### The prediction, and how it did

Written into the script before the first run:

| # | Predicted | Actual | |
| --- | --- | --- | --- |
| 1 | Ingress connection pool breaks first | Queueing, yes — but nothing "broke" | ~ |
| 2 | Graceful, via `submitTimeout` returning 503s | Graceful, via **latency**. Zero 503s | ❌ |
| 3 | Between 3,000 and 6,000 rps | **~1,872 rps** | ❌ |
| 4 | Recovery immediate, no restart | Immediate, no restart | ✅ |

**Two of four wrong.** The interesting one is #2.

The failure mode is graceful degradation, as predicted — but by the wrong
mechanism. `submitTimeout` is 5s and the worst request observed took **3.50s**,
so the timeout never fired and not a single request was refused. Every one of
308,829 requests got a 202. The system absorbed a 5× overload by making
everyone wait a bit under the limit at which it would have started saying no.

That is a more fragile shape than the one predicted, and worth naming: the
safety valve was never reached, so its behaviour under real overload is still
unobserved. Latency sat just below the cliff rather than going over it. A
slightly larger ramp, or a slightly slower disk, and the 503s appear — which is
the behaviour that was designed for, and it remains untested at this scale.

### The confound, stated

**267,670 dropped iterations.** k6 could not generate the offered rate, running
8,000 VUs on the same 32 cores as the services it was measuring.

So **1,872 rps is a lower bound on the true ceiling, not the ceiling.** Part of
what was measured is the generator. This cannot be resolved without moving load
generation off the box, and that is the next experiment rather than a
correction to make on paper.

### Recovery

Clean, and better than expected. Thirty seconds after the ramp ended, ingress
was back to **p95 6.06ms** — against a 4.65ms cold baseline, so the same order,
no restart, no intervention.

The detail that matters: it recovered **while 358,045 requests were still
backlogged**. Ingress latency is genuinely independent of queue depth, which is
the strongest form of ADR-0003's claim and the one hardest to argue without a
test. A third of a million messages outstanding and the write path did not
notice.

### What breaks next, if ingress were fixed

A hypothesis, recorded with the same willingness to be wrong:

1. **The load generator, immediately.** Already the binding constraint at
   1,872 rps. Nothing else can be measured until it moves off-host.
2. **Then the ingress pool, this time reaching the 503 path.** Prediction #2
   was not so much wrong as premature — it describes what happens past a load
   this hardware could not offer.
3. **Then Postgres WAL and `tasking.outbox` write amplification.** Every
   accepted request is two inserts, and the outbox row is the larger of them.
   Nothing here has yet been profiled at the database.

The pipeline's bottleneck is not on this list because it is not in question:
one feasibility worker at 0.25 rps, four thousand times slower than ingress,
measured above.

## Planner allocation

The allocation policy benchmark (`make benchmark`,
[docs/policy-benchmark.md](policy-benchmark.md)) measures the round itself
against generated problems:

| Candidates | GREEDY_BY_BID | GREEDY_BY_VALUE_DENSITY | VICKREY_SEALED_BID |
| --- | --- | --- | --- |
| 5000 | 36.7ms | 55.9ms | 130.7ms |

| Criterion | Target | Measured | |
| --- | --- | --- | --- |
| Round with 5000 opportunities p95 | < 800ms | **55.9ms** (configured default) | ✅ |

5000 is the contract's candidate cap, so this is the worst case the schema
permits rather than a convenient number.

## Cold start against the five-minute budget

`scripts/stack-up.sh` enforces two budgets and prints the elapsed time. From
the most recent green run on `main` (GitHub `ubuntu-latest`, two cores — slower
than the machine every other number here came from):

| Phase | Budget | Measured | |
| --- | --- | --- | --- |
| Infrastructure — Postgres, NATS, Tempo, collector, Prometheus, Grafana | 300s | **112s** | ✅ |
| Application services, from a cold image build | 900s | **190s** | ✅ |

The two budgets are separate on purpose. The 300s claim is about
infrastructure; the second pays for four image builds — three multi-stage Go
and one `npm ci` plus a Next.js build — that the first was never about.

The budget is a gate, not a note: `stack-up.sh` exits non-zero when it is
exceeded.

## What was tuned, and what was left on the table

**Tuned to reach these numbers:** nothing.

That is the honest answer and it is worth stating plainly, because a
performance document usually implies a tuning campaign. There was none. No pool
size was changed, no index added, no GOMAXPROCS set, no batch size adjusted.
Every number above is the system as it was already configured, and the ingress
figures cleared their targets by four to six times on the first run.

The one change made during this work was to the MEASUREMENT, not the system:
`pipeline.js` needed a configurable rate, because the sustainable rate is below
one request per second and k6 cannot express that without a `timeUnit`.

**Left on the table**, in the order that would matter:

1. **Feasibility runs one worker.** The single largest number in this document
   — 0.25 rps against ingress's 1000 — and the work is embarrassingly parallel.
   The service is already an idempotent consumer on a durable queue, which is
   the precondition for scaling out; nothing has been done with that.
2. **SGP4 cost per request is unprofiled.** 3.9s across nine satellites is
   accepted here as a measurement, not explained. ADR-0016 settled the sampling
   policy; nobody has looked at the propagation itself.
3. **`FETCH_WAIT` is 5s on three streams.** Up to 15s of pure waiting in the
   end-to-end path. The cheapest available improvement and deliberately not
   taken, because shortening it before fixing the queue would move a number
   without changing the experience.
4. **Load generation shares the box.** It bounded the breakpoint result at
   1,872 rps and is the first thing to fix before any further capacity claim.
5. **The 503 path is unobserved under load.** The overload test never reached
   `submitTimeout`, so the refusal behaviour the bulkhead exists to provide has
   been tested in unit tests and never in anger.

Nothing in this list is scoped to M3. It is written down so the next person
does not have to rediscover it.

## Reproducing

```bash
make up-all && make seed
make loadtest              # both scenarios, thresholds as gates
```

Thresholds fail the build — `make loadtest` exits non-zero on a breach,
verified at exit 99. `ingress.js` carries the acceptance criteria verbatim;
`pipeline.js` carries a 60s regression threshold rather than the 5s SLO, with
the gap recorded above and in the script.

**They are not run in CI, and that is deliberate.** The ingress scenario was
added to the cold-start workflow and removed again after it failed on
`dropped_iterations` and on the 100 rps rung. That was not a regression in the
service: the runner is already hosting Postgres, NATS, Tempo, Prometheus,
Grafana and four application services, so k6 competes with all of them and
reports the latency of a queue it created itself. `dropped_iterations` is the
evidence, and it holds whatever the core count — k6 could not offer the load,
so the thresholds described a load that was never applied.

The choice was to lower the numbers until they passed, or to admit the
measurement does not belong there. Lowering them is how a gate stops meaning
anything — still green, no longer evidence. It is the same confound recorded
above for the breakpoint test, in its most extreme form, and #190 is the issue
for fixing it by moving load generation off-host.



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

## Outbound waits, audited

#51 asks for "timeouts on every outbound call, no unbounded waits anywhere".
Audited rather than asserted — every outbound path, and what actually bounds it:

| Path | Bound | How it is set |
| --- | --- | --- |
| Submit → Postgres | 5s | `SUBMIT_TIMEOUT`, added for #50 — before it, an exhausted pool held the request open forever |
| Readiness → Postgres | 2s | `READINESS_TIMEOUT` |
| Outbox relay → JetStream | 5s | An explicit context over the whole publish |
| Consumer fetch → JetStream | per service | A derived context on every `Fetch`; an idle stream is a timeout, not a failure |
| Celestrak → HTTP | 10s per call, and a breaker after three failures | `timeout_s` plus `feasibility.resilience.Breaker` |
| Shutdown | `SHUTDOWN_TIMEOUT` | A deadline on `context.Background()`, which is the one correct use of it here |

The finding worth keeping: **an unoptioned JetStream publish was never
unbounded, but its bound was not the one the call site suggested.** nats.go
waits `defaultRequestWait` (5s) and retries `DefaultPubRetryAttempts` (2) more
times with `DefaultPubRetryWait` (250ms) between — about fifteen seconds per
message, multiplied by the batch the relay is draining. Read out of the
library's source rather than its documentation. It is now one explicit context
over the whole call, retries included, because a relay's job when the broker is
slow is to give up quickly and leave the rows for the next tick.

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

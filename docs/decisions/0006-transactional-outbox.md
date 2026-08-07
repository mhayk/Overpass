# 0006 — Publish through a transactional outbox, not directly from the handler

- **Status:** accepted
- **Date:** 2026-08-08
- **Deciders:** Mhayk Whandson

## Context and problem statement

Every service in Overpass changes state in Postgres and then tells the rest of
the system about it over NATS. Those are two different systems, and there is no
transaction spanning them.

That is the dual-write problem, and it has two failure modes, both silent:

- **Persist, then crash before publishing.** The request exists and nobody
  downstream knows. The customer's request sits in `RECEIVED` forever, and the
  only symptom is an image that never arrives.
- **Publish, then roll back.** An event announces a fact that never became true.
  The planner allocates against an opportunity for a request that does not
  exist, and the inconsistency is discovered much later by someone holding a
  plan they cannot explain.

Neither logs anything. Neither fails a health check. Both are found by a human
noticing that two parts of the system disagree.

The question: **how does a service change state and publish the corresponding
event without a window in which one happened and the other did not?**

## Decision drivers

1. **Silent failure is the thing to eliminate.** A loud failure that retries is
   acceptable; a quiet divergence between the database and the event log is not.
2. **Consumers already tolerate duplicates.** Every consumer in this system is
   idempotent by [ADR-0008](0008-idempotency.md), so at-least-once delivery costs
   nothing extra.
3. **Acceptance must survive a broker outage.** `POST /v1/tasking-requests`
   returning 503 because NATS is down would turn a recoverable delay into
   refused customer traffic.
4. **The trace must survive the hop.** A distributed trace that stops at the
   async boundary is a trace of the synchronous part only, which is the easy
   part.
5. **One Postgres already exists.** Anything that needs a second store to solve
   this loses on [ADR-0005](0005-docker-compose-over-kubernetes.md)'s cold-start
   budget before it is even evaluated.

## Considered options

1. **Publish inside the business transaction** — call NATS between BEGIN and
   COMMIT
2. **Publish after commit**, in the same handler
3. **Transactional outbox** — insert the event as a row in the same transaction,
   and drain it with a relay
4. **Change data capture** — read the Postgres WAL and derive events from it

## Decision outcome

Chosen: **Option 3, the transactional outbox.** The event is INSERTed into an
`outbox` table inside the same transaction as the state change, so it exists if
and only if the state change does. A relay polls the table, publishes, and marks
the row sent.

What that buys is precise: the dual write becomes a single write. There is no
longer any instant at which one of the two has happened and the other has not,
because there is only one of them. Publication moves out of the critical path
and becomes a separate, retryable concern.

The cost is that publication is now at-least-once rather than exactly-once, and
that cost is already paid: every consumer deduplicates on `event_id`, which the
outbox row keeps stable across every retry.

### How it is built

- **`enqueue` takes a cursor, not a connection.** There is no connection in
  scope to open a second transaction with, so the event structurally cannot
  commit separately from the result it describes. An interface with `Save` and
  `Publish` as separate calls would permit the bug; this one does not.
- **The relay's transaction spans the publish.** Claim with `FOR UPDATE SKIP
  LOCKED`, publish, mark, commit. Crash after publishing and before committing
  and the row goes again. Mark-first would turn the same crash into a lost
  event.
- **`SKIP LOCKED` so relays need no coordination.** Two instances partition the
  work rather than duplicating it, with no leader election and no lock service.
- **`traceparent` is captured at WRITE time**, in the handler, and carried on
  the row. Capturing it in the relay attributes the event to the poll loop and
  severs the trace at exactly the hop this project claims to preserve.
- **Backoff is exponential and capped.** A broker that is down should not be
  hammered once per poll interval for as long as it stays down.
- **Relay start-up failure is a warning, not fatal.** If refusing to start when
  NATS is unreachable were the behaviour, the outbox would have bought nothing.

### Consequences

**Good**

- The two silent failure modes are gone. The event and the state change commit
  or fail together, and there is no third outcome.
- Ingress survives a broker outage. Requests are persisted and drain when the
  relay reconnects, which is the property that lets `/readyz` deliberately not
  check NATS.
- The relay is horizontally scalable for free, because `SKIP LOCKED` needs no
  coordination.
- Publication failures are visible and bounded: `attempts` and `last_error` sit
  on the row, and the count of unpublished rows is the lag metric to alert on.

**Bad**

- **Latency between commit and publish.** An event is not on the wire the
  instant it is written; it waits for a poll. The poll interval is 250ms, so the
  added latency is small, but it is not zero and it is a real change to the
  system's timing.
- The outbox table grows and needs pruning. Published rows are kept for now
  because they are useful for debugging, and that is a decision with an
  expiry date rather than a permanent answer.
- A relay holding row locks across a network publish means a slow broker holds
  locks. The batch is small for exactly that reason, but it is a coupling
  between broker latency and database lock duration that did not exist before.
- Two relays are now two processes to run, monitor and reason about. In M1 they
  run in-process with their services, which is a deliberate simplification and
  will want revisiting when either becomes a bottleneck.

**Neutral**

- Ordering is per-subject and not global, which was already true of JetStream.
  Nothing in this system assumes causal ordering from arrival order.

### Confirmation

- **The event and the result commit together:** a handler that fails after
  enqueuing leaves no outbox row. Asserted in both services.
- **A restart publishes everything exactly once:** a relay is killed mid-flight
  and restarted; every event is published once, checked by id rather than by
  count.
- **Two relays do not duplicate:** six events drained concurrently by two
  instances are published six times in total, not twelve.
- **The trace survives:** the `traceparent` written by the HTTP handler is read
  back off the NATS stream in the feasibility relay's tests.
- **The decision is wrong** if the relay's lag becomes the system's dominant
  latency, or if the outbox table's growth forces a pruning policy complex
  enough to have its own failure modes. Either would mean the pattern is being
  asked to do more than it should.

## Pros and cons of the options

### Option 1 — Publish inside the business transaction

- Good, because it looks atomic and requires no new table.
- **Bad, because it is not atomic at all.** The publish succeeds and the
  transaction can still roll back, announcing a fact that never became true.
  This is the worse of the two dual-write failures, and it is the one this
  option makes more likely rather than less.
- Bad, because it puts broker latency inside a database transaction.

### Option 2 — Publish after commit, in the handler

- Good, because it is the simplest thing that mostly works, and most systems do
  it.
- Bad, because the crash window is real and the failure is silent: state
  changed, nobody told. Under load and during deploys — exactly when crashes
  happen — it is not rare.
- Bad, because the handler now owns retry logic, and a retry loop inside a
  request handler either blocks the response or is abandoned when it returns.

### Option 3 — Transactional outbox (chosen)

- Good, as above.
- Bad, as above: added latency, a table to prune, locks held across a network
  call.

### Option 4 — Change data capture from the WAL

- Good, because it needs no outbox table and no application discipline at all —
  the events are derived from what actually committed, which is the strongest
  possible guarantee.
- Good, because it cannot be bypassed by a developer who forgets.
- **Bad, because the events would be row diffs, not domain events.** The
  contracts in `contracts/events/` are deliberately not table shapes;
  reconstructing `feasibility.opportunities.computed.v1` from a WAL stream means
  writing the mapping anyway, in a place with less context.
- Bad, because it needs Debezium or equivalent: another container, another
  connector to configure, and a replication slot whose failure mode is a
  Postgres that cannot recycle WAL. That is a large addition against the
  cold-start budget.
- Genuinely the stronger guarantee, and the right answer at a scale this project
  does not have.

## More information

- The implementation: `services/tasking-api/internal/adapter/outbox/`,
  `services/feasibility/src/feasibility/messaging/`
- The pattern as the contracts state it: `contracts/nats/topology.md`
- The consumer side of the bargain: [ADR-0008](0008-idempotency.md)
- Why `/readyz` does not check NATS: `contracts/openapi/tasking-api.v1.yaml`

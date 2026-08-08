# NATS JetStream topology

Streams, subjects, consumers, and the DLQ. This file is part of the contract:
changing a subject or a consumer's delivery policy changes system behaviour as
surely as changing a schema does, so it lives beside the schemas rather than in
a service's config.

Broker choice and its alternatives:
[ADR-0002](../../docs/decisions/0002-nats-jetstream-over-kafka-rabbitmq.md).

## Subject naming

```
<domain>.<aggregate>.<event>.v<major>
```

The subject is byte-identical to the event's `event_type` field. That redundancy
is deliberate: consumers dispatch on `event_type`, not on the subject they
received the message on, so replaying an event log from a file behaves
identically to replaying from the stream. A consumer that dispatches on subject
silently breaks the moment you replay from anywhere else.

| Subject | Producer | Primary consumers |
| --- | --- | --- |
| `tasking.request.received.v1` | tasking-api | feasibility-service, plan-gateway |
| `tasking.request.rejected.v1` | tasking-api | plan-gateway |
| `feasibility.opportunities.computed.v1` | feasibility-service | planner-service, plan-gateway |
| `feasibility.ephemeris.computed.v1` | feasibility-service | plan-gateway |
| `feasibility.failed.v1` | feasibility-service | tasking-api (state machine), plan-gateway |
| `planning.round.triggered.v1` | planner-service | plan-gateway |
| `planning.plan.committed.v1` | planner-service | plan-gateway, acquisition-simulator |
| `planning.request.unfulfilled.v1` | planner-service | tasking-api (state machine), plan-gateway |
| `acquisition.executed.v1` | acquisition-simulator | plan-gateway, tasking-api |

**One of these is not caused by a message.**
`feasibility.ephemeris.computed.v1` is produced by a timer sweeping the
constellation, not by a consumer reacting to a delivery. It therefore has no
upstream `event_id` to inherit deduplication from, and derives its own as a
UUIDv5 over `(satellite_id, horizon.start, tle_epoch)`. That keeps the envelope's
rule intact — the same logical event always carries the same `event_id` — and it
is what makes a sweep that overlaps its own previous horizon a no-op at the
outbox rather than a duplicate on the wire.

**The major version is in the subject, not only in the payload.** A `.v2` event
is published to a different subject, so v1 and v2 consumers coexist without
either one seeing messages it cannot parse. Versioning inside the payload alone
would force every consumer to parse before it could decide whether it can parse.

## Streams

Three streams, split by producing domain rather than one stream per subject.
Per-subject streams would multiply operational surface for no gain; one giant
stream would tie unrelated retention and replay decisions together.

| Stream | Subjects | Storage | Retention | Max age | Replicas |
| --- | --- | --- | --- | --- | --- |
| `TASKING` | `tasking.>` | file | limits | 168h | 1 |
| `FEASIBILITY` | `feasibility.>` | file | limits | 72h | 1 |
| `PLANNING` | `planning.>`, `acquisition.>` | file | limits | 168h | 1 |

Notes on the choices:

- **`limits` retention, not `workqueue`.** Multiple consumer groups read the same
  subjects — `plan-gateway` materialises every event, and other services react to
  a subset. `workqueue` would delete a message once any one consumer acked it,
  which silently breaks fan-out.
- **`FEASIBILITY` has shorter retention** because its payloads are by far the
  largest (thousands of opportunities per request, and a sampled ephemeris track
  per satellite per bucket) and its replay value decays fastest — a three-day-old
  opportunity set is describing access windows that have already passed, and a
  three-day-old ephemeris bucket is describing where a satellite already was.
- **`replicas: 1`** because the deployment target is a single machine
  ([ADR-0005](../../docs/decisions/0005-docker-compose-over-kubernetes.md)).
  Production would be 3, and this is the line that would change.
- **File storage, not memory**, everywhere. Memory storage would make a broker
  restart lose in-flight work, which would make the durability claims false.

## Consumers

All consumers are **durable pull consumers with explicit ack**. Push consumers
would hand flow control to the broker; pull lets each service decide its own
concurrency, which matters because the feasibility sweep and the planner have
completely different cost profiles per message.

| Consumer | Stream | Filter subject | Ack wait | Max deliver | Max ack pending |
| --- | --- | --- | --- | --- | --- |
| `feasibility-worker` | TASKING | `tasking.request.received.v1` | 120s | 5 | 64 |
| `planner-opportunities` | FEASIBILITY | `feasibility.opportunities.computed.v1` | 60s | 5 | 32 |
| `planner-lifecycle` | TASKING | `tasking.request.>` | 30s | 5 | 64 |
| `gateway-projector` | TASKING, FEASIBILITY, PLANNING | `>` | 30s | 10 | 256 |
| `tasking-state-machine` | FEASIBILITY, PLANNING | see note | 30s | 5 | 64 |
| `simulator-executor` | PLANNING | `planning.plan.committed.v1` | 60s | 3 | 16 |

`tasking-state-machine` filters `feasibility.failed.v1`,
`planning.request.unfulfilled.v1`, and `acquisition.executed.v1` — the events
that drive the `TaskingRequest` state machine back in `tasking-api`.

**Why `feasibility-worker` has a 120s ack wait.** An SGP4 sweep across the
constellation over a multi-day horizon is genuinely slow. Ack wait must exceed
worst-case processing time, or the broker redelivers a message that is still
being worked on and we do real work twice. The number comes from the p99 of the
sweep in the M1 benchmark, with headroom — it is measured, not guessed, and
`docs/performance.md` records the measurement.

**Why `gateway-projector` gets `max_deliver: 10`.** It is a pure projector with
no side effects beyond its own read model, so retrying it aggressively is cheap
and safe. Consumers that publish further events get 5, because each retry there
risks amplifying downstream work.

## Delivery guarantees, precisely

JetStream gives **at-least-once delivery**. It does not give exactly-once, and
nothing in this system claims it does.

What we build on top is **effectively-once processing**:

```sql
BEGIN;
  INSERT INTO processed_events (event_id, consumer, processed_at)
  VALUES ($1, $2, now());          -- unique (event_id, consumer)

  -- ... the actual state change ...

COMMIT;
-- then ACK
```

A duplicate delivery violates the unique constraint, the whole transaction rolls
back, and the message is acked anyway — because the work was already done. The
state change and the deduplication record commit or fail together, which is the
entire point: if they were separate transactions, a crash between them would
either lose the work or leave it permanently marked as done without having
happened.

**Ack after commit, never before.** Acking first and crashing before commit loses
the message. Committing first and crashing before ack causes a redelivery, which
the dedup handles. The failure modes are not symmetric, so the ordering is not a
matter of taste.

**Ordering is not assumed anywhere.** JetStream orders per subject, but consumers
process concurrently and messages redeliver out of band. Handlers use
`occurred_at` plus explicit state-machine guards, and reject transitions that do
not make sense from the current state rather than assuming arrival order implies
causal order. `plan_version` on `planning.plan.committed.v1` exists for exactly
this reason: a lower version arriving after a higher one is stale and dropped.

## Poison messages and the DLQ

A message that fails `max_deliver` times is a poison message. Retrying it forever
would consume a consumer slot indefinitely and starve healthy traffic.

```
DLQ_TASKING       <- dlq.tasking.>
DLQ_FEASIBILITY   <- dlq.feasibility.>
DLQ_PLANNING      <- dlq.planning.>, dlq.acquisition.>
```

**Dead letters use a `dlq.` PREFIX, not a `.dlq.` infix.** The obvious-looking
`tasking.dlq.>` is already captured by the `TASKING` stream's `tasking.>`
wildcard, and NATS refuses to create two streams whose subjects overlap
(`subjects overlap with an existing stream`, error 10065). Prefixing keeps the
subject spaces disjoint, and mapping between them stays a pure string operation
in both directions — which is what keeps the replay tooling trivial.

On terminal failure the consumer publishes the original payload to
`dlq.<original-subject>` with these headers, then acks the original:

| Header | Meaning |
| --- | --- |
| `Overpass-Dlq-Reason` | Terminal error class |
| `Overpass-Dlq-Original-Subject` | Where it came from |
| `Overpass-Dlq-Delivery-Count` | Attempts made |
| `Overpass-Dlq-First-Failed-At` | RFC 3339 |
| `Overpass-Dlq-Consumer` | Which consumer gave up |
| `traceparent` | Preserved, so the trace of a dead message is still complete |

### Replay procedure

1. `make dlq-inspect STREAM=DLQ_FEASIBILITY` — list dead messages with reason and count.
2. Diagnose from the preserved `traceparent`. The original trace is intact, so
   the failure is inspectable in Grafana rather than reconstructed from logs.
3. Fix the cause. A poison message is almost always a code bug or a bad
   deployment, not a bad message.
4. `make dlq-replay STREAM=DLQ_FEASIBILITY EVENT_ID=<uuid>` — republish to the
   original subject.

Replay is safe **because consumers are idempotent**. If a message partially
succeeded before failing, its `processed_events` row exists and the replay is a
no-op. This is the payoff for the idempotency tax: recovery is a routine
operation rather than a data-integrity incident.

## Trace propagation across the async hop

Every published message carries a W3C `traceparent` header (and `tracestate`
where present). Producers inject the current span context; consumers extract it
and start their handler span as a **link plus a child** of the producing span.

This is what makes one trace span the whole pipeline:

```
HTTP POST /v1/tasking-requests   [tasking-api]
  └─ outbox.relay.publish         [tasking-api]
      └─ feasibility.handle       [feasibility-service]   ← async hop, Go→Python
          └─ sgp4.propagate
          └─ geometry.filter
          └─ publish opportunities
              └─ planner.round     [planner-service]      ← async hop, Python→Go
                  └─ policy.allocate
                  └─ plan.commit
                      └─ gateway.project [plan-gateway]
```

Getting a trace to survive an async hop — and a language boundary — is the part
most implementations skip, and it is the difference between distributed tracing
that is configured and distributed tracing that works. It is a deliverable, not a
nicety.

## Configuration as code

Streams and consumers are declared in `deploy/nats/streams.yaml` and applied by
an init container on `docker compose up`. Declaring them here rather than
creating them lazily from application code means the topology is reviewable in a
pull request, and means two services cannot race to create the same stream with
different settings.

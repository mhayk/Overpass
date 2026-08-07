# 0002 — NATS JetStream as the event backbone, over Kafka and RabbitMQ

- **Status:** accepted
- **Date:** 2026-08-07
- **Deciders:** Mhayk Whandson

## Context and problem statement

The request lifecycle is a choreographed saga across four services. A tasking
request is received, feasibility is computed, opportunities are batched into a
planning round, a plan is committed, and acquisitions are eventually executed.
Each hop must survive a consumer restart without losing work, and we need to be
able to replay a stream to rebuild the read model in `plan-gateway`.

That rules out fire-and-forget messaging. We need **durable streams, at-least-once
delivery, per-consumer acknowledgement, redelivery with a max-deliver limit, a
dead-letter path, and replay from a sequence or timestamp.**

The question: **which broker gives us those properties at the lowest operational
cost, given that the whole system must come up under a single `docker compose up`
in under five minutes on a clean machine?**

## Decision drivers

1. **Durability and replay** — non-negotiable, they are load-bearing for the
   read model and for the idempotency story in
   [ADR-0008](README.md#planned).
2. **Operational footprint** — the deployment target is Docker Compose on a
   laptop ([ADR-0005](0005-docker-compose-over-kubernetes.md)). Every extra
   container, JVM, or coordination service is a direct cost against the
   five-minute cold-start budget.
3. **Throughput headroom at our actual volumes**, which are on the order of
   thousands of messages per second in bursts — not millions sustained.
4. **First-class client quality in Go and Python**, since both halves of the
   system consume ([ADR-0001](0001-polyglot-go-python-split.md)).
5. **Operational familiarity.** Running a broker I have reasoned about in
   production is worth more than a broker that benchmarks marginally better and
   surprises me at 3am.

## Considered options

1. **Apache Kafka** (or Redpanda as a lighter-weight, protocol-compatible variant)
2. **RabbitMQ**, with streams or classic queues
3. **NATS JetStream**
4. **Postgres as the queue** — `LISTEN/NOTIFY` plus a polled table

## Decision outcome

Chosen: **NATS JetStream.**

It provides every property on the must-have list — durable streams, at-least-once
delivery, explicit per-message acking, redelivery limits, replay by sequence or
time, and subject-based routing — in a single static binary with no external
coordination service and a container that starts in well under a second. At our
volumes, the throughput ceiling of the alternatives is headroom we would pay for
and never use.

The subject hierarchy also fits the domain cleanly. `tasking.request.received.v1`,
`feasibility.opportunities.computed.v1`, and so on are naturally hierarchical, so
wildcard subscriptions (`feasibility.>`) fall out of the naming rather than
requiring a separate routing configuration. See `contracts/nats/topology.md` for
the stream, consumer, and DLQ layout.

### Consequences

**Good**

- One container, no ZooKeeper, no KRaft controller quorum, no Erlang runtime. The
  cold-start budget survives.
- Replay is a first-class operation, which is what makes read-model rebuild in
  `plan-gateway` a routine action rather than an incident.
- The Go client is excellent and the Python client is solid, which matters given
  the polyglot split.
- Subject wildcards give us routing without a separate exchange/binding concept
  to configure and document.

**Bad**

- **At-least-once means every consumer must be idempotent.** This is a real,
  permanent tax on every handler we write, discharged through the
  `processed_events` table pattern. It is a cost, not a footnote.
- Smaller ecosystem than Kafka: fewer off-the-shelf connectors, fewer stream
  processing frameworks, a smaller pool of engineers who have operated it.
- JetStream's storage tier is less battle-tested at extreme retention than
  Kafka's log. We keep short retention, so this does not bite us — but it would
  if requirements changed.
- No built-in exactly-once transactional semantics across a publish and a
  database write. We solve this with the transactional outbox instead, which is
  the correct solution anyway but is now mandatory rather than optional.

**Neutral**

- Ordering is per-subject, not global. We treat this as a feature and state it
  plainly: handlers use `occurred_at` plus state-machine guards rather than
  assuming sequence. Any of the three brokers would have forced the same
  discipline in a multi-partition setup.

### Confirmation

We revisit this decision if any of the following happen:

- Sustained throughput exceeds roughly 50 000 messages/second, where Kafka's
  partitioned log and mature consumer-group rebalancing start to earn their
  operational weight.
- We need retention measured in months for audit or reprocessing, rather than the
  hours we currently need.
- We need to fan out to a heterogeneous ecosystem of off-the-shelf sinks, where
  Kafka Connect would replace code we would otherwise write.

The k6 breakpoint test in M3 gives us the first real data point: if JetStream is
the component that breaks first under load, this ADR gets a successor.

## Pros and cons of the options

### Option 1 — Kafka / Redpanda

- Good, because it is the correct answer at high sustained throughput with long
  log retention, and consumer groups plus partitioning give a well-understood
  scaling model.
- Good, because the ecosystem is enormous — Connect, Streams, schema registry.
- Bad, because the operational surface is substantial. Even Redpanda, which
  removes the JVM and ZooKeeper, brings partition management, consumer-group
  rebalancing semantics, and retention tuning as things you must understand
  before you are safe.
- Bad, because none of that capacity is used at our volumes. Paying operational
  cost for unused headroom is the definition of over-engineering, and I would
  rather be asked why I did not choose Kafka than fail to explain why I did.

### Option 2 — RabbitMQ

- Good, because its routing model is the richest of the three: exchanges,
  bindings, and topic patterns handle complex fan-out elegantly.
- Good, because it is mature, well documented, and widely operated.
- Bad, because the streaming and replay story is weaker and newer than its queue
  story. We want replay as a routine capability, not a bolt-on.
- Bad, because per-message overhead is higher, and the mental model splits between
  classic queues and streams — two models to explain instead of one.

### Option 3 — NATS JetStream (chosen)

- Good, because it hits every hard requirement with the smallest operational
  footprint of the three.
- Good, because subject hierarchies match the event taxonomy we were going to
  write anyway.
- Bad, because of the smaller ecosystem and the idempotency tax noted above.

### Option 4 — Postgres as the queue

- Good, because it would eliminate a component entirely, and the transactional
  outbox already gives us a durable write path inside the database — publishing
  from the same transaction would make the dual-write problem disappear.
- Good, because `SELECT ... FOR UPDATE SKIP LOCKED` is a genuinely competent
  queue for modest volumes.
- Bad, because it collapses the read and write load of four services onto one
  Postgres instance, and the planner already needs that instance to be
  responsive while holding allocation locks.
- Bad, because replay, consumer groups, and DLQ semantics would all be code we
  write and maintain rather than infrastructure we configure. That is a poor
  trade at this scale, and it would hide the event-driven architecture the
  project is meant to demonstrate rather than expose it.

## More information

- Stream, consumer, and DLQ topology: `contracts/nats/topology.md`
- Event schemas: `contracts/events/`
- Deployment constraint driving footprint:
  [ADR-0005](0005-docker-compose-over-kubernetes.md)

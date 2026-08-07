# 0003 — Draw service boundaries along consistency requirements, and place each service deliberately on the CAP tradeoff

- **Status:** accepted
- **Date:** 2026-08-07
- **Deciders:** Mhayk Whandson

## Context and problem statement

The usual way to decompose a system is by noun — a request service, a satellite
service, a plan service — or by org chart. Both produce services that share
invariants across a network boundary, which is how distributed monoliths get
built: every meaningful operation becomes a distributed transaction, and the
consistency guarantee ends up being "whatever happens to happen".

Overpass has one invariant that genuinely cannot be relaxed:

> No two acquisitions on the same satellite may overlap in time, and consecutive
> acquisitions must be separated by at least `slew_time(a, b)`.

If two customers can both win the same window, the system is not merely
inconsistent — it is producing a plan that is physically impossible to fly.

It also has requirements that pull hard in the opposite direction. Ingress must
accept a customer request at 1 000 rps and must not drop one because a downstream
component is slow or down. The read side must serve a 3D globe to browsers and
should stay up and fast even when the planner is mid-round.

The question: **where do the service boundaries go, and what consistency
guarantee does each side of each boundary make?**

## Decision drivers

1. **The non-overlap invariant must be enforced somewhere that cannot race.**
2. **Ingress availability**, because a dropped customer request is unrecoverable
   business damage, whereas a delayed one is an inconvenience.
3. **Read-side scalability**, because the frontend is data-intensive and the
   spec explicitly weights real-time visualisation.
4. **Each boundary must be defensible as a consistency boundary**, not merely as
   a code-organisation boundary.

## Considered options

1. **Modular monolith** — one deployable, one database, modules internally
2. **Decompose by noun** — request-service, satellite-service, plan-service
3. **Decompose by consistency requirement** — availability-leaning ingress,
   stateless compute, strongly-consistent allocation, eventually-consistent reads
4. **Decompose by consistency requirement, with the planner as a leader-elected
   singleton** rather than a partition-locked service

## Decision outcome

Chosen: **Option 3 — boundaries follow consistency requirements**, with each
service placed explicitly on the CAP tradeoff rather than accidentally.

| Service | Posture under partition | Why |
| --- | --- | --- |
| `tasking-api` | **AP** — availability-leaning | Accepts and durably records the request, publishes through the outbox, returns `202`. It never blocks on feasibility or planning. If everything downstream is dead, ingress still succeeds and the work drains later. |
| `feasibility-service` | **Neither — stateless** | Owns no mutable state, so CAP does not apply to it. It is a pure function from (target, horizon, TLE set) to opportunities. Scale it by adding consumers; a crash costs a redelivery, not correctness. |
| `planner-service` | **CP** — consistency-leaning | The only serialised component. Allocation for a given `(satellite_id, bucket_start)` runs under a Postgres advisory lock, and the resulting plan is committed atomically. If it cannot reach Postgres, it **stops**. It does not guess. |
| `plan-gateway` | **AP** — eventually consistent by design | Serves materialised read models. Can be stale, must be fast, scales horizontally and caches freely. Staleness is surfaced in the API rather than hidden. |

Two points that make this more than a table:

**CAP applies per-service only because each service owns its own state.** The
statement "this service is AP and that one is CP" is meaningless if they share a
database and can therefore be partitioned from each other's data. Here, the
partition under discussion is the network between services and between a service
and its own storage — which is why the boundary is drawn where the storage
ownership is.

**PACELC is the more honest frame.** Partitions are rare on a Compose network.
The interesting question is the "else" branch: in normal operation, do we trade
latency for consistency? The planner does — it takes a lock and serialises, which
costs latency deliberately. `plan-gateway` does the opposite. That is the real,
everyday tradeoff; CAP is the degenerate case.

**Where the concurrency lives.** Serialising the planner sounds like a
throughput ceiling, and it would be if the lock were global. It is not: the lock
key is `(satellite_id, bucket_start)`, so the constellation plans in parallel and
only a single satellite's single time bucket is serialised. The invariant is
per-satellite, so the lock is per-satellite. Matching lock granularity to
invariant granularity is the whole trick.

### Consequences

**Good**

- The hardest invariant is enforced in exactly one place, in a transaction, with
  a database-level exclusion constraint (`tstzrange` + GiST) as the backstop —
  not in application logic that could be bypassed by a second code path.
- Ingress latency is decoupled from planning cost. This is the architecture that
  answers "what happens at 10 vs 100 vs 1 000 rps" with a flat async curve
  instead of an opinion.
- Each service can be scaled and operated according to its own posture.

**Bad**

- The system is eventually consistent end to end. A customer who submits a
  request and immediately reads the plan may not see themselves in it yet. This
  is a real UX problem, discharged by returning `202` with a request state, and
  by making the frontend live via SSE rather than poll-and-hope.
- Four services means four deployables, four health checks, four sets of
  dashboards, and a distributed trace to debug anything interesting.
- The planner is a serialisation point per partition. If one satellite's bucket
  has pathologically many opportunities, that bucket's round is slow and nothing
  else about that satellite proceeds.
- Debugging requires distributed tracing to be genuinely working, not just
  configured. That is why OTel propagation across the NATS boundary is treated as
  a deliverable rather than a nicety.

**Neutral**

- The saga is choreographed, not orchestrated. There is no central coordinator to
  inspect, which is simpler operationally and harder to visualise. The trace is
  the visualisation.

### Confirmation

- **The invariant:** property-based tests generate arbitrary opportunity sets and
  assert that no committed plan ever contains overlapping acquisitions or
  violates slew time. Separately, an integration test attempts a conflicting
  insert directly against Postgres and asserts the exclusion constraint rejects
  it. If the constraint can be violated through any path, this decision has
  failed at its central claim.
- **The ingress posture:** a chaos test kills `feasibility-service` and
  `planner-service` mid-load and asserts that `POST /v1/tasking-requests`
  continues to return `202` with unchanged p99, and that the backlog drains
  correctly on restart.
- **The lock granularity:** a load test with N satellites should show planner
  throughput scaling roughly linearly in N. If it does not, the lock is coarser
  than we think.

## Pros and cons of the options

### Option 1 — Modular monolith

- Good, because the non-overlap invariant becomes a single local transaction, and
  the entire distributed-systems tax disappears: no outbox, no idempotent
  consumers, no distributed tracing to make debugging possible.
- Good, because it is genuinely the right answer for many systems this size, and
  saying so is more honest than pretending otherwise.
- Bad, because ingress availability and planning consistency would share a fate.
  A slow planning round would consume the same connection pool and process that
  ingress needs, and the flat-latency-under-load property would be lost.
- Bad, because it cannot demonstrate the event-driven and consistency-boundary
  reasoning that is the point of this project. **This is an honest bias, and it
  is recorded here rather than hidden:** the brief is to build a system that
  exercises these tradeoffs, and that requirement is itself a decision driver.

### Option 2 — Decompose by noun

- Good, because it is the most immediately intuitive decomposition, and the
  service names map to things a customer would recognise.
- Bad, because the non-overlap invariant spans "plan-service" and
  "satellite-service", so enforcing it requires either a distributed transaction
  or a saga with compensations — for an invariant that is fundamentally local to
  one satellite's timeline.
- Bad, because noun services almost always end up sharing a database to make the
  invariants work, at which point they are one service with extra network hops.

### Option 3 — Decompose by consistency requirement (chosen)

- Good, because every boundary is justifiable by pointing at a different
  consistency or availability requirement on each side of it.
- Good, because it puts the serialisation exactly where the invariant is and
  nowhere else.
- Bad, because of the eventual-consistency UX cost and the operational surface
  noted above.

### Option 4 — Planner as a leader-elected singleton

- Good, because a single active planner makes the "only one writer" property
  trivially obvious, with no per-partition locking to reason about.
- Bad, because it makes the planner a single point of failure with a
  leader-election dependency (Raft, or a lease in Postgres) that we would then
  own and have to explain.
- Bad, because it serialises across the whole constellation rather than per
  satellite, throwing away the parallelism that the invariant's own granularity
  hands us for free. The advisory lock gets the same safety with strictly more
  concurrency.

## More information

- Stream and consumer topology: `contracts/nats/topology.md`
- Broker choice that this posture depends on:
  [ADR-0002](0002-nats-jetstream-over-kafka-rabbitmq.md)
- Storage choice that makes the exclusion constraint possible:
  [ADR-0004](0004-postgresql-jsonb-over-document-store.md)

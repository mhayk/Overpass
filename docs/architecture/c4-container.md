# C4 Level 2 — Containers

The inside of the Overpass box. Every boundary here is a consistency boundary —
see [ADR-0003](../decisions/0003-consistency-boundaries-and-cap-position.md) for
why, and for each service's position on the CAP tradeoff.

```mermaid
C4Container
    title Overpass — Container Diagram

    Person(customer, "Tasking Customer")
    Person(operator, "Constellation Operator")

    System_Boundary(overpass, "Overpass") {
        Container(web, "web", "TypeScript, Next.js, CesiumJS, deck.gl", "3D globe, 2D planning view, per-satellite timeline with visible slew gaps, and the 'why did my request lose?' panel.")

        Container(taskingapi, "tasking-api", "Go", "REST ingress. Validates, enforces Idempotency-Key, persists the TaskingRequest write model and its state machine, publishes via transactional outbox. AVAILABILITY-LEANING: never blocks on computation.")

        Container(feasibility, "feasibility-service", "Python, sgp4/Skyfield, pyproj, Shapely", "SGP4 propagation, access-window search, SAR geometry filtering (incidence, look side, squint, slant range), footprint generation. STATELESS: pure function, horizontally scalable.")

        Container(planner, "planner-service", "Go", "Batches opportunities into planning rounds keyed by (satellite_id, bucket_start). Runs the pluggable AllocationPolicy, resolves conflicts, commits an atomic CollectionPlan. STRONGLY CONSISTENT: single-writer per partition via advisory lock.")

        Container(gateway, "plan-gateway", "Go", "Read side. Materialised views, CZML and GeoJSON serving, SSE stream of plan changes. EVENTUALLY CONSISTENT by design; scales and caches freely.")

        ContainerDb(postgres, "PostgreSQL + PostGIS", "Postgres 16", "Relational core, JSONB for semi-structured payloads, tstzrange + GiST exclusion constraint enforcing acquisition non-overlap per satellite. Also holds outbox, processed_events, idempotency_keys.")

        ContainerQueue(nats, "NATS JetStream", "NATS 2.x", "Durable streams, at-least-once delivery, per-consumer acking, replay, DLQ. Carries W3C traceparent in message headers.")

        Container(otel, "OTel Collector + Prometheus + Grafana", "Observability", "Distributed traces spanning the async hops, RED metrics, and domain metrics (opportunities per request, plan value per round, unfulfilled by reason).")
    }

    System_Ext(celestrak, "Celestrak", "Public TLE source")

    Rel(customer, taskingapi, "POST /v1/tasking-requests", "HTTPS + Idempotency-Key")
    Rel(customer, web, "Uses", "HTTPS")
    Rel(operator, web, "Uses", "HTTPS")
    Rel(web, gateway, "Reads plans, opportunities, CZML; subscribes to changes", "HTTPS / SSE")

    Rel(taskingapi, postgres, "Writes request + outbox row in ONE transaction", "SQL")
    Rel(taskingapi, nats, "Outbox relay publishes tasking.request.*", "NATS")

    Rel(nats, feasibility, "tasking.request.received.v1", "durable pull consumer")
    Rel(feasibility, celestrak, "Fetches TLE sets", "HTTPS")
    Rel(feasibility, nats, "feasibility.opportunities.computed.v1 / failed.v1", "NATS")

    Rel(nats, planner, "feasibility.opportunities.computed.v1", "durable pull consumer")
    Rel(planner, postgres, "Advisory lock, allocate, commit plan atomically", "SQL")
    Rel(planner, nats, "planning.plan.committed.v1 / request.unfulfilled.v1", "via outbox")

    Rel(nats, gateway, "planning.* , tasking.*", "durable pull consumer")
    Rel(gateway, postgres, "Materialised read models", "SQL")

    UpdateLayoutConfig($c4ShapeInRow="3", $c4BoundaryInRow="1")
```

## Event flow — the happy path, and where it can stop

The C4 diagram shows structure; this shows sequence. Note that every arrow into a
service is at-least-once, so every handler is idempotent.

```mermaid
flowchart TD
    A["Customer<br/>POST /v1/tasking-requests<br/>Idempotency-Key: uuid"] --> B{"idempotency_keys<br/>unique constraint"}
    B -->|"replay"| B1["Return original 202<br/>no new request"]
    B -->|"new"| C["TX: INSERT tasking_request<br/>+ INSERT outbox<br/>COMMIT"]
    C --> D["202 Accepted<br/>state = RECEIVED"]
    C --> E["Outbox relay publishes<br/>tasking.request.received.v1"]

    E --> F["feasibility-service<br/>SGP4 sweep over horizon<br/>SAR geometry filter"]
    F -->|"tle_epoch too old"| G1["feasibility.failed.v1<br/>reason: TLE_STALE"]
    F -->|"no geometric access"| G2["feasibility.failed.v1<br/>reason: NO_ACCESS<br/>state = INFEASIBLE"]
    F -->|"n opportunities"| G["feasibility.opportunities.computed.v1<br/>state = AWAITING_PLANNING"]

    G --> H{"Round trigger<br/>cadence timer OR<br/>opportunity debounce"}
    H --> I["planning.round.triggered.v1<br/>key = satellite_id, bucket_start"]
    I --> J["Advisory lock on round key<br/>run AllocationPolicy"]

    J --> K["TX: INSERT acquisitions<br/>GiST exclusion constraint<br/>guarantees no overlap<br/>+ outbox rows<br/>COMMIT"]
    K --> L["planning.plan.committed.v1<br/>plan_version, supersedes_plan_id"]
    J --> M["planning.request.unfulfilled.v1<br/>LOST_TO_HIGHER_VALUE<br/>BLOCKED_BY_SLEW<br/>DUTY_CYCLE_EXHAUSTED<br/>DEADLINE_PASSED<br/>SUPERSEDED"]

    L --> N["plan-gateway<br/>materialise read model"]
    M --> N
    G2 --> N
    N --> O["SSE to web<br/>globe re-plans, timeline updates,<br/>'why?' panel populated"]
```

The failure branches are drawn deliberately. `planning.request.unfulfilled.v1`
carrying a specific reason code is what makes the "why was my request rejected?"
panel possible, and that panel is the UI equivalent of an ADR: the system
explains its decisions rather than merely announcing them.

## Why these four services and not one, or seven

Each boundary earns its place by having a *different* consistency or availability
requirement on each side. If two of these services had the same posture and the
same data ownership, they would be one service.

| Boundary | What differs across it |
| --- | --- |
| `tasking-api` ↔ `feasibility-service` | Availability-leaning ingress must not block on an expensive, unbounded computation. This is also where the async latency curve stays flat while the sync path would degrade. |
| `feasibility-service` ↔ `planner-service` | Stateless, embarrassingly parallel compute versus serialised, transactional allocation. Opposite scaling models. |
| `planner-service` ↔ `plan-gateway` | Strong consistency and single-writer versus eventual consistency and free horizontal read scaling. CQRS-lite: the write model and the read model have different owners. |
| Go ↔ Python | Ecosystem fit ([ADR-0001](../decisions/0001-polyglot-go-python-split.md)). Note this line falls *on* an existing consistency boundary rather than adding a new seam. |

## Cross-cutting mechanisms

| Mechanism | Where it lives | What it buys |
| --- | --- | --- |
| Transactional outbox | `tasking-api`, `planner-service` | Removes the dual-write problem: a state change and its event are one transaction, or neither happened |
| Idempotent consumer | every consumer | `processed_events` insert in the same transaction as the state change turns at-least-once *delivery* into effectively-once *processing* |
| Advisory lock per `(satellite_id, bucket_start)` | `planner-service` | Single-writer per partition; the constellation still plans in parallel because the lock granularity matches the invariant granularity |
| GiST exclusion constraint | PostgreSQL | The non-overlap invariant is unbypassable, including by future code paths and manual SQL |
| W3C traceparent in NATS headers | all | One trace survives HTTP ingress → publish → Python consumer → publish → Go planner → commit |
| DLQ + max-deliver | NATS JetStream | Poison messages stop retrying and become a documented, replayable operational task |

## Deployment view

All nine containers come up under a single `docker compose up`
([ADR-0005](../decisions/0005-docker-compose-over-kubernetes.md)). Services are
twelve-factor — configuration from environment, logs to stdout, no local state —
so moving to a real scheduler later is a packaging change, not a code change.

# Services

| Directory | Language | Responsibility | Consistency posture |
| --- | --- | --- | --- |
| `tasking-api` | Go | REST ingress, idempotency, outbox, request state machine | Availability-leaning |
| `feasibility` | Python | SGP4, access windows, SAR geometry, footprints | Stateless |
| `planner` | Go | Allocation, de-confliction, atomic plan commit | Strongly consistent |
| `plan-gateway` | Go | Read models, CZML/GeoJSON, SSE | Eventually consistent |

Boundaries follow consistency requirements, not nouns — see
[ADR-0003](../docs/decisions/0003-consistency-boundaries-and-cap-position.md).
The Go/Python line falls *on* an existing boundary rather than adding a new one;
[ADR-0001](../docs/decisions/0001-polyglot-go-python-split.md).

The Go services use ports and adapters: `internal/domain` holds the logic and
imports nothing from `internal/adapter`. That is enforced by a test, not by
convention, and it buys one specific thing — the allocation and state-machine
logic is testable without Postgres or NATS, which is what makes property-based
testing practical at all.

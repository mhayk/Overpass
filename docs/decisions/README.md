# Architecture Decision Records

Every non-obvious decision in Overpass is recorded here. The rule the project
runs on:

> If a choice was made that a reasonable engineer could have made differently,
> it gets an ADR naming the alternatives and why they lost. A decision with no
> rejected alternatives was not a decision.

Format is [MADR](https://adr.github.io/madr/), lightly adapted. The template is
[`0000-template.md`](0000-template.md). Every ADR carries a **Confirmation**
section stating what would prove it wrong — a decision that nothing could
falsify is a preference wearing a decision's clothes.

Superseded ADRs are kept, not deleted. Changing your mind in public with a
paper trail is the point.

## Accepted

| # | Title | Status | Milestone |
| --- | --- | --- | --- |
| [0001](0001-polyglot-go-python-split.md) | Split the system across Go and Python rather than staying monoglot | accepted | M0 |
| [0002](0002-nats-jetstream-over-kafka-rabbitmq.md) | NATS JetStream as the event backbone, over Kafka and RabbitMQ | accepted | M0 |
| [0003](0003-consistency-boundaries-and-cap-position.md) | Draw service boundaries along consistency requirements, and place each service deliberately on the CAP tradeoff | accepted | M0 |
| [0004](0004-postgresql-jsonb-over-document-store.md) | PostgreSQL with JSONB for semi-structured data, instead of adding a document store | accepted | M0 |
| [0005](0005-docker-compose-over-kubernetes.md) | Docker Compose as the deployment target, not Kubernetes | accepted | M0 |

## Planned

These are decisions we know we owe an ADR for. They are written when the
decision is actually made and tested — not speculatively, because an ADR written
before the constraint is felt is fiction.

| # | Title | Milestone |
| --- | --- | --- |
| 0006 | Transactional outbox for the dual-write problem | M1 |
| 0007 | Allocation strategy, and the heuristic-versus-optimal tradeoff | M2 |
| 0008 | Idempotency: inbound HTTP keys and the idempotent-consumer pattern | M1 |
| 0009 | CesiumJS and deck.gl division of labour | M1 |
| 0010 | Test strategy and coverage targets | M0/M1 |
| 0011 | TLE sourcing: live Celestrak fetch at seed time, frozen snapshot for tests | M1 |
| 0012 | Plan supersession semantics for re-planned horizon buckets | M2 |

### Why 0011 and 0012 exist

They are not in the original spec's ADR list. Both are consequences of decisions
taken during M0 planning, and both are the kind of thing that would otherwise
become an undocumented assumption:

- **0011** — TLEs are fetched live from Celestrak at seed time, which exercises
  the `tle_epoch` staleness logic honestly but makes results non-deterministic.
  Golden-reference tests for orbital math therefore run against a *frozen*
  snapshot committed to `testdata/tle/`. Two TLE sources with two different
  purposes is exactly the sort of split that needs writing down.
- **0012** — a horizon bucket can be planned more than once, because planning
  rounds are triggered by either a cadence timer or an opportunity-arrival
  debounce. That makes plan supersession a first-class concept:
  `planning.plan.committed.v1` carries `plan_version` and `supersedes_plan_id`,
  and a request can be unfulfilled with reason `SUPERSEDED`.

## Conventions

- Filename: `NNNN-kebab-case-title.md`, zero-padded to four digits.
- Title is phrased as the decision in the imperative, not as a topic.
  "NATS JetStream over Kafka", not "Messaging".
- Status is one of `proposed`, `accepted`, `rejected`, `deprecated`,
  `superseded by ADR-NNNN`.
- ADRs are immutable once accepted. To change a decision, write a new ADR that
  supersedes it and update the old one's status line only.

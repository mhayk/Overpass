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
| [0010](0010-test-strategy-and-coverage.md) | Treat the test suite as the verification harness for generated code, and gate coverage at 80/95 | accepted | M0 |
| [0011](0011-tle-sourcing-live-and-frozen.md) | Fetch TLEs live from Celestrak at seed time, and test orbital math against a frozen snapshot | accepted | M1 |
| [0012](0012-retain-superseded-acquisitions.md) | Retain superseded acquisitions with a status, and make the non-overlap constraint partial and deferred | accepted | M1 |
| [0013](0013-parallel-agent-execution-in-worktrees.md) | Run parallel agent work as one git worktree per contract boundary, not as concurrent agents in one checkout | accepted | M1 |

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
| 0014 | Planner-side re-planning semantics: round triggers, debounce, and in-flight requests | M2 |

### Why 0014 exists

It is not in the original spec's ADR list. It is a consequence of a decision
taken later, and the kind of thing that would otherwise become an undocumented
assumption:

- **0014** — the half of supersession that [0012](0012-retain-superseded-acquisitions.md)
  deliberately did not decide. 0012 settles how superseded acquisitions are
  *stored*, because M1-01 cannot write the exclusion constraint without knowing.
  When a round fires, how the debounce interacts with the cadence timer, what
  happens to a request holding an acquisition in a replaced plan, and whether
  re-planning may be partial are planner decisions, and the constraint will not be
  felt until M2.

### Why the numbers are out of order

Numbers are allocated in the order decisions are *made*, not in the order they
were anticipated. 0006–0012 were reserved during M0 planning for decisions M1 and
M2 would force, and they stay unwritten until the constraint is actually felt.

Two arrived early. **0013** was forced by closing M0 — that is precisely what made
concurrent work possible, so the question of how to execute it landed at the
M0/M1 boundary. **0012** was reserved for M2 and forced in M1 instead: M1-01 has
to write the non-overlap exclusion constraint, and the naive form of that
constraint collides with the plan it is replacing. The storage half of
supersession had to be decided before a single migration could be written.

## Conventions

- Filename: `NNNN-kebab-case-title.md`, zero-padded to four digits.
- Title is phrased as the decision in the imperative, not as a topic.
  "NATS JetStream over Kafka", not "Messaging".
- Status is one of `proposed`, `accepted`, `rejected`, `deprecated`,
  `superseded by ADR-NNNN`.
- ADRs are immutable once accepted. To change a decision, write a new ADR that
  supersedes it and update the old one's status line only.

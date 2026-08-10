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

## Written

The status column carries the real state. An ADR appears here once the file
exists, which is not the same as the decision being final — 0007 sat as
`proposed` for a day, its measured-results section an explicit placeholder,
until the M2-13 benchmark produced the numbers that completed it.

| # | Title | Status | Milestone |
| --- | --- | --- | --- |
| [0001](0001-polyglot-go-python-split.md) | Split the system across Go and Python rather than staying monoglot | accepted | M0 |
| [0002](0002-nats-jetstream-over-kafka-rabbitmq.md) | NATS JetStream as the event backbone, over Kafka and RabbitMQ | accepted | M0 |
| [0003](0003-consistency-boundaries-and-cap-position.md) | Draw service boundaries along consistency requirements, and place each service deliberately on the CAP tradeoff | accepted | M0 |
| [0004](0004-postgresql-jsonb-over-document-store.md) | PostgreSQL with JSONB for semi-structured data, instead of adding a document store | accepted | M0 |
| [0005](0005-docker-compose-over-kubernetes.md) | Docker Compose as the deployment target, not Kubernetes | accepted | M0 |
| [0006](0006-transactional-outbox.md) | Publish through a transactional outbox, not directly from the handler | accepted | M1 |
| [0007](0007-allocation-strategy.md) | Choose the allocation algorithm by measurement behind a policy interface, rather than committing to one algorithm | accepted | M2 |
| [0008](0008-idempotency.md) | Idempotency in two places: a required HTTP key at ingress, and a dedup ledger in every consumer | accepted | M1 |
| [0009](0009-cesium-deckgl-division.md) | Split the frontend by question: Cesium answers where and when, deck.gl answers how much and where things collide | accepted | M1 |
| [0010](0010-test-strategy-and-coverage.md) | Treat the test suite as the verification harness for generated code, and gate coverage at 80/95 | accepted | M0 |
| [0011](0011-tle-sourcing-live-and-frozen.md) | Fetch TLEs live from Celestrak at seed time, and test orbital math against a frozen snapshot | accepted | M1 |
| [0012](0012-retain-superseded-acquisitions.md) | Retain superseded acquisitions with a status, and make the non-overlap constraint partial and deferred | accepted | M1 |
| [0013](0013-parallel-agent-execution-in-worktrees.md) | Run parallel agent work as one git worktree per contract boundary, not as concurrent agents in one checkout | accepted | M1 |
| [0014](0014-replanning-semantics.md) | Fire a round on a quiet-period debounce under a staleness ceiling, recompute the whole bucket, and give incumbency no advantage | accepted | M2 |
| [0015](0015-planner-projects-its-own-request-value.md) | Give the planner its own projection of request value, instead of reading the tasking schema | accepted | M2 |
| [0016](0016-ephemeris-sampling-and-horizon.md) | Sample the ephemeris every ten seconds, in aligned three-hour buckets, over a rolling day | accepted | M1 |
| [0017](0017-dlq-publication-and-replay-semantics.md) | Dead letters are published by the consumer, before the Term, or not at all | accepted | M3 |
| [0018](0018-metrics-by-otlp-push-with-units-in-names.md) | Push metrics over OTLP, and bake units into instrument names | accepted | M3 |
| [0019](0019-head-based-sampling-at-one-for-the-demo.md) | Head-based sampling, at 1.0 for the demo and configurable everywhere | accepted | M3 |
| [0020](0020-browser-origins-are-an-explicit-allow-list.md) | Browser origins are an explicit allow-list, in the services | accepted | M4 |

## Planned

Decisions we know we owe an ADR for. They are written when the decision is
actually made and tested — not speculatively, because an ADR written before the
constraint is felt is fiction.

*Nothing outstanding.* Every ADR the project has identified is now written.

### Why 0014 and 0015 exist

Neither is in the original spec's ADR list. Both are consequences of decisions
taken later, and the kind of thing that would otherwise become an undocumented
assumption:

- **0014** — the half of supersession that [0012](0012-retain-superseded-acquisitions.md)
  deliberately did not decide. 0012 settles how superseded acquisitions are
  *stored*, because M1-01 cannot write the exclusion constraint without knowing.
  When a round fires, how the debounce interacts with the cadence timer, what
  happens to a request holding an acquisition in a replaced plan, and whether
  re-planning may be partial are planner decisions, and the constraint was not
  felt until M2-01 needed both triggers at once.

- **0015** — forced by starting M2. M1-01 gave the planner every table it writes
  and none that it reads, and the bid, tier and deadline it allocates by arrive on
  a different event from the candidates. Where those facts come from at round time
  is a consistency decision, not a plumbing detail, so it was made before
  `planner-service` had a line of code rather than discovered inside it.

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

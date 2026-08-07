1. Mission

Overpass is a satellite tasking, feasibility, and collection-planning system for a simulated constellation of SAR (synthetic aperture radar) satellites.

A customer asks for imagery of a point or polygon on Earth, with a deadline and a bid. Overpass:

Accepts the request and acknowledges it fast, under load.
Computes when, if ever, each satellite in the constellation can actually image that target, using real orbital propagation and SAR acquisition geometry.
Resolves competition — many customers want overlapping windows on the same satellite — via a priority-and-bid allocation mechanism, producing a conflict-free collection plan.
Exposes the constellation, the candidate opportunities, and the resulting plan through an interactive 3D globe and a 2D planning view.

The interesting engineering problem is #3. #2 is the domain flavour that makes #3 real.

Non-goals
No real satellite command uplink, no real SAR image formation, no real downlink.
No auth/multi-tenancy beyond a stub customer_id (state this in the README as a deliberate scope cut, not an oversight).
No Kubernetes. Docker Compose is the deployment target; say why in an ADR.
Legal / provenance note (put this in the README)

Built from public sources only: public TLEs from Celestrak, published SAR geometry concepts from open literature. No proprietary data or internal knowledge from any employer.

2. Domain primer (the agent must get this right)

Do not treat this as generic CRUD. The physics is what makes the scheduling problem hard.

TLE + SGP4. Each satellite's orbit is described by a Two-Line Element set. SGP4 propagates it to a position/velocity at any time t. TLEs decay in accuracy — days-old TLEs drift. Model this: store tle_epoch, and refuse or flag feasibility results computed against a TLE older than a configured threshold.

SAR is side-looking. This is the single most important domain fact. Unlike an optical sensor pointing at nadir, a SAR images a swath off to one side of the ground track. A target directly beneath the satellite is not imageable. Access requires:

Incidence angle within the sensor's valid band (model e.g. 15°–45°).
Look side — left or right of the ground track. Some modes/satellites are constrained to one side.
Squint angle within limits (how far fore/aft of broadside the beam can be steered).
Minimum elevation / slant range bounds.

Roll manoeuvre and agility. To image off-nadir at a chosen incidence angle, the satellite rolls. Two acquisitions on the same satellite need enough time between them for the attitude slew plus settling. Model this as a transition-time function slew_time(acq_a, acq_b) — this is what turns naive interval scheduling into scheduling with sequence-dependent setup times, and it is where the real difficulty lives.

Imaging modes. Model at least three, with different durations, swath widths, and resolutions — e.g. Spotlight (small area, high res, longer dwell), Stripmap (medium), Scan (wide area, coarse). Mode selection interacts with feasibility and with plan value.

Duty cycle budget. A satellite cannot image continuously — power and thermal limits. Enforce a per-orbit imaging-seconds budget. This creates a knapsack dimension on top of the interval constraints.

Vocabulary to use consistently in code and docs: TaskingRequest → Opportunity (a feasible satellite × time window × geometry) → Acquisition (an opportunity that won allocation) → CollectionPlan (the conflict-free set of acquisitions per satellite per planning horizon).

3. Architecture
   3.1 Services
   Service Language Responsibility
   tasking-api Go REST ingress. Validates requests, enforces idempotency, persists, publishes via transactional outbox. Owns the TaskingRequest write model and its state machine.
   feasibility-service Python Consumes request events. SGP4 propagation, access-window search, SAR geometry filtering, footprint generation. Publishes OpportunitiesComputed.
   planner-service Go The heart. Consumes opportunities, batches them into planning rounds, runs the allocation policy, resolves conflicts, commits an atomic CollectionPlan.
   plan-gateway Go Read side. Materialised views, CZML/GeoJSON serving, SSE stream of plan changes to the frontend.
   web TS / Next.js CesiumJS globe + deck.gl 2D planning view + per-satellite timeline.

Infrastructure: NATS JetStream, PostgreSQL, OpenTelemetry Collector, Prometheus + Grafana, k6. All orchestrated by a single docker compose up.

3.2 Why this split (be ready to defend it)

The split follows consistency boundaries, not org chart or CRUD nouns:

tasking-api is availability-leaning. Its job is to never drop a customer request and never block on computation.
feasibility-service is pure and stateless per request — embarrassingly parallel, horizontally scalable, and the natural place for Python's scientific stack.
planner-service is the only strongly-consistent, serialised component. Allocation must be atomic — two customers cannot both win the same window.
plan-gateway is eventually consistent by design and can be scaled and cached freely.

This is the CAP conversation made concrete: different services sit at different points on the C/A tradeoff, deliberately. Write this up as ADR-0003.

3.3 Language choice

Go for the platform services: goroutines/channels map cleanly onto concurrent consumers, latency is predictable under load (which matters because we publish p95/p99 numbers), and the NATS Go client is first-class.

Python for feasibility: the orbital mechanics ecosystem (Skyfield, sgp4, pyproj, shapely) is mature and correct. Reimplementing SGP4 in Go to keep a monoglot repo would be a worse engineering decision than accepting a polyglot one.

Frame this in the interview as "right tool per bounded context," and be honest about the cost: two toolchains, two CI paths, two sets of dependency management, and a serialisation boundary. The contract-first approach (§4) is what makes that cost tolerable.

4. Contracts first

Before any service code exists:

contracts/events/\*.schema.json — JSON Schema for every event. Versioned (v1). CI validates every published event against its schema in integration tests.
contracts/openapi.yaml — the full REST surface of tasking-api and plan-gateway.
Code generation from both into Go and Python, checked in, with a CI check that regeneration produces no diff.

Contracts-first is not ceremony here — it is the enabling condition for parallel agent work (§10) and for the polyglot split. Say so explicitly in the AI-engineering write-up.

Core events
tasking.request.received.v1
tasking.request.rejected.v1
feasibility.opportunities.computed.v1
feasibility.failed.v1
planning.round.triggered.v1
planning.plan.committed.v1
planning.request.unfulfilled.v1
acquisition.executed.v1 (simulated, M4)

Every event carries: event_id (UUID), correlation_id, causation_id, occurred_at, schema_version, and a W3C traceparent in NATS headers.

5. Data model (PostgreSQL)

Tables: customers, satellites, tle_sets, tasking_requests, opportunities, collection_plans, acquisitions, outbox, processed_events, idempotency_keys.

Where JSONB is used and why (this is an ADR the interviewer will enjoy):

Relational core, because the allocation invariant — no two acquisitions on one satellite may overlap or violate slew time — is a transactional constraint. That is exactly what ACID and exclusion constraints exist for. A document store would push that invariant into application code and hand you a race condition.

But some data is genuinely semi-structured: raw TLE payloads, sensor mode parameter sets that differ per mode, computed geometry blobs, footprint polygons. Those live in JSONB columns, with GIN indexes where queried.

The punchline: we did not need a separate NoSQL store because Postgres covers the document use case without giving up transactions. Adding one would mean two consistency models, dual writes, and no ACID across them — real cost, no benefit at this scale.

Also use, and be ready to explain:

tstzrange + GiST exclusion constraint on acquisitions to enforce non-overlap per satellite at the database level, not just in application logic. This is the single most convincing artifact of "I understand where invariants belong."
PostGIS for target geometry and footprint queries.
Optimistic concurrency (version column) on collection_plans. 6. Delivery guarantees, idempotency, atomicity

NATS JetStream gives at-least-once. Every consumer must therefore be idempotent. Implement and document:

Inbound HTTP idempotency. Idempotency-Key header → idempotency_keys table with a unique constraint. Replay returns the original response, does not create a second request.
Idempotent consumer. Insert event_id into processed_events in the same transaction as the state change. Duplicate delivery hits the unique constraint, the transaction rolls back, the message is acked. This is how you get effectively-once processing on top of at-least-once delivery — be precise about that distinction, it is a classic interview probe.
Transactional outbox. Never publish inside a business transaction. Write to outbox in the same transaction, then a relay publishes and marks sent. Explain the dual-write problem this solves.
Serialised allocation. Planning rounds are partitioned by (satellite_id, horizon_bucket). Use a Postgres advisory lock (or SELECT ... FOR UPDATE) per partition so allocation for a given satellite is single-writer. Different satellites plan in parallel — that is where the concurrency lives.
Poison messages. Max-deliver limits, DLQ stream, and a documented replay procedure.
Ordering. State plainly that global ordering is not assumed. Handlers must tolerate out-of-order arrival; use occurred_at plus state-machine guards rather than assuming sequence. 7. The allocation problem (the centrepiece)
7.1 Formulation

Given a set of candidate Opportunity records across satellites and time, each tied to a TaskingRequest with a bid, a priority tier, and a deadline — select a subset maximising total plan value, subject to:

At most one acquisition per request (do not image the same target twice).
No two acquisitions on the same satellite overlap.
Between consecutive acquisitions on a satellite, gap ≥ slew_time(a, b).
Per-orbit duty-cycle budget not exceeded.
Deadline respected.

This is a weighted job scheduling problem with sequence-dependent setup times on parallel machines. It is NP-hard. Say that out loud, then say what you did about it.

7.2 Strategy pattern — pluggable allocation policies

Define AllocationPolicy as an interface and implement several. This is the best single design decision in the project, because it turns "which algorithm?" from a guess into a measurement.

Policy Character
GreedyByBid Naive baseline. Sort by bid, take what fits.
GreedyByValueDensity Sort by bid ÷ (duration + expected slew cost). Usually much better.
VickreySealedBid Second-price clearing. Winner pays the runner-up's bid. Truthful bidding — a genuinely interesting mechanism-design point.
ExactDP Optimal via DP/branch-and-bound. Small instances only. Its purpose is to be the ground truth that bounds the heuristics in tests.

Then benchmark them against each other on generated scenarios: report plan value achieved, requests fulfilled, satellite utilisation, and runtime, with ExactDP as the 100% reference. A chart of "greedy reaches 94% of optimal in 3ms where exact takes 40s" is worth more in an interview than any amount of clean code.

7.3 Fairness

Pure highest-bid allocation starves low-bid customers forever. Since this domain has government/civil-protection users alongside commercial ones, model priority tiers with a multiplier, plus an ageing factor so a repeatedly-losing request gains weight. Document the tradeoff — this shows product judgement, not just algorithmic skill.

8. Frontend

Next.js (App Router) + TypeScript + Tailwind.

CesiumJS globe: constellation in 3D, orbit tracks, ground tracks, sensor swath footprints as CZML, animated along a timeline. Selecting a request highlights its candidate opportunities.
deck.gl 2D view: target density, coverage heatmap, footprint polygons, conflict clusters.
Timeline / Gantt panel: per satellite, acquisitions on a time axis with slew gaps visible. Conflicts and rejected candidates rendered as ghosts so the de-confliction decision is visible, not just its result.
Live updates: SSE from plan-gateway. Submitting a request should visibly ripple through to a new plan.
A "why?" panel: click any losing request and see the reason — no geometric access, lost to a higher bid, blocked by slew constraint, duty cycle exhausted. This is the feature that will get remembered. It is the UI equivalent of an ADR.

Performance: virtualise the timeline, throttle Cesium entity updates, and measure — data-intensive real-time visualisation is explicitly in the job spec.

9. Quality bar
   9.1 Tests
   Unit — allocation policies, geometry math, state machines. Table-driven in Go, pytest-parametrised in Python.
   Property-based (gopter / hypothesis) on the scheduler: for any generated input, the output plan must never contain overlapping acquisitions or violate slew time. Invariants over examples. This is a strong signal.
   Golden/reference tests for orbital math: validate access-window computation against known passes for a public satellite with a fixed TLE and epoch. Physics needs an oracle, not a snapshot of your own output.
   Integration — real Postgres and real NATS via Testcontainers. Explicitly test: duplicate event delivery, out-of-order delivery, consumer crash mid-transaction, outbox relay restart.
   Contract tests — every emitted event validated against its JSON Schema.
   E2E — Playwright, one happy path plus one contested-window path.
   Coverage gate in CI: 80% overall, 95% on the planner and geometry packages. Report it in the README badge and be ready to say why 100% would be theatre.
   9.2 Load testing

k6 scenarios with thresholds as hard CI gates, not decoration. Publish results in docs/performance.md with graphs.

Suggested SLOs (tune to your hardware, then defend the numbers):

Scenario Target
POST /tasking-requests at 1 000 rps p95 < 40 ms, p99 < 120 ms, 0 errors
Same at 100 rps p95 < 15 ms
End-to-end request → plan committed, 200 rps sustained p99 < 5 s
Planner round, 5 000 opportunities p95 < 800 ms

Run a breakpoint test to find where it falls over, and write up what broke first and why. Knowing your system's failure mode is more impressive than a clean graph.

This is also where you answer the "10 vs 100 vs 1 000 rps" question with data instead of opinion: show the synchronous-path latency curve degrading and the async path staying flat, and the architecture defends itself.

9.3 Observability

OpenTelemetry traces propagated across the NATS boundary via message headers, so one trace spans HTTP ingress → publish → Python consumer → publish → Go planner → commit. Getting distributed tracing to survive an async hop is a detail most candidates skip.

RED metrics per service, plus domain metrics: opportunities per request, plan value per round, allocation latency, requests unfulfilled by reason. Grafana dashboards committed as JSON. Structured JSON logs with correlation_id on every line.

9.4 Patterns to apply deliberately

Hexagonal (ports & adapters) in the Go services · Transactional outbox · Idempotent consumer · Choreographed saga for the request lifecycle · CQRS-lite (write model in tasking-api, read model in plan-gateway) · Strategy (allocation policies) · Repository · Circuit breaker + bulkhead on cross-service calls · DLQ · Explicit state machine for TaskingRequest.

Rule: every pattern gets one sentence in the ADR saying what it bought. A pattern you cannot justify is a pattern that will be used against you in the interview.

10. AI-assisted engineering (make this a first-class artifact)

The role explicitly weights AI fluency twice. So do not merely use AI — document the methodology as an engineering deliverable.

Create docs/ai-engineering/:

00-methodology.md — How work was decomposed for parallel agents. The key insight to state: contracts-first is what makes agent parallelism safe. Once event schemas and OpenAPI are frozen, tasking-api, feasibility-service, and web can be built concurrently by separate agent sessions with no merge collisions, because the interface between them is already law. Without that, parallel agents produce integration debt faster than they produce features.

01-agent-roles.md — The specialised roles used: spec-writer, implementer, test-author, adversarial reviewer, docs. Include the actual prompts in prompts/.

02-verification.md — The accountability loop. What did you not trust the model on, and how did you check? Suggested honest answer: orbital geometry and concurrency-safety were the two areas where generated code looked plausible and was subtly wrong, which is exactly why the golden reference tests and property-based invariants exist. Frame the test strategy as the verification harness for AI-generated code — that reframing is the strongest thing you can say on this topic.

03-what-worked-what-didnt.md — Be genuinely honest. Where AI saved hours; where it cost time (plausible-but-wrong SGP4 handling, over-abstracted early code, confident hallucination of library APIs). The job spec asks for a feel for "where it quietly costs you time." Answering that with specifics is a credibility multiplier. A candidate who claims AI never misled them has not used it seriously.

CLAUDE.md at the repo root — see Appendix B.

Warning to yourself: the failure mode is a repo that looks machine-generated and a candidate who cannot defend it. Mitigate by writing the ADRs and the AI-engineering docs yourself, in your own voice, and by being able to whiteboard the allocation algorithm with no editor open. If you cannot explain it on a whiteboard, cut it from the project.

11. Milestones

Sequenced so there is a demoable vertical slice early. Each milestone is a GitHub Milestone; each bullet is at minimum one issue.

M0 — Foundations & contracts (no service code) Repo skeleton, ADR-0001..0005, C4 diagrams, event schemas, OpenAPI, codegen, Docker Compose skeleton, CI pipeline, issue/PR templates, CLAUDE.md.

M1 — Vertical slice tasking-api with idempotency + outbox → NATS → feasibility-service computing real access windows from real TLEs → plan-gateway read model → minimal Cesium globe showing satellites and one target's opportunities. Ship this end-to-end before adding anything.

M2 — The planner planner-service, plan state machine, exclusion constraint, slew model, duty cycle, all four allocation policies behind the Strategy interface, policy benchmark harness and report.

M3 — Resilience & performance Idempotent-consumer hardening, DLQ, chaos tests (kill a consumer mid-flight), OTel end-to-end, Grafana dashboards, full k6 suite, docs/performance.md.

M4 — Frontend depth deck.gl 2D view, per-satellite timeline with visible slew gaps, SSE live updates, the "why was my request rejected?" panel, acquisition execution simulator.

M5 — Presentation README with architecture diagram and GIF, docs/decisions/ index, AI-engineering write-up, a 5-minute demo script, and a one-page "questions I expect and my answers" cheat sheet (for you, not the repo).

12. GitHub hygiene
    Milestones M0–M5. Every issue has a milestone, labels (type/_, area/_, risk/\*), and acceptance criteria.
    Issue template includes an "Engineering decisions" section — even small ones. The commit history should read as a narrative of reasoning.
    One branch per issue, one PR per branch, Closes #N. Conventional Commits. Squash-merge.
    Branch protection: CI green + coverage gate required.
    GitHub Projects board with the milestone swimlanes.
    ADRs in docs/decisions/NNNN-title.md, MADR format, statuses maintained (accepted, later superseded by ADR-00XX where you changed your mind — showing a superseded ADR is a flex, not a weakness).

Initial ADRs: 0001 Polyglot Go + Python split · 0002 NATS JetStream over Kafka/RabbitMQ · 0003 Consistency boundaries and CAP position per service · 0004 PostgreSQL with JSONB instead of a separate document store · 0005 Docker Compose over Kubernetes · 0006 Transactional outbox · 0007 Allocation strategy and heuristic-vs-optimal tradeoff · 0008 Idempotency approach · 0009 CesiumJS + deck.gl division of labour · 0010 Test strategy and coverage targets.

ADR-0002 talking points (messaging): RabbitMQ excels at complex routing and traditional task queues but its streaming story is weaker and per-message overhead is higher. Kafka is the right answer at very high sustained throughput with long log retention, but brings substantial operational weight — overkill here, and the added ops surface is a real cost with no matching benefit at our volumes. NATS JetStream gives durable streams, at-least-once delivery, replay, and per-consumer acking with a fraction of the operational footprint. And say the quiet part in the interview: it is also what this team already runs, which means the tradeoffs I reasoned about are the tradeoffs they live with.

13. Definition of done

The project is interview-ready when:

git clone && docker compose up produces a working system with seeded data in under five minutes, on a clean machine.
A make demo script submits contested requests and the globe visibly re-plans.
CI is green: lint, unit, integration, contract, E2E, coverage gates, k6 thresholds.
Every ADR is written, and you can defend each one without notes.
docs/performance.md has real numbers from your hardware, including a documented failure mode.
You can whiteboard the allocation algorithm and its complexity from memory.
You can honestly answer: "Which part of this did AI get wrong, and how did you catch it?"
Appendix A — Scope reality check

M0–M2 alone is already a strong portfolio project and a complete story: real orbital mechanics, real event-driven architecture, a genuinely hard scheduling problem, measured tradeoffs. If time gets short, ship M0–M2 polished rather than M0–M5 half-built. A finished small thing beats an unfinished large one, and the interviewer will notice which one you built. M3–M5 are upside.

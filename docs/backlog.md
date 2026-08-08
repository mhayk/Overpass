# Backlog — milestones and issues

The delivery plan for Overpass. Six milestones, sequenced so there is a
demoable vertical slice as early as possible and each milestone leaves the repo
in a defensible state.

Import into GitHub with `./scripts/gh-seed.sh` (idempotent — safe to re-run).

## Sequencing rationale

M1 ships **end to end before anything is deep**. A thin slice that runs — real
TLE in, real access window out, one satellite on a globe — is worth more than
three excellent services that have never spoken to each other. Integration
problems found in week one are cheap; found in week four they are the project.

M2 is the centrepiece. Everything before it exists to make the allocation
problem real, and everything after it exists to prove the system holds up and to
make the decisions visible.

**If time runs short, ship M0–M2 polished rather than M0–M5 half-built.** M0–M2
is already a complete story: real orbital mechanics, real event-driven
architecture, a genuinely hard scheduling problem, measured tradeoffs. A finished
small thing beats an unfinished large one, and the difference is obvious to a
reviewer.

## Labels

| Label | Meaning |
| --- | --- |
| `type/feature` | New capability |
| `type/chore` | Tooling, scaffolding, dependencies |
| `type/docs` | Documentation |
| `type/adr` | An architecture decision record |
| `type/test` | Test infrastructure or coverage |
| `type/perf` | Performance work or load testing |
| `type/spike` | Time-boxed investigation with a written outcome |
| `type/bug` | Defect |
| `area/contracts` | Schemas, OpenAPI, codegen |
| `area/tasking-api` · `area/feasibility` · `area/planner` · `area/plan-gateway` · `area/web` | Per service |
| `area/infra` | Compose, Postgres, NATS, migrations |
| `area/ci` | Pipelines and gates |
| `area/observability` | Tracing, metrics, dashboards |
| `risk/high` | Could invalidate a design decision, or is where correctness is hardest |
| `risk/medium` · `risk/low` | |

`risk/high` is the label that matters. It marks the issues where generated code
is most likely to look plausible and be subtly wrong — orbital geometry,
concurrency safety, allocation invariants — and therefore the issues that get
property-based or golden-reference tests rather than example tests.

---

## M0 — Foundations and contracts

**Goal:** every interface is law before any implementation exists. No service
code in this milestone.

**Exit criteria:** contracts frozen and generating into both languages, CI green
on an empty repo, `docker compose up` brings up the infrastructure, ADRs 0001–0005
written and defensible.

| # | Issue | Labels |
| --- | --- | --- |
| M0-01 | Repo skeleton, Makefile, `.gitignore`, `.editorconfig` | `type/chore` `area/infra` `risk/low` |
| M0-02 | ADR template and ADRs 0001–0005 | `type/adr` `type/docs` `risk/low` |
| M0-03 | C4 context and container diagrams | `type/docs` `risk/low` |
| M0-04 | Event contract schemas (8 events + common defs) | `type/feature` `area/contracts` `risk/high` |
| M0-05 | OpenAPI spec for `tasking-api` | `type/feature` `area/contracts` `risk/medium` |
| M0-06 | NATS stream, consumer, and DLQ topology | `type/feature` `area/contracts` `area/infra` `risk/medium` |
| M0-07 | Codegen pipeline: Go and Python, with drift check | `type/chore` `area/contracts` `area/ci` `risk/medium` |
| M0-08 | Docker Compose skeleton: Postgres+PostGIS, NATS, OTel, Prometheus, Grafana | `type/chore` `area/infra` `risk/medium` |
| M0-09 | CI pipeline: lint, test, contract validation, coverage gate | `type/chore` `area/ci` `risk/medium` |
| M0-10 | Issue and PR templates, labels, branch protection | `type/chore` `area/ci` `risk/low` |
| M0-11 | `CLAUDE.md` and `docs/ai-engineering/` scaffolding | `type/docs` `risk/low` |
| M0-12 | ADR-0010 — test strategy and coverage targets | `type/adr` `type/test` `risk/medium` |

---

## M1 — Vertical slice

**Goal:** a request submitted over HTTP produces real access windows from real
TLEs and appears on a globe. Thin but complete.

**Exit criteria:** `make demo` submits one request and a satellite plus its
opportunity footprints render in Cesium, with the whole path visible in one
distributed trace.

| # | Issue | Labels |
| --- | --- | --- |
| M1-01 | Postgres schema and migrations, including `tstzrange` + GiST exclusion constraint | `type/feature` `area/infra` `risk/high` |
| M1-02 | `tasking-api`: hexagonal skeleton, config, health, structured logging | `type/feature` `area/tasking-api` `risk/low` |
| M1-03 | `tasking-api`: `POST /v1/tasking-requests` with validation | `type/feature` `area/tasking-api` `risk/medium` |
| M1-04 | `tasking-api`: HTTP idempotency via `Idempotency-Key` | `type/feature` `area/tasking-api` `risk/high` |
| M1-05 | `tasking-api`: transactional outbox and relay | `type/feature` `area/tasking-api` `risk/high` |
| M1-06 | ADR-0006 — transactional outbox · ADR-0008 — idempotency approach | `type/adr` `risk/low` |
| M1-07 | `tasking-api`: `TaskingRequest` state machine | `type/feature` `area/tasking-api` `risk/medium` |
| M1-08 | TLE ingestion from Celestrak with staleness classification | `type/feature` `area/feasibility` `risk/high` |
| M1-09 | ADR-0011 — TLE sourcing: live fetch versus frozen test snapshot | `type/adr` `risk/low` |
| M1-10 | `feasibility-service`: SGP4 propagation and access-window search | `type/feature` `area/feasibility` `risk/high` |
| M1-11 | `feasibility-service`: SAR geometry filter and footprint generation | `type/feature` `area/feasibility` `risk/high` |
| M1-12 | Golden-reference tests for orbital math against known passes | `type/test` `area/feasibility` `risk/high` |
| M1-13 | `feasibility-service`: idempotent consumer and publisher | `type/feature` `area/feasibility` `risk/high` |
| M1-14 | `plan-gateway`: read model projector and REST reads | `type/feature` `area/plan-gateway` `risk/medium` |
| M1-15 | `plan-gateway`: CZML and GeoJSON serialisation | `type/feature` `area/plan-gateway` `risk/medium` |
| M1-16 | `web`: Next.js shell and Cesium globe with constellation | `type/feature` `area/web` `risk/medium` |
| M1-17 | ADR-0009 — CesiumJS and deck.gl division of labour | `type/adr` `risk/low` |
| M1-18 | Integration tests with Testcontainers: duplicate and out-of-order delivery | `type/test` `area/ci` `risk/high` |
| M1-19 | Seed data and `make demo` | `type/chore` `area/infra` `risk/low` |
| M1-20 | OTel tracing across the first async hop | `type/feature` `area/observability` `risk/medium` |
| M1-21 | OpenAPI spec for `plan-gateway` read API | `type/feature` `area/contracts` `area/plan-gateway` `risk/medium` |
| M1-22 | ADR-0013 — parallel agent execution model | `type/adr` `type/docs` `risk/low` |
| M1-23 | Cold-start gate is a required check that cannot report on most PRs | `type/bug` `area/ci` `risk/medium` |
| M1-24 | Enforce the ADR-0013 path-ownership rule in CI | `type/chore` `area/ci` `risk/medium` |

M1-21 to M1-24 are not in the original decomposition. All four descend from
[ADR-0013](decisions/0013-parallel-agent-execution-in-worktrees.md), which decided
how M1 is executed concurrently — and then, in checking its own premise, kept
turning things up:

- **M1-21** — `plan-gateway`'s read API is frozen in no contract, so the tracks
  that produce and consume it cannot fork.
- **M1-23** — the cold-start gate was a *required* status check with a
  path-filtered trigger, so it never reported on PRs outside the filter and
  blocked them permanently. Found by the ADR's own PR being unable to merge.
- **M1-24** — the ADR's path-ownership rule is stated and unenforced, which the
  ADR itself lists as a cost. #81 is the argument for closing it: a CI gate nobody
  had watched work did not work.

**M1-01 and M1-21 are prerequisites, not parallel tracks.** M1-21 blocks M1-16 —
`web` renders what `plan-gateway` serves, and with no contract between them those
two tracks cannot fork without inventing incompatible interpretations of the same
boundary. M1-01 blocks tracks A and C, which both build against that schema.
M1-24 should also land before the fan-out; retrofitting an ownership rule onto
work that already violated it is the expensive order.

---

## M2 — The planner

**Goal:** the centrepiece. A genuinely hard scheduling problem, solved four ways
and measured.

**Exit criteria:** contested requests produce a conflict-free plan; the policy
benchmark report shows each heuristic's plan value as a percentage of the ExactDP
optimum, with runtimes.

| # | Issue | Labels |
| --- | --- | --- |
| M2-01 | `planner-service` skeleton, round trigger (cadence + debounce), advisory locking | `type/feature` `area/planner` `risk/high` |
| M2-02 | Slew-time model `slew_time(a, b)` | `type/feature` `area/planner` `risk/high` |
| M2-03 | Per-orbit duty-cycle budget enforcement | `type/feature` `area/planner` `risk/medium` |
| M2-04 | `AllocationPolicy` interface and plan commit transaction | `type/feature` `area/planner` `risk/high` |
| M2-05 | Policy: `GreedyByBid` | `type/feature` `area/planner` `risk/low` |
| M2-06 | Policy: `GreedyByValueDensity` | `type/feature` `area/planner` `risk/medium` |
| M2-07 | Policy: `VickreySealedBid` | `type/feature` `area/planner` `risk/high` |
| M2-08 | Policy: `ExactDP` as ground truth | `type/feature` `area/planner` `risk/high` |
| M2-09 | Fairness: priority-tier multipliers and ageing | `type/feature` `area/planner` `risk/medium` |
| M2-10 | Plan supersession and re-planning | `type/feature` `area/planner` `risk/high` |
| M2-11 | ADR-0014 — planner-side re-planning semantics | `type/adr` `risk/low` |
| M2-12 | Property-based tests for scheduler invariants | `type/test` `area/planner` `risk/high` |
| M2-13 | Policy benchmark harness and `docs/policy-benchmark.md` | `type/perf` `area/planner` `risk/medium` |
| M2-14 | ADR-0007 — allocation strategy, heuristic versus optimal | `type/adr` `risk/low` |
| M2-15 | Unfulfilment reasons with structured explanations | `type/feature` `area/planner` `risk/medium` |
| M2-16 | Planner input projections: `request_snapshots` and `candidate_opportunities` | `type/feature` `area/infra` `area/planner` `risk/high` |

M2-16 is not in the original decomposition, and like M1-21 it is a **prerequisite,
not a track.** M1-01 gave the planner every table it writes and none that it
reads: the candidates arrive on one event, and the bid, tier and deadline the
planner allocates by arrive on another. Where those facts come from when a round
fires is a consistency decision — settled in
[ADR-0015](decisions/0015-planner-projects-its-own-request-value.md) — and it has
to be settled before M2-01 exists, because every issue from M2-01 onward reads
those two tables.

It also touches `db/migrations/`, which [ADR-0013](decisions/0013-parallel-agent-execution-in-worktrees.md)
names a shared-fate path no track may write to. That is what makes it solo work in
the main session rather than the first item of a planner track.

**Sequencing for M2**, on the same principle
[`00-methodology.md`](ai-engineering/00-methodology.md) states — parallelise where
a contract exists, serialise where an invariant lives:

| Phase | Work | Concurrency |
| --- | --- | --- |
| Prerequisite | M2-16 projections and ADR-0015 | Solo, main session |
| Core | M2-01, M2-02, M2-04, M2-10 — the invariant lives here | Solo, one owner |
| Fan-out | M2-05…M2-08, the four policies behind the frozen interface | Parallel |
| Integration | M2-03, M2-09, M2-12, M2-13, M2-15 | Solo, after merge |

The policies are the only genuinely parallel work in M2, and they are parallel for
exactly one reason: `AllocationPolicy` is a contract, each policy is a pure
function behind it, and no two policies share a file. That contract does not exist
until M2-04, so the fan-out cannot start earlier no matter how many agents are
available.

---

## M3 — Resilience and performance

**Goal:** prove it holds up, and know exactly how it fails.

**Exit criteria:** `docs/performance.md` contains real numbers from real hardware
including a documented failure mode from the breakpoint test; chaos tests pass in
CI.

| # | Issue | Labels |
| --- | --- | --- |
| M3-01 | Idempotent-consumer hardening across all consumers | `type/feature` `area/infra` `risk/high` |
| M3-02 | DLQ implementation and replay tooling | `type/feature` `area/infra` `risk/medium` |
| M3-03 | Chaos tests: kill a consumer mid-transaction, restart the outbox relay | `type/test` `area/ci` `risk/high` |
| M3-04 | Circuit breaker and bulkhead on cross-service calls | `type/feature` `area/infra` `risk/medium` |
| M3-05 | End-to-end OTel tracing across every async hop | `type/feature` `area/observability` `risk/medium` |
| M3-06 | RED and domain metrics, Grafana dashboards as committed JSON | `type/feature` `area/observability` `risk/low` |
| M3-07 | k6 suite with thresholds as CI gates | `type/perf` `area/ci` `risk/high` |
| M3-08 | Breakpoint test and failure-mode write-up | `type/perf` `risk/high` |
| M3-09 | `docs/performance.md` with graphs | `type/docs` `type/perf` `risk/low` |

---

## M4 — Frontend depth

**Goal:** make the system's reasoning visible, not just its output.

**Exit criteria:** submitting a contested request visibly re-plans the globe and
timeline; clicking a losing request explains exactly which constraint bound.

| # | Issue | Labels |
| --- | --- | --- |
| M4-01 | deck.gl 2D planning view: coverage, density, conflict clusters | `type/feature` `area/web` `risk/medium` |
| M4-02 | Per-satellite timeline with visible slew gaps | `type/feature` `area/web` `risk/high` |
| M4-03 | SSE live updates from `plan-gateway` | `type/feature` `area/web` `area/plan-gateway` `risk/medium` |
| M4-04 | The "why was my request rejected?" panel | `type/feature` `area/web` `risk/high` |
| M4-05 | Ghost rendering of losing candidates on the timeline | `type/feature` `area/web` `risk/medium` |
| M4-06 | Acquisition execution simulator | `type/feature` `area/planner` `risk/medium` |
| M4-07 | Frontend performance: virtualisation and Cesium update throttling | `type/perf` `area/web` `risk/high` |
| M4-08 | Playwright E2E: happy path and contested-window path | `type/test` `area/web` `risk/medium` |

---

## M5 — Presentation

**Goal:** the repo defends itself when nobody is in the room to defend it.

| # | Issue | Labels |
| --- | --- | --- |
| M5-01 | README with architecture diagram, demo GIF, and scope cuts | `type/docs` `risk/low` |
| M5-02 | `docs/ai-engineering/` write-up, all four documents | `type/docs` `risk/medium` |
| M5-03 | ADR index review, statuses, supersessions | `type/docs` `risk/low` |
| M5-04 | Five-minute demo script | `type/docs` `risk/low` |
| M5-05 | Cold-start verification on a clean machine | `type/chore` `area/ci` `risk/medium` |
| M5-06 | Coverage badges and CI status | `type/chore` `area/ci` `risk/low` |

---

## Issue conventions

Every issue body carries:

- **Context** — why this exists, in one or two sentences
- **Acceptance criteria** — checkboxes, testable, no "works correctly"
- **Engineering decisions** — the choices this issue forces, and whether any of
  them needs an ADR. This section is mandatory even when the answer is "none" —
  the point is that the question is always asked

Workflow: one branch per issue, one PR per branch, `Closes #N`, Conventional
Commits, squash-merge. Branch protection requires CI green including the coverage
gate.

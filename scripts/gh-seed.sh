#!/usr/bin/env bash
#
# Seed GitHub milestones, labels, and issues from docs/backlog.md.
#
# Idempotent: labels are upserted, milestones and issues are matched by title
# and skipped if they already exist. Safe to re-run after editing.
#
#   ./scripts/gh-seed.sh              # create everything
#   ./scripts/gh-seed.sh --dry-run    # print what would be created
#
set -euo pipefail

REPO="${REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}"
DRY_RUN=false
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=true

say()  { printf '\033[0;36m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[0;32m+\033[0m %s\n' "$*"; }
skip() { printf '  \033[0;90m=\033[0m %s (exists)\n' "$*"; }

say "Repository: $REPO"
$DRY_RUN && say "DRY RUN — nothing will be created"

# ---------------------------------------------------------------------------
# Labels
# ---------------------------------------------------------------------------

label() {
  local name="$1" color="$2" desc="$3"
  if $DRY_RUN; then ok "label $name"; return; fi
  gh label create "$name" --repo "$REPO" --color "$color" --description "$desc" --force >/dev/null
  ok "label $name"
}

say "Labels"
label "type/feature"       "1d76db" "New capability"
label "type/chore"         "cfd3d7" "Tooling, scaffolding, dependencies"
label "type/docs"          "0075ca" "Documentation"
label "type/adr"           "5319e7" "Architecture decision record"
label "type/test"          "0e8a16" "Test infrastructure or coverage"
label "type/perf"          "fbca04" "Performance work or load testing"
label "type/spike"         "d4c5f9" "Time-boxed investigation with a written outcome"
label "type/bug"           "d73a4a" "Defect"

label "area/contracts"     "006b75" "Schemas, OpenAPI, codegen"
label "area/tasking-api"   "1f6feb" "Go REST ingress"
label "area/feasibility"   "1f6feb" "Python SGP4 and SAR geometry"
label "area/planner"       "1f6feb" "Go allocation and de-confliction"
label "area/plan-gateway"  "1f6feb" "Go read models and SSE"
label "area/web"           "1f6feb" "Next.js, Cesium, deck.gl"
label "area/infra"         "5a5a5a" "Compose, Postgres, NATS, migrations"
label "area/ci"            "5a5a5a" "Pipelines and gates"
label "area/observability" "5a5a5a" "Tracing, metrics, dashboards"

label "risk/high"          "b60205" "Correctness is hard here; needs property-based or golden-reference tests"
label "risk/medium"        "e99695" "Non-trivial, but failure modes are visible"
label "risk/low"           "f9d0c4" "Mechanical or well-understood"

# ---------------------------------------------------------------------------
# Milestones
# ---------------------------------------------------------------------------

milestone() {
  local title="$1" desc="$2"
  if gh api "repos/$REPO/milestones?state=all&per_page=100" -q '.[].title' | grep -Fxq "$title"; then
    skip "milestone $title"; return
  fi
  if $DRY_RUN; then ok "milestone $title"; return; fi
  gh api "repos/$REPO/milestones" -X POST -f title="$title" -f description="$desc" >/dev/null
  ok "milestone $title"
}

say "Milestones"
milestone "M0 — Foundations and contracts" \
  "Every interface is law before any implementation exists. No service code. Exit: contracts frozen and generating into Go and Python, CI green, docker compose up brings the infrastructure online, ADRs 0001-0005 written and defensible."
milestone "M1 — Vertical slice" \
  "A request submitted over HTTP produces real access windows from real TLEs and appears on a globe. Thin but complete, end to end. Exit: make demo submits one request and a satellite plus its opportunity footprints render in Cesium, with the whole path visible in one distributed trace."
milestone "M2 — The planner" \
  "The centrepiece. A genuinely hard scheduling problem solved four ways and measured. Exit: contested requests produce a conflict-free plan, and the policy benchmark shows each heuristic's plan value as a percentage of the ExactDP optimum with runtimes."
milestone "M3 — Resilience and performance" \
  "Prove it holds up, and know exactly how it fails. Exit: docs/performance.md has real numbers from real hardware including a documented failure mode from the breakpoint test, and chaos tests pass in CI."
milestone "M4 — Frontend depth" \
  "Make the system's reasoning visible, not just its output. Exit: submitting a contested request visibly re-plans the globe and timeline, and clicking a losing request explains which constraint bound."
milestone "M5 — Presentation" \
  "The repo defends itself when nobody is in the room to defend it."

# ---------------------------------------------------------------------------
# Issues
# ---------------------------------------------------------------------------

EXISTING="$(gh issue list --repo "$REPO" --state all --limit 500 --json title -q '.[].title' || true)"

issue() {
  local title="$1" ms="$2" labels="$3" body="$4"
  if grep -Fxq "$title" <<<"$EXISTING"; then skip "$title"; return; fi
  if $DRY_RUN; then ok "$title"; return; fi
  gh issue create --repo "$REPO" --title "$title" --milestone "$ms" \
    --label "$labels" --body "$body" >/dev/null
  ok "$title"
}

M0="M0 — Foundations and contracts"
M1="M1 — Vertical slice"
M2="M2 — The planner"
M3="M3 — Resilience and performance"
M4="M4 — Frontend depth"
M5="M5 — Presentation"

say "Issues — $M0"

issue "M0-01 — Repo skeleton, Makefile, and editor config" "$M0" "type/chore,area/infra,risk/low" \
'## Context
The directory layout is the first thing a reviewer sees. It should make the contracts-first structure obvious without explanation.

## Acceptance criteria
- [ ] Directory skeleton per `docs/backlog.md`: `contracts/`, `services/`, `web/`, `db/migrations/`, `deploy/`, `loadtest/`, `testdata/`, `docs/`, `scripts/`
- [ ] `Makefile` with `help` as the default target, listing every task with a one-line description
- [ ] `.gitignore` covering Go, Python, Node, and generated artefacts (but NOT `gen/`, which is checked in deliberately)
- [ ] `.editorconfig` consistent with gofmt, ruff, and prettier
- [ ] `make help` runs on a clean clone with no toolchain installed

## Engineering decisions
- `gen/` is committed rather than gitignored. Reviewers see real types in a PR and contributors build without installing three generators; the drift check in M0-07 stops the committed copy becoming a lie.
- `gen/` sits at the repo root rather than inside each service, so the drift check is a single CI job and the contract reads as upstream of all four services.
- Make over Taskfile/just: zero install on macOS and Linux, and the spec already commits to `make demo`.'

issue "M0-02 — ADR template and ADRs 0001-0005" "$M0" "type/adr,type/docs,risk/low" \
'## Context
No decision in this repo may be silent. The five foundational decisions are made before any code exists, because they constrain everything after them.

## Acceptance criteria
- [ ] `docs/decisions/0000-template.md` in MADR format, including a **Confirmation** section
- [ ] ADR-0001 polyglot Go + Python split
- [ ] ADR-0002 NATS JetStream over Kafka and RabbitMQ
- [ ] ADR-0003 consistency boundaries and CAP position per service
- [ ] ADR-0004 PostgreSQL with JSONB instead of a separate document store
- [ ] ADR-0005 Docker Compose over Kubernetes
- [ ] `docs/decisions/README.md` index with statuses and a Planned section
- [ ] Every ADR names the rejected alternatives and why each lost
- [ ] Every ADR has a Confirmation section stating what would prove it wrong

## Engineering decisions
- The template mandates Confirmation. A decision nothing could falsify is a preference, and the section forces the distinction.
- Superseded ADRs are kept with an updated status line, never edited or deleted. A visible change of mind with a paper trail is a strength.'

issue "M0-03 — C4 context and container diagrams" "$M0" "type/docs,risk/low" \
'## Context
The architecture has to be legible before it is built, and the container diagram is what the M1 issues are cut against.

## Acceptance criteria
- [ ] `docs/architecture/c4-context.md` — actors, external dependencies, explicit scope cuts at the boundary
- [ ] `docs/architecture/c4-container.md` — all nine containers with responsibility and CAP posture
- [ ] An event-flow diagram showing the happy path **and** every failure branch
- [ ] A table justifying each service boundary by what differs across it
- [ ] Mermaid, rendering correctly on GitHub

## Engineering decisions
- Mermaid over Structurizr or PlantUML: renders natively in GitHub, so the diagram is visible in the browser rather than requiring a build step nobody will run.
- Failure branches are drawn, not just the happy path. The failure branches are where the interesting design is.'

issue "M0-04 — Event contract schemas" "$M0" "type/feature,area/contracts,risk/high" \
'## Context
Eight event schemas plus shared definitions. These are frozen before any service exists — they are what makes the polyglot split affordable and parallel work safe.

## Acceptance criteria
- [ ] `contracts/common/`: `envelope`, `primitives`, `geometry`, `sar` (JSON Schema 2020-12)
- [ ] Eight event schemas in `contracts/events/`, one file per subject
- [ ] Every event carries `event_id`, `event_type`, `schema_version`, `occurred_at`, `correlation_id`, `causation_id`, `producer`
- [ ] Every schema has at least one `examples` entry that validates against itself
- [ ] `make contracts-validate` validates all examples and fixtures
- [ ] `contracts/README.md` documents versioning rules and the strict-producer / tolerant-consumer split

## Engineering decisions
- Envelope fields are repeated per event with `$ref` to shared field definitions rather than composed with `allOf`. Costs seven lines per file; buys readable generated code in both languages, since both generators produce awkward output for `allOf`.
- `additionalProperties: false` is a **producer-side** contract enforced in CI. Consumers deserialise into generated structs and ignore unknown fields, keeping the tolerant-reader property. Validating strictly on the consumer side is how a backwards-compatible change takes down a system.
- Major version lives in the subject, not only the payload, so v1 and v2 consumers coexist.
- `planning.request.unfulfilled.v1` carries structured per-reason explanation data rather than a message string, because a shortfall a customer can act on has to be a number.'

issue "M0-05 — OpenAPI spec for tasking-api" "$M0" "type/feature,area/contracts,risk/medium" \
'## Context
The full REST surface of the ingress service, frozen before implementation.

## Acceptance criteria
- [ ] `contracts/openapi/tasking-api.v1.yaml` — submit, get, list, cancel, `healthz`, `readyz`
- [ ] `Idempotency-Key` is a **required** header on submit
- [ ] RFC 9457 `application/problem+json` for every error response
- [ ] The request lifecycle state machine is documented in the spec description
- [ ] `422` is distinguished from `400`, and `409` from an idempotent replay
- [ ] Cursor pagination on list, not offset
- [ ] Spec passes `spectral lint`

## Engineering decisions
- `202 Accepted`, never `201`. At submission time nobody knows if the target is imageable; a synchronous answer would put orbital propagation inside the ingress latency budget.
- `Idempotency-Key` required rather than optional. Optional means the default is unsafe under retry and clients find out in production.
- Same key + different body returns `409`, not a replay. Treating it as a replay silently discards a request the customer believes they submitted.
- `readyz` checks Postgres but deliberately **not** NATS. The outbox decouples publication from acceptance; failing readiness on a broker outage would turn a recoverable delay into refused traffic and discard the outbox pattern entire benefit.
- Cancellation is `POST /cancellation`, not `DELETE`. It is a recorded state transition, not a deletion.
- OpenAPI 3.0.3 rather than 3.1 despite the JSON Schema dialect mismatch, because oapi-codegen 3.1 support is immature. Documented in `contracts/README.md` rather than left as a silent inconsistency.'

issue "M0-06 — NATS stream, consumer, and DLQ topology" "$M0" "type/feature,area/contracts,area/infra,risk/medium" \
'## Context
Subject naming, streams, consumer settings, and the DLQ are part of the contract: changing a delivery policy changes behaviour as surely as changing a schema.

## Acceptance criteria
- [ ] `contracts/nats/topology.md` — subjects, three streams, six consumers, DLQ, replay procedure
- [ ] `deploy/nats/streams.yaml` — declarative stream and consumer definitions applied by an init container
- [ ] Every consumer has justified `ack_wait`, `max_deliver`, and `max_ack_pending`
- [ ] DLQ subject convention and header set documented
- [ ] Trace-propagation contract documented (W3C `traceparent` in NATS headers)

## Engineering decisions
- `limits` retention, not `workqueue`. Multiple consumer groups read the same subjects; `workqueue` deletes on first ack and silently breaks fan-out.
- Three streams by producing domain rather than one per subject (operational surface for no gain) or one giant stream (couples unrelated retention decisions).
- Pull consumers, not push. Each service sets its own concurrency, which matters because a feasibility sweep and a planner round have completely different cost profiles.
- Consumers dispatch on `event_type`, never on the subject received, so replay from a file behaves identically to replay from the stream.
- Topology is declared as config applied by an init container rather than created lazily from application code, so it is reviewable in a PR and two services cannot race to create the same stream with different settings.'

issue "M0-07 — Codegen pipeline with drift check" "$M0" "type/chore,area/contracts,area/ci,risk/medium" \
'## Context
Generated code is what makes the contract enforceable across the language boundary rather than merely documented.

## Acceptance criteria
- [ ] `oapi-codegen` generates Go types and chi server interfaces from OpenAPI
- [ ] `go-jsonschema` generates Go structs from event schemas
- [ ] `datamodel-code-generator` generates Pydantic v2 models from the same schemas
- [ ] Output committed under `gen/`
- [ ] `make contracts-generate` and `make contracts-verify`
- [ ] CI fails if regeneration produces any diff
- [ ] Generator versions pinned exactly

## Engineering decisions
- Three generators is the cost; the drift check as a build-time gate is what it buys. It catches the exact failure mode that kills polyglot systems: one side changes a field, the other finds out in production.
- `ogen` rejected — generates a complete server but far more code with a strong opinion on handler shape, and every line has to be defensible.
- No-codegen rejected — fewest dependencies but manual cross-language sync, with drift found at test time instead of build time.
- Versions pinned exactly because a generator minor bump reformatting output would fail the drift check for no semantic reason.'

issue "M0-08 — Docker Compose skeleton" "$M0" "type/chore,area/infra,risk/medium" \
'## Context
The definition of done is `git clone && docker compose up` producing a working, seeded system in under five minutes on a clean machine. That budget is the constraint everything here serves.

## Acceptance criteria
- [ ] Postgres 16 + PostGIS with a persistent volume
- [ ] NATS JetStream with file storage and a topology init container
- [ ] OTel Collector, Prometheus, Grafana with provisioned datasources
- [ ] Health-gated `depends_on` so start order is correct rather than lucky
- [ ] `.env.example` with every variable documented
- [ ] `docker compose up` reaches all-healthy in under five minutes on a cold cache
- [ ] Service placeholders present but not yet implemented

## Engineering decisions
- File storage for NATS, not memory. Memory storage would make a broker restart lose in-flight work and make the durability claims false.
- Health-gated `depends_on`, not `sleep`. Timing-based startup is a flake generator.
- PostGIS materially increases image size and therefore cold-start time. Measured and accepted; the number goes in `docs/performance.md`.'

issue "M0-09 — CI pipeline with coverage gates" "$M0" "type/chore,area/ci,risk/medium" \
'## Context
CI is the verification harness for AI-generated code. It has to be a gate, not a report.

## Acceptance criteria
- [ ] Go: `golangci-lint`, `go test -race`, coverage
- [ ] Python: `ruff`, `mypy --strict`, `pytest`, coverage
- [ ] TypeScript: `eslint`, `tsc --noEmit`, `vitest`
- [ ] Contract validation job: all examples validate against their schemas
- [ ] Codegen drift job
- [ ] Cold-start job asserting `docker compose up` is healthy within five minutes
- [ ] Coverage gate: 80% overall, 95% on planner and geometry packages
- [ ] Jobs run in parallel; path filters skip untouched languages

## Engineering decisions
- `-race` always on in CI. The concurrency bugs this project can produce are exactly the ones that are invisible without it.
- 80/95 rather than 100. 100% coverage on adapter and wiring code is theatre; 95% on the packages where correctness is genuinely hard is a real signal. To be defended in ADR-0010.
- The cold-start check is a gate because it is a stated requirement. Requirements that are not gated are aspirations.'

issue "M0-10 — Issue and PR templates, labels, branch protection" "$M0" "type/chore,area/ci,risk/low" \
'## Context
The commit and issue history is read as closely as the code. It should read as a narrative of reasoning.

## Acceptance criteria
- [ ] `.github/ISSUE_TEMPLATE/` — feature, bug, spike, ADR
- [ ] Every issue template has a mandatory **Engineering decisions** section
- [ ] `.github/PULL_REQUEST_TEMPLATE.md` with `Closes #N`, decisions made, and how it was verified
- [ ] `.github/labels.yml` and a sync workflow
- [ ] Branch protection on `main`: CI green required, squash-merge only, linear history
- [ ] `CONTRIBUTING.md` documenting Conventional Commits and one-branch-per-issue

## Engineering decisions
- The Engineering decisions section is mandatory even when the answer is "none". The point is that the question is always asked; optional sections are never filled in.
- Squash-merge with linear history: the commit log becomes the narrative, and one commit per issue is readable in a way a merge-bubble graph is not.'

issue "M0-11 — CLAUDE.md and ai-engineering scaffolding" "$M0" "type/docs,risk/low" \
'## Context
AI-assisted engineering is a first-class deliverable here, not a footnote. The methodology is the artifact.

## Acceptance criteria
- [ ] `CLAUDE.md` at the repo root: non-negotiables, layout, conventions, working style
- [ ] `docs/ai-engineering/00-methodology.md` — how work was decomposed for parallel agents
- [ ] `01-agent-roles.md` — the roles used, with actual prompts in `prompts/`
- [ ] `02-verification.md` — what was not trusted, and how it was checked
- [ ] `03-what-worked-what-didnt.md` — filled in as the project runs, not retrofitted at the end
- [ ] `prompts/` with the real prompts used

## Engineering decisions
- The central claim of `00-methodology.md`: contracts-first is what makes agent parallelism safe. Once schemas and OpenAPI are frozen, three services can be built concurrently with no merge collisions because the interface is already law. Without that, parallel agents produce integration debt faster than features.
- `03` is written continuously. Retrofitted honesty reads as retrofitted.'

issue "M0-12 — ADR-0010: test strategy and coverage targets" "$M0" "type/adr,type/test,risk/medium" \
'## Context
The test strategy is the verification harness for generated code. It needs to be a deliberate design, written down before the tests exist.

## Acceptance criteria
- [ ] `docs/decisions/0010-test-strategy-and-coverage.md`
- [ ] Each test layer justified: unit, property-based, golden-reference, integration, contract, E2E
- [ ] Explains why orbital geometry gets golden-reference tests rather than snapshots
- [ ] Explains why the scheduler gets property-based tests rather than examples
- [ ] Defends 80/95 coverage and explains why 100% would be theatre
- [ ] Names what is deliberately not tested and why

## Engineering decisions
- Physics needs an oracle, not a snapshot of our own output. A snapshot test on SGP4 asserts that the code still does what it did yesterday, including if what it did yesterday was wrong. Validation is against known passes for a public satellite at a fixed TLE and epoch.
- The scheduler gets invariants over examples: for any generated input, the output plan must never contain overlapping acquisitions or violate slew time. Example tests only cover the cases you thought of, and the failures here are in cases nobody thought of.
- This framing — the test suite as the verification harness for AI-generated code — is the strongest available answer to "how do you know the generated code is right?"'

say "Issues — $M1"

issue "M1-01 — Postgres schema, migrations, and the exclusion constraint" "$M1" "type/feature,area/infra,risk/high" \
'## Context
The single most convincing artifact in this repo: the non-overlap invariant enforced by the database rather than by application logic.

## Acceptance criteria
- [ ] Tables: `customers`, `satellites`, `tle_sets`, `tasking_requests`, `opportunities`, `collection_plans`, `acquisitions`, `outbox`, `processed_events`, `idempotency_keys`
- [ ] `acquisitions` has a `tstzrange` window column and a GiST `EXCLUDE` constraint on `(satellite_id WITH =, window WITH &&)`
- [ ] PostGIS geometry columns with GiST indexes on targets and footprints
- [ ] JSONB with GIN indexes for raw TLE, mode parameters, computed geometry
- [ ] `version` column on `collection_plans` for optimistic concurrency
- [ ] Migrations run forward and backward cleanly
- [ ] **Test: two overlapping acquisitions inserted via raw SQL, bypassing all application code, are rejected by the constraint**

## Engineering decisions
- The raw-SQL rejection test is the load-bearing one. If the constraint can be bypassed through any path, ADR-0004 has failed at its central claim.
- Schema-per-service namespaces in one database: ownership is explicit and a service cannot accidentally write another service tables, without paying for a second database.
- JSONB restricted to four documented cases. Anything appearing in a `WHERE` clause on a hot path gets promoted to a real column.'

issue "M1-02 — tasking-api: hexagonal skeleton, config, health, logging" "$M1" "type/feature,area/tasking-api,risk/low" \
'## Context
The shape the other three Go services will follow. Worth getting right once.

## Acceptance criteria
- [ ] `cmd/` plus `internal/{domain,app,port,adapter}` — domain has no imports from adapter
- [ ] Configuration from environment with validation at startup and fail-fast on missing values
- [ ] `/healthz` and `/readyz` per the OpenAPI spec
- [ ] Structured JSON logs with `correlation_id` on every line
- [ ] Graceful shutdown draining in-flight requests
- [ ] An architecture test asserting the domain layer imports nothing from adapters

## Engineering decisions
- Hexagonal here buys one specific thing: the allocation and state-machine logic is testable without Postgres or NATS, which is what makes property-based testing practical at all. That is the justification; "clean architecture" is not.
- The import-direction rule is enforced by a test, not by convention. Conventions decay.'

issue "M1-03 — tasking-api: submit endpoint with validation" "$M1" "type/feature,area/tasking-api,risk/medium" \
'## Context
The ingress path. Must be fast and must never accept a request it did not store.

## Acceptance criteria
- [ ] `POST /v1/tasking-requests` matching the generated server interface
- [ ] Validation: window ordering, deadline in the future, geometry well-formed and closed, horizon within the configured maximum, constraints satisfiable by some configured sensor
- [ ] RFC 9457 problem responses with JSON Pointer field errors
- [ ] `422` distinguished from `400`
- [ ] `503` rather than `202` when the request cannot be persisted
- [ ] Table-driven tests covering every rejection reason code

## Engineering decisions
- Validation is cheap and local only. Whether the target is imageable requires propagating the constellation, which is the entire reason this endpoint is asynchronous.
- `CONSTRAINTS_UNSATISFIABLE` is caught at ingress rather than wasting a full feasibility sweep to discover it.
- Never `202` for a request we failed to store. Acknowledging a dropped request is unrecoverable business damage; a `503` the client retries is not.'

issue "M1-04 — tasking-api: HTTP idempotency" "$M1" "type/feature,area/tasking-api,risk/high" \
'## Context
Clients retry. Without idempotency, a retry creates a second request that competes with the first — the customer pays twice and gets one image.

## Acceptance criteria
- [ ] `idempotency_keys` table with a unique constraint on `(customer_id, key)`
- [ ] Key insert and request insert in the same transaction
- [ ] Replay with identical body returns the original `202` and original `request_id`, with `Idempotency-Replayed: true`
- [ ] Replay with a different body returns `409`
- [ ] Body fingerprint stored as a hash of the canonicalised payload, not the raw bytes
- [ ] 24-hour expiry with a cleanup job
- [ ] **Integration test: N concurrent identical submissions create exactly one request**

## Engineering decisions
- Unique constraint over check-then-insert. Check-then-insert is a race, and the concurrency test exists specifically to prove which one was implemented.
- Fingerprint the canonicalised body: key reuse with different content is a client bug that must surface, not be silently treated as a replay.
- Same transaction as the request insert. Separate transactions leave a crash window where the key exists and the request does not, permanently swallowing that submission.'

issue "M1-05 — tasking-api: transactional outbox and relay" "$M1" "type/feature,area/tasking-api,risk/high" \
'## Context
The dual-write problem: write to Postgres and publish to NATS, and a crash between them means the state changed but nobody was told, or the reverse. Both are corruption.

## Acceptance criteria
- [ ] `outbox` table written in the same transaction as the business state change
- [ ] Relay polls with `FOR UPDATE SKIP LOCKED`, publishes, marks sent
- [ ] `traceparent` captured at write time and injected into NATS headers at publish time
- [ ] At-least-once publication with a stable `event_id` across retries
- [ ] Exponential backoff on publish failure
- [ ] Relay metrics: lag, batch size, failure rate
- [ ] **Integration test: relay killed mid-publish; on restart every event is published exactly once as observed by an idempotent consumer**

## Engineering decisions
- Never publish inside the business transaction. The publish would succeed and the transaction could still roll back, announcing a fact that never became true.
- `SKIP LOCKED` so multiple relay instances can run without coordination or double publication.
- `traceparent` captured at write time, not publish time. Capturing it at publish time attributes the event to the relay poll loop and severs the trace at exactly the hop this project claims to preserve.
- Stable `event_id` across retries is what makes consumer-side deduplication possible at all.'

issue "M1-06 — ADR-0006 transactional outbox and ADR-0008 idempotency" "$M1" "type/adr,risk/low" \
'## Context
Two mechanisms that carry most of the delivery-guarantee weight. Both need writing up while the reasoning is fresh.

## Acceptance criteria
- [ ] `0006-transactional-outbox.md` — the dual-write problem, alternatives rejected (2PC, CDC/Debezium, publish-then-write, listen/notify)
- [ ] `0008-idempotency-approach.md` — HTTP keys and the idempotent-consumer pattern
- [ ] ADR-0008 states precisely the difference between at-least-once **delivery** and effectively-once **processing**
- [ ] Both name what would falsify them

## Engineering decisions
- CDC/Debezium is the strongest rejected alternative and deserves a fair hearing: it removes the relay entirely and has no polling lag. It loses on operational weight — a Kafka Connect cluster to avoid one polling loop — and on the fact that the outbox row is a deliberate, reviewable contract while a CDC feed of raw table changes is not.
- The at-least-once / effectively-once distinction is a classic interview probe and the ADR should be precise enough to answer it cold.'

issue "M1-07 — tasking-api: request state machine" "$M1" "type/feature,area/tasking-api,risk/medium" \
'## Context
Requests move through states driven by events arriving out of order from three services. Implicit state transitions would be unauditable.

## Acceptance criteria
- [ ] Explicit state machine: `RECEIVED`, `AWAITING_PLANNING`, `PLANNED`, `ACQUIRED`, `INFEASIBLE`, `REJECTED`, `EXPIRED`, `CANCELLED`
- [ ] Transition table as data, with illegal transitions rejected and logged rather than ignored
- [ ] Consumers drive transitions from `feasibility.*`, `planning.*`, and `acquisition.*`
- [ ] Out-of-order arrival tolerated via `occurred_at` plus state guards
- [ ] A losing request returns to `AWAITING_PLANNING` rather than failing
- [ ] Table-driven tests over every legal and illegal transition

## Engineering decisions
- Transitions as data, not scattered `if` statements. The table is directly comparable with the diagram in the OpenAPI spec, so drift between documentation and behaviour is visible.
- Illegal transitions are rejected and logged, never silently ignored. An ignored illegal transition is a bug that has already happened and left no evidence.
- Losing a round is not a failure state. Requests age, gain fairness weight, and compete again — which is only expressible if the state machine says so.'

issue "M1-08 — TLE ingestion from Celestrak with staleness classification" "$M1" "type/feature,area/feasibility,risk/high" \
'## Context
TLEs decay. A days-old element set drifts enough to move an access window by seconds to minutes, which is the difference between a flyable plan and an unflyable one.

## Acceptance criteria
- [ ] Fetch TLEs from Celestrak at seed time for the configured constellation
- [ ] `tle_epoch` decoded from the element set itself, not from fetch time
- [ ] Staleness classification against configured thresholds: `FRESH`, `AGING`, `STALE`
- [ ] Raw TLE stored in JSONB with full provenance
- [ ] Cached snapshot committed so `docker compose up` works with no network
- [ ] **Frozen snapshot in `testdata/tle/` used by golden tests, never the live fetch**
- [ ] Feasibility **refuses** to compute against a `STALE` TLE and emits `feasibility.failed.v1`

## Engineering decisions
- Live fetch at seed time exercises staleness honestly; a frozen fixture would make the whole `tle_epoch` mechanism decorative.
- Two TLE sources with two purposes — live for runtime, frozen for tests. Tests must be deterministic; nothing about orbital mechanics is provable against input that changes daily. Needs ADR-0011.
- Refusing to compute on `STALE` is the right behaviour. Publishing a confidently wrong access window is worse than publishing nothing, and this is exactly where generated code tends to helpfully carry on.'

issue "M1-09 — ADR-0011: TLE sourcing" "$M1" "type/adr,risk/low" \
'## Context
Live fetch at runtime plus a frozen snapshot for tests is a deliberate split that would otherwise become an undocumented assumption.

## Acceptance criteria
- [ ] `docs/decisions/0011-tle-sourcing.md`
- [ ] Alternatives rejected: fully frozen fixtures, fully live including tests, synthetic TLEs
- [ ] Documents the staleness thresholds and why those numbers
- [ ] States the cost: a network dependency at seed time, and a cached fallback needed to preserve the offline cold start

## Engineering decisions
- Honest about the tradeoff taken: determinism in tests was bought at the price of two sources of truth for the same data, which is a real maintenance cost and a real risk of them diverging.'

issue "M1-10 — feasibility-service: SGP4 propagation and access-window search" "$M1" "type/feature,area/feasibility,risk/high" \
'## Context
The physics. Everything downstream is only as correct as this.

## Acceptance criteria
- [ ] SGP4 propagation via the `sgp4`/Skyfield stack — no hand-rolled orbital mechanics
- [ ] Coarse-to-fine access search: coarse step to bracket, refinement to converge on window boundaries
- [ ] ECI to ECEF to geodetic conversions via `pyproj`, WGS84 throughout
- [ ] Orbit number computed per access, for the per-orbit duty-cycle budget
- [ ] Horizon clamped to the configured maximum, with `truncated` declared on the event
- [ ] Deterministic given a fixed TLE and epoch
- [ ] Benchmarked: full constellation over a 24h horizon within the p99 informing the consumer `ack_wait`

## Engineering decisions
- Library, never a reimplementation. This is the highest-risk code in the project for plausible-but-wrong output, and the mitigation is to use code that thousands of people have validated against real ephemerides.
- Coarse-to-fine over fixed fine stepping: a step small enough to never miss a short window is far too slow across a constellation and a multi-day horizon.
- Determinism is a hard requirement, not a nicety — golden tests are impossible without it.'

issue "M1-11 — feasibility-service: SAR geometry filter and footprints" "$M1" "type/feature,area/feasibility,risk/high" \
'## Context
The single most important domain fact: SAR is side-looking. A target directly beneath the satellite is not imageable. Getting this wrong models a different instrument entirely.

## Acceptance criteria
- [ ] Incidence angle computed on the WGS84 ellipsoid and filtered to the sensor band (default 15-45 degrees)
- [ ] Look side determined from the cross-track sign relative to the velocity vector; satellites and modes restricted to one side are respected
- [ ] Squint angle computed and filtered to the steering limit
- [ ] Slant range bounds enforced
- [ ] Roll angle computed per opportunity — the input to `slew_time(a, b)` in M2
- [ ] Footprint polygon generated per mode, geodesically correct, via Shapely and `pyproj`
- [ ] Point targets satisfied by footprint containment; polygon targets require full containment
- [ ] `quality_score` derived from position within the incidence band and squint magnitude
- [ ] **Test: a target at nadir produces zero opportunities**

## Engineering decisions
- The nadir test is the canary. Any implementation that treats this as a nadir-pointing sensor passes every other test and fails this one.
- `quality_score` is kept strictly separate from value. Value comes from bid and tier; conflating geometric quality with commercial value would quietly corrupt the allocation mechanism.
- Geodesic footprints, not planar approximations. A planar approximation is visibly wrong at high latitude, and the constellation includes sun-synchronous orbits that spend their time exactly there.'

issue "M1-12 — Golden-reference tests for orbital math" "$M1" "type/test,area/feasibility,risk/high" \
'## Context
Physics needs an oracle. A snapshot of our own output asserts only that the code still does what it did yesterday — including if yesterday was wrong.

## Acceptance criteria
- [ ] Access windows validated against known passes for a public satellite at a fixed TLE and epoch, from an independent source
- [ ] Frozen TLE fixtures in `testdata/tle/`, never a live fetch
- [ ] Tolerances stated explicitly and justified, not tuned until green
- [ ] Sub-satellite point cross-checked against an independent propagator at sampled times
- [ ] Coordinate transforms tested against published reference values
- [ ] Tests run with no network access

## Engineering decisions
- Independent oracle or the test is worthless. This is the specific issue where generated code was most likely to be plausible and wrong, and it is the reason this test exists as a first-class deliverable rather than as coverage.
- Tolerances are chosen from the physics — SGP4 accuracy against a TLE of known age — and written down. A tolerance tuned until the test passes is a test that asserts nothing.'

issue "M1-13 — feasibility-service: idempotent consumer and publisher" "$M1" "type/feature,area/feasibility,risk/high" \
'## Context
JetStream is at-least-once. Recomputing a sweep on redelivery is expensive; republishing opportunities creates phantom candidates the planner will happily allocate.

## Acceptance criteria
- [ ] Durable pull consumer with explicit ack and the configured `ack_wait`
- [ ] `processed_events` insert in the same transaction as the result write
- [ ] Duplicate delivery rolls back and acks — the message is not reprocessed and not redelivered forever
- [ ] Ack strictly after commit, never before
- [ ] Publishing via the outbox pattern
- [ ] Retryable and non-retryable failures distinguished; non-retryable ones do not consume redelivery budget
- [ ] **Integration test: same event delivered five times produces exactly one set of opportunities**

## Engineering decisions
- Ack ordering is not a matter of taste. Ack-then-crash loses the message; commit-then-crash causes a redelivery the dedup absorbs. The failure modes are asymmetric.
- `NO_ACCESS_IN_HORIZON` is a correct negative answer, not a failure. Retrying a physical impossibility burns compute forever; the `retryable` flag on `feasibility.failed.v1` exists precisely to keep these apart.'

issue "M1-14 — plan-gateway: read model projector and REST reads" "$M1" "type/feature,area/plan-gateway,risk/medium" \
'## Context
The read side. Eventually consistent by design, and honest about it.

## Acceptance criteria
- [ ] Projector consuming every stream and materialising read models
- [ ] Idempotent projection — replay from sequence zero rebuilds identical state
- [ ] REST reads: satellites, opportunities by request, plans by satellite and window
- [ ] Staleness surfaced in responses rather than hidden
- [ ] Stale `plan_version` arriving after a newer one is dropped
- [ ] **Test: full replay from the beginning of the stream produces byte-identical read models**

## Engineering decisions
- The replay test is what makes read-model rebuild a routine operation rather than an incident, and it is the practical payoff for choosing a broker with first-class replay.
- Staleness surfaced, not hidden. A read model that silently lies about its freshness is worse than one that admits it, especially behind a live globe.
- Version guards rather than assumed ordering. Nothing in this system assumes message order.'

issue "M1-15 — plan-gateway: CZML and GeoJSON serialisation" "$M1" "type/feature,area/plan-gateway,risk/medium" \
'## Context
Cesium consumes CZML; deck.gl consumes GeoJSON. Serving both from the read model keeps geometry logic out of the browser.

## Acceptance criteria
- [ ] CZML for satellite positions, orbit tracks, ground tracks, and swath footprints with a time-dynamic interval
- [ ] GeoJSON for footprints, targets, and coverage
- [ ] Time-windowed queries so the client fetches only the visible horizon
- [ ] Response sizes measured and bounded; payload budget documented
- [ ] `ETag` and conditional requests

## Engineering decisions
- Serialisation server-side, not in the browser. The alternative ships raw ephemeris and makes the client recompute geometry, which is slow, duplicated across two view libraries, and a second place for the physics to be wrong.
- Payload size is a first-class constraint. A globe that takes eight seconds to populate reads as broken regardless of correctness.'

issue "M1-16 — web: Next.js shell and Cesium globe" "$M1" "type/feature,area/web,risk/medium" \
'## Context
The end of the vertical slice. The moment a real access window computed from a real TLE appears on a globe, the whole pipeline is proven.

## Acceptance criteria
- [ ] Next.js App Router, TypeScript strict, no `any`, Tailwind
- [ ] Cesium globe with the constellation, orbit tracks, and ground tracks from CZML
- [ ] Timeline control scrubbing satellite positions
- [ ] Submit a request from the UI and see its opportunity footprints rendered
- [ ] Selecting a request highlights its candidate opportunities
- [ ] Cesium loaded client-side only, with a real loading state
- [ ] `make demo` drives the whole path end to end

## Engineering decisions
- Cesium is client-only and large. It is dynamically imported with a deliberate loading state rather than allowed to block first paint.
- Server components by default, client components only where Cesium and interaction genuinely require them.'

issue "M1-17 — ADR-0009: CesiumJS and deck.gl division of labour" "$M1" "type/adr,risk/low" \
'## Context
Two heavyweight visualisation libraries in one frontend needs justifying, not assuming.

## Acceptance criteria
- [ ] `docs/decisions/0009-cesium-deckgl-division.md`
- [ ] States exactly which questions each library answers
- [ ] Rejected alternatives: Cesium alone, deck.gl alone, Mapbox/MapLibre plus a custom globe
- [ ] Names the cost — two rendering stacks, two mental models, a large bundle

## Engineering decisions
- The split is by question, not by taste. Cesium answers "where is the satellite and what can it see, in 3D, over time" — it has a real ellipsoid, real time dynamics, and CZML. deck.gl answers "where is the demand and where are the conflicts" — it is far better at large aggregated 2D layers.
- Cesium alone is the strongest rejected option and the honest reason it loses is that its 2D aggregation story is weak, and the density and conflict-cluster views are where the planner reasoning becomes visible.'

issue "M1-18 — Integration tests with Testcontainers" "$M1" "type/test,area/ci,risk/high" \
'## Context
Real Postgres and real NATS. Mocks would assert that our understanding of the infrastructure is self-consistent, which is not the thing in doubt.

## Acceptance criteria
- [ ] Testcontainers spinning real Postgres+PostGIS and real NATS JetStream
- [ ] Duplicate event delivery produces exactly one state change
- [ ] Out-of-order delivery is handled correctly
- [ ] Consumer killed mid-transaction: no partial state, message redelivered
- [ ] Outbox relay restart publishes every pending event exactly once
- [ ] Exclusion constraint rejects overlapping acquisitions via raw SQL
- [ ] Runs in CI in under five minutes

## Engineering decisions
- These four scenarios are named in the spec because they are the ones that are actually hard and the ones most systems never test. They are gates, not extras.
- Containers per test class rather than per test: per-test is correct in principle and too slow to run on every push, and a test suite nobody waits for is a test suite nobody runs.'

issue "M1-19 — Seed data and make demo" "$M1" "type/chore,area/infra,risk/low" \
'## Context
Five minutes from clone to a working, populated system. A reviewer who has to construct their own scenario will not.

## Acceptance criteria
- [ ] Seed: constellation with live TLEs, sensor mode parameters, sample customers across all four priority tiers
- [ ] `make demo` submits a scripted set of requests including deliberately contested ones
- [ ] Idempotent — re-running does not duplicate
- [ ] Works offline from the cached TLE snapshot
- [ ] Completes within the five-minute cold-start budget

## Engineering decisions
- The demo scenario is designed to contend. A demo where every request wins shows nothing about the system that matters; the interesting output is the de-confliction.
- All four tiers seeded so the fairness model is visible in the very first run rather than needing to be explained.'

issue "M1-20 — OTel tracing across the first async hop" "$M1" "type/feature,area/observability,risk/medium" \
'## Context
Getting a trace to survive an async hop and a language boundary is the part most implementations skip. Proving it on the first hop de-risks doing it everywhere in M3.

## Acceptance criteria
- [ ] OTel SDK in `tasking-api` (Go) and `feasibility-service` (Python)
- [ ] `traceparent` injected into NATS headers at publish and extracted at consume
- [ ] The consumer span is both a child and a link of the producing span
- [ ] One trace spans HTTP ingress, outbox publish, and the Python consumer
- [ ] `correlation_id` on every log line, joinable with the trace
- [ ] Collector configured, traces visible in Grafana
- [ ] **Test: submit a request, assert a single trace contains spans from both services**

## Engineering decisions
- Child **and** link. A pure child relationship misrepresents an async hop as synchronous; a pure link loses the parent chain. Both is the correct modelling and it is a detail worth being able to explain.
- Proven on one hop in M1 rather than attempted across all of them at once in M3. If the mechanism is wrong, it is wrong once and cheaply.'

say "Issues — $M2"

issue "M2-01 — planner-service: round trigger and advisory locking" "$M2" "type/feature,area/planner,risk/high" \
'## Context
The only strongly-consistent component. Allocation must be single-writer per partition or two customers win the same window.

## Acceptance criteria
- [ ] Round identity is `(satellite_id, bucket_start)`, buckets aligned to UTC
- [ ] Postgres advisory lock keyed on the round key, held for the allocation transaction
- [ ] Cadence timer trigger
- [ ] Opportunity-arrival debounce trigger
- [ ] `planning.round.triggered.v1` emitted with the full candidate set
- [ ] Different satellites plan concurrently — proven by a test, not asserted
- [ ] **Test: concurrent rounds for the same key serialise; for different keys they overlap**

## Engineering decisions
- Lock granularity matches invariant granularity. The non-overlap constraint is per-satellite, so the lock is per-satellite. That is the whole trick, and it is why serialising the planner does not create a global throughput ceiling.
- Advisory lock over `SELECT ... FOR UPDATE`: no row needs to exist for a bucket that has never been planned, and inventing a placeholder row to lock is a worse design than locking the concept directly.
- Fixed UTC-aligned buckets, not a rolling window. Fixed alignment makes rounds reproducible and replayable; a rolling window makes the same input produce different partitions depending on when it ran.'

issue "M2-02 — Slew-time model" "$M2" "type/feature,area/planner,risk/high" \
'## Context
`slew_time(a, b)` is what turns naive interval scheduling into scheduling with sequence-dependent setup times. It is where the real difficulty lives.

## Acceptance criteria
- [ ] `slew_time(a, b)` from the roll-angle delta, with a slew-rate model plus settling time
- [ ] Asymmetric where the physics is asymmetric
- [ ] Mode-transition overhead included
- [ ] Configurable per satellite
- [ ] Property test: `slew_time(a, a)` is the settling floor, and the function is monotonic in roll delta
- [ ] Unit tests against hand-computed values

## Engineering decisions
- Modelled as a function of the pair, not as a constant gap. A constant gap collapses the problem to ordinary interval scheduling and removes the only genuinely hard constraint.
- Settling time separate from slew time. A satellite that has finished rotating is not yet stable enough to image, and merging the two hides a real physical distinction.
- Documented as an approximation. The real relationship is not linear in angle; a defensible simplification stated openly is stronger than an unstated one.'

issue "M2-03 — Per-orbit duty-cycle budget" "$M2" "type/feature,area/planner,risk/medium" \
'## Context
A SAR satellite cannot image continuously — power and thermal limits. This adds a knapsack dimension on top of the interval constraints.

## Acceptance criteria
- [ ] Per-orbit imaging-seconds budget, configurable per satellite
- [ ] Budget consumed by `duty_cycle_cost_s`, which may exceed acquisition duration
- [ ] Enforced across orbit boundaries within a bucket
- [ ] `DUTY_CYCLE_EXHAUSTED` unfulfilment reason with remaining and required seconds
- [ ] Utilisation and duty-cycle usage on the committed plan metrics
- [ ] Property test: no committed plan exceeds the budget for any orbit

## Engineering decisions
- `duty_cycle_cost_s` is distinct from acquisition duration. Modes with warm-up or calibration overhead charge more, and that difference is exactly what makes this a knapsack constraint rather than a restatement of the interval constraint.
- Per orbit, not per bucket. Power recovery is tied to the orbital cycle, and averaging over a bucket would permit physically impossible bursts.'

issue "M2-04 — AllocationPolicy interface and plan commit" "$M2" "type/feature,area/planner,risk/high" \
'## Context
The Strategy interface is the best single design decision in the project: it turns "which algorithm?" from a guess into a measurement.

## Acceptance criteria
- [ ] `AllocationPolicy` interface taking candidates and constraints, returning a plan plus per-request unfulfilment reasons
- [ ] Policy selected by configuration and recorded on every event
- [ ] Commit transaction: acquisitions, plan row, outbox rows, `processed_events` — all atomic
- [ ] Optimistic concurrency on `collection_plans.version`
- [ ] Constraint violation from the database aborts the whole round rather than committing partially
- [ ] Policies unit-testable with no database

## Engineering decisions
- The interface returns unfulfilment reasons alongside the plan. A policy that returns only winners cannot power the "why?" panel, and retrofitting explanations onto an algorithm that has already discarded the information is far harder than producing them as it goes.
- The exclusion constraint is a backstop, not the primary mechanism. If it ever fires, the policy has a bug — so it aborts loudly instead of skipping the offending row.
- Policies are pure functions over their inputs, which is what makes property-based testing possible at all.'

issue "M2-05 — Policy: GreedyByBid" "$M2" "type/feature,area/planner,risk/low" \
'## Context
The naive baseline. Its purpose is to be beaten, visibly and with numbers.

## Acceptance criteria
- [ ] Sort by effective value, take what fits subject to every constraint
- [ ] Deterministic tie-breaking
- [ ] Correct unfulfilment reasons
- [ ] Table-driven tests including cases where it is provably suboptimal

## Engineering decisions
- Deliberately kept naive. Improving the baseline would flatter the heuristics and destroy the comparison the benchmark exists to make.
- Deterministic tie-breaking so benchmark runs are reproducible; a random tie-break makes every comparison noisy.'

issue "M2-06 — Policy: GreedyByValueDensity" "$M2" "type/feature,area/planner,risk/medium" \
'## Context
Sort by value divided by resource consumed. Usually much better than sorting by bid, and the improvement should be measured rather than asserted.

## Acceptance criteria
- [ ] Density = effective value / (duration + expected slew cost)
- [ ] Expected slew cost estimated from neighbouring candidates
- [ ] Duty-cycle cost included in the denominator
- [ ] Tests showing it beats `GreedyByBid` on constructed adversarial instances
- [ ] Benchmarked against `ExactDP`

## Engineering decisions
- Slew cost belongs in the denominator, which is the entire point. Ignoring it means a high-value acquisition that forces a huge attitude manoeuvre looks free, and the plan pays for it twice over.
- Expected rather than actual slew cost, because actual cost depends on the sequence being built. This approximation is where the heuristic loses ground to optimal, and the benchmark should show exactly how much.'

issue "M2-07 — Policy: VickreySealedBid" "$M2" "type/feature,area/planner,risk/high" \
'## Context
Second-price clearing: the winner pays the runner-up bid. Truthful bidding becomes the dominant strategy — a genuine mechanism-design point rather than a scheduling one.

## Acceptance criteria
- [ ] Winner determination plus second-price clearing per contested slot
- [ ] `clearing_price_credits` on each acquisition
- [ ] Runner-up determined per contested resource, not globally
- [ ] Documented honestly: this is not a fully incentive-compatible combinatorial auction
- [ ] Tests: overbidding does not improve outcome; truthful bidding is not dominated

## Engineering decisions
- The honesty requirement is the important one. VCG in a combinatorial setting with sequence-dependent setup costs is not straightforwardly incentive-compatible, and claiming otherwise would be the kind of overreach a knowledgeable reviewer would catch immediately. This is second-price clearing applied per contested slot, and the limitation is documented.
- Clearing price computed and stored but never settled. There is no billing — stated as a scope cut.'

issue "M2-08 — Policy: ExactDP as ground truth" "$M2" "type/feature,area/planner,risk/high" \
'## Context
The problem is weighted job scheduling with sequence-dependent setup times on parallel machines: NP-hard. This policy exists to bound the heuristics, not to run in production.

## Acceptance criteria
- [ ] Exact solver via DP or branch-and-bound, correct on small instances
- [ ] Hard instance-size limit with a clear error above it
- [ ] Configurable timeout returning best-known plus a bound
- [ ] **Test: on small instances, no heuristic ever exceeds ExactDP plan value**
- [ ] Complexity documented and defensible from memory
- [ ] Used as the reference in the benchmark harness

## Engineering decisions
- The "no heuristic beats optimal" test is the strongest correctness check in the whole planner. If a heuristic ever wins, one of them is violating a constraint — and that is a bug this test finds and nothing else would.
- NP-hardness is stated out loud, then answered with measurement rather than hand-waving. "Greedy reaches 94% of optimal in 3ms where exact takes 40s" is worth more than any amount of clean code.
- Instance-size limit is explicit and loud. A silently degrading exact solver would be worse than no exact solver at all, because it would corrupt the reference the benchmark depends on.'

issue "M2-09 — Fairness: tier multipliers and ageing" "$M2" "type/feature,area/planner,risk/medium" \
'## Context
Pure highest-bid allocation starves low-bid customers forever. With government and civil-protection users alongside commercial ones, that is a product failure, not just an unfairness.

## Acceptance criteria
- [ ] Priority-tier multipliers as planner configuration
- [ ] Ageing factor so a repeatedly-losing request gains weight
- [ ] `effective_value = f(bid, tier, age)`, computed in one place
- [ ] `age_rounds` tracked and emitted on unfulfilment
- [ ] Ageing curve bounded so it cannot invert the tier ordering entirely
- [ ] **Test: a persistently low-bid request eventually wins**
- [ ] The starvation-versus-value tradeoff documented with measured numbers

## Engineering decisions
- Multipliers and the ageing curve are planner configuration, deliberately kept out of the published contract, so fairness can be re-tuned without versioning a schema and clients cannot optimise against a published formula.
- The ageing curve is bounded. Unbounded ageing eventually lets an ancient trivial request outrank an urgent civil-protection one, which is a worse failure than the starvation it fixes.
- The eventual-win test is the acceptance criterion for the whole fairness model. Without it, "we have ageing" is an untested claim.'

issue "M2-10 — Plan supersession and re-planning" "$M2" "type/feature,area/planner,risk/high" \
'## Context
A bucket can be planned more than once, because rounds fire on a cadence timer or on an opportunity debounce. That makes supersession a first-class concept rather than an edge case.

## Acceptance criteria
- [ ] `plan_version` monotonic per `(satellite_id, bucket_start)`
- [ ] `supersedes_plan_id` set on re-plan
- [ ] Superseded acquisitions released, and their requests returned to contention
- [ ] `SUPERSEDED` unfulfilment for requests that held a slot and lost it
- [ ] Optimistic concurrency: a concurrent commit at the same version loses and retries
- [ ] Consumers drop a lower `plan_version` arriving after a higher one
- [ ] **Test: rapid re-planning converges and never leaves orphaned acquisitions**

## Engineering decisions
- This is the direct cost of choosing a debounce trigger alongside cadence, and it was accepted openly: a livelier demo in exchange for a real state machine around supersession.
- Version guards on the consumer side, because message ordering is not assumed anywhere in this system.
- A request that loses its slot to a re-plan must return to contention rather than silently disappearing. Silently losing a won slot is the worst possible customer experience and the easiest bug to write.'

issue "M2-11 — ADR-0014: planner-side re-planning semantics" "$M2" "type/adr,risk/low" \
'## Context
Supersession is a consequence of the round-trigger design and needs to be written down as a decision, not discovered as behaviour.

## Acceptance criteria
- [ ] `docs/decisions/0014-planner-side-re-planning-semantics.md`
- [ ] Rejected alternatives: cadence only (simpler, deader demo), immutable plans with append-only deltas, debounce as round identity
- [ ] Documents the cost: a re-plan state machine, version guards on every consumer, and requests that can lose a slot they had already won
- [ ] Names what would falsify it

## Engineering decisions
- Honest framing: this complexity was bought, not forced. Cadence-only planning would have been materially simpler and the demo would have been materially worse.'

issue "M2-12 — Property-based tests for scheduler invariants" "$M2" "type/test,area/planner,risk/high" \
'## Context
Example tests cover the cases you thought of. Scheduler bugs live in the cases nobody thought of.

## Acceptance criteria
- [ ] Generators producing arbitrary opportunity sets: overlapping windows, extreme roll deltas, tight duty cycles, deadline pressure
- [ ] **Invariant: no committed plan ever contains two overlapping acquisitions on one satellite**
- [ ] **Invariant: every consecutive pair has gap >= `slew_time(a, b)`**
- [ ] **Invariant: per-orbit duty cycle never exceeded**
- [ ] **Invariant: no acquisition finishes after its request deadline**
- [ ] **Invariant: at most one acquisition per request**
- [ ] **Invariant: every candidate request appears either as an acquisition or as an unfulfilment — conservation**
- [ ] All invariants hold for all four policies
- [ ] Shrinking produces minimal counterexamples

## Engineering decisions
- Invariants over examples. Every one of these holds for *any* input, which is a far stronger statement than any finite set of cases.
- The conservation invariant is the one that protects customers directly: a request that silently vanishes between rounds is the worst failure mode this system has, and nothing else would catch it.
- Run against all four policies from one property suite, so adding a fifth policy inherits the entire correctness bar for free.'

issue "M2-13 — Policy benchmark harness and report" "$M2" "type/perf,area/planner,risk/medium" \
'## Context
The chart that makes the Strategy pattern worth having. "Greedy reaches 94% of optimal in 3ms where exact takes 40s" is the single most persuasive artifact this project can produce.

## Acceptance criteria
- [ ] Reproducible scenario generator with a fixed seed: contention level, deadline pressure, geographic clustering
- [ ] All four policies over identical inputs
- [ ] Reported: plan value, requests fulfilled, satellite utilisation, runtime, optimality ratio versus ExactDP
- [ ] `docs/policy-benchmark.md` with graphs
- [ ] Identifies which scenario class each heuristic handles worst
- [ ] Rerunnable via `make benchmark`

## Engineering decisions
- Identical inputs across policies, fixed seed, or the comparison is meaningless.
- Reporting where each heuristic is *worst* matters more than reporting where it is best. Knowing your algorithm failure mode is a stronger signal than a flattering average.
- Optimality ratio only on instances ExactDP can actually solve, and the harness says clearly which those were rather than quietly excluding them.'

issue "M2-14 — ADR-0007: allocation strategy, heuristic versus optimal" "$M2" "type/adr,risk/low" \
'## Context
The centrepiece decision. Written after the benchmark exists, so it cites numbers rather than intuition.

## Acceptance criteria
- [ ] `docs/decisions/0007-allocation-strategy.md`
- [ ] Formal problem statement and the NP-hardness claim, correctly justified
- [ ] Why Strategy rather than one committed algorithm
- [ ] Rejected alternatives: an ILP/CP-SAT solver, simulated annealing, ExactDP everywhere
- [ ] Cites real benchmark numbers
- [ ] States the default policy and why

## Engineering decisions
- A CP-SAT solver is the strongest rejected alternative and deserves a fair hearing: it would likely beat every heuristic here on quality and handle the constraints declaratively. It loses on a dependency whose behaviour is hard to explain line by line, on unpredictable runtime under a p95 budget, and on the fact that the interview value is in understanding the problem rather than in delegating it.
- The ADR is written after the benchmark, deliberately. Written before, it would be a prediction dressed as a decision.'

issue "M2-15 — Unfulfilment reasons with structured explanations" "$M2" "type/feature,area/planner,risk/medium" \
'## Context
The data behind the feature that will get remembered. Most schedulers say no; this one says which constraint bound and by how much.

## Acceptance criteria
- [ ] Every candidate request that does not win gets exactly one `planning.request.unfulfilled.v1`
- [ ] Reason codes: `LOST_TO_HIGHER_VALUE`, `BLOCKED_BY_SLEW_CONSTRAINT`, `DUTY_CYCLE_EXHAUSTED`, `DEADLINE_PASSED`, `NO_OPPORTUNITY_IN_BUCKET`, `SUPERSEDED`, `CANCELLED_BY_CUSTOMER`
- [ ] Structured explanation per reason: shortfall in credits, required slew versus available gap, duty cycle remaining versus required
- [ ] `best_rejected_opportunity_id` for ghost rendering on the timeline
- [ ] Reason precedence documented and tested where several constraints bind at once
- [ ] `age_rounds` and `eligible_for_retry` populated
- [ ] **Test: conservation — no candidate request is ever silently dropped**

## Engineering decisions
- Structured data, never a message string. Strings cannot be aggregated, charted, or acted on; a shortfall a customer can act on has to be a number.
- Reason precedence is defined and tested. When both slew and duty cycle bind, reporting either at random makes the explanation untrustworthy — and an explanation nobody trusts is worse than none.
- Produced by the policy as it decides, not reconstructed afterwards. Reconstruction is guesswork once the algorithm has discarded the information.'

say "Issues — $M3"

issue "M3-01 — Idempotent-consumer hardening" "$M3" "type/feature,area/infra,risk/high" \
'## Context
Every consumer must be idempotent. M1 and M2 built this per service; M3 makes it uniform and proves it under adversarial conditions.

## Acceptance criteria
- [ ] Shared idempotent-consumer helper in Go and in Python
- [ ] `processed_events` retention policy and cleanup that cannot delete rows still within the redelivery window
- [ ] Consistent ack-after-commit ordering everywhere
- [ ] Poison detection before `max_deliver` is exhausted
- [ ] Metrics: duplicates suppressed, ack latency, redelivery rate
- [ ] Chaos test: duplicates injected at every hop; end state identical

## Engineering decisions
- One helper per language, not per service. Four subtly different implementations of the same pattern is how one of them ends up wrong.
- Cleanup must not outrun the redelivery window, or a late redelivery is reprocessed as new — a silent correctness bug that only appears under load.'

issue "M3-02 — DLQ implementation and replay tooling" "$M3" "type/feature,area/infra,risk/medium" \
'## Context
A poison message retried forever consumes a consumer slot and starves healthy traffic. A DLQ without replay tooling is just a place messages go to be forgotten.

## Acceptance criteria
- [ ] DLQ streams per domain, per `contracts/nats/topology.md`
- [ ] Terminal-failure publication with the full documented header set, `traceparent` preserved
- [ ] `make dlq-inspect` and `make dlq-replay`
- [ ] Replay procedure documented in a runbook
- [ ] Alerting on DLQ depth
- [ ] Test: a poison message lands in the DLQ, is fixed, is replayed, and processes correctly

## Engineering decisions
- `traceparent` preserved into the DLQ so a dead message failure is inspectable in Grafana rather than reconstructed from logs.
- Replay is safe only because consumers are idempotent. This is the concrete payoff for the idempotency tax: recovery is routine rather than a data-integrity incident.'

issue "M3-03 — Chaos tests" "$M3" "type/test,area/ci,risk/high" \
'## Context
Knowing how the system fails is more valuable than a clean graph. These tests kill things at the worst possible moment on purpose.

## Acceptance criteria
- [ ] Consumer killed mid-transaction: no partial state, message redelivered and reprocessed correctly
- [ ] Outbox relay killed mid-publish: every event published exactly once on restart
- [ ] NATS restarted under load: consumers reconnect, no message loss
- [ ] Postgres connection pool exhausted: ingress degrades to `503` rather than corrupting state
- [ ] Planner killed while holding an advisory lock: lock released, round retried, no double allocation
- [ ] Runs in CI, not just locally
- [ ] Every failure mode written up in `docs/performance.md`

## Engineering decisions
- The planner-killed-holding-the-lock case is the important one. Postgres releases advisory locks on connection loss, which is precisely why an advisory lock was chosen over an application-level lease that would need its own expiry logic — but the behaviour must be proven, not assumed.
- In CI, not local-only. A chaos test that only runs when someone remembers is a chaos test that stops working silently.'

issue "M3-04 — Circuit breaker and bulkhead" "$M3" "type/feature,area/infra,risk/medium" \
'## Context
A slow dependency is worse than a dead one: it consumes connections, threads, and latency budget while still appearing available.

## Acceptance criteria
- [ ] Circuit breaker on cross-service HTTP and on the Celestrak fetch
- [ ] Bulkhead: separate connection pools so a slow read cannot starve the write path
- [ ] Timeouts on every outbound call, no unbounded waits anywhere
- [ ] Breaker state exposed as a metric
- [ ] Test: a dependency made slow trips the breaker and the caller degrades rather than queueing

## Engineering decisions
- Bulkheads primarily protect the ingress write path. Read traffic is the variable load and ingress availability is the property this architecture exists to protect.
- Breaker state as a metric, not just a log line. "Why did latency drop while errors rose?" is only answerable if breaker state is on the dashboard.'

issue "M3-05 — End-to-end OTel tracing" "$M3" "type/feature,area/observability,risk/medium" \
'## Context
M1 proved trace propagation on one hop. M3 makes one trace span the entire pipeline across two languages and three async boundaries.

## Acceptance criteria
- [ ] Every service instrumented, every publish and consume propagating context
- [ ] One trace: HTTP ingress -> outbox publish -> Python consumer -> publish -> Go planner -> commit -> projector
- [ ] Domain-meaningful span attributes: `request_id`, `satellite_id`, `policy`, opportunity counts
- [ ] Sampling configured and justified
- [ ] Test asserting end-to-end trace completeness

## Engineering decisions
- Sampling is head-based with a documented rate rather than always-on. Always-on at 1000 rps produces trace volume nobody will store, and an unaffordable observability stack gets turned off — which is worse than sampling.
- Domain attributes on spans, not just HTTP metadata. `satellite_id` and `policy` are what make a trace answer a question rather than merely display a timeline.'

issue "M3-06 — RED and domain metrics with Grafana dashboards" "$M3" "type/feature,area/observability,risk/low" \
'## Context
RED metrics show the system is alive. Domain metrics show it is doing the right thing, which is the harder and more interesting question.

## Acceptance criteria
- [ ] RED per service: rate, errors, duration, with p50/p95/p99
- [ ] Domain metrics: opportunities per request, plan value per round, allocation latency, satellite utilisation, requests unfulfilled **by reason**, TLE staleness distribution
- [ ] Grafana dashboards committed as JSON and provisioned automatically
- [ ] Prometheus alert rules for DLQ depth, outbox lag, TLE staleness
- [ ] Dashboards populate from `make demo` with no manual setup

## Engineering decisions
- "Unfulfilled by reason" is the highest-value metric in the system. It is the difference between knowing requests are failing and knowing whether the constellation is contended, poorly slewed, or power-limited — three completely different remedies.
- Dashboards as committed JSON, provisioned on startup. A dashboard that exists only in someone Grafana instance does not exist.'

issue "M3-07 — k6 suite with thresholds as CI gates" "$M3" "type/perf,area/ci,risk/high" \
'## Context
Thresholds as gates, not decoration. This is where the architecture defends itself with data instead of opinion.

## Acceptance criteria
- [ ] `POST /v1/tasking-requests` at 1000 rps: p95 < 40ms, p99 < 120ms, zero errors
- [ ] Same at 100 rps: p95 < 15ms
- [ ] End to end request -> plan committed at 200 rps sustained: p99 < 5s
- [ ] Planner round with 5000 opportunities: p95 < 800ms
- [ ] Thresholds fail the build, not just the report
- [ ] Latency curve captured at 10, 100, and 1000 rps
- [ ] Results published to `docs/performance.md`

## Engineering decisions
- The 10/100/1000 rps curve is the point. It shows the synchronous ingress path degrading while the async path stays flat, which turns ADR-0003 from an argument into a measurement.
- Thresholds tuned to this hardware and then defended, with the hardware stated. An SLO with no stated environment is a number with no meaning.'

issue "M3-08 — Breakpoint test and failure-mode write-up" "$M3" "type/perf,risk/high" \
'## Context
Knowing what breaks first, and why, is more impressive than a clean graph — and more useful.

## Acceptance criteria
- [ ] Ramping load until the system fails
- [ ] The first component to break identified, with evidence
- [ ] Failure mode characterised: graceful degradation or cliff
- [ ] Recovery behaviour after load is removed
- [ ] Written up with the actual numbers and the actual bottleneck
- [ ] A hypothesis for the *next* bottleneck if the first were fixed

## Engineering decisions
- Predict the bottleneck before running, then record whether the prediction was right. Being wrong in public with the evidence is more credible than a retrofitted explanation, and far more interesting to discuss.
- Recovery matters as much as the break. A system that fails and recovers cleanly is operationally very different from one that needs a restart, and only the test can tell you which you built.'

issue "M3-09 — docs/performance.md" "$M3" "type/docs,type/perf,risk/low" \
'## Context
Real numbers from real hardware, including the parts that did not look good.

## Acceptance criteria
- [ ] Hardware and environment stated precisely
- [ ] k6 results with graphs for every scenario
- [ ] The 10/100/1000 rps latency curve with the async path shown flat
- [ ] Breakpoint results and the documented failure mode
- [ ] Cold-start timing breakdown against the five-minute budget
- [ ] Policy benchmark results cross-referenced
- [ ] What was tuned to reach these numbers, and what was left on the table

## Engineering decisions
- Include the unflattering numbers. A performance document with no disappointing results has been curated, and a reviewer who has written one before will assume exactly that.'

say "Issues — $M4"

issue "M4-01 — deck.gl 2D planning view" "$M4" "type/feature,area/web,risk/medium" \
'## Context
The 2D view answers a different question from the globe: where is demand concentrated, and where is the system fighting itself.

## Acceptance criteria
- [ ] Target density layer
- [ ] Coverage heatmap from committed footprints
- [ ] Footprint polygons with mode and satellite encoded visually
- [ ] Conflict clusters: regions where many requests contend for few opportunities
- [ ] Layer toggles and a legend
- [ ] Time-window filter synchronised with the globe
- [ ] 60fps at full seeded scale

## Engineering decisions
- Conflict clusters are the layer that justifies deck.gl existing here. Everything else could be done in Cesium; large aggregated 2D contention views could not, and this is the view that makes the scheduling problem visible as a problem.
- Colour choices must survive being read by someone who cannot distinguish red from green. A planning tool that fails for a colour-blind operator is broken, not imperfect.'

issue "M4-02 — Per-satellite timeline with visible slew gaps" "$M4" "type/feature,area/web,risk/high" \
'## Context
The Gantt view where the sequence-dependent setup cost stops being an abstraction and becomes a visible block of time.

## Acceptance criteria
- [ ] One row per satellite, acquisitions on a shared time axis
- [ ] **Slew gaps rendered as distinct occupied blocks, not as idle time**
- [ ] Duty-cycle consumption shown per orbit
- [ ] Zoom and pan from hours to days
- [ ] Selecting an acquisition cross-highlights it on the globe and 2D view
- [ ] Virtualised: only visible rows and intervals render
- [ ] Smooth at full seeded scale

## Engineering decisions
- Rendering slew as occupied rather than idle is the whole point of this view. Idle-looking gaps invite "why is the satellite doing nothing?" — the answer is that it is rotating, and it is expensive, and that is the constraint that makes this problem hard.
- Virtualisation from the start, not as a later optimisation. Retrofitting virtualisation into a timeline that assumes everything is mounted is a rewrite.'

issue "M4-03 — SSE live updates" "$M4" "type/feature,area/web,area/plan-gateway,risk/medium" \
'## Context
Submitting a request should visibly ripple through to a new plan. That moment is the demo.

## Acceptance criteria
- [ ] SSE endpoint on `plan-gateway` streaming plan and request state changes
- [ ] Filtered subscriptions by satellite, time window, or request
- [ ] Client reconnect with `Last-Event-ID` resumption
- [ ] Updates batched and throttled so a burst does not thrash rendering
- [ ] Backpressure: a slow client is dropped rather than allowed to consume unbounded memory
- [ ] Test: submit a contested request and observe the plan change without a refresh

## Engineering decisions
- SSE over WebSockets: the traffic is unidirectional, SSE reconnects and resumes natively, and it survives proxies that mangle WebSocket upgrades. A bidirectional protocol for a unidirectional problem is unearned complexity.
- Dropping slow clients is deliberate. Unbounded per-client buffering turns one bad connection into a server-wide memory problem.'

issue "M4-04 — The why-was-my-request-rejected panel" "$M4" "type/feature,area/web,risk/high" \
'## Context
The feature that will get remembered. It is the UI equivalent of an ADR: the system explains its decisions rather than announcing them.

## Acceptance criteria
- [ ] Click any unfulfilled request to see its structured reason
- [ ] `LOST_TO_HIGHER_VALUE`: the shortfall in credits, shown as a number
- [ ] `BLOCKED_BY_SLEW_CONSTRAINT`: required slew versus available gap, with the blocking acquisition linked
- [ ] `DUTY_CYCLE_EXHAUSTED`: remaining versus required seconds
- [ ] `NO_FEASIBLE_OPPORTUNITY`: which geometric constraint eliminated every candidate
- [ ] Actionable suggestion where one exists: bid more, widen the window, accept more modes
- [ ] Links to the best rejected opportunity, highlighted on globe and timeline

## Engineering decisions
- Every number shown comes from `planning.request.unfulfilled.v1`. Nothing is recomputed in the browser — the explanation the customer sees is exactly the explanation the planner produced, which is what makes it trustworthy.
- Suggestions only where genuinely actionable. Telling a customer to raise their bid when the real problem is that no satellite can see their target is worse than saying nothing.'

issue "M4-05 — Ghost rendering of losing candidates" "$M4" "type/feature,area/web,risk/medium" \
'## Context
Rendering only winners shows the result of de-confliction. Rendering losers as ghosts shows the decision.

## Acceptance criteria
- [ ] Rejected candidate opportunities rendered as ghosts on the timeline
- [ ] Visually distinct without competing with committed acquisitions
- [ ] Hovering a ghost shows why it lost
- [ ] Toggleable, off by default
- [ ] Rendering stays smooth with ghosts enabled at full scale

## Engineering decisions
- Off by default. Ghosts are dense and would overwhelm the primary view; the toggle is what makes them a tool rather than noise.
- This is the visual counterpart of the "why?" panel: the panel explains one request, the ghosts show the shape of the whole contention.'

issue "M4-06 — Acquisition execution simulator" "$M4" "type/feature,area/planner,risk/medium" \
'## Context
Closes the request lifecycle and introduces the one thing a planner cannot control: reality diverging from the plan.

## Acceptance criteria
- [ ] Simulator consuming `planning.plan.committed.v1` and emitting `acquisition.executed.v1`
- [ ] Configurable failure injection across every `failure_reason`
- [ ] `actual_window` drifts from `scheduled_window` realistically
- [ ] `TLE_DRIFT_MISS` correlates with TLE staleness at plan time
- [ ] Request state machine advances to `ACQUIRED` on success
- [ ] Read model handles a committed acquisition that then failed

## Engineering decisions
- `TLE_DRIFT_MISS` correlating with staleness closes the loop on why `tle_epoch` is tracked at all. It makes a too-loose staleness threshold produce a visible, physical consequence rather than remaining a theoretical concern.
- Failure paths are exercised deliberately, because the read model handling an acquisition that was committed and then did not happen is a materially different code path — and the one most demo systems never run.'

issue "M4-07 — Frontend performance" "$M4" "type/perf,area/web,risk/high" \
'## Context
Data-intensive real-time visualisation is explicitly in scope. Measured, not assumed.

## Acceptance criteria
- [ ] Timeline virtualised: only visible rows and intervals mounted
- [ ] Cesium entity updates throttled and batched; no per-frame entity churn
- [ ] SSE updates coalesced before hitting React state
- [ ] Memoisation on the expensive derived selectors
- [ ] Bundle analysed; Cesium and deck.gl code-split
- [ ] Measured: frame time, time to interactive, memory over a 10-minute session
- [ ] Numbers published in `docs/performance.md`

## Engineering decisions
- Measured with the profiler, not by feel. "It feels smooth on my machine" is exactly the claim that collapses on a reviewer laptop.
- The 10-minute memory measurement is deliberate. Leaks in live-updating visualisations do not show up in a 30-second look, and this is a live-updating visualisation.'

issue "M4-08 — Playwright E2E" "$M4" "type/test,area/web,risk/medium" \
'## Context
Two paths, end to end, in a real browser against the real stack.

## Acceptance criteria
- [ ] Happy path: submit -> opportunities appear -> plan commits -> acquisition renders
- [ ] Contested path: two requests compete, one wins, the loser reason is correct in the panel
- [ ] Runs against the full Compose stack in CI
- [ ] Deterministic — no arbitrary sleeps, waits on real conditions
- [ ] Screenshots and traces captured on failure

## Engineering decisions
- Two tests, not twenty. E2E is the slowest and most brittle layer; it earns its place only on the paths that cross every boundary, and everything else belongs lower in the pyramid.
- The contested path is the one that matters. It is the only test that exercises the actual point of the entire system.'

say "Issues — $M5"

issue "M5-01 — README with architecture diagram, GIF, and scope cuts" "$M5" "type/docs,risk/low" \
'## Context
Most reviewers will read the README and a handful of files. It has to carry the project.

## Acceptance criteria
- [ ] What this is and what problem it solves, in the first paragraph
- [ ] Architecture diagram rendered inline
- [ ] Demo GIF: contested requests re-planning the globe
- [ ] Quickstart: `git clone && docker compose up`, verified on a clean machine
- [ ] **Scope cuts stated explicitly as deliberate**: no auth, no multi-tenancy, no Kubernetes, simulated execution, no downlink scheduling
- [ ] **Provenance note**: built from public sources only — Celestrak TLEs and published SAR geometry from open literature. No proprietary data or internal knowledge from any employer
- [ ] Links to ADRs, performance results, and the AI-engineering write-up
- [ ] Coverage and CI badges

## Engineering decisions
- Scope cuts stated as decisions, not discovered as gaps. "No auth, deliberately, because tenancy would change the fairness model and the read model" reads completely differently from silence on the subject.
- The provenance note is not optional. It is a statement about professional conduct and it belongs at the top level.'

issue "M5-02 — AI-engineering write-up" "$M5" "type/docs,risk/medium" \
'## Context
The role weights AI fluency twice. The methodology is a deliverable, not a footnote — and the honesty in `03` is the part that carries credibility.

## Acceptance criteria
- [ ] `00-methodology.md` — decomposition for parallel agents, and why contracts-first is the enabling condition
- [ ] `01-agent-roles.md` — spec-writer, implementer, test-author, adversarial reviewer, docs; real prompts in `prompts/`
- [ ] `02-verification.md` — what was not trusted and how it was checked
- [ ] `03-what-worked-what-didnt.md` — specific, honest, with real examples of cost
- [ ] Prompts are the ones actually used, not cleaned-up reconstructions

## Engineering decisions
- The central claim of `02`: the test strategy **is** the verification harness for AI-generated code. Orbital geometry and concurrency safety were the two areas where generated code looked plausible and was subtly wrong, which is precisely why golden-reference tests and property-based invariants exist. That reframing is the strongest available thing to say on this topic.
- `03` must include real costs with specifics — plausible-but-wrong SGP4 handling, over-abstracted early code, confidently hallucinated library APIs. A candidate who claims AI never misled them has not used it seriously, and a reviewer who has used it knows that.'

issue "M5-03 — ADR index review and supersessions" "$M5" "type/docs,risk/low" \
'## Context
Twelve ADRs written across five milestones. Some will have been overtaken by what was actually measured.

## Acceptance criteria
- [ ] Every ADR reviewed against what was actually built
- [ ] Any decision that changed has a superseding ADR, with the original kept and its status updated
- [ ] Index complete with accurate statuses
- [ ] Every Confirmation section revisited against real evidence
- [ ] Cross-references correct

## Engineering decisions
- A superseded ADR is a flex, not a weakness. It is evidence of a decision revisited against evidence, which is the behaviour the whole ADR practice exists to produce.
- Confirmation sections are checked against what actually happened. An ADR that predicted a falsification condition that then occurred, unnoticed, is worse than no ADR.'

issue "M5-04 — Five-minute demo script" "$M5" "type/docs,risk/low" \
'## Context
A live demo that wanders is worse than no demo. Five minutes, rehearsed, hitting the three things that matter.

## Acceptance criteria
- [ ] Minute-by-minute script
- [ ] Opens with the problem, not the architecture
- [ ] Shows: real orbital geometry, live de-confliction, the "why?" panel
- [ ] Includes the failure story from the breakpoint test
- [ ] A recovery path for each step if something breaks live
- [ ] Rehearsed end to end and timed

## Engineering decisions
- Lead with the problem. An architecture diagram before the problem it solves is a diagram nobody has a reason to care about.
- Including the failure story is deliberate. Volunteering what broke and why is more credible than a demo where everything works, and it is the part that starts the interesting conversation.'

issue "M5-05 — Cold-start verification on a clean machine" "$M5" "type/chore,area/ci,risk/medium" \
'## Context
The definition of done says five minutes on a clean machine. Verify it on an actually clean machine, not on the one that built it.

## Acceptance criteria
- [ ] Verified with no Docker layer cache and no local toolchains
- [ ] Timed and recorded
- [ ] Every prerequisite documented
- [ ] Verified on both arm64 and amd64
- [ ] CI job asserting the budget on every push to `main`
- [ ] Offline path verified using the cached TLE snapshot

## Engineering decisions
- Both architectures, because a reviewer on Apple silicon hitting an amd64-only image is a five-second failure that costs the whole impression.
- The offline path is verified, not assumed. The live Celestrak fetch is the one external dependency and the fallback is the only thing standing between it and a broken first run.'

issue "M5-06 — Coverage badges and CI status" "$M5" "type/chore,area/ci,risk/low" \
'## Context
Badges are a summary, and they should be honest ones.

## Acceptance criteria
- [ ] Coverage badge, overall and for the planner and geometry packages separately
- [ ] CI status badge
- [ ] Badges update automatically
- [ ] README explains why the target is 80/95 and not 100

## Engineering decisions
- Reporting planner and geometry coverage separately is the honest presentation. A single 82% number hides whether the 18% uncovered is wiring code or the allocation algorithm, and those are not remotely the same fact.'

say "Done."
$DRY_RUN || say "View: gh issue list --repo $REPO --milestone '$M0'"

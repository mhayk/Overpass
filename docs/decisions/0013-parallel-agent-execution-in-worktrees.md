# 0013 — Run parallel agent work as one git worktree per contract boundary, not as concurrent agents in one checkout

- **Status:** accepted
- **Date:** 2026-08-07
- **Deciders:** Mhayk Whandson

## Context and problem statement

[`docs/ai-engineering/00-methodology.md`](../ai-engineering/00-methodology.md)
already argues *why* parallel agent work is safe once contracts are frozen, and
that argument is the premise here, not the decision. This ADR is about the part
that document does not answer: **how the parallel work is actually executed.**

The distinction matters because "three sessions cannot collide" is a claim about
*interfaces*, not about *filesystems*. Two agents holding a coherent, compatible
understanding of `opportunity.identified.v1` will still corrupt each other's work
if they are writing into the same checkout — one runs `go mod tidy` while the
other is mid-edit, one's failing test run is actually the other's half-written
file. Frozen contracts eliminate semantic collision. They do nothing about
physical collision.

The timing is itself part of the decision and is why this is being written now
rather than at project start. M0 was deliberately sequential — every artifact in
it is a dependency of everything downstream, so there was nothing to parallelise
and any concurrency would have been agents racing to define the same schema. With
M0 closed (12/12 issues, contracts frozen and drift-gated, CI green), M1 is the
first milestone where more than one unit of work is simultaneously unblocked.

The question: **given frozen contracts, what is the concrete execution model for
running more than one agent at a time, and what rules keep it from producing
integration debt faster than it produces features?**

## Decision drivers

1. **Physical isolation of the working tree.** This is the dominant driver.
   Concurrent writes to one checkout are a correctness problem, not an
   inconvenience, and they fail in ways that look like flaky tests.
2. **Preserve one issue → one branch → one PR → `Closes #N`.** This is already the
   working agreement in `CLAUDE.md`. An execution model that produces one large
   undifferentiated diff discards the review granularity the repo is built on.
3. **CI is the integration harness.** `contracts-verify`, the per-language jobs,
   and the cold-start gate already exist and are enforced by branch protection.
   The execution model should route every track through them rather than invent a
   second mechanism.
4. **Review capacity is the bottleneck, not agent count.** *This is a judgement,
   not a measurement.* Three concurrent PRs of `risk/high` code is already more
   adversarial review than one person does well in one sitting, and unreviewed
   parallel output is not throughput.
5. **Every M0 defect compiled, passed `go vet`, and looked idiomatic.** Parallel
   tracks multiply that surface linearly. The model must not weaken per-track
   verification in exchange for concurrency.
6. **I have to explain every line of this repo to a hiring manager.** A merge
   whose provenance is "four agents wrote it at once" is not explainable. Per-track
   branches with their own PR narrative are.

## Considered options

1. **Sequential single session** — the M0 status quo, one issue at a time
2. **Concurrent subagents inside one checkout** — fan out from one session, shared
   working directory
3. **One git worktree per track, one agent session per worktree** — filesystem and
   branch isolation, one PR per track
4. **Cloud/remote agents** — isolation by running elsewhere entirely

## Decision outcome

Chosen: **Option 3 — one git worktree per contract boundary, one agent session
per worktree, integrating through the existing PR and CI gates.** Option 2 is
retained but restricted to *read-only* roles.

A worktree gives each track its own directory and its own checked-out branch from
one shared object database. That maps exactly onto the decomposition already in
place: a unit of agent work is one contract boundary, one branch, one PR. The
isolation is the filesystem's, so it needs no cooperation between agents to hold
— which is the property that matters, because agents cannot be relied upon to
respect a convention they were not reminded of in the last few thousand tokens.

Option 2 is not rejected outright, it is *scoped*. The `adversarial-reviewer` and
exploration roles from
[`01-agent-roles.md`](../ai-engineering/01-agent-roles.md) do not write files, so
they carry no collision risk and genuinely benefit from running many at once
against finished work. The restriction is precise: **an agent that writes gets a
worktree; an agent that only reads does not need one.**

### The ownership rule

Isolation of the tree is necessary but not sufficient — two tracks can still
collide inside git by editing the same file on different branches. Each track
therefore owns a disjoint set of paths and may write nowhere else:

| Track | Owns | Language |
| --- | --- | --- |
| A | `services/tasking-api/` | Go |
| B | `services/feasibility/` | Python |
| C | `services/plan-gateway/` | Go |
| D | `web/` | TS |

**No track may write to `contracts/`, `gen/`, `db/migrations/`, `CLAUDE.md`, or
`docs/decisions/`.** These are the shared-fate paths: the first two *are* the
interface, the third is a shared schema whose ordering matters, and the last two
are the standing agreement. A track that believes it needs a contract change stops
and raises it — the change is made once, in the main session, and the other tracks
rebase onto it. **Contract changes serialise. That is the whole point of M0 and it
does not stop being true because four agents are running.**

### Sequencing for M1

Two facts constrain the fan-out and neither is optional:

**M1-01 is a prerequisite, not a track.** The Postgres schema — including the
`tstzrange` + GiST exclusion constraint — is what tracks A and C both build
against. It is done solo, first, and merged before anything forks. Handing it to a
parallel track would mean two services building against a schema still being
argued about.

**Tracks C and D do not have a frozen contract between them.** Verified, not
assumed: `contracts/openapi/` contains exactly one document,
`tasking-api.v1.yaml`, declaring five paths, all of them `tasking-api`'s.
`plan-gateway`'s *inbound* contract is frozen — it consumes published events — but
its *outbound* read API, the REST reads and the CZML/GeoJSON serialisation the web
track renders, exists in no contract at all. Forking C and D concurrently would be
doing precisely the thing `00-methodology.md` says is the failure mode: two
sessions inventing plausible, compatible-looking, incompatible interpretations of
a boundary. The premise of this ADR is not satisfied for that pair, so the
parallelism is not licensed for that pair.

There are two honest resolutions and both are acceptable; the unacceptable one is
forking anyway:

- **Extend the contract first** — an M0-shaped task adding `plan-gateway.v1.yaml`
  covering the read endpoints and the CZML/GeoJSON response shapes, after which C
  and D fork cleanly. Costs a serial step, buys a genuine fourth track.
- **Serialise C → D** — three tracks in M1, with D starting once C's read API
  exists in fact rather than in contract.

This gives, for M1:

| Phase | Work | Concurrency |
| --- | --- | --- |
| Prerequisite | M1-01 schema and migrations | Solo |
| Fan-out | A: M1-02…M1-05, M1-07, M1-06 · B: M1-08…M1-13, M1-09 · C: M1-14, M1-15 | Parallel |
| Fan-out | D: M1-16, M1-17 | Gated on the C/D contract question above |
| Integration | M1-18 Testcontainers, M1-19 `make demo`, M1-20 OTel | Solo, after merge |

Track B is started first. It is the longest track and carries the most `risk/high`
issues — SGP4, access-window search, SAR geometry — so it sets the critical path
and its golden-reference oracle (M1-12) is the slowest thing to get right.

The integration phase is deliberately serial. M1-18, M1-19 and M1-20 each span
every service, so they own no disjoint path set and there is nothing to isolate.
This is the same principle `00-methodology.md` states for the planner:
**parallelise where a contract exists, serialise where an invariant lives.**

### Consequences

**Good**

- Collisions are prevented by the filesystem rather than by agent cooperation,
  which is the only enforcement that survives a long session.
- Every track lands as its own branch and PR, so review granularity, the
  `Closes #N` convention, and per-PR CI are all preserved unchanged.
- Worktrees share one `.git`, so branching is cheap and a track is discarded by
  removing a directory.
- The failure mode is loud. A track that violates the ownership rule produces a
  merge conflict in a shared path — visible, at merge time, rather than a silent
  semantic divergence discovered at integration.
- It makes the M0 investment legible. The contracts milestone stops being process
  ceremony and becomes the thing that visibly unlocked four concurrent tracks.

**Bad**

- **Review becomes the bottleneck, and it is a worse bottleneck than the one it
  replaced.** Four tracks produce four PRs of code that compiles, passes `go vet`,
  and looks idiomatic — which M0 established is exactly the code that is wrong.
  Sequential work at least meant defects arrived one at a time.
- Each worktree is a full checkout with its own build artifacts, module cache
  behaviour, and toolchain state. Disk and CPU contention are real, and running
  the compose stack from more than one worktree simultaneously will collide on
  host ports.
- The ownership rule is stated, not enforced. Nothing in CI currently checks that
  a branch touched only its own paths. Until something does, this is a convention
  and conventions decay.
- Rebase load grows with track lifetime. Long-lived tracks diverge from `main`,
  and a shared-path change forces every open track to rebase.
- Context is per-session and does not travel. Four sessions each rediscover repo
  conventions, which is a real token cost and a real inconsistency risk — mitigated
  only by `CLAUDE.md`, the issue body, and the ADRs actually being sufficient.

**Neutral**

- The number of tracks is a dial, not a constant. Four is the M1 shape because M1
  happens to have four disjoint service boundaries; M2 is mostly one serialised
  planner and will run at far lower concurrency.
- Nothing about the services changes. This is a decision about how work is
  scheduled, not about what is built, and reverting it costs nothing but time.

### Confirmation

The claim being tested is that parallel tracks are cheaper than sequential ones
*after* integration cost. M1 is the experiment, and these are the falsifiers:

- **Integration defect location.** If M1-18 — the first test that exercises real
  cross-service delivery — surfaces more defects at the seams than were found
  inside the tracks, then the contracts were not as frozen as claimed and the
  parallelism was borrowing against integration.
- **Shared-path conflicts.** More than one merge conflict in `contracts/`, `gen/`,
  or `db/migrations/` means the ownership boundaries are drawn in the wrong place.
- **Contract renegotiation rate.** More than one track needing a contract change
  mid-flight falsifies "frozen" and the fan-out should narrow until it is true.
- **Review latency.** If PRs sit unreviewed while tracks continue producing, the
  model is manufacturing inventory, not throughput — and the correct response is
  fewer tracks, not faster review.

The decision is wrong the moment any of those hold, and the fallback is
mechanical: reduce concurrency, finish M1 serially, and record the outcome in
[`03-what-worked-what-didnt.md`](../ai-engineering/03-what-worked-what-didnt.md).
`00-methodology.md` currently states that the parallelism claim is "well-founded
but not yet demonstrated at scale". M1 is what changes that sentence, in whichever
direction the evidence points.

## Pros and cons of the options

### Option 1 — Sequential single session

- Good, because there is exactly one writer, so no isolation problem exists and
  every defect arrives alone and gets full attention.
- Good, because context accumulates within one session instead of being
  rediscovered.
- Bad, because it leaves the entire M0 investment unrealised. Contracts were
  frozen specifically to unlock concurrency; never using it makes the milestone
  retroactively hard to justify.
- Bad, because wall-clock time is the project's scarcest resource and four
  independent service boundaries sitting idle is the largest available saving.

### Option 2 — Concurrent subagents in one checkout (scoped, not rejected)

- Good, because it is the lowest-friction option — one session, no setup, shared
  context, and no rebase load.
- Good, and genuinely better than worktrees, for read-only work: adversarial
  review and exploration write nothing and parallelise freely.
- Bad, because concurrent writers to one tree race on files, build caches, and
  dependency state, and the resulting failures present as flakiness rather than as
  conflicts.
- Bad, because all output lands on one branch. That collapses four reviewable PRs
  into one diff and discards `Closes #N` traceability.
- **Retained for the reviewer and exploration roles, forbidden for implementers.**

### Option 3 — One worktree per track (chosen)

- Good, because isolation is structural rather than cooperative.
- Good, because it maps one-to-one onto the branch-and-PR convention already in
  `CLAUDE.md`, so no new process is introduced.
- Good, because the existing CI gates become the integration harness with no
  changes.
- Bad, because of disk and CPU contention, port collisions on the compose stack,
  and rebase load — all of which are real and none of which are correctness bugs.
- Bad, because the path-ownership rule is currently unenforced.

### Option 4 — Cloud or remote agents

- Good, because isolation is total and local machine resources stop mattering.
- Bad, because the verification story is local. The definition of done is
  `docker compose up` bringing up Postgres+PostGIS, NATS, OTel and Grafana, and a
  track that cannot run the stack it is building against cannot verify its own
  work — which M0 established is the only thing that counts.
- Bad, because inspecting work in progress is slower, and the loop that catches
  plausible-but-wrong generated code depends on tight inspection.
- Not rejected permanently. It becomes attractive for M3 load testing, where the
  work is genuinely isolated and locally expensive.

## More information

- The premise, and the parallelise-where-a-contract-exists principle:
  [`00-methodology.md`](../ai-engineering/00-methodology.md)
- Role separation, and why a writer must not review its own work:
  [`01-agent-roles.md`](../ai-engineering/01-agent-roles.md)
- What frozen contracts cost to establish: `contracts/README.md`, and the
  hard-won rules section of `CLAUDE.md`
- Verification strategy the tracks inherit unchanged:
  [ADR-0010](0010-test-strategy-and-coverage.md)
- Milestone sequencing and issue decomposition: [`docs/backlog.md`](../backlog.md)

# 0001 — Split the system across Go and Python rather than staying monoglot

- **Status:** accepted
- **Date:** 2026-08-07
- **Deciders:** Mhayk Whandson

## Context and problem statement

Overpass has two workloads with genuinely different centres of gravity.

The first is orbital: propagate SGP4 from a TLE, search for access windows,
filter them by SAR acquisition geometry (incidence band, look side, squint,
slant range), and generate footprint polygons on a reference ellipsoid. This is
numerical code where being *approximately* right is the same as being wrong — a
half-degree error in incidence angle silently produces an opportunity that no
real sensor could fly.

The second is platform: accept HTTP requests at a target of 1 000 rps without
dropping any, consume from durable streams concurrently, hold a transaction open
around an allocation decision, and publish p95/p99 latency numbers that survive
scrutiny.

The question: **do we pay the cost of two toolchains to put each workload in the
ecosystem built for it, or do we pay the cost of forcing one workload into a
language that fits it badly?**

## Decision drivers

1. **Correctness of the orbital math is non-negotiable.** It is the part of this
   system I am least able to eyeball for errors, so it must lean on libraries
   that thousands of people have already validated against real ephemerides.
2. **Latency predictability under concurrency**, because §9.2 of the spec commits
   to publishing tail latency, and a tail-latency claim is only as good as the
   runtime's scheduling behaviour.
3. **My ability to defend every line.** A language I write fluently beats a
   language that benchmarks better.
4. **Integration cost between the two halves**, which is the price of this
   decision and had to be bounded before accepting it.

## Considered options

1. **Go everywhere** — port or wrap SGP4 and the geometry stack in Go
2. **Python everywhere** — FastAPI plus asyncio for ingress, planner, and read side
3. **Go for platform services, Python for feasibility** — split on workload
4. **Rust for the planner**, Go for the rest, Python for feasibility

## Decision outcome

Chosen: **Option 3 — Go for `tasking-api`, `planner`, and `plan-gateway`; Python
for `feasibility-service`.**

The split follows the shape of the work rather than a preference for uniformity.
Feasibility is pure, stateless per request, and embarrassingly parallel: it takes
a target and a horizon, and returns opportunities. That is exactly the shape that
tolerates a process boundary well, and it is the only component where the
scientific ecosystem (`sgp4`, Skyfield, `pyproj`, Shapely) is a decisive
advantage. Everything else is concurrent I/O and transactional state, which is
Go's home ground.

Critically, this decision is only affordable because of the contracts-first rule
(see `contracts/README.md`): the interface between the two languages is a frozen
JSON Schema, generated into both, with a CI drift check. Without that, a polyglot
split degenerates into two teams guessing at each other's payloads.

### Consequences

**Good**

- SGP4 and the geometry stack come from libraries validated against real
  ephemerides. We are not the first people to run this code.
- Feasibility scales horizontally by adding consumer instances, with no shared
  state to coordinate — the natural consequence of it being pure.
- Goroutines map cleanly onto "one durable consumer, N concurrent handlers", and
  Go's scheduler gives us tail latency we can quote without asterisks.
- The language boundary is also a failure boundary: a feasibility crash cannot
  take ingress down.

**Bad**

- Two toolchains, two dependency managers (Go modules, `uv`), two lint/type
  configurations, two CI paths, two container base images.
- A serialisation boundary that costs real microseconds and, worse, real
  cognitive overhead: every shared concept exists twice.
- Onboarding cost is higher. A contributor must be at least literate in both.
- Refactoring a shared concept is a two-repo-shaped change inside one repo.

**Neutral**

- Codegen becomes mandatory rather than nice-to-have. That is a cost, but it also
  makes the contract enforceable, which we wanted anyway.

### Confirmation

The decision is wrong if either of these becomes true:

- The Go↔Python boundary shows up as a top-three contributor to end-to-end p99 in
  `docs/performance.md`. (Expected: it will not — the boundary is async, and the
  SGP4 sweep dominates by orders of magnitude.)
- More than a third of pull requests have to touch both languages to land a
  single behavioural change. That would mean the boundary is in the wrong place,
  not that polyglot was wrong.

## Pros and cons of the options

### Option 1 — Go everywhere

- Good, because one toolchain, one CI path, one container base, trivially simpler
  onboarding and release story.
- Good, because it removes the serialisation boundary entirely.
- Bad, because the Go SGP4 ports are thinner, less exercised, and less
  cross-validated than the Python ones. We would inherit ownership of physics
  bugs we are not equipped to find.
- Bad, because there is no credible Go equivalent of Shapely plus `pyproj` for
  geodesic footprint work. We would be hand-rolling geodesy, which is precisely
  the category of code that looks plausible and is subtly wrong.

### Option 2 — Python everywhere

- Good, because the scientific stack is available everywhere and the codebase is
  uniform.
- Bad, because the ingress path targets 1 000 rps with a p99 budget. Reaching
  that in Python means multiple worker processes and careful shared-nothing
  design — achievable, but the latency story becomes an argument rather than a
  measurement.
- Bad, because the planner holds a transaction and an advisory lock while running
  an allocation loop. Predictable, low-variance execution matters there, and the
  GIL makes that a harder claim to defend.

### Option 3 — Split on workload (chosen)

- Good, because each half sits in the ecosystem built for it.
- Good, because the split lines up with an existing consistency boundary
  (see [ADR-0003](0003-consistency-boundaries-and-cap-position.md)), so it is not
  an extra seam — it is the same seam.
- Bad, because of the toolchain and serialisation costs listed above.

### Option 4 — Add Rust for the planner

- Good, because the allocation inner loop is the one place where raw speed
  translates directly into a better plan, since a faster policy can explore more.
- Bad, because it introduces a third toolchain to save milliseconds on a
  component whose measured budget is 800 ms.
- Bad, and decisively: I cannot currently defend idiomatic Rust line by line
  under questioning. Choosing a language I cannot explain would undermine the
  entire premise of this repository.

## More information

- Contract layout and codegen: `contracts/README.md`
- Consistency boundaries that this split follows:
  [ADR-0003](0003-consistency-boundaries-and-cap-position.md)

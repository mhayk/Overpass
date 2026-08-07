# 0010 — Treat the test suite as the verification harness for generated code, and gate coverage at 80/95

- **Status:** accepted
- **Date:** 2026-08-07
- **Deciders:** Mhayk Whandson

## Context and problem statement

Much of this system's code is written with AI assistance. That changes what a
test suite is *for*.

When a person writes a function, the tests mostly guard against future change:
the author understood the problem at the time, and the risk is that someone
later breaks it. When a model writes a function, the risk profile inverts. The
code is fluent, idiomatic, well-commented, and **may be confidently wrong on its
first line**. It compiles. It passes review by inspection, because inspection is
exactly the thing fluent-and-wrong code defeats.

This is not hypothetical here. Three defects survived reading and were caught
only by execution, during M0, before a single service existed:

1. `go-jsonschema` emits `type OccurredAt time.Time` — a *defined type*, not an
   alias. Defined types do not inherit methods, so **every timestamp in every
   event failed to decode at runtime**. The generated code compiled, `go vet`
   was clean, and the type looked exactly right.
2. `format: uuid` combined with a redundant `pattern` made pydantic raise
   `TypeError` at parse time, so **every event carrying an id was unparseable in
   Python**. Both keywords are individually correct; the combination is not.
3. The codegen drift gate's config file set `output:`, which overrides the `-o`
   flag — so the gate regenerated into the directory it was comparing against,
   **compared it to itself, and could never fail**. A verification mechanism
   that cannot fail is worse than none, because it is trusted.

Number 3 is the one worth dwelling on. It was a bug *in the verification layer*,
and no amount of reading the verification layer would have found it. Only
deliberately breaking a schema and demanding that the gate notice did.

The question: **what test strategy actually catches this class of defect, and
what coverage target reflects that rather than flattering it?**

## Decision drivers

1. **Some code is generated or AI-assisted and looks right while being wrong.**
   The suite must be an independent check, not a restatement of the code.
2. **Two areas have silent failure modes.** A wrong incidence angle produces a
   plausible opportunity for an unimageable target. A scheduler that violates
   slew time produces a plan that simply cannot be flown. Neither throws.
3. **Verification mechanisms must themselves be verified.** See defect 3.
4. **The gate must be one people keep.** A coverage target that forces tests
   nobody believes in gets lowered, then removed, and takes the real checks with
   it.

## Considered options

1. **Example-based unit tests plus integration tests** — the conventional pyramid
2. **Layered strategy**: unit, property-based, golden-reference, integration,
   contract, E2E — each layer chosen for a specific failure mode
3. **Heavy end-to-end testing**, on the grounds that only the whole system matters
4. **100% coverage** as the gate, versus a tiered target

## Decision outcome

Chosen: **Option 2 — a layered strategy where each layer exists to catch a named
failure mode**, with coverage gated at **80% overall and 95% on the planner and
geometry packages**.

The organising principle: **the test suite is the verification harness for
generated code.** Every layer answers "how would I know if this were confidently
wrong?"

| Layer | Catches | Why not something cheaper |
| --- | --- | --- |
| **Unit** (table-driven Go, parametrised pytest) | Ordinary logic errors, state-machine transitions | Nothing cheaper exists |
| **Property-based** (gopter, hypothesis) | Scheduler invariant violations on inputs nobody imagined | Example tests only cover cases you thought of; scheduling bugs live in the ones you did not |
| **Golden-reference** | Orbital math that is plausibly, quietly wrong | A snapshot of our own output asserts only that the code still does what it did yesterday, **including if yesterday was wrong** |
| **Integration** (Testcontainers, real Postgres and NATS) | Duplicate delivery, out-of-order arrival, crash mid-transaction, relay restart | Mocks assert that our understanding of the infrastructure is self-consistent, which is not the thing in doubt |
| **Contract** (schema validation + cross-language round-trip) | Go and Python silently disagreeing about a payload | Two independently written generators have no reason to agree; it is checked, not assumed |
| **E2E** (Playwright: happy path + contested window) | The seams between everything | Slowest and most brittle layer; earns exactly two tests |

Three of these deserve their reasoning stated in full.

### Physics needs an oracle, not a snapshot

The access-window computation is validated against **known passes for a public
satellite at a fixed TLE and epoch, from an independent source** — not against a
recording of our own first run.

This is the single most important line in this ADR. A snapshot test on SGP4
output feels like a test and is not one: it asserts that today's answer equals
yesterday's answer. If the original implementation had the sign of the cross-track
component backwards, the snapshot enshrines that error permanently and every
future run "passes". Orbital mechanics has published ground truth. Using anything
else here would be choosing to not find out.

Tolerances come from the physics — SGP4's known accuracy against a TLE of a given
age — and are written down with their justification. A tolerance widened until
the test goes green is a test that asserts nothing.

### The scheduler gets invariants, not examples

For **any** generated input, the committed plan must satisfy:

- no two acquisitions on one satellite overlap
- every consecutive pair has `gap >= slew_time(a, b)`
- the per-orbit duty-cycle budget is never exceeded
- no acquisition finishes after its request's deadline
- at most one acquisition per request
- **every candidate request appears either as an acquisition or as an
  unfulfilment** — conservation

That last one protects customers directly, and no example test would ever find
its violation: a request silently vanishing between rounds is invisible unless
you are specifically counting.

These run against all four allocation policies from one suite, so a fifth policy
inherits the entire correctness bar for free.

### Verification mechanisms are themselves verified

Because of defect 3 above, this is now a standing rule: **any check whose job is
to catch a problem must be demonstrated failing on that problem.**

The drift gate was proven by adding a field to a schema without regenerating and
confirming it reported the difference in both languages, then restoring it. The
exclusion constraint is proven in CI by attempting an overlapping insert *and* by
attempting the same window on a different satellite — the second half matters,
because a constraint that rejected both would look like it worked while
serialising the entire constellation.

Negative fixtures follow the same logic: a schema that accepts everything passes
every positive test ever written, so the eight `contracts/examples/invalid/`
fixtures are the ones carrying information.

### Coverage: 80 overall, 95 on planner and geometry

- **95%** on `planner/internal/{domain,allocation}` and `feasibility/geometry`.
  These are where correctness is hardest and failure is silent. Uncovered lines
  here are genuinely alarming.
- **80%** overall. Adapters, wiring, and configuration are covered incidentally
  by integration tests; chasing them individually produces tests that assert a
  constructor was called.
- **Generated code is excluded entirely.** Nobody wrote it, it cannot be changed
  without editing a generator, and its correctness is established by the
  round-trip tests and the drift gate — both stronger evidence than a
  percentage. Including it would also drown the overall figure in thousands of
  generated lines and make the number meaningless for the code that matters.

**Why not 100%.** Because it measures the wrong thing and costs the right thing.
The remaining 20% is error paths on infrastructure calls, `String()` methods, and
generated wiring. Covering them means writing tests that mock a database failure
to assert that an error is returned — asserting that `if err != nil { return err }`
does what it says. Meanwhile 100% says nothing about whether the SAR geometry is
correct, because **a fully covered wrong function is still wrong**. Coverage
measures which lines executed, never whether the assertions meant anything.

The real objection is second-order: a 100% gate creates pressure to write
assertion-free tests that execute lines to move a number. That is worse than
uncovered code, because it looks like safety.

### What is deliberately not tested

Naming these is part of the decision, so their absence is not mistaken for
oversight:

- **Generated code**, per above.
- **Cesium and deck.gl rendering internals.** We test that the right data reaches
  them; we do not test that a third-party renderer draws a polygon.
- **Third-party library behaviour.** No tests that SGP4 propagates correctly —
  that is the library's job, and testing it would be re-deriving the oracle.
- **Load behaviour, in the unit suite.** That is k6's job, with thresholds as CI
  gates, and it belongs in `docs/performance.md` with real numbers.

## Consequences

**Good**

- Every layer has a stated failure mode, so "should this be a test?" has an
  answer instead of a debate.
- Property-based and golden-reference tests catch precisely the defects that
  survive code review, which is the category AI-assisted code produces most.
- The strategy is a direct, honest answer to "how do you know the generated code
  is right?" — a question this project will be asked.

**Bad**

- Property-based tests are slower to write and their failures are harder to read
  than example tests. Shrinking to minimal counterexamples is mandatory or the
  suite becomes something people skip.
- Golden-reference tests need an independent oracle, which is real research work
  and cannot be produced by the same process that produced the code.
- Testcontainers integration tests are slow. Containers are per test class rather
  than per test — theoretically worse isolation, chosen because a suite nobody
  waits for is a suite nobody runs.
- Two coverage thresholds are more configuration than one.

**Neutral**

- Coverage numbers are reported per package rather than as a single figure. An
  aggregate 82% hides whether the uncovered 18% is wiring or the allocation
  algorithm, and those are not remotely the same fact.

## Confirmation

This decision is wrong if any of these become true:

- A production-class bug reaches `main` in the planner or geometry packages with
  the gate green. That would mean the 95% is measuring the wrong lines, and the
  response is better invariants, not a higher number.
- Property-based tests are routinely skipped or their seeds pinned to avoid
  failures. That is the observable symptom of a suite people have stopped
  believing, and it is the failure mode this ADR is most concerned about.
- Golden-reference tolerances get widened more than once without a physics
  justification recorded in the commit.

The M1 goal is concrete: the golden-reference tests must reproduce a known pass
for a public satellite at a fixed epoch, from an independent source, within a
stated and justified tolerance. If that cannot be achieved, the geometry
implementation is wrong and no amount of coverage will say so.

## Pros and cons of the options

### Option 1 — Conventional unit plus integration

- Good, because it is well understood, fast to write, and familiar to everyone.
- Bad, because example tests cover the cases the author thought of — and the
  author here is partly a language model, so the cases it thought of and the
  cases its code gets wrong are correlated in exactly the wrong direction.
- Bad, because it has no answer for orbital math: an example test needs an
  expected value, and if that value comes from running the code, it is a
  snapshot.

### Option 2 — Layered by failure mode (chosen)

- Good, because each layer is justified by something it catches that nothing
  cheaper does.
- Good, because it reframes the suite as verification of generated code, which
  is both true and the strongest available answer on the subject.
- Bad, because of the cost and complexity noted in Consequences.

### Option 3 — Heavy end-to-end

- Good, because E2E tests exercise real integration and cannot be fooled by a
  mock that agrees with a wrong assumption.
- Bad, because they are slow, flaky, and — decisively — give terrible
  *localisation*. "The plan was wrong" does not tell you whether the incidence
  angle, the slew model, or the allocation policy was at fault, and that is
  precisely the distinction that matters here.
- Bad, because they cannot express invariants over arbitrary inputs, which is
  where the scheduler's real risk sits.

### Option 4 — 100% coverage

- Good, because it is unambiguous, needs no negotiation about which packages are
  important, and prevents whole untested modules from creeping in.
- Bad, because it measures line execution, not assertion quality. A fully
  covered wrong function is still wrong.
- Bad, because the last 20% is error paths and wiring, and reaching it produces
  tests that assert error propagation propagates errors.
- Bad, and this is the real objection: it creates pressure to write
  assertion-free tests that move a number. That looks like safety and is not,
  which makes it worse than the gap it closes.

## More information

- Contract testing and the generator defects: `contracts/README.md`
- Round-trip tests: `gen/go/contracttest/roundtrip_test.go`,
  `scripts/contracts_smoke.py`
- Coverage gate: `scripts/coverage-gate.sh`
- CI gates: `.github/workflows/`
- The AI-engineering write-up that this ADR is the technical half of:
  `docs/ai-engineering/02-verification.md`

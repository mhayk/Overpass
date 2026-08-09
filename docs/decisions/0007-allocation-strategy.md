# 0007 — Choose the allocation algorithm by measurement behind a policy interface, rather than committing to one algorithm

- **Status:** accepted
- **Date:** 2026-08-08, accepted 2026-08-09 with the M2-13 numbers
- **Deciders:** Mhayk Whandson

> Written 2026-08-08 as `proposed`, with the measured-results section an
> explicit placeholder, because M2-04 through M2-08 needed the problem statement
> and the interface boundary before they could be built. Accepted 2026-08-09,
> when M2-13 (#45) produced the numbers below. The expectation recorded on day
> one — that `GreedyByValueDensity` would be the default — survived contact with
> the measurement, which is worth exactly as much as it would have been worth to
> see it fail.

## Context and problem statement

This is the decision the project exists to demonstrate. Everything upstream —
the contracts, the outbox, the projections of ADR-0015 — is plumbing that
delivers a set of candidates to one function. That function decides who flies.

The plumbing is now in place. `planning.candidate_opportunities` and
`planning.request_snapshots` (M2-16, #104) give a round everything it allocates
by, inside one schema, inside one transaction, behind one advisory lock. What
they do not give is the algorithm.

The temptation is to pick one. Sorting by value density is the obvious choice,
it is defensible in an interview, and it would be in `main` by this afternoon.
The problem with that is not that it would be wrong — it is very likely the
right default — but that nothing about the repository would then be able to say
*how* right. "Greedy is good enough" is an assertion. It is exactly the class of
confident, untested claim that [CLAUDE.md](../../CLAUDE.md) records as the source
of every M0 defect, and it fails the same way: it compiles, it looks idiomatic,
and it is unfalsifiable.

The question: **what algorithm allocates opportunities to requests, how is that
choice justified rather than asserted, and what is the cost of keeping the
choice open?**

## The problem, stated formally

Given a set of candidate opportunities `O`, each `o` carrying its request
`r(o)`, satellite `s(o)`, orbit `k(o)`, an access window `[w⁻ₒ, w⁺ₒ]` **in which
the acquisition may start**, a duration `pₒ`, and a duty-cycle cost `cₒ`; a
per-request effective value `v_r` and deadline `d_r`; a slew function
`slew(a, b) ≥ 0` between two acquisitions on one satellite; and a per-orbit
duty-cycle budget `B_{s,k}` —

choose a subset `S ⊆ O` and a start time `tₒ ∈ [w⁻ₒ, w⁺ₒ]` for each `o ∈ S`,
maximising

    Σ_{o ∈ S} v_{r(o)}

subject to

1. `|{o ∈ S : r(o) = r}| ≤ 1` for every request — do not image the same target twice
2. for `a, b ∈ S` consecutive on satellite `s`:  `t_b ≥ t_a + p_a + slew(a, b)`
3. `Σ_{o ∈ S, s(o)=s, k(o)=k} cₒ ≤ B_{s,k}` for every satellite-orbit
4. `tₒ + pₒ ≤ d_{r(o)}`

Constraint 2 subsumes non-overlap, since `slew ≥ 0`. Constraint 3 is the
knapsack dimension. **The start time `tₒ` is a decision variable, not an input** —
`00006_planning_inputs.sql:106` is explicit that the access window bounds the
start and the duration is carried separately, because "collapsing them into one
fixed interval would delete the system's main scheduling freedom". That single
modelling choice is what makes the rest of this section non-trivial, and it is
worth knowing that the easy version of this problem is the one where `tₒ` is
fixed.

### Why it is NP-hard, and which kind

Two reductions, because they establish different things and only one of them
carries weight.

**Weakly NP-hard, via 0/1 Knapsack.** Restrict to one satellite and one orbit;
make every access window a single instant, so start times are forced and
constraint 2 is vacuous under `slew ≡ 0`; let no deadline bind. What remains is:
choose a maximum-value subset with `Σ cₒ ≤ B`. That is 0/1 Knapsack, so the
problem is NP-hard.

This reduction is the one most people reach for, and on its own it does not
justify much. Knapsack has a pseudo-polynomial dynamic program; if the duty-cycle
budget is the only hard part, an exact algorithm polynomial in `B` exists and
"NP-hard" would be close to an excuse.

**Strongly NP-hard, via TSP with Time Windows.** Take one satellite, an
unbounded duty-cycle budget, one opportunity per request, and `pₒ = 0`. Ask the
decision question: is there a schedule serving *all* `n` requests? A solution is
an ordering of the `n` acquisitions together with start times, such that each
`tₒ` lies in its window and consecutive acquisitions are separated by
`slew(a, b)`. With `slew` as the inter-city distance and `[w⁻ₒ, w⁺ₒ]` as the
time window, that is precisely the feasibility question of the Travelling
Salesman Problem with Time Windows, which is strongly NP-complete. The
optimisation form here — take a *subset*, maximising value — is its
prize-collecting variant.

This is the reduction that matters, and it is the sequence-dependent slew that
supplies it. No pseudo-polynomial algorithm follows from it, and the SPEC's
description of slew as "where the real difficulty lives" is literally correct:
delete constraint 2 and the single-satellite problem with fixed starts collapses
to weighted interval scheduling, solvable exactly by a textbook `O(n log n)` DP.

**One honest qualification, stated because a reviewer will find it otherwise.**
`slew(a, b)` is not an arbitrary distance matrix. It is a physical function of
two look geometries — roll, look side, squint — computed by M2-02, and it very
likely satisfies structure a general TSPTW instance does not, plausibly including
the triangle inequality. The reduction therefore proves the *general shape* of
this problem is strongly NP-hard; it does **not** prove that the physically
realisable subclass is. Nor does anything here prove that subclass is easy.

That gap is not a weakness in the argument, because the decision does not rest
on the asymptotic class at all. Worst-case complexity is a reason to **measure**,
never a proof that measuring is unnecessary. A polynomial algorithm too slow for
the p95 budget is useless, and an NP-hard problem whose real instances solve in
40 ms is fine. What follows is built on that premise.

## Decision drivers

1. **"Which algorithm?" has to become a measurement.** This is the dominant
   driver and the reason the milestone exists. Asserted algorithm quality is the
   exact failure mode the repository's hard-won M0 rules were written against.
2. **A heuristic without a ground truth is unfalsifiable.** "Greedy does well"
   means nothing without an optimum to divide by. Somebody has to compute the
   optimum, even if only on instances small enough to admit one.
3. **`p95 < 800 ms` for a 5 000-opportunity round is a hard budget** (SPEC §9).
   The round holds an advisory lock while it runs, so allocation latency is not
   a user-facing nicety, it is the width of the system's one serialisation point.
   Unpredictable runtime is disqualifying in a way that merely-slow runtime is not.
4. **I have to whiteboard this algorithm and its complexity from memory** (SPEC
   §13). This is a portfolio project for an interview, and it is a real
   engineering constraint here rather than vanity: an algorithm I cannot explain
   without an editor open is one I cannot defend when it produces a strange plan.
5. **Fairness must live outside the algorithm.** Priority tiers and ageing (M2-09,
   #41) are product policy, and they change on different timescales than
   scheduling code. If they leak into each policy, four implementations acquire
   the same product decision four times.
6. **No new dependency without stating what it buys and what it costs**
   (CLAUDE.md). This bears directly on the CP-SAT option below, which is the
   strongest rejected alternative and loses partly on this driver.

## Considered options

1. **Commit to one heuristic** — implement `GreedyByValueDensity`, no interface
2. **A CP-SAT or ILP solver** — model the constraints declaratively, let OR-Tools solve
3. **A metaheuristic** — simulated annealing or large-neighbourhood search
4. **Exact everywhere** — run the optimal solver in production, accept the runtime
5. **An `AllocationPolicy` interface with four implementations**, and a default
   chosen by benchmark

## Decision outcome

Chosen: **Option 5 — `AllocationPolicy` as an interface, with `GreedyByBid`,
`GreedyByValueDensity`, `VickreySealedBid` and `ExactDP` behind it, and the
production default selected on benchmark evidence rather than named here.**

The interface is chosen because of driver 1, and it is worth being precise about
why, since "use the Strategy pattern" is otherwise the kind of thing that gets
applied reflexively. The pattern is not here for extensibility — nobody is going
to write a fifth policy — and not to defer a decision. It is here because it makes
four algorithms **comparable over identical inputs**, and comparability is what
turns the central claim of this milestone from an opinion into a number. A repo
containing one greedy allocator can say it chose well. A repo containing four
policies and a benchmark can show it.

`ExactDP` is what makes the other three honest, and it earns its place on driver
2 alone. It is not production code, it is the denominator: without it,
`GreedyByValueDensity` has no optimality ratio and the strongest correctness test
in the planner — *no heuristic may ever exceed the exact plan value on an instance
both can solve* — cannot be written. That test finds constraint violations
nothing else would, because a heuristic that outscores the optimum is not clever,
it is cheating on constraint 2 or 3.

Fairness stays out of all four, per driver 5. Every policy allocates by
`effective value`, a single projected quantity that M2-09 defines as the bid
adjusted by tier multiplier and ageing factor. The policies never see a tier.
This is what keeps "should civil protection outrank a commercial bid?" a product
question with one implementation, rather than a scheduling question with four.

### Why not the solver, in particular

Option 2 deserves more than a line, because it would probably win on plan
quality. A CP-SAT model expresses constraints 1–4 almost directly, handles the
sequence-dependent setup times as a native circuit constraint, and would beat
every heuristic here on most instances while taking a fraction of the code.

It loses on three drivers at once, and each would be survivable alone:

- **Driver 4.** A hiring manager asks why a request lost. With OR-Tools the
  honest answer is "the solver did not include it", and the reasoning is inside a
  dependency I cannot whiteboard. The M4 explanation panel — the feature the SPEC
  calls the one that will be remembered — needs a decision procedure whose
  rejections are attributable to a named constraint.
- **Driver 3.** CP-SAT runtime under a fixed budget is not predictable from
  instance size. It is excellent on average and occasionally not, and "occasionally
  not" happens while holding the advisory lock.
- **Driver 6.** It is a large dependency whose behaviour would have to be
  characterised before it could be trusted, and characterising it is most of the
  work the four policies do anyway.

None of that makes CP-SAT the wrong tool in general. In a production tasking
system with a real revenue function, delegating to a mature solver would very
likely be correct, and this ADR should not be read as arguing otherwise. It is
the wrong tool *for an artifact whose purpose is to demonstrate that the author
understands the problem*, which is an unusual driver and is stated openly rather
than dressed up as a technical objection.

### Measured results

From [`docs/policy-benchmark.md`](../policy-benchmark.md), seeded and
regenerable with `make benchmark`. Ratios are against **proven** optima only:
74 of 96 instances were solved exactly, and the report names which — the
unsolved ones cluster in the *uncontended* classes, where everything is
feasible and the branch-and-bound's incumbent never prunes, which is the
opposite of intuition and stated because a reviewer would ask.

| finding | number |
| --- | --- |
| `GreedyByValueDensity`, worst class | **98.0%** of optimal |
| `GreedyByBid`, worst class | **85.7%** — the same class where density scores 98.8% |
| every heuristic, every uncontended class | 99–100% |
| `VickreySealedBid` ratios | identical to `GreedyByBid` in all eight classes |
| runtime at the 5 000-candidate cap | bid 39 ms, density 54 ms, Vickrey 137 ms |
| plan value at 5 000 candidates | density 141 373 vs bid 105 492 |

**The default policy is `GreedyByValueDensity`**, on three of those numbers.
Its worst class is thirteen points better than the baseline's worst — and it is
the *same* class, contended with dispersed geometry, which is precisely where a
tasking system earns its keep. Its 54 ms at the contract's candidate cap sits
fifteen-fold inside the round's 800 ms p95 budget. And nothing beats it
anywhere: the baseline never exceeds it in any class, which the
no-heuristic-beats-optimal test would have caught as a bug if it had.

Two findings worth keeping beside the decision. Vickrey matching the baseline
in every class confirms by data that second-price clearing changes what winners
*pay* and never who wins — so choosing it would be choosing a pricing story,
not plan value. And the uncontended classes flattening every policy to ~100%
says the whole comparison only matters under contention, which is the regime
the fairness model and the debounce were built for anyway.

### Consequences

**Good**

- The central claim becomes a measurement. "Greedy reaches N% of optimal in X ms
  where exact takes Y s" is a sentence backed by a rerunnable harness.
- `ExactDP` gives the planner a ground-truth oracle, and with it the
  no-heuristic-beats-optimal test, which is the only test in the system that
  catches a constraint violation disguised as a good plan.
- Each policy is independently table-testable against the same constraint
  checkers, so constraint bugs surface as a policy disagreeing with its peers.
- The unfulfilment reasons of M2-15 stay attributable to a named constraint,
  because each policy rejects candidates at a point in its own code that knows
  which of the four constraints bound.
- Fairness has exactly one implementation, and changing tier weights does not
  touch scheduling code.

**Bad**

- **Four implementations of one thing is four times the surface.** The 95%
  coverage gate on the planner applies to all of them, including `ExactDP`, which
  will never run in production. This is the real cost of the decision and it is
  not small.
- The interface fixes what a policy is allowed to see. All four here allocate per
  satellite-partition, matching the advisory-lock boundary, so a future policy
  wanting to reason across satellites — trading a request between two — cannot be
  expressed without changing the interface. That constraint is inherited from the
  lock, not from this ADR, but this ADR makes it structural.
- `VickreySealedBid` computes a clearing price that is never settled. There is no
  billing, so it is a correct mechanism attached to nothing, and that is a scope
  cut which has to be stated out loud every time the policy is discussed.
- The benchmark is only as honest as its scenario generator. A generator that
  happens to produce instances greedy is good at proves nothing, and no amount of
  rigour downstream repairs that.
- Optimality ratios exist only where `ExactDP` terminates, which is the small
  instances — precisely the instances least likely to discriminate between
  policies.

**Neutral**

- No event contract changes. `planning.plan.committed.v1` does not name the
  policy that produced the plan, and it does not need to; which policy ran is
  operational configuration, not part of the published shape of a plan.
- The default policy being configurable means production and benchmark run the
  same code path, which is a testing convenience rather than a design win.

### Confirmation

- **The interface earns itself** if the benchmark changes anyone's mind. The
  honest verdict now the numbers exist: the expectation held, but the numbers
  are load-bearing anyway — 85.7% versus 98.0% in the worst class is the
  difference between an assertion and a defence, and the Vickrey result was
  not predictable from the code.
- **The ground truth holds:** on every instance `ExactDP` solves within its limit,
  no heuristic reports a higher plan value (M2-08, #40). A failure of this test is
  a constraint bug, not a scoring bug, and it is the check the whole arrangement
  exists to make possible.
- **The invariants hold under generation:** property-based tests (M2-12, #44)
  over every policy produce no plan that overlaps, violates slew, exceeds duty
  cycle, or misses a deadline.
- **The budget holds:** a 5 000-opportunity round completes within `p95 < 800 ms`
  under the chosen default. If it does not, the default is wrong regardless of
  its plan value, because the lock is held for the duration.
- **The decision is wrong** if the measured spread between `GreedyByBid` and
  `GreedyByValueDensity` is negligible on realistic contention. That would mean
  the problem's difficulty is not where this ADR claims it is, the slew term is
  not actually binding, and the correct response is to simplify to one policy and
  supersede this ADR rather than to keep four for the story.

## Pros and cons of the options

### Option 1 — Commit to one heuristic

- Good, because it is a fraction of the code and ships today.
- Good, because there is no abstraction to justify and the call site reads plainly.
- Bad, because the choice is then an assertion. Nothing in the repository can say
  how far from optimal the plan is, and the most interesting question in the
  project goes unanswered.
- Bad, because without an exact reference there is no way to distinguish a good
  heuristic from one silently violating a constraint and scoring well for it.

### Option 2 — CP-SAT or ILP solver

- Good, because it would almost certainly produce the best plans here, and
  express constraints 1–4 declaratively and readably.
- Good, because sequence-dependent setup times are a solved modelling problem in
  CP-SAT rather than the hard part.
- Bad, because rejections become unattributable, which directly undermines the
  product's centrepiece feature.
- Bad, because worst-case runtime is unpredictable under a p95 budget held behind
  a lock.
- Bad, because it is a heavyweight dependency whose behaviour must be
  characterised before being trusted — and that characterisation is the work
  being avoided.

### Option 3 — Metaheuristic (simulated annealing, LNS)

- Good, because it scales past what exact methods reach and handles the messy
  constraints without a declarative model.
- Bad, because it is stochastic, so plan value varies between runs on identical
  input. Benchmarks become noisy and the "same request, same answer" property that
  makes re-planning explainable is lost.
- Bad, because tuning is a research project — cooling schedules and neighbourhood
  operators — and the tuning would be undocumented folklore in a repo whose whole
  premise is documented reasoning.
- Bad, because it is neither the honest baseline nor the ground truth, so it does
  not occupy a useful position in the comparison.

### Option 4 — Exact everywhere

- Good, because plan value is optimal by construction and the fairness discussion
  becomes purely about the value function.
- Bad, because it does not terminate in useful time at 5 000 opportunities, which
  is the stated scale. This is disqualifying on its own.
- Bad, because a silently degrading exact solver — one that times out and returns
  its incumbent — would corrupt the reference the entire benchmark depends on.
  This is why `ExactDP` has a loud instance-size limit rather than a graceful one.

### Option 5 — `AllocationPolicy` interface with four implementations (chosen)

- Good, because it converts the milestone's central question into a measurement.
- Good, because it produces the ground truth that makes the heuristics testable.
- Good, because it isolates fairness in the value function, where product policy
  belongs.
- Bad, because it is four times the implementation and test surface under a 95%
  coverage gate.
- Bad, because the interface boundary is drawn at the advisory-lock partition and
  quietly forecloses cross-satellite policies.

## More information

- Problem statement and the Strategy argument: `docs/SPEC.md` §7
- Inputs a round reads, and why they are projected: [ADR-0015](0015-planner-projects-its-own-request-value.md)
- Storage model for plans this produces: [ADR-0012](0012-retain-superseded-acquisitions.md)
- Consistency boundary the round runs inside: [ADR-0003](0003-consistency-boundaries-and-cap-position.md)
- The slew function supplying the strong reduction: M2-02 (#34)
- The duty-cycle budget supplying the knapsack dimension: M2-03 (#35)
- Effective value, where fairness lives: M2-09 (#41)
- Ground truth and the no-heuristic-beats-optimal test: M2-08 (#40)
- Benchmark that completes this ADR: M2-13 (#45), `docs/policy-benchmark.md`

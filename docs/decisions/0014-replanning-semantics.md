# 0014 — Fire a round on a quiet-period debounce under a staleness ceiling, recompute the whole bucket, and give incumbency no advantage

- **Status:** accepted
- **Date:** 2026-08-08
- **Deciders:** Mhayk Whandson

## Context and problem statement

[ADR-0012](0012-retain-superseded-acquisitions.md) settled how a superseded
acquisition is *stored*, and said in as many words that it was not settling the
planner side:

> The planner-side semantics of re-planning — when a round fires, how the
> debounce interacts with the cadence timer, what happens to a request that held
> an acquisition in a plan now replaced, and whether re-planning may ever be
> partial — are M2 decisions and are not settled here. They get their own ADR
> (0014) when the constraint is actually felt, rather than being guessed at now.

The constraint is now felt. M2-01 (#33) has "cadence timer trigger" and
"opportunity-arrival debounce trigger" as separate acceptance criteria and cannot
be implemented without knowing how the two interact, what the second one does to
a bucket the first one already planned, and whether a sitting acquisition is
defended or re-contested.

Two things constrain the answer before it is chosen, and both were checked rather
than remembered.

**The contract is frozen and already decides more than it looks like it does.**
`planning.round.triggered.v1` fixes round identity as `(satellite_id,
bucket_start)`, makes that pair the advisory-lock key, carries the `policy` on
the event, and enumerates `trigger` as `CADENCE | OPPORTUNITY_DEBOUNCE | MANUAL |
REPLAN`. It also states that `causation_id` is "the event_id of the opportunities
event that tipped the threshold. Null for CADENCE." Whatever is decided here has
to be expressible in those fields, because the schema is not being versioned for
a decision that should have been made under it.

**The contract imposes a conservation property that reaches back into the
trigger.** `candidate_request_ids` is documented as "the input side of the
ledger: every one of these must appear either as an acquisition in the committed
plan or as a `planning.request.unfulfilled.v1`", with a contract test asserting
exactly that. That is not a note about the output — it constrains what a round is
allowed to *open with*, and it collides with the held-candidate rule of
[ADR-0015](0015-planner-projects-its-own-request-value.md) in a way that has to be
resolved here.

The question: **what fires a round, what does that round recompute, and what
claim does a previously-committed acquisition have on the outcome?**

## Decision drivers

1. **Allocation must stay a pure function of the candidate set.** This is the
   dominant driver and it is not aesthetic. M2-12's property tests generate an
   input and assert invariants over the output; M2-13's benchmark runs four
   policies over *identical* inputs and divides by `ExactDP`. Both are meaningless
   if the plan also depends on what was planned an hour ago. Any option that
   threads history into the allocation is paying for it in the two artifacts this
   milestone exists to produce.
2. **The round holds the advisory lock while it runs.** Firing more often is not
   free: it is contention on the one serialisation point in the system, and
   [ADR-0003](0003-consistency-boundaries-and-cap-position.md) placed it there
   deliberately.
3. **No bucket may wait indefinitely.** A bucket that is dirty and never planned
   is a request that silently never flies, which the contract calls "the worst
   possible failure mode for a customer".
4. **Fixed UTC-aligned buckets, so rounds are reproducible and replayable** — the
   contract's own wording, and the reason a rolling window was rejected: the same
   input would produce different partitions depending on when it ran.
5. **Out-of-order arrival is a fact to absorb, not an error to report**
   (ADR-0015). A candidate whose request snapshot has not landed is *held*. A
   bucket can therefore be dirty with candidates that cannot be allocated by
   anybody, and the trigger has to survive that without looping.
6. **`SUPERSEDED` must stay answerable.** ADR-0012 retained the rows specifically
   so a losing customer can be told what replaced them, and this ADR must not
   produce supersessions that cannot be explained.

## Considered options

Three sub-questions, taken together because they are not independent — the answer
to the third is what makes the second cheap.

**When does a round fire?**

1. Cadence only — a periodic sweep plans every dirty bucket
2. Debounce only — a quiet period after opportunity arrival, no timer
3. **Quiet-period debounce under a staleness ceiling** — whichever comes first

**What does a round recompute?**

4. The whole bucket, from the full candidate set
5. Only the unallocated remainder, with committed acquisitions pinned

**What claim does a sitting acquisition have?**

6. None — it re-competes on effective value like any other candidate
7. An incumbency multiplier, to damp churn
8. Free re-competition outside a lock-in window near flight, frozen inside it

## Decision outcome

Chosen: **option 3, option 4 and option 6.** A round fires when opportunities for
a bucket have stopped arriving for a quiet period `X`, or when the bucket has been
dirty for a ceiling `N`, whichever comes first. It recomputes the entire bucket
from the full candidate set. A previously-committed acquisition carries no
advantage into that recomputation.

The three answers are one decision, and driver 1 is why. Recomputing the whole
bucket (4) is only affordable because incumbency is worth nothing (6): if a
sitting acquisition had a claim, a full recompute would have to know which
acquisitions were sitting, and `plan = f(candidates)` would quietly become
`plan = f(candidates, previous_plan)`. That is the signature that breaks the
property tests and the benchmark simultaneously. Conversely, pinning committed
acquisitions (5) would make the plan depend on the *order rounds happened to
fire*, so the same candidate set would produce different plans on replay —
directly against driver 4, which the contract already committed to at the bucket
level and which would be undone one layer up.

What is bought is a strong and checkable property: **for a given
`(satellite_id, bucket_start)` and a given candidate set, the committed plan is
the same regardless of how many rounds preceded it or in what order candidates
arrived.** That is testable, it is what makes a re-plan explainable — the plan
changed because the *candidates* changed, and for no other reason — and it is
what lets M2-13 compare policies at all.

### Firing: `X` and `N`, and why there is no third knob

A bucket is marked dirty by any candidate arriving for it. Each arrival arms, or
re-arms, a timer of `X`. Silence for `X` fires the round. Independently, a bucket
that has been continuously dirty for `N` fires regardless of arrivals. A clean
bucket is never fired by the ceiling — the cadence sweep is a ceiling on
staleness, not a heartbeat, so an idle constellation does no work.

`X` and `N` are configuration, not constants chosen here, and the planner
validates `X < N` at startup rather than trusting them. Naming specific values in
an ADR before M3 has measured anything would be exactly the prediction-dressed-as-
decision that [ADR-0007](0007-allocation-strategy.md) is explicitly avoiding.

The contract's description of `OPPORTUNITY_DEBOUNCE` permits more than this: "*enough
new opportunities landed for this bucket*, or the debounce timer expired". A count
threshold is therefore contract-legal and is **deliberately not implemented**. It
would be a third knob interacting with two others, and the failure it protects
against — unbounded latency under a sustained arrival stream — is already covered
by the ceiling `N`. If M3 measures a case where `N` is too coarse, the threshold
can be added without touching the schema, which is the reason for recording that
it was considered.

### Two questions the frozen contract forces, and their answers

**`REPLAN` is not orthogonal to `CADENCE` and `OPPORTUNITY_DEBOUNCE`, and the
overlap has to be resolved rather than noticed later.** A debounce-fired round
over a bucket that already has a live plan is honestly describable as either. The
contract breaks the tie itself: `supersedes_plan_id` is "the plan being replaced,
**when trigger is `REPLAN`**", and null "on a first plan for the bucket". So:

> **`trigger` is `REPLAN` whenever a live plan for the bucket is being replaced,
> whatever physically fired the round. What fired it is then read from
> `causation_id` — non-null means an opportunities event tipped it, null means the
> ceiling did.**

The cost is real and is stated rather than buried: one enum field now carries two
orthogonal facts, so `trigger` cannot answer "was this a cadence round?" without
also consulting `causation_id`. That is a `v2` candidate, not a `v1` edit — the
schema is frozen and this is a semantics decision taken under it, which is the
whole point of freezing it.

**A held candidate must not appear in `candidate_request_ids`.** This falls
straight out of the conservation property and is the sharpest interaction in this
ADR. A candidate whose request snapshot has not landed has no bid, no tier and no
deadline; ADR-0015 holds it rather than guessing. But every id in
`candidate_request_ids` must produce either an acquisition or a
`planning.request.unfulfilled.v1`, and a held candidate can produce neither —
there is no value to lose on and no reason code that honestly describes "we have
not heard of you yet". Listing it would fail the conservation test; inventing an
unfulfilment reason for it would tell a customer they lost a competition they were
never entered in.

So the round opens over the *joinable* candidate set, and held candidates are
invisible to it. They are not dropped: they sit in
`planning.candidate_opportunities` until a later round, exactly as ADR-0015
intends.

**The dirty flag is therefore cleared by a round running, never by a candidate
being allocated.** This looks like a detail and is a live trap. Clearing dirtiness
only when candidates are consumed means a permanently-held candidate — its
snapshot lost, its consumer dead-lettered — keeps its bucket dirty forever, and
the ceiling `N` re-fires the same round into the same lock indefinitely, which is
a hot loop on the system's one serialisation point.

### Consequences

**Good**

- `plan = f(candidates)` holds exactly, which is what M2-12's property tests and
  M2-13's benchmark both rest on.
- A re-plan is explainable in one sentence — the candidate set changed — and the
  `SUPERSEDED` reason code stays truthful, because the replacing plan really is
  the allocation of a different input.
- The bucket is reproducible on replay: same candidates, same plan, regardless of
  round history. That makes an incident investigable by re-running the round.
- Idle satellites cost nothing. The ceiling applies to dirty buckets only, so an
  empty constellation does no allocation work at all.
- No schema change. Every decision here is expressible in the frozen `v1`.

**Bad**

- **Churn is real and this decision maximises it.** A customer can hold a slot,
  lose it to a later arrival, and regain it, several times before flight. The
  system currently offers no stability guarantee whatsoever, and for a real
  tasking product that would be a serious defect rather than a rough edge.
- No lock-in near flight. An acquisition can be superseded arbitrarily close to
  its start time, which is operationally wrong — a real system has a command
  upload deadline after which the plan is physically committed. This is a scope
  cut, taken because the executed-acquisition path is M4 and the deadline would be
  a time-dependent rule layered over allocation with nothing yet to validate it
  against.
- Full recomputation is strictly more work than patching the remainder, and it is
  repeated on every trigger. The bet is that the round fits its `p95 < 800 ms`
  budget anyway, and if it does not, this is the first decision to revisit.
- `trigger` is overloaded, as described above, so reading it correctly requires
  knowing to also read `causation_id`.
- Two timers per bucket is more moving state than one, and the `X < N` relation
  is an invariant enforced at startup rather than by construction.

**Neutral**

- `MANUAL` is unaffected: an operator-forced round is a `REPLAN` if a live plan
  exists and a first plan otherwise, by the same rule.
- A cadence sweep over an empty bucket committing an empty plan stays legal and
  meaningful, exactly as the contract says.

### Confirmation

- **Purity, which is the whole decision:** running the same candidate set through
  a round produces a byte-identical plan whether it is the bucket's first round or
  its fifth, and regardless of the order the candidates arrived in. If this test
  cannot be written, the decision was not implemented.
- **Serialisation:** concurrent rounds on one `(satellite_id, bucket_start)`
  serialise; rounds on different satellites overlap in wall-clock time. This is
  M2-01's load-bearing test and the contract's claim about lock granularity.
- **Conservation:** every id in `candidate_request_ids` appears either as an
  acquisition or as an unfulfilment event, with held candidates absent from the
  list entirely.
- **No hot loop:** a candidate held indefinitely — snapshot never arriving — does
  not cause its bucket to re-fire more than once. Demonstrated by holding a
  candidate and observing exactly one round, not a stream of them.
- **The decision is wrong** if measured churn makes plans useless in practice — if
  a request typically wins and loses its slot several times, customers cannot act
  on a plan, and the purity that bought the benchmark cost the product its output.
  The response would be a lock-in window (option 8), which preserves purity
  outside the window, rather than an incumbency bonus (option 7), which does not.

## Pros and cons of the options

### Option 1 — Cadence only

- Good, because it is one timer and one code path, and round frequency is bounded
  by construction.
- Good, because it is trivially starvation-free.
- Bad, because every request pays up to a full sweep interval in latency, even
  when the system is otherwise idle. The demo — submit a request, watch the globe
  re-plan — becomes a wait.

### Option 2 — Debounce only

- Good, because latency is minimal and the system reacts as fast as arrivals allow.
- Bad, because a sustained arrival stream re-arms the timer forever and the bucket
  never plans. That is starvation, against driver 3.
- Bad, because a bucket left dirty by a lost or dead-lettered event is never
  planned again — there is nothing to notice it.

### Option 3 — Debounce under a staleness ceiling (chosen)

- Good, because it absorbs bursts while bounding worst-case latency at `N`.
- Good, because the ceiling doubles as the recovery path for a bucket left dirty
  by a failure, which option 2 has no answer for.
- Bad, because it is two timers and a startup-validated relation between them
  rather than one timer.

### Option 4 — Recompute the whole bucket (chosen)

- Good, because it keeps allocation a pure function of the candidate set, which
  is what makes the property tests and the policy benchmark possible.
- Good, because it needs no notion of which acquisitions are sitting, so the
  commit path stays the one path.
- Bad, because it discards work and repeats it on every trigger.
- Bad, because it maximises churn, since nothing resists a plan changing.

### Option 5 — Pin committed acquisitions, plan the remainder

- Good, because churn drops sharply and a committed plan means something to a
  customer.
- Good, because each round is cheaper, touching only the free time.
- **Bad, because the plan becomes a function of round history.** The same
  candidates arriving in a different order produce different plans, which
  contradicts driver 4 at the plan level and makes the M2-13 comparison
  incoherent: two policies could no longer be run over "identical inputs", because
  the input would include each policy's own past.
- Bad, because early-arriving low-value requests get permanent squatting rights on
  good geometry, which is the opposite of what the fairness work in M2-09 is for.

### Option 6 — Incumbency has no claim (chosen)

- Good, because effective value stays the single quantity policies allocate by,
  and M2-09 remains the only place fairness is expressed.
- Good, because it needs no new state and no new configuration.
- Bad, because it is the maximum-churn choice, as recorded above.

### Option 7 — Incumbency multiplier

- Good, because it damps churn with one number and no structural change.
- Bad, because it puts the previous plan inside the value function, so
  `plan = f(candidates, previous_plan)`. The benchmark's premise — identical
  inputs across policies — no longer holds, and the property tests would have to
  generate plan *histories* rather than candidate sets.
- Bad, because the multiplier is unfalsifiable without a churn metric that does
  not exist yet, so it would be a tuned constant with no evidence behind it.

### Option 8 — Lock-in window near flight

- Good, because it models the real operational constraint: past the command upload
  deadline, the plan is physically committed and arguing about it is meaningless.
- Good, because purity survives outside the window — a frozen acquisition removes
  itself from the candidate pool rather than reweighting it, so the round is still
  a pure function of what it is given.
- Bad, because it introduces a time-dependent rule with nothing yet to validate it
  against; the executed-acquisition path arrives in M4.
- **The right escalation if churn proves to be a real problem**, and preferred over
  option 7 for the reason above.

## More information

- The storage half of supersession, which deferred this: [ADR-0012](0012-retain-superseded-acquisitions.md)
- Held candidates and the projections a round reads: [ADR-0015](0015-planner-projects-its-own-request-value.md)
- Why the round is the system's serialisation point: [ADR-0003](0003-consistency-boundaries-and-cap-position.md)
- What the policies do once a round opens: [ADR-0007](0007-allocation-strategy.md)
- Fields, trigger enum and the conservation property:
  `contracts/events/planning.round.triggered.v1.schema.json`
- Reason codes a superseded request receives:
  `contracts/events/planning.request.unfulfilled.v1.schema.json`
- Implementation of the trigger and the lock: M2-01 (#33)
- Implementation of supersession: M2-10 (#42)

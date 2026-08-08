# 0015 — Give the planner its own projection of request value, instead of reading the tasking schema

- **Status:** accepted
- **Date:** 2026-08-07
- **Deciders:** Mhayk Whandson

## Context and problem statement

M1-01 gave the planner everything it writes: `planning.collection_plans`,
`planning.acquisitions`, and the exclusion constraint that makes the whole
project's central claim true. It gave the planner nothing it reads.

A planning round allocates candidates by value. The candidates arrive on
`feasibility.opportunities.computed.v1`, which carries the geometry, the windows,
the durations and the duty-cycle costs — everything about whether an acquisition
is *physically possible*. It carries nothing about whether it is *worth flying*.
Bid, priority tier, deadline and customer live on the `TaskingRequest`, and reach
the world on a different event, `tasking.request.received.v1`, published by a
different service through a different stream.

That split is correct and should not be undone: feasibility computes geometry,
not commerce. But it leaves a question that M2-01 cannot start without answering:

> When a round fires, where does the planner get the bid, the tier and the
> deadline it is supposed to allocate by?

Two facts make this harder than it looks. The first is that the two events race —
the spec states plainly that global ordering is not assumed, and these are
genuinely independent streams, so a request's opportunities can and will arrive
before the request itself. The second is that the round is the one place in this
system that ADR-0003 designates strongly consistent and single-writer. Whatever
the answer is, it executes inside a transaction holding an advisory lock, and
anything slow or absent in there is a stall on the critical path.

## Decision drivers

- **Service autonomy over convenience.** `00001_reference.sql` already states the
  rule the hard way: a table several services read has no single owner, and
  pretending otherwise invites the ownership rule to be quietly broken.
- **The round must not block on another service.** ADR-0003 places the planner at
  the consistent end of the CAP tradeoff deliberately. A serialised component
  that also has a synchronous fan-out has two availability problems, not one.
- **Out-of-order arrival is a fact to absorb, not an error to report.** The
  system's stated position is that handlers tolerate out-of-order arrival using
  `occurred_at` and state-machine guards.
- **Never change a published event schema in place** (CLAUDE.md). Any answer that
  needs a new field in an existing event is really a v2, with the migration cost
  that implies.
- **Project what is decided with, not everything that exists.** Every projected
  column is a column that can drift from its source.

## Considered options

1. **Cross-schema read** — the planner `SELECT`s `tasking.tasking_requests`
   directly during a round.
2. **Enrich the opportunities event** — publish
   `feasibility.opportunities.computed.v2` carrying bid, tier and deadline
   alongside the geometry.
3. **Synchronous call** — the planner asks `tasking-api` over HTTP at round time,
   behind a circuit breaker.
4. **Project into the planning schema** — the planner consumes
   `tasking.request.received.v1` into `planning.request_snapshots` and
   `feasibility.opportunities.computed.v1` into
   `planning.candidate_opportunities`, then reads only its own schema.

## Decision outcome

Chosen: **Option 4**, because it is the only one that leaves the round reading a
single schema inside a single transaction with no external dependency, which is
what "serialised, strongly consistent, single-writer" has to mean if it means
anything. It follows the autonomy driver directly: the planner ends up owning
every table it reads, so the ownership rule is enforced by the schema layout
rather than by everyone remembering it.

The cost is real and is not hidden: two projections to keep correct, and a
staleness window between a request being accepted and the planner knowing about
it. Both are priced below a synchronous dependency inside the allocation lock.

### Two sub-decisions that carry most of the weight

**There is no foreign key from `candidate_opportunities.request_id` to
`request_snapshots.request_id`, and its absence is deliberate.** This is the line
most likely to be "fixed" by someone tidying up, so it is worth stating why. The
events race. A foreign key would reject a candidate whose request has not been
projected yet; the consumer would nack; the batch would redeliver until the other
stream happened to catch up. That converts a benign ordering fact into a retry
storm, and it does so under exactly the load that makes ordering skew likely.

Instead the join happens at read time, and a candidate with no snapshot is
**held**, not dropped. It has no value and no deadline, so allocating it would be
guessing — but a later round finds it once the snapshot lands. Out-of-order
tolerance belongs in a state-machine guard here, not in a constraint.
`scripts/db-invariants.sh` phase 3 builds the foreign-key version as a mutant and
requires it to reject the out-of-order arrival, so the absence is demonstrated
load-bearing rather than asserted in a comment.

**The projection is deliberately thin.** `planning.request_snapshots` carries
`customer_id`, `priority_tier`, `bid_credits`, `request_window` and
`submitted_at` — five columns, each with a named consumer:

| Column | Why the planner needs it |
| --- | --- |
| `customer_id` | `planning.acquisitions.customer_id` is `NOT NULL` and no opportunity carries a customer |
| `priority_tier`, `bid_credits` | the allocation objective |
| `request_window` | the deadline check, and it is **not** redundant — see below |
| `submitted_at` | the origin of the fairness ageing factor (M2-09) |

The deadline deserves the caveat because it looks redundant and is not.
Feasibility clamps its search horizon to the request window, so every candidate
*starts* in time by construction. But an acquisition occupies
`acquisition_duration_s` from whatever start the planner chooses, and one that
begins before `upper(request_window)` can still finish after it. The contract is
explicit that such an acquisition has no value and is infeasible rather than
late. That check has no other home: feasibility does not know the start the
planner will pick.

What is **not** projected, and why:

- **`constraints`** (`AcquisitionConstraints`) — the customer's narrowing of
  acceptable geometry. Feasibility already applied it when generating the
  candidates. Re-applying it here would put one rule in two services, which is
  how two services come to disagree.
- **`requested_modes`** — same reason. Feasibility emits candidates only in
  requested modes, and mode admissibility is not a term in the objective.
- **Per-batch metadata** (`truncated`, `horizon`, `tle_references`,
  `satellites_evaluated`) — real provenance, but per-event rather than
  per-candidate, so projecting it means a third table. Deferred, with the cost
  named in Confirmation below.

## Consequences

**Good**

- The round reads one schema. No cross-service call, no cross-schema join, no
  second failure mode inside the advisory lock.
- Every table the planner reads is a table the planner owns, so ADR-0013's
  ownership rule holds structurally instead of by convention.
- Out-of-order arrival degrades into latency rather than into errors: a held
  candidate is simply allocated a round later.
- The planner survives `tasking-api` being down. It plans against what it has
  already been told, which is the availability posture ADR-0003 assigns it.

**Bad**

- Two more projections to keep correct, and two more idempotent consumers to
  write. Both are the same shape as the ones that already exist, which lowers the
  cost but does not remove it.
- A staleness window: between `tasking-api` accepting a request and the planner
  projecting it, that request is invisible to allocation. Bounded by outbox relay
  latency, but real, and it means "submitted" and "eligible for planning" are
  different instants.
- Duplicated storage. The bid lives in `tasking.tasking_requests` and again in
  `planning.request_snapshots`. If the tasking write model ever mutates a bid in
  place, the projection is wrong until the corresponding event arrives — and no
  such event exists today.
- The thin projection is a bet. Every field left out is one that a later issue may
  discover it needed, at the cost of a migration and a backfill.

**Neutral**

- Audit trails still cross schemas, and that is fine: an acquisition carries
  `opportunity_id`, so tracing it back to the TLE that produced it goes through
  `feasibility.opportunities`. Cross-schema reads for *forensics* are a different
  activity from cross-schema reads inside a transaction, and only the second one
  is what this ADR rejects.
- `orbit_number` is nullable on `candidate_opportunities` because the contract
  does not require it on an opportunity. That is not this decision's doing — it
  is inherited — but it lands on M2-03, which must decide whether a candidate
  with no orbit number is skipped or charged to a synthetic bucket.

### Confirmation

This decision is wrong if any of the following happens, and each is observable:

- **Held candidates stop resolving.** A gauge of candidates whose snapshot has
  not arrived after some multiple of expected relay latency should sit near zero.
  If it does not, the ordering assumption is wrong and the projection needs an
  explicit reconciliation path rather than patience.
- **M2-15 cannot explain an unfulfilled request.** If the structured unfulfilment
  reasons turn out to require `truncated` or `tle_references`, the projection was
  cut too thin and the per-batch table has to be added. That is the specific,
  named way this ADR gets amended.
- **The staleness window shows up in a test.** M1-18's out-of-order integration
  test should demonstrate a candidate arriving first and being planned correctly
  afterwards. If it cannot be made to pass without ordering guarantees, Option 4
  is not actually absorbing the race.
- **The mutant stops failing.** `make db-test` phase 3 must keep showing that the
  foreign-key version of the schema rejects an out-of-order candidate. If that
  mutant ever passes, the assertion has stopped proving anything.

## Pros and cons of the options

### Option 1 — Cross-schema read

- Good, because it is the least code and the data is guaranteed fresh.
- Good, because there is no projection to drift.
- Bad, because it breaks the ownership rule structurally, not just stylistically:
  `tasking` gains a reader it does not know about, and the tasking track can no
  longer change its write model without breaking the planner.
- Bad, because it puts another schema's locks and another service's write load
  inside a transaction that is already serialised per satellite.
- Bad, because it makes the planner's availability a function of the tasking
  write model's availability, which is the opposite of the position ADR-0003
  assigns each of them.

### Option 2 — Enrich the opportunities event

- Good, because the planner would need exactly one consumer and no join.
- Bad, because it is a v2 of a published schema, with both bindings, the drift
  gate and the golden tests to carry along.
- Bad, because it makes a pure geometric service carry auction data. Feasibility
  would have to read the bid to publish it, which is a dependency it currently
  does not have and has no reason to want.
- Bad, because bid and tier can change independently of geometry. Binding them
  into the same event means either recomputing geometry to change a bid, or
  publishing an event whose geometry half is stale — and neither is defensible.

### Option 3 — Synchronous call at round time

- Good, because the data is fresh and nothing is stored twice.
- Good, because the dependency is explicit rather than implied by a schema.
- Bad, because it is a synchronous fan-out inside the advisory lock. A slow
  response does not degrade the round, it holds the lock open for the satellite.
- Bad, because a circuit breaker does not rescue this. Open, the round allocates
  without value data, which is worse than not allocating; closed and slow, it is
  the stall above. There is no fallback that is both available and correct.
- Bad, because it reintroduces request-response coupling into the one path the
  whole event-driven architecture exists to keep asynchronous.

### Option 4 — Project into the planning schema (chosen)

- Good, because the round reads one schema, inside one transaction, with no
  external dependency.
- Good, because it is the same idempotent-consumer shape already built twice in
  this repo, so it is boring in the way CLAUDE.md asks for.
- Good, because holding a candidate is a strictly better failure mode than
  rejecting it: the information is retained and the round is retried.
- Bad, because of the staleness window and the duplicated storage, both listed
  above.
- Bad, because "which fields to project" is a judgement call that has to be made
  before the consumers exist, and getting it wrong costs a migration.

## More information

- `db/migrations/00006_planning_inputs.sql` — the two tables, with the reasoning
  for each column at the column.
- `scripts/db-invariants.sh` phase 3 — mutant D, the foreign-key schema, shown
  rejecting an out-of-order candidate.
- [ADR-0003](0003-consistency-boundaries-and-cap-position.md) — why the planner is
  the strongly-consistent component, which is what rules out Option 3.
- [ADR-0012](0012-retain-superseded-acquisitions.md) — the storage half of
  supersession; this ADR is the input half of the same service.
- [ADR-0013](0013-parallel-agent-execution-in-worktrees.md) — the ownership rule
  that Option 1 breaks, and the reason this change was made in the main session
  rather than in a track.

**An open question this work surfaced but did not decide.** `AccessGeometry`
requires `incidence_angle_deg`, `look_side`, `squint_angle_deg`,
`slant_range_km` and `elevation_angle_deg`, but `roll_angle_deg` and
`ground_azimuth_deg` are optional. The slew model (M2-02) is a function of
attitude, so a contract-valid opportunity may not carry the field the slew model
most wants. `candidate_opportunities.geometry` stores the blob verbatim and takes
no position; M2-02 has to either derive roll from incidence and look side, or
make the case for tightening the contract. Recording it here so it is a decision
someone makes rather than an assumption someone inherits.

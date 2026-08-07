# 0012 — Retain superseded acquisitions with a status, and make the non-overlap constraint partial and deferred

- **Status:** accepted
- **Date:** 2026-08-07
- **Deciders:** Mhayk Whandson

## Context and problem statement

A horizon bucket can be planned more than once. Rounds fire on either a cadence
timer or an opportunity-arrival debounce, so `planning.plan.committed.v1` carries
`plan_version`, incrementing per `(satellite_id, bucket_start)`, and
`supersedes_plan_id` pointing at the plan replaced. That much was frozen in M0.

What M0 did not settle is what happens to the *rows*. M1-01 has to create the
constraint that the whole architecture rests on:

```sql
EXCLUDE USING gist (satellite_id WITH =, acq_window WITH &&)
```

Written that way, it is correct for a system that plans each bucket once and
wrong for this one. Re-planning a bucket means inserting v2's acquisitions while
v1's are still in the table, occupying overlapping times on the same satellite —
so the constraint fires against the very plan being replaced, and the commit
transaction fails. The invariant defends the table against the system that owns
it.

This is not hypothetical and it is not deferrable to M2. The constraint is written
in M1-01, it is the first thing a reviewer looks at, and its shape determines
whether plan history exists at all.

The question: **when a plan is superseded, what happens to its acquisitions, and
what shape must the non-overlap invariant take so that re-planning is possible
without weakening it?**

## Decision drivers

1. **The invariant stays in the database.** [ADR-0003](0003-consistency-boundaries-and-cap-position.md)
   and [ADR-0004](0004-postgresql-jsonb-over-document-store.md) both rest on the
   claim that no code path, migration, or manual `INSERT` can produce overlapping
   acquisitions. Any option that moves the check into application code loses.
2. **`SUPERSEDED` has to be explainable.** `planning.request.unfulfilled.v1`
   carries a `SUPERSEDED` reason code, and the "why was my request rejected?"
   panel is the product's centrepiece. Telling a customer their acquisition was
   replaced, while having deleted the evidence of what replaced it, is not an
   explanation.
3. **Plan commit is one transaction.** The acquisitions and the outbox row are
   written together, so the event exists if and only if the plan does. Whatever
   happens to the old rows happens inside that same transaction.
4. **The schema must not depend on statement order that nothing enforces.** The
   planner is the most heavily generated component in the system and its commit
   path will be refactored. An implicit ordering contract is exactly the kind of
   thing that survives review and breaks later.
5. **Storage growth is real but bounded**, and cheaper than the alternative. This
   is a driver, but the weakest one, and it does not decide anything by itself.

## Considered options

1. **Delete superseded acquisitions** inside the commit transaction
2. **Retain them with a status**, and make the exclusion constraint partial over
   the live set
3. **Add `plan_id` to the constraint key**
4. **Move superseded rows to a separate history table**

## Decision outcome

Chosen: **Option 2 — acquisitions carry a status, superseded rows are retained,
and the exclusion constraint is partial over the live set and declared
`DEFERRABLE INITIALLY DEFERRED`.**

Retention is what makes the `SUPERSEDED` reason code answerable, and the reason
code is already in a frozen contract — the schema has to be able to serve what the
event promises. Deleting the rows would leave the system able to say *your
acquisition was replaced* and unable to say *by what*, which is the difference
between this project's unfulfilment story and every other scheduler's.

Making the constraint partial is what makes retention possible without weakening
the invariant. Over the live set the guarantee is exactly what it was: no two
overlapping acquisitions on one satellite, enforced by Postgres, unbypassable.
Superseded rows are not scheduling claims, they are history, and history does not
contend for sensor time.

### The deferral is not cosmetic

Verified against `postgis/postgis:16-3.4` (PostgreSQL 16.4) before this ADR was
written, because the repository's first hard-won rule is that library behaviour
gets run, not asserted. Nine cases:

| # | Case | Result |
| --- | --- | --- |
| 1 | Partial `EXCLUDE ... WHERE (status = 'ACTIVE')` creates | accepted |
| 2 | First live acquisition | accepted |
| 3 | Overlapping **live** row, same satellite | **rejected** |
| 4 | Overlapping **superseded** row | accepted — history survives |
| 5 | Same window, different satellite | accepted — per-satellite, not global |
| 6 | Supersession, demote-then-insert | accepted |
| 7 | Supersession, **insert-then-demote**, immediate constraint | **rejected** |
| 8 | Same, `DEFERRABLE INITIALLY DEFERRED` | accepted |
| 9 | Genuinely conflicting plan, deferred constraint | **rejected at `COMMIT`** |

Case 7 is the finding. With an immediate constraint, the plan-commit transaction
acquires a mandatory statement order — demote v1 before inserting v2 — that lives
nowhere: not in a contract, not in a test, not in a comment. A future refactor
that reorders those two statements breaks plan commit **only on the supersession
path**, which is the least-exercised path in the system and the one that appears
under load rather than in a unit test.

Case 9 is what stops the deferral being a shortcut. Deferring changes *when* the
check runs, not *whether* it runs: a genuinely conflicting plan is still rejected,
at `COMMIT` instead of at the statement. The invariant is intact; only its timing
moved.

### What this ADR does not decide

The planner-side semantics of re-planning — when a round fires, how the debounce
interacts with the cadence timer, what happens to a request that held an
acquisition in a plan now replaced, and whether re-planning may ever be partial —
are M2 decisions and are not settled here. They get their own ADR (0014) when the
constraint is actually felt, rather than being guessed at now. This ADR settles
the storage model, because that is what M1-01 cannot proceed without.

### Consequences

**Good**

- The `SUPERSEDED` reason code becomes answerable from the database, so the
  unfulfilment explanation is backed by retained evidence rather than by a
  message string.
- The invariant remains database-enforced and unbypassable over the live set.
  ADR-0003's central claim survives re-planning intact.
- The plan-commit transaction has no ordering requirement, so the planner's commit
  path can be refactored without a latent supersession-only failure.
- Plan history is queryable, which makes the read model's versioned plan reads
  (#78) possible and gives M4 something real to render as ghost candidates.
- The projection is append-only, which is the easier side of the eventual
  consistency tradeoff to reason about and to test.

**Bad**

- **The invariant is now conditional, and that is a genuine qualification of
  ADR-0003.** "No overlapping acquisitions, enforced by the database" becomes "no
  overlapping acquisitions *in the live set*". Anyone reading the constraint has
  to also read the predicate to know what is guaranteed. A wrong predicate is an
  invariant that looks correct and is not, and no amount of GiST protects against
  that.
- Violations surface at `COMMIT` rather than at the offending statement, so error
  attribution is weaker. Accepted deliberately: a hidden ordering contract is
  worse than a less precise error message.
- `acquisitions` grows without bound in a system that re-plans frequently. No
  retention policy is defined here, and one will eventually be needed.
- Every query against live acquisitions must carry the predicate. Forgetting it
  reads superseded rows as though they were scheduled, and that mistake is silent
  and plausible-looking — precisely the failure mode
  [ADR-0010](0010-test-strategy-and-coverage.md) exists to catch.

**Neutral**

- The event contracts are unaffected. `plan_version` and `supersedes_plan_id` were
  already frozen in M0; this decides how the rows behind them are stored.
- Whether the live set is `status = 'ACTIVE'` or `status <> 'SUPERSEDED'` is left
  to M1-01. It is a real question — an `EXECUTED` acquisition consumed sensor time
  that actually happened and should almost certainly keep blocking overlap — but
  it is a predicate detail, not a change of model.

### Confirmation

- **The invariant:** raw SQL, bypassing all application code, cannot insert two
  overlapping live acquisitions on one satellite. This is M1-01's load-bearing
  test and it is demonstrated failing before it passes.
- **Retention:** an overlapping superseded row is accepted, and a plan read at an
  older `plan_version` returns the acquisitions that plan contained.
- **No ordering contract:** a supersession transaction in either statement order
  commits, and a genuinely conflicting plan is rejected in both.
- **The predicate is not forgotten:** property-based tests over generated plans
  (M2-12) assert that no live plan contains overlapping acquisitions, reading
  through the same query path the planner uses.
- **The decision is wrong** if retained history is never actually read. If the
  read model ships without versioned plan reads and the "why did I lose?" panel
  answers `SUPERSEDED` without citing the replacing plan, then the storage cost
  bought nothing and deleting was the right call all along.

## Pros and cons of the options

### Option 1 — Delete superseded acquisitions in the commit transaction

- Good, because the constraint stays total and unqualified, and the strongest
  possible version of ADR-0003's claim survives: no predicate to read, nothing to
  forget in a query.
- Good, because the table stays small and no retention policy is ever needed.
- Bad, because it makes the `SUPERSEDED` reason code unexplainable. The contract
  promises the customer an account of why they lost, and the account is deleted in
  the same transaction that creates the need for it.
- Bad, because it is destructive and irreversible in a system whose entire pitch
  is that it explains its decisions.

### Option 2 — Retain with a status, partial constraint (chosen)

- Good, because retention and the invariant coexist, as demonstrated above.
- Good, because it makes plan history a first-class queryable thing rather than a
  reconstruction from an event log.
- Bad, because the invariant becomes conditional on a predicate, and the predicate
  is now a correctness surface of its own.
- Bad, because unbounded growth is deferred rather than solved.

### Option 3 — Add `plan_id` to the constraint key

- Good, because it makes the collision disappear with a one-word change and no
  status column.
- **Bad, because it destroys the invariant.** Scoping the exclusion to a single
  plan means two live plans may overlap freely, which is exactly the thing the
  constraint exists to prevent. It is listed because it is the tempting option: it
  makes the failing test pass, the constraint still *looks* like a real GiST
  exclusion, and the system quietly loses its central guarantee.

### Option 4 — Move superseded rows to a separate history table

- Good, because the live table keeps a total, unqualified constraint while history
  is still retained — arguably the best of options 1 and 2.
- Bad, because the move happens inside the hot plan-commit transaction, adding a
  delete plus an insert per superseded acquisition to the path that must stay
  atomic and fast.
- Bad, because an acquisition's identity would move between tables, so anything
  referencing it — unfulfilment records, executed-acquisition events, the read
  model — needs to resolve across both. Two places to look is two places to forget.
- Bad, because the schemas must be kept in lockstep across every future migration,
  and drift between them would be silent.
- Genuinely close to option 2, and reasonable people would choose it. It loses on
  the reference-resolution cost, not on the storage model.

## More information

- Supersession fields and the definition of conflict-free:
  `contracts/events/planning.plan.committed.v1.schema.json`
- The reason code this retention exists to serve:
  `contracts/events/planning.request.unfulfilled.v1.schema.json`
- The claim this qualifies: [ADR-0003](0003-consistency-boundaries-and-cap-position.md)
- Schema and migrations that implement it: M1-01 (#13)
- Read-API consequences, including versioned plan reads: #78
- Planner-side re-planning semantics, still open: ADR-0014 (M2)

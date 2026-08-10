# 0021 — The execution simulator computes drift rather than rolling for it

- **Status:** accepted
- **Date:** 2026-08-11
- **Deciders:** Mhayk Whandson

## Context and problem statement

M4-06 closes the request lifecycle by introducing the one thing a planner cannot
control: reality diverging from the plan. `acquisition.executed.v1` and the
`simulator-executor` durable consumer have existed since M1; nothing produced or
drained them, which is why that consumer sat with 44 pending messages.

The contract names seven failure reasons and singles one out:

> `TLE_DRIFT_MISS` is the domain-specific one and the reason it is worth
> simulating: the plan was computed against an element set that had drifted, the
> satellite was not quite where the propagation said it would be, and the target
> fell outside the swath.

The question: **what does the simulator have to actually compute for that
sentence to be true, rather than merely asserted?**

## Decision drivers

- **A simulator that rolls dice proves the dice work.** The acceptance criterion
  is that `TLE_DRIFT_MISS` correlates with staleness at plan time. A probability
  that rises with age satisfies the letter of that and demonstrates nothing: the
  correlation would be the formula restating itself.
- **It has to close a loop that is already open.** `tle_epoch` is tracked, ages
  are exported as a metric, and `StalenessPolicy` refuses at 72 hours. Nothing
  in the system shows what a too-loose threshold *costs*. That is the whole
  value of this milestone item.
- **A demo cannot wait for a pass.** Acquisitions are scheduled hours ahead. A
  simulator that executed them at their real time would produce nothing during a
  five-minute demo.
- **Every publish goes through the outbox, every consumer is idempotent.** This
  service is not exempt because it is a simulator.

## Considered options

1. **Compute the miss from two real element sets** — propagate the planning
   observation and a later one to the acquisition instant and compare.
2. **Model it probabilistically** — `P(miss)` as an explicit function of element
   set age.
3. **Perturb the planning elements** — synthesise a "truth" set by adding an
   invented along-track error.

## Decision outcome

**Option 1.** The repository now carries two genuine Celestrak observations of
the same nine spacecraft, three days apart (#211). The planning snapshot is what
the planner computed against; the later one is where the satellites actually
were. The separation between them is what tracking recorded — orbit
determination updates, drag, and in a few cases a manoeuvre.

Two properties fall out of the data rather than being configured into it:

- **Separation grows with the planning set's age.** Measured: everything under
  6 km is ~90 hours old; every case past 98 hours is tens to hundreds of km.
- **Narrow swaths miss first.** SPOTLIGHT's half-swath is 2.5 km, STRIPMAP's 15,
  SCAN's 50. A stale element set costs the high-resolution modes before it costs
  anything else.

That second property is the one worth having. It is not something anyone chose;
it is what the geometry does, and it is exactly the physical consequence of a
loose staleness threshold.

Option 3 was rejected for a reason worth recording, because it was tried first
in a weaker form. Deriving a truth set by rewriting the fixture's epoch is not
drift: `rebase_epoch` carries mean anomaly over unchanged, so shifting an epoch
moves the satellite *along its orbit* by the shift. A six-hour shift on a
95-minute orbit separated the two propagations by 700 to 13,000 km,
non-monotonic with age — a different point on the orbit, not a drifted one.
Built on that, `TLE_DRIFT_MISS` would have fired at random while looking like
physics. Measured before anything was built on it.

Option 2 remains the model for the **other six** failure reasons, and that
asymmetry is deliberate rather than lazy. `ATTITUDE_ERROR`, `SENSOR_FAULT`,
`THERMAL_LIMIT` and their siblings are spacecraft-internal events this system
has no state for; inventing a thermal model to roll against would be a
simulation of a simulation. They are injected at configurable rates from a
seeded generator, so a run is reproducible and a demo can be told to produce
one. `TLE_DRIFT_MISS` is the one the system has the information to compute, so
it is the one that is computed.

### Consequences of the design, stated

**The truth element set must be invisible to planning**, or feasibility would
plan against it and there would be no drift to find. It is: the seeder places
truth epochs in the future, and `newest_element_sets` selects
`WHERE epoch <= at ORDER BY epoch DESC`. No change to feasibility was needed,
which is why the two-snapshot arrangement lives in the seeder.

**Execution is compressed, not scheduled.** The simulator executes an
acquisition when the plan commits, not when the window opens. A demo that waited
for a real pass would show nothing, and the alternative — a scheduler holding
acquisitions for hours — is a second timing system to get wrong for no
demonstrative gain. `actual_window` is derived from `scheduled_window` with
drift applied, so plan-versus-actual remains measurable; what is simulated away
is the waiting, not the divergence.

**The target comes from the event's footprint, not from a database lookup.** The
committed acquisition carries its footprint, and its centroid is the target the
plan was built around. Reading `tasking.requests` instead would put this service
across another service's schema boundary to recover something the contract
already hands it.

## Consequences

**Good.** The staleness metric gains a visible physical consequence, and the
mode gradient — SPOTLIGHT failing while SCAN succeeds on the same pass — is a
result nobody configured. Failure injection stays honest about which failures
are computed and which are asserted.

**Bad.** A fifth service to build, deploy and keep in CI, and a second frozen
TLE fixture to keep in step with the first. The drift is fixed by the data: two
snapshots give one set of separations, so the *distribution* of misses is not
tunable the way an injected rate is. That is the price of it being real.

**Also.** Every non-drift outcome is a knob, and a knob nobody documents is a
knob that gets set wrong. The rates and the seed are configuration with stated
defaults, and the seed is logged at startup so a surprising run can be
reproduced.

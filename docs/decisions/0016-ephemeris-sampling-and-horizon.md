# 0016 — Sample the ephemeris every ten seconds, in aligned three-hour buckets, over a rolling day

- **Status:** accepted
- **Date:** 2026-08-08
- **Deciders:** mhayk

## Context and problem statement

`/v1/geo/plans/.../czml` served acquisition footprints and a clock and no
satellite path. [#27](https://github.com/mhayk/overpass/issues/27) declined to
fake one — interpolating a curve through committed footprint centroids draws
something that looks like an orbit and is not one, which is worse than an absent
layer because a viewer would believe it — and that decision stands. The
consequence was that the orbit-track half of M1's globe could not be built at
all: there were no positions in the system to draw.

[#128](https://github.com/mhayk/overpass/issues/128) settled the architecture:
feasibility propagates and publishes, plan-gateway projects and renders. That
left one question the issue explicitly refused to answer in code, and it is a
domain question rather than a coding one:

> **How finely, and how far ahead, should a satellite's position be sampled?**

It is not a free choice in either direction. A LEO SAR satellite moves at about
7 km/s and its ground track at about 6.6 km/s, so a sixty-second sample is
roughly 400 km from its neighbour. Cesium interpolates between whatever samples
it is given; across a gap that size the drawn curve visibly cuts the corner
wherever the track bends hardest — which, for the sun-synchronous constellation
this system seeds, is over the poles, where it spends most of its time. Sampling
every second would be beyond argument on fidelity and is a payload violation
before it is anything else. And ephemeris is by far the largest thing the globe
will ever load, so the answer is a size decision as much as a fidelity one.

## Decision drivers

- **The path has to be right where it is most looked at.** A track that is
  accurate over the equator and visibly wrong over the pole is worse than one
  that is uniformly approximate, because the error appears exactly where a
  reviewer's eye goes.
- **The payload budget is a gate, not a guideline.** `docs/payload-budget.md` is
  generated from a measurement and enforced by a test. Whatever is chosen has to
  live inside a ceiling somebody can read.
- **Idempotence of a rolling sweep.** The producing sweep runs on a timer and
  necessarily re-covers ground it already covered. Whatever the horizon is, the
  same bucket has to be recognisable as the same bucket across ticks, or every
  tick republishes the whole horizon.
- **One query per rendering.** The CZML endpoint renders one plan over one
  bucket. If ephemeris buckets and plan buckets are different sizes, every
  rendering stitches.
- **Honesty about what is measured.** The interval could not be defended by
  assertion. It had to be measured at more than one value.

## Considered options

1. **10 s samples, 3-hour aligned buckets, 24-hour rolling horizon**
2. **60 s samples**, same bucketing — a sixth of the payload
3. **1 s samples** — beyond argument on fidelity
4. **No fixed interval: adaptive sampling, denser where the track curves**

## Decision outcome

Chosen: **option 1 — ten-second samples, three-hour buckets aligned to a fixed
UTC grid, a twenty-four-hour rolling horizon.**

Ten seconds because it is the coarsest interval whose interpolation error is
invisible where it matters, and because the measurement says the cost is
affordable: a three-hour track at ten seconds adds **36,138 bytes** to a plan
document, against 12,092 at thirty seconds and 6,125 at sixty. Six times the
payload of the sixty-second option, for 36 kB — which is smaller than the
footprints already in the same document (41 kB for forty acquisitions) and a
tenth of a full footprints page. The lever that makes this affordable is not the
interval at all; it is the conditional request. A globe polls the same bucket
every few seconds and gets a 304 until the plan changes.

Three-hour buckets because that is the planner's allocation bucket, so rendering
one plan is one range scan rather than a stitch across boundaries. **Aligned** to
a fixed grid rather than relative to the sweep, because the published event id is
derived from `(satellite_id, bucket start, tle_epoch)` — an unaligned grid would
give every tick a new id for an overlapping track, and the outbox's unique
constraint would stop protecting anything.

Twenty-four hours because that is the span of the demo's request windows. It is
the cheapest of the three numbers to change and the one with the least riding
on it.

All three are configuration — `EPHEMERIS_INTERVAL_S`, `EPHEMERIS_BUCKET_S`,
`EPHEMERIS_HORIZON_S` — with these as defaults, because the number that is right
for a demo globe is not necessarily right for a deployment.

### Consequences

**Good**

- The orbit track is drawn from propagated positions, so it is the orbit rather
  than a plausible curve. The provenance travels with it: every published track
  names the element set it came from.
- A rolling sweep is idempotent by construction rather than by bookkeeping. No
  "what have I already published" table exists, because the derived event id and
  the outbox's unique constraint answer the question.
- The steady-state sweep does no physics. Ids are derived without propagating, so
  a tick that uncovers no new bucket costs one `SELECT`.
- The read model is a row per sample, so a plan whose bucket does not align with
  an ephemeris bucket still renders correctly — the query is a range scan, not a
  blob lookup.

**Bad**

- **104,000 rows per day** in `readmodel.ephemeris` for twelve satellites, and
  about 4 MB of event payload for a full day's horizon. Both are comfortable and
  both are new; neither existed before this decision.
- **36 kB on a cold plan document**, which nearly doubles it. Behind a warm ETag
  this is paid once; a client that revalidates poorly pays it repeatedly.
- **A tick that uncovers a bucket does real work** — twelve satellites × 1,080
  instants of SGP4. It runs off the event loop for that reason, and it is a
  visible CPU spike every three hours rather than a smooth load.
- A stale element set produces a track that is drawn anyway (see below), so the
  globe can show an orbit that is hours out of date. It states its own staleness
  on the event; nothing in the UI surfaces that yet.

**Neutral**

- Sampling refuses nothing on staleness, deliberately unlike `evaluate()`, which
  refuses to compute access windows from a STALE TLE. The asymmetry is the
  point: an access window is a commitment the planner acts on, and a confidently
  wrong one is worse than none; a track is a drawing that carries its own age,
  and refusing to draw it empties the globe on the fourth day of a demo for a
  reason no viewer can see.

### Confirmation

Three things would falsify this, and each has somewhere to show up:

- **The budget test.** `maxCZMLBytesPerEphemerisSample` (36) and
  `maxPlanWithTrackBytes` (85 kB) are enforced in
  `services/plan-gateway/internal/render/budget_test.go`, and the table in
  `docs/payload-budget.md` is regenerated from the same measurement. A change
  that makes a sample materially larger fails the build. Demonstrated failing:
  removing the six-decimal rounding takes a sample from 33 to 49 bytes and the
  gate fires.
- **Visible interpolation error.** If the drawn path departs from the sampled
  positions at high latitude — which is what the LAGRANGE interpolation and the
  ten-second spacing exist to prevent — the interval is too coarse and this
  decision was wrong on fidelity. The check is a screenshot over a pole, and it
  is not automated.
- **Sweep cost.** If a tick's propagation stops fitting comfortably inside the
  tick interval as the constellation grows, the horizon is too long or the
  interval too fine. `overpass.ephemeris.propagated` is on the sweep span for
  exactly this.

## Pros and cons of the options

### Option 1 — 10 s

- Good, because interpolation error is invisible at the latitudes where the
  track curves hardest, which is where the constellation spends its time.
- Good, because 36 kB is smaller than the footprints already in the same
  document, so it does not change the shape of the payload problem.
- Bad, because it is six times option 2 for an improvement that is invisible over
  most of the globe.
- Bad, because 104k rows a day is real storage for a projection.

### Option 2 — 60 s

- Good, because 6 kB is nearly free and the row count drops to 17k a day.
- Bad, because 400 km between samples is visibly wrong at the poles. This is the
  disqualifying one: the whole reason for refusing centroid interpolation in #27
  was that a wrong-looking curve is worse than no curve, and shipping a
  differently wrong curve would give that argument away.

### Option 3 — 1 s

- Good, because interpolation stops mattering at all.
- Bad, because it is 360 kB per bucket — larger than a full footprints page, for
  precision no viewer can resolve at any zoom the globe supports.

### Option 4 — adaptive sampling

- Good, because it spends bytes only where curvature demands them, and would
  plausibly beat a fixed 10 s on both axes.
- Bad, because the sample instants stop being derivable from the bucket, so the
  event has to carry them (it does — samples are self-describing) AND the
  downstream reasoning about resolution becomes per-track rather than global.
- Bad, because it is a second thing to get right in the highest-risk code in the
  project for output that is plausible and wrong, and the fixed interval is
  already inside budget. Rejected as unnecessary complexity, not as a bad idea —
  it is the obvious move if the horizon ever needs to be much longer.

## More information

- Issue [#128](https://github.com/mhayk/overpass/issues/128) — the architectural
  half of this decision (feasibility publishes, plan-gateway projects), and the
  open question this ADR answers.
- [ADR-0001](0001-polyglot-go-python-split.md) — why the propagation has exactly
  one home, which is what ruled out plan-gateway propagating in Go.
- [ADR-0009](0009-cesium-deckgl-division.md) — why the gateway renders both view
  formats from one read model, which is what ruled out feasibility serving CZML.
- [ADR-0011](0011-tle-sourcing-live-and-frozen.md) — the frozen snapshot the
  sweep propagates from in a demo.
- `docs/payload-budget.md` — the measurement, regenerated from the test.
- `contracts/events/feasibility.ephemeris.computed.v1.schema.json` — the event.
- `db/migrations/00008_readmodel_ephemeris.sql` — the projection.

# 0009 — Split the frontend between CesiumJS and deck.gl by question, not by taste

- **Status:** accepted
- **Date:** 2026-08-08
- **Deciders:** Mhayk Whandson

## Context and problem statement

The frontend has to answer two different questions about the same data, and they
pull in opposite directions.

The first is **"where is this satellite, what can it see, and when?"** That is a
question about a rotating ellipsoid, a time axis, and geometry that only makes
sense in three dimensions. An access window is a fact about where a spacecraft
is relative to a point on the ground at an instant; a swath is a shape projected
from an off-nadir look angle. Flattening any of it to a Mercator plane throws
away the thing being asked about, and near the poles — where a sun-synchronous
constellation spends a disproportionate share of its passes — the distortion is
not cosmetic. [ADR-0011](0011-tle-sourcing-live-and-frozen.md) already commits
this system to real SGP4 propagation; showing the result on a flat map would
discard most of what that buys.

The second is **"where is the demand, and where did requests collide?"** That is
a question about hundreds or thousands of footprints, targets and losing
candidates at once. It is answered by aggregation — density, overlap, clusters
of contention — and the answer is legible precisely because it is flattened.
A globe showing two thousand translucent polygons is a globe showing a smear.

M4 makes both of these load-bearing. M4-01 is a 2D planning view, M4-05 renders
losing candidates as ghosts against the winner, and M4-04 is the panel that
explains a rejection. Those are aggregation and comparison views. M1-16 and the
demo, meanwhile, are the globe — the thing that makes a reviewer believe the
orbital mechanics are real.

Two heavyweight visualisation libraries in one frontend is a cost that has to be
argued for rather than assumed, and this ADR is that argument.

**The question: which library renders what, and is the split worth its price?**

## Decision drivers

1. **Geometry must not be recomputed in the browser.**
   [Issue #27](https://github.com/mhayk/Overpass/issues/27) puts CZML and
   GeoJSON serialisation server-side for a reason: shipping raw ephemeris and
   letting the client derive footprints makes the physics wrong in a second
   place, and wrong differently in each view library. Whatever is chosen, both
   libraries must consume a *rendered* document.
2. **Time is a first-class axis, not a filter.** A plan is a schedule. Scrubbing
   through a bucket and watching acquisitions fire is the demo, and a library
   that treats time as "re-fetch with a different `where` clause" cannot do it
   smoothly.
3. **The conflict views are where the planner becomes visible.** The whole
   product claim is that the system *explains* its decisions. If the density and
   ghost-candidate views are weak, the claim is unsupported no matter how good
   the globe looks.
4. **Bundle size is a real budget, and this decision spends most of it.** Cesium
   is large. Adding a second renderer on top has to earn its place against that.
5. **One geospatial mental model per developer is already a lot.** Two rendering
   stacks means two coordinate conventions, two camera models, two ways to be
   subtly wrong about longitude order.

## Considered options

1. **CesiumJS alone** — globe for everything, including the 2D views
2. **deck.gl alone** — 2D layers for everything, no true globe
3. **Mapbox/MapLibre plus a hand-written globe** — a 2D map library and custom
   3D for the orbital view
4. **CesiumJS for the globe, deck.gl for the 2D analytical views** — split by
   question

## Decision outcome

**Option 4: split by question.** Cesium answers *where and when*; deck.gl
answers *how much and where do things collide*.

Concretely:

| View | Library | Why it belongs there |
|---|---|---|
| Orbit tracks, ground tracks, satellite position over a bucket | Cesium | Real WGS84 ellipsoid, real time dynamics, CZML consumed directly |
| Swath footprint for one acquisition, in context | Cesium | The shape only means something against the body it is projected onto |
| Timeline scrubbing across a plan | Cesium | Its clock drives the scene; time is the axis, not a filter |
| Coverage density over a window (M4-01) | deck.gl | Aggregation over many polygons is what it is built for |
| Contention clusters and losing candidates (M4-05) | deck.gl | Comparing many overlapping shapes needs a flat plane to be legible |
| The rejection-explanation panel's map (M4-04) | deck.gl | One target, its competitors, and the winner — a comparison, not a place |

The split is enforced at the data boundary rather than by convention. The
gateway serves **CZML to Cesium** and **GeoJSON to deck.gl**, from the same read
model, and neither library ever sees the other's format. That is what stops the
split from becoming two divergent interpretations of the same rows: if a
footprint disagrees between the two views, exactly one serialiser is wrong and
it is server-side, testable, and covered by golden files.

### What this decision does NOT permit

- **No deck.gl overlay on the Cesium globe.** deck.gl can render onto Cesium,
  and it is tempting, and it is the version of this decision that gets all of
  the cost and none of the clarity — two renderers fighting over one depth
  buffer, and a reviewer who cannot tell which library drew what.
- **No geometry computed client-side in either.** Both consume rendered
  documents. If a view needs a shape the gateway does not serve, the gateway
  gains an endpoint.
- **Cesium is not loaded on routes that do not show a globe.** It is the larger
  half of the bundle and the 2D views must not pay for it.

## Consequences

### Accepted costs

**Two rendering stacks.** Two camera models, two coordinate conventions, two
sets of performance characteristics. A developer fluent in one is not
automatically fluent in the other.

**Bundle size.** Cesium alone is the dominant asset in this frontend, and
deck.gl is not small. Route-level code splitting keeps the 2D views off the
Cesium payload, but the globe route is heavy and will stay heavy. M4-07 owns
measuring it; this ADR owns admitting it.

**Two places for longitude to be backwards.** Both libraries take
`[longitude, latitude]`, which is the same convention as
[RFC 7946](https://datatracker.ietf.org/doc/html/rfc7946) and the contracts —
but the failure is silent and relocates a target to another hemisphere. The
mitigation is that neither library receives raw coordinates; both receive
documents produced by one server-side serialiser with a contract test asserting
the ordering.

**A judgement call at every new view.** "Which library?" becomes a recurring
question. The table above is the answer, and a view that fits neither cleanly is
a signal the view is asking two questions at once.

### What this buys

The globe is credible and the analytics are legible, and neither compromises for
the other. That is the entire justification, and if either half turns out not to
be load-bearing, this ADR should be revisited rather than defended.

## Pros and cons of the options

### 1. CesiumJS alone

- **Good:** one library, one mental model, one bundle. The globe is
  world-class — genuine ellipsoid, genuine time dynamics, CZML as a first-class
  input format.
- **Good:** it can draw a 2D projection, so nothing is strictly impossible.
- **Bad, and decisive:** its aggregation story is weak. There is no real
  equivalent of a hexbin, heatmap or aggregated-polygon layer, and thousands of
  translucent footprints on a globe read as a smear rather than as density.
- **Bad:** the ghost-candidate view in M4-05 is a comparison of many overlapping
  shapes. On a sphere, with perspective, the comparison is harder to make and
  harder to screenshot.

This is the strongest rejected option, and it is worth being honest about why it
loses. It does not lose on the globe — it wins there outright. It loses because
the views where the planner's *reasoning* becomes visible are aggregation views,
and those are the views this product is actually about.

### 2. deck.gl alone

- **Good:** excellent at exactly the aggregation and comparison work M4 needs;
  much smaller; one mental model.
- **Good:** its GlobeView exists and renders a sphere.
- **Bad, and decisive:** GlobeView is a projection, not a body. There is no
  serious time-dynamic model, no CZML, and no path to scrubbing a schedule and
  watching acquisitions fire.
- **Bad:** it would put the orbital geometry back in the browser, or leave the
  system unable to show the one thing that makes the SGP4 work believable.

### 3. Mapbox/MapLibre plus a hand-written globe

- **Good:** a mature 2D map, full control over the 3D view, potentially the
  smallest bundle.
- **Bad, and decisive:** writing a correct time-dynamic orbital viewer is a
  project, not a task. Cesium is fifteen years of exactly that work.
- **Bad:** it moves geometry back into the browser, contradicting driver 1 and
  [issue #27](https://github.com/mhayk/Overpass/issues/27).
- **Bad:** for a portfolio project, hand-rolling a globe is the kind of effort
  that looks like misplaced priorities rather than engineering judgement.

### 4. CesiumJS for the globe, deck.gl for the 2D views — **chosen**

- **Good:** each question is answered by the library built for it.
- **Good:** the split is enforced by the data format rather than by discipline,
  so it cannot quietly erode.
- **Good:** it matches the milestone structure — M1-16 is the globe, M4-01 and
  M4-05 are the analytics.
- **Bad:** the largest bundle of the four options.
- **Bad:** two mental models, honestly.

## More information

- [ADR-0011](0011-tle-sourcing-live-and-frozen.md) — real propagation is what
  the globe exists to make visible
- [Issue #27](https://github.com/mhayk/Overpass/issues/27) — CZML and GeoJSON
  served from the read model, which is what makes this split enforceable
- [Issue #28](https://github.com/mhayk/Overpass/issues/28) — the Next.js shell
  and the Cesium globe
- [Issues #57 and #61](https://github.com/mhayk/Overpass/issues/57) — the deck.gl
  planning view and the ghost-candidate rendering
- [RFC 7946](https://datatracker.ietf.org/doc/html/rfc7946) — GeoJSON, longitude
  first
- [CZML structure](https://github.com/AnalyticalGraphicsInc/czml-writer/wiki/CZML-Structure)

**Revisit this if** the deck.gl views turn out not to be load-bearing — in which
case option 1 becomes correct and the bundle halves — or if Cesium gains a
credible aggregated-layer story.

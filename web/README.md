# web

Next.js App Router frontend. Server components by default; the globe and the
submission form are the only client islands.

## Running it

    npm install        # also copies Cesium's assets into public/cesium
    npm run dev

It reads two services, both overridable:

| Variable | Default | What it is |
|---|---|---|
| `NEXT_PUBLIC_GATEWAY_URL` | `http://localhost:8083` | plan-gateway, the read side |
| `NEXT_PUBLIC_TASKING_API_URL` | `http://localhost:8080` | tasking-api, the write side |

## Cesium

Not a normal npm package: it ships workers, WebAssembly and textures that must
be reachable at a known URL at runtime. `postinstall` copies them into
`public/cesium` and `CESIUM_BASE_URL` points there. A globe that renders a black
sphere and logs nothing is almost always this.

It is imported dynamically with `ssr: false` and a visible loading state, for
two reasons: it touches `window` at import time, and it is the largest asset in
the frontend by a wide margin. The page is interactive before it arrives.

## What the globe draws, and where each layer comes from

Three layers, two sources, and the split is deliberate.

**Orbit tracks** come from `/v1/geo/satellites/czml` and are handed straight to
Cesium's `CzmlDataSource`. Cesium's own loader is the only thing that should
interpret a CZML packet stream, and the server already renders it from the one
read model — re-modelling it here would be the second implementation of geometry
that [ADR-0009](../docs/decisions/0009-cesium-deckgl-division.md) exists to
prevent, and the two would disagree eventually. The document's clock is also what
makes scrubbing move satellites rather than merely move a cursor.

That endpoint is deliberately **independent of any plan**. The per-plan CZML
draws the orbit of the satellite its plan belongs to, which is right when a plan
is what you are looking at and wrong for a globe: the constellation exists before
the first plan is committed, and satellites that appear only once something has
been scheduled tell the viewer something false.

**Acquisition footprints** stay hand-built entities, because they need
per-request selection highlighting — mutating material and outline alpha on
entities this component owns. `CzmlDataSource` owns everything it loads, so
driving selection through it would mean rewriting packets and reloading the
document on every click.

**Candidate footprints** are fetched per selected request and drawn as unfilled
ghosts. The losers are the point: a winner shown alone explains nothing about why
it won.

An empty constellation is a legitimate state, not an error. The ephemeris sweep
runs on its own timer, so a window it has not reached is a globe without
satellites — and a failure fetching it must not blank the acquisitions that did
load.

What remains off the table: interpolating a path through footprint centroids. It
renders something that looks like an orbit and is not one, which is worse than an
absent layer because a viewer would believe it.

## Tests

`npm test` runs vitest over the API clients and the shaping around them. Cesium
itself needs WebGL, which jsdom does not have, so the globe is covered by
Playwright in M4-08 rather than pretended at here.

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

## What the globe does not draw yet

Satellites. The read model holds the acquisitions the planner committed, not the
ephemeris they were derived from, so there are no positions to render and no
path to scrub. Interpolating one through footprint centroids would draw
something that looks like an orbit and is not one — worse than an absent layer,
because a viewer would believe it. Tracked in #128.

## Tests

`npm test` runs vitest over the API clients and the shaping around them. Cesium
itself needs WebGL, which jsdom does not have, so the globe is covered by
Playwright in M4-08 rather than pretended at here.

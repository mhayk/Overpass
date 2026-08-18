# 0022 — One @cesium/engine, pinned by an npm override

- **Status:** accepted
- **Date:** 2026-08-18
- **Deciders:** Mhayk Whandson

## Context and problem statement

Enabling a basemap on the globe crashed rendering with

    DeveloperError: Width must be less than or equal to the maximum texture
    size (0). Check maximumTextureSize.

in a browser whose WebGL context reports a maximum texture size of 8192. The
same viewer, the same tileset and the same pattern worked in a standalone page
built from `Build/Cesium`.

The cause is the dependency tree, not the code. `cesium@1.124.0` declares
caret ranges — `@cesium/engine ^13.0.0`, `@cesium/widgets ^10.0.0` — and the
lockfile had resolved them to **two** engines:

    cesium@1.124.0
    ├── @cesium/engine@13.1.0
    └── @cesium/widgets@10.2.0
        └── @cesium/engine@14.0.0

`Viewer` (re-exported from widgets) creates the GL context in engine 14 and
writes its limits into engine 14's `ContextLimits` module singleton. The
`ImageryLayer` and `TileMapServiceImageryProvider` the application constructs
come from engine 13, whose `ContextLimits` still holds its initial zero. The
first imagery tile texture validates against the wrong copy and throws.

Nothing before the basemap ever created a texture through the engine-13 copy,
which is why a mixed tree shipped and rendered for weeks. The bug was latent
in the committed lockfile, not introduced by any code change.

## Decision

Pin `@cesium/widgets` to `10.1.0` — the newest release that declares
`@cesium/engine ^13.1.0` — with an npm `overrides` entry in `web/package.json`.
npm then dedupes both consumers onto a single `@cesium/engine@13.1.0`.

## Considered alternatives

- **Upgrade `cesium` to current.** The metapackage keeps caret ranges, so the
  same drift can recur at any future install; it also swaps the entire Cesium
  API surface to fix a packaging problem. Worth doing eventually, on its own
  schedule, with its own verification.
- **Override `@cesium/engine` itself to one version.** Forces widgets 10.2 to
  run against an engine major it does not declare support for. The override
  chosen constrains the package whose range actually caused the split, and
  every resolved version remains one its dependents declare.
- **Avoid constructing imagery types at the application layer** (route
  everything through `viewer.imageryLayers.addImageryProvider`). Duck-typing
  instances across two engine majors happens to work today and is exactly the
  kind of accident this repository avoids relying on.

## Consequences

- `npm ls @cesium/engine` must show a single deduped version; that is the
  invariant this override exists to hold.
- Removing the override is the first step of any future `cesium` upgrade, and
  the upgrade must re-verify that the tree resolves to one engine.
- The lockfile must be regenerated with the same npm major the web image uses
  (`node:22-alpine`, npm 10); a lockfile written by a newer npm failed
  `npm ci` inside the image while passing locally.

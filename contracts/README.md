# Contracts

**This directory is the source of truth.** Event schemas and the OpenAPI
documents are written and frozen *before* any service code exists, and the
services are built to them.

## Why contracts-first is load-bearing here, not ceremony

Two reasons, both structural:

**It is what makes the polyglot split affordable.** Go and Python cannot share a
type system ([ADR-0001](../docs/decisions/0001-polyglot-go-python-split.md)). The
only thing that keeps `feasibility-service` and `planner-service` agreeing about
what an Opportunity is, is a schema that generates into both languages with a CI
check that regeneration produces no diff. Without that, the two halves drift and
the drift is discovered at runtime, in production, at 3am.

**It is what makes parallel agent work safe.** Once the event schemas and the
OpenAPI documents are law, `tasking-api`, `feasibility-service`, and `web` can be
built concurrently in separate sessions with no merge collisions — because the
interface between them already exists. Without frozen contracts, parallel agents
produce integration debt faster than they produce features, and you spend the
time you saved reconciling three plausible-but-incompatible interpretations of
the same field. This is written up properly in
`docs/ai-engineering/00-methodology.md`.

## Layout

```
contracts/
├── common/      Shared definitions, $ref'd by everything
│   ├── envelope.v1.schema.json     event_id, correlation_id, causation_id, ...
│   ├── primitives.v1.schema.json   Uuid, Credits, PriorityTier, TimeWindow, ...
│   ├── geometry.v1.schema.json     GeoJSON subset: Point, Polygon
│   └── sar.v1.schema.json          ImagingMode, LookSide, AccessGeometry, TLE refs
├── events/      One file per event type, named exactly as its subject
├── openapi/     tasking-api.v1.yaml  (plan-gateway.v1.yaml lands in M1)
├── nats/        topology.md — streams, consumers, DLQ, trace propagation
└── examples/    Valid and invalid fixtures, driven by the contract test suite
```

## Rules

1. **Never edit a published schema in place.** Version it. A `.v2` event gets a
   new file and a new subject, so v1 and v2 consumers coexist.
2. **`schema_version` is semantic within a major line.** Adding an optional field
   is a minor bump. Removing a field, making an optional field required, or
   narrowing a type is a new major line.
3. **Every event carries the full envelope**: `event_id`, `event_type`,
   `schema_version`, `occurred_at`, `correlation_id`, `causation_id`, `producer`.
   A W3C `traceparent` travels in NATS headers, not in the body — it describes
   the transport, not the fact.
4. **Every event has at least one `examples` entry**, and CI validates it against
   its own schema. An example that does not validate is a schema that nobody has
   actually tried to use.
5. **CI validates every emitted event** against its schema in integration tests.
   The contract is a gate, not documentation.

### Why the envelope is repeated in each event instead of composed with `allOf`

Each event schema lists the seven envelope fields explicitly and `$ref`s the
*field definitions* in `common/envelope.v1.schema.json`, rather than composing
the whole envelope with `allOf`.

- **What it costs:** seven repeated lines per event file.
- **What it buys:** each event file is readable on its own, and both code
  generators stay on well-trodden paths. `go-jsonschema` and
  `datamodel-code-generator` both handle `$ref`-to-a-`$def` cleanly; both produce
  awkward output for `allOf` composition — usually embedded anonymous structs in
  Go and multiple-inheritance shims in Python.

Trading seven lines of duplication for generated code that reads like code
somebody wrote is a good trade, and it is the sort of small decision that would
otherwise silently become "the generator output is weird and nobody knows why."

### Why strict `additionalProperties: false`, and how consumers stay tolerant

Every schema sets `additionalProperties: false`. That is a **producer-side**
contract, enforced in CI and in integration tests: if a service emits a field
that is not in the schema, the build fails.

Consumers do **not** run schema validation at runtime. They deserialise into
generated structs, which ignore unknown fields by default. That is what keeps
the tolerant-reader property intact: a producer on `1.1.0` can add an optional
field and a consumer still on `1.0.0` will ignore it rather than reject the
message.

Strict producers, tolerant consumers. Getting this backwards — validating
strictly on the consumer side — is how a minor, backwards-compatible schema
change takes down half a system.

## Code generation

Generated code is **checked in**, and CI fails if regeneration produces a diff.
Checking it in means a reviewer can see the actual types in a pull request and a
contributor can build without installing three generators; the drift check is
what stops the checked-in copy from becoming a lie.

| Source | Generator | Output |
| --- | --- | --- |
| `openapi/*.yaml` | `oapi-codegen` | `gen/go/<service>/` — types + chi server interface |
| `events/*.schema.json` | `go-jsonschema` | `gen/go/events/` — structs |
| `events/*.schema.json` | `datamodel-code-generator` | `gen/python/overpass_contracts/` — Pydantic v2 models |

```bash
make contracts-generate   # regenerate everything
make contracts-verify     # regenerate into a temp dir, fail on any diff
make contracts-validate   # validate all examples/ fixtures against their schemas
```

**Cost of this choice:** three generator dependencies and two toolchains.
**What it buys:** the contract becomes a build-time gate rather than a
convention, and the drift check catches the exact failure mode that kills
polyglot systems — one side changing a field and the other finding out in
production.

The alternatives considered were `ogen` (generates more, but far more code and a
strong opinion about handler shape — harder to defend line by line) and no
codegen at all (fewest dependencies, but manual cross-language sync and drift
found at test time rather than build time).

## OpenAPI dialect note

`openapi/tasking-api.v1.yaml` is **OpenAPI 3.0.3**, while the event schemas are
**JSON Schema 2020-12**. Aligning them on 3.1 would be preferable and is not done
because `oapi-codegen`'s 3.1 support is not yet stable, and a generator that
silently mis-generates is worse than a dialect mismatch that is written down.
The practical difference is small — `nullable` versus type unions, `example`
versus `examples`. Revisit when 3.1 support lands properly.

## Split OpenAPI documents, one per service

`tasking-api` and `plan-gateway` get separate documents rather than one combined
file. They are independently deployable, have opposite consistency postures
([ADR-0003](../docs/decisions/0003-consistency-boundaries-and-cap-position.md)),
and will version on different schedules. Shared types live in `common/` and are
`$ref`'d. A single document would couple two release cadences for the sake of
one fewer file.

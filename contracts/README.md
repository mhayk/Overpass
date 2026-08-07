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
make contracts-validate   # schemas valid, examples and fixtures behave
make contracts-generate   # regenerate everything
make contracts-verify     # regenerate into a temp dir, fail on any diff
make contracts-smoke      # round-trip fixtures through the generated Go AND Python
make contracts            # all of the above
```

### Generator constraints, and the two real bugs they caused

These are not trivia. Each one produced code that compiled, passed `go vet`,
looked correct, and was broken — which is precisely the failure mode this
project's test strategy exists to catch. They are recorded here because the next
person to touch a schema will otherwise rediscover them the hard way.

**Absolute `$id`, relative `$ref`.** `go-jsonschema` resolves `$ref` as a
filesystem path; handed an absolute URI it tries to *HTTP-fetch* it and fails
with a JSON parse error on the returned HTML. Python's `referencing` resolves a
relative ref against the document's `$id`, producing the absolute URI it already
has registered. Absolute `$id` plus relative `$ref` is the only combination both
accept. Absolute refs work only in Python; dropping `$id` would forfeit the
stable identity that versioning depends on.

**`const` is rendered as `interface{}`.** `go-jsonschema` has no handling for
`const`, so `"event_type": {"const": "..."}` generated an untyped
`interface{}` field — losing exactly the type safety a discriminator exists to
provide. A single-value `enum` is semantically identical in JSON Schema and
generates a proper named type with a constant. Every `const` in these schemas is
written as an enum of one for this reason.

**Defined types do not inherit methods — timestamps could not decode.**
`go-jsonschema` emits `type OccurredAt time.Time` for a `date-time` field. That
is a *defined type*, not an alias, so it does **not** inherit `time.Time`'s
`MarshalJSON`/`UnmarshalJSON`. Every timestamp in every event failed to decode
at runtime:

```
json: cannot unmarshal string into Go struct field ... of type events.OccurredAt
```

`scripts/contracts-generate.sh` emits `gen/go/events/time_json.gen.go` with the
missing methods, discovering the affected types by scanning the generated output
so a new timestamp field in a future schema is covered automatically. Found by
the round-trip test in `gen/go/contracttest`, not by reading the output.

**`format: uuid` plus `pattern` is unparseable in Python.**
`datamodel-code-generator` maps `format: uuid` onto Python's `UUID` type, and a
string `pattern` cannot be applied to it — pydantic raises `TypeError` at parse
time, so every event carrying an id was unparseable. The pattern was
belt-and-braces, added because JSON Schema treats `format` as an annotation
rather than an assertion by default. The correct fix is one rule stated once:
keep `format`, and turn on format *assertion* in the validator
(`FormatChecker`), rather than smuggling the same constraint in twice in
mutually incompatible ways.

**`prefixItems` is silently dropped by the Python generator.** The GeoJSON
`Position` — a 2-tuple with longitude in `[-180, 180]` and latitude in
`[-90, 90]` — becomes a bare `RootModel[list]` with only a length constraint.
Element bounds vanish, so a longitude of `200.5` or a swapped lat/lon pair parses
cleanly. **This one is not fixed, it is documented and asserted**, in the
`PYDANTIC_REJECTS` table in `scripts/contracts_smoke.py` and the `structural`
table in `gen/go/contracttest/roundtrip_test.go`. An unclassified fixture fails
the run, so the gap cannot grow quietly.

**Toolchain pins that are not about reproducibility.**
`datamodel-code-generator` 0.28.5 maps `sys.version_info` onto a closed enum and
raises on anything it does not recognise, so it *cannot run* under Python 3.14.
`scripts/contracts-generate.sh` pins the interpreter it runs under (3.12),
independently of whatever `python3` is on `PATH`.

**`output:` must not be set in the oapi-codegen config.** The config file's
output path wins over the `-o` flag, which silently made the drift check
regenerate into `gen/` instead of its scratch tree — so it compared a directory
against itself and passed unconditionally. A verification gate that cannot fail
is worse than no gate, because it is trusted. Output location is a property of
the invocation and is always passed with `-o`.

### What the two bindings actually guarantee

The generated types are **not** the authority on validity. The JSON Schema is.
Services validate against the schema at the boundary — inbound on consume,
outbound before publish — and never treat "it parsed into the model" as "it is
valid".

| Constraint | JSON Schema | Go (`encoding/json`) | Python (Pydantic) |
| --- | --- | --- | --- |
| Required fields | yes | yes | yes |
| Undeclared fields (`additionalProperties: false`) | yes | yes, with `DisallowUnknownFields` | yes, `extra='forbid'` |
| Enum membership | yes | **no** — named string types | yes, `Literal` |
| Numeric bounds | yes | **no** | yes |
| Array length | yes | **no** | yes |
| String pattern / format | yes | **no** | yes |
| `prefixItems` element bounds | yes | **no** | **no** |

Go is the weaker binding, because the generator renders constrained scalars as
plain named types. That is a fact about the tool, not a reason to distrust the
contract — it is a reason to validate at the boundary, which is where validation
belongs anyway.

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

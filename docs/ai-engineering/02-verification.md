# The accountability loop: what I did not trust, and how I checked

The honest summary: **fluency is not correctness, and reading is not
verification.** Every defect recorded here compiled, passed static analysis,
looked idiomatic, and survived being read. All of them were caught by execution.

That is not an argument against using AI to write code. It is an argument about
where the effort goes: **the test suite is the verification harness, and the
review effort belongs on the harness rather than on the output.**
[ADR-0010](../decisions/0010-test-strategy-and-coverage.md) is the technical
half of this document.

## Where I did not extend trust

Two areas, identified before writing any code, both flagged `risk/high` in the
backlog:

**Orbital geometry.** SAR is side-looking; a target at nadir is not imageable.
Incidence, look side, squint, and slant range all have to hold simultaneously.
Generated code in this area produces confident, plausible, well-commented
functions whose output is wrong by a few degrees — and a few degrees is the
difference between a flyable acquisition and one no sensor could make. There is
no exception thrown, no test failure, no visible symptom. Just a wrong answer,
delivered with the same tone as a right one.

**Concurrency and delivery semantics.** Idempotent consumers, the transactional
outbox, advisory locking. Code that is *almost* right here — acking before
committing rather than after, checking-then-inserting instead of relying on a
unique constraint — works perfectly under every test that does not specifically
attack it, and loses data under load.

The mitigations are structural: golden-reference tests against an independent
oracle for the first, property-based invariants and real-infrastructure
integration tests for the second.

## What actually went wrong in M0

Four defects. None found by reading.

### 1. Every timestamp in every event failed to decode

`go-jsonschema` renders a `date-time` field as:

```go
type OccurredAt time.Time
```

A **defined type**, not an alias. In Go, a defined type does not inherit the
methods of its underlying type, so `OccurredAt` has no `UnmarshalJSON` and every
event failed at runtime:

```
json: cannot unmarshal string into Go struct field ... of type events.OccurredAt
```

The code compiled. `go vet` was clean. The type declaration is *correct Go* and
reads exactly as intended. This is the archetype: a defect that lives in the gap
between what the code says and what the language does with it.

**Caught by:** a round-trip test that decoded real contract fixtures into the
generated types. **Fixed by:** emitting the missing methods from the generate
script, discovering the affected types by scanning the output so future timestamp
fields are covered without anyone remembering.

### 2. Every event carrying an id was unparseable in Python

The schemas declared both `"format": "uuid"` and a redundant `"pattern"` — belt
and braces, because JSON Schema treats `format` as an annotation rather than an
assertion by default.

`datamodel-code-generator` maps `format: uuid` onto Python's `UUID` type, and a
string `pattern` cannot be applied to a `UUID`. Pydantic raised `TypeError` at
parse time.

Both keywords are individually correct. **Only the combination fails**, and only
in one of the two bindings. No amount of reading either schema would surface it.

**Caught by:** round-tripping fixtures through the generated Pydantic models.
**Fixed by:** stating the rule once — keep `format`, and enable format
*assertion* in the validator — rather than smuggling the same constraint in twice
in mutually incompatible ways.

### 3. The verification gate could not fail

This is the most instructive one.

`oapi-codegen` lets the config file's `output:` path win over the `-o` flag. The
drift check therefore regenerated into `gen/` — **the very directory it was
comparing against** — and compared it with itself. It passed unconditionally.

A verification mechanism that cannot fail is worse than no mechanism, because it
is trusted. And no amount of reading the drift-check script would have found it:
the script was correct. The bug was in an interaction between a flag and a config
file, one layer down.

**Caught by:** deliberately introducing a field into a schema without
regenerating, and demanding the gate notice. It did not. **Fixed by:** removing
`output:` from the config, then re-running the same deliberate break to confirm
the gate now reports the difference in both languages simultaneously.

**Standing rule it produced:** *any check whose job is to catch a problem must be
demonstrated failing on that problem.* Not reasoned about — demonstrated.

### 4. Two generator version pins were wrong, and CI found it, not me

`go-jsonschema` was pinned to `v0.17.0`, but the committed `gen/` had been
produced by `v0.24.1` — the version a stray `@latest` install left on the
development machine. Locally everything passed. On a clean CI runner, the pinned
version regenerated and the drift gate reported 1483 lines of difference.

The gate caught, on its first real run, exactly the class of problem it was built
for: **committed generated code that no longer matches the pinned generator.**
The pin was the wrong number rather than the output being wrong, so the pin moved.

## Where the bindings are weaker than the contract — asserted, not assumed

Nothing guarantees two independently written generators agree. They do not:

| Constraint | JSON Schema | Go | Python |
| --- | --- | --- | --- |
| Required fields | yes | yes | yes |
| Undeclared fields | yes | yes (`DisallowUnknownFields`) | yes (`extra='forbid'`) |
| Enum membership | yes | **no** | yes |
| Numeric bounds | yes | **no** | yes |
| `prefixItems` element bounds | yes | **no** | **no** |

The last row has teeth. The GeoJSON `Position` — longitude in `[-180, 180]`,
latitude in `[-90, 90]` — loses its element bounds in **both** bindings. A
swapped lat/lon pair parses cleanly in Go and in Python while failing the schema.
A service trusting its own types would happily compute a confident, wrong
footprint on the other side of the planet.

This is not fixed. It is **documented and asserted**: classification tables in
both test suites declare which negative fixtures each binding can catch, and an
unclassified fixture fails the run. The gap cannot grow quietly.

The architectural conclusion — now written into `contracts/README.md` — is that
**the JSON Schema is the authority on validity**. Generated types are a
convenience for handling data already known to be valid. Services validate at the
boundary and never treat "it parsed" as "it is valid".

## The loop, stated plainly

1. **Name the risk before writing.** Which parts would be plausible and wrong?
   Those get `risk/high` and a verification strategy chosen up front.
2. **Choose the oracle before the implementation.** For physics, published pass
   data. For the scheduler, invariants. For contracts, cross-language round-trip.
   An oracle produced by the same process as the code is not an oracle.
3. **Execute, do not read.** Every defect above survived reading.
4. **Break the checker on purpose.** A gate never seen failing is a gate not
   known to work.
5. **Write down where verification stops.** The `prefixItems` gap is asserted
   rather than hidden, because an unstated limitation becomes a false assumption
   the moment someone else reads the code.

## What this does not cover, yet

Being straight about the boundary of the claim: everything above concerns
contracts and infrastructure, because that is what M0 built. The two areas
identified as highest-risk — orbital geometry and concurrency — have their
verification strategies designed and their issues labelled, but **the golden-
reference tests and property-based invariants do not exist yet**. They land in
M1 and M2.

The methodology has been exercised, and it has already caught four real defects.
It has not yet been exercised on the hardest material. When it has, this document
gets updated with what it found — including if the answer is that it missed
something.

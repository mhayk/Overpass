# Overpass — Agent Working Agreement

## What this is

Satellite tasking, feasibility, and collection-planning system.
Portfolio project for a senior engineering interview. See docs/SPEC.md.

## Non-negotiables

- Contracts before implementations. Never change a published event schema
  in place — version it.
- Every non-obvious decision gets an ADR in docs/decisions/. If you make a
  choice I did not ask for, write it down or ask me.
- Every consumer is idempotent. Every publish goes through the outbox.
- No new dependency without telling me what it buys and what it costs.
- Tests are the verification harness for generated code. Physics and
  concurrency logic require property-based or golden-reference tests, not
  snapshots of your own output.
- Prefer boring, explicit code over clever abstraction. I have to explain
  every line of this repo to a hiring manager.

## Hard-won rules (M0)

Each of these cost real time. Do not relearn them.

- **Never state library behaviour without running it.** Three M0 defects came
  from confident, wrong claims about tool behaviour. Check `--version`, run one
  smoke case, then build on it.
- **Any check whose job is to catch a problem must be demonstrated FAILING on
  that problem.** The codegen drift gate silently compared a directory against
  itself and passed unconditionally. A gate never seen failing is not known to
  work. Break it on purpose.
- **Reading is not verification for generated code.** Every M0 defect compiled,
  passed `go vet`, and looked idiomatic. Execute it.
- **The JSON Schema is the authority on validity, not the generated types.**
  Both bindings silently drop `prefixItems`, so a swapped lat/lon parses cleanly
  in Go and Python while failing the schema. Validate at the boundary.

## Contract conventions

These exist because of specific generator behaviour — see contracts/README.md.

- Absolute `$id`, **relative** `$ref`. go-jsonschema resolves refs as filesystem
  paths and tries to HTTP-fetch absolute URIs.
- Use a single-value `enum`, never `const`. go-jsonschema emits `interface{}`
  for `const`, discarding the discriminator's type safety.
- One rule stated once. `format: uuid` plus a redundant `pattern` is
  unparseable in Python; keep `format` and assert it in the validator.
- Generated code is committed. `make contracts-verify` is what keeps it honest.

## Layout

    services/tasking-api  Go  REST ingress, outbox, request state machine
    services/feasibility  Py  SGP4, access windows, SAR geometry
    services/planner      Go  allocation, de-confliction, plan commit
    services/plan-gateway Go  read models, CZML/GeoJSON, SSE
    services/simulator    Py  acquisition execution, TLE drift, failure injection
    web                   TS  Next.js, Cesium, deck.gl
    contracts             --  JSON Schema events + OpenAPI (source of truth)
    gen                   --  generated types, committed, drift-gated
    deploy                --  compose config: nats, postgres, otel, grafana
    scripts               --  contract tooling, stack bootstrap, gates
    db/migrations         --  one Postgres, schema per service
    testdata              --  frozen TLEs for golden tests, benchmark scenarios
    docs/decisions        --  ADRs
    docs/ai-engineering   --  methodology, prompts, verification, retro

## Commands

    make help                what you can do
    make contracts           validate, regenerate, round-trip
    make contracts-verify    drift gate (CI runs this)
    make up / down / clean   the stack
    make test / lint         per language
    make coverage            80% overall, 95% planner and geometry

## Conventions

Go: standard layout, hexagonal, table-driven tests, errors wrapped with
context, no panics outside main.
Python: uv, ruff, mypy strict, pytest.
TS: strict mode, no `any`, server components by default.
Commits: Conventional Commits, one issue per branch, `Closes #N`.

## Working style

- Work one milestone at a time. Do not scope-creep into the next.
- Ask before assuming on anything domain-related (geometry, constraints,
  scheduling semantics). Guessing physics is expensive.
- When you finish a unit of work, tell me what you would attack next and
  what you are least confident about.

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

## Layout

services/tasking-api Go REST ingress, outbox, request state machine
services/feasibility Py SGP4, access windows, SAR geometry
services/planner Go allocation, de-confliction, plan commit
services/plan-gateway Go read models, CZML/GeoJSON, SSE
web TS Next.js, Cesium, deck.gl
contracts - JSON Schema events + OpenAPI (source of truth)
docs/decisions - ADRs
docs/ai-engineering - methodology, prompts, verification, retro

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

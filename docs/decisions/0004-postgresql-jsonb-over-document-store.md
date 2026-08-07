# 0004 — PostgreSQL with JSONB for semi-structured data, instead of adding a document store

- **Status:** accepted
- **Date:** 2026-08-07
- **Deciders:** Mhayk Whandson

## Context and problem statement

Overpass stores two kinds of data with genuinely different shapes.

**Rigidly structured, invariant-bearing data.** Requests, satellites,
opportunities, plans, acquisitions. These carry the constraint that the whole
system exists to protect: no two acquisitions on one satellite may overlap or
violate slew time
([ADR-0003](0003-consistency-boundaries-and-cap-position.md)).

**Genuinely semi-structured data.** Raw TLE payloads as fetched from Celestrak.
Sensor mode parameter sets, where Spotlight, Stripmap, and Scan each carry
different fields. Computed geometry blobs from the feasibility sweep — incidence,
squint, slant range, doppler terms — whose shape will change as the geometry model
gets refined. Footprint polygons.

The reflex answer to the second category is "add MongoDB for the flexible parts."
The question: **is a second store actually warranted, or does Postgres cover both
shapes well enough that the second store is pure cost?**

## Decision drivers

1. **The allocation invariant is transactional.** Whatever else changes, this
   must be enforceable by the database.
2. **Schema volatility in the geometry and mode-parameter payloads** is real, not
   hypothetical — these will change repeatedly through M1 and M2.
3. **Spatial query support**, because targets and footprints are geometry and we
   will want containment and intersection queries.
4. **Number of consistency models a developer must hold in their head.** Every
   additional one is a permanent tax.
5. **Cold-start budget** — every extra container competes with the five-minute
   `docker compose up` target.

## Considered options

1. **PostgreSQL only**, with JSONB (plus PostGIS) for the semi-structured parts
2. **PostgreSQL for the relational core + MongoDB for documents**
3. **PostgreSQL only, fully normalised** — a table per mode, columns for every
   geometry term, no JSONB anywhere
4. **PostgreSQL + a separate spatial store** rather than PostGIS in-place

## Decision outcome

Chosen: **Option 1 — PostgreSQL only, with JSONB where the data is genuinely
semi-structured and PostGIS for geometry.**

The relational core stays relational precisely because that is where the
invariants live. Three Postgres features carry disproportionate weight here and
are worth naming individually:

**`tstzrange` + a GiST exclusion constraint on `acquisitions`.** This enforces
non-overlap per satellite *in the database*:

```sql
ALTER TABLE acquisitions
  ADD CONSTRAINT acquisitions_no_overlap_per_satellite
  EXCLUDE USING gist (
    satellite_id WITH =,
    window       WITH &&
  );
```

The invariant is now unbypassable. A bug in the planner, a manual `INSERT`, a
future second code path — none of them can produce an overlapping plan. This is
the difference between an invariant and an intention.

**JSONB with GIN indexes** for `tle_sets.raw`, `opportunities.geometry`, and
`satellites.mode_parameters`. Schemaless where schemalessness is honest, queryable
where we need it, and inside the same transaction as everything else.

**PostGIS** for target and footprint geometry, so "which requests does this
footprint cover?" is an indexed spatial query rather than application-side
filtering over a full scan.

The punchline for the second store: **we do not need one, because Postgres covers
the document use case without giving up transactions.** Adding MongoDB would mean
two consistency models, a dual-write problem between them with no ACID across the
pair, and a second operational surface — for capability we already have.

### Consequences

**Good**

- The system's central invariant is enforced by the database engine, at the
  lowest possible level, with a mechanism designed exactly for it.
- One connection pool, one migration path, one backup story, one transaction
  boundary. Everything is `BEGIN`-able together — which is also what makes the
  transactional outbox and the idempotent-consumer pattern possible at all.
- Schema evolution on the volatile payloads costs nothing: JSONB absorbs new
  geometry terms without a migration.
- Spatial queries are indexed and local.

**Bad**

- **JSONB is a discipline problem.** Nothing stops a field from drifting into a
  JSONB blob because adding a column felt like effort. Mitigation: JSONB is
  permitted only for the four documented cases (raw TLE, mode parameters,
  computed geometry, footprint), and anything queried in a `WHERE` clause on a
  hot path is a candidate for promotion to a real column.
- JSONB is weakly typed at the storage layer. A typo in a key is a runtime
  discovery, not a compile-time one. Mitigation: the same JSON Schemas that
  define the event contracts validate these payloads at the service boundary.
- Postgres becomes a single point of failure for the whole system. Accepted
  deliberately at this scale; the honest scaling answer is read replicas for
  `plan-gateway` before it is a second database technology.
- PostGIS makes the container image substantially larger, which costs cold-start
  time. Measured and accepted.

**Neutral**

- Exclusion constraints and `tstzrange` are less widely known than they deserve
  to be, so this code needs a comment explaining what it does. That is a
  documentation cost, not a design cost.

### Confirmation

- An integration test attempts to insert two overlapping acquisitions for the
  same satellite **directly via SQL**, bypassing all application code, and
  asserts the constraint rejects it. If that test can be made to pass by
  application changes alone, the invariant is not where this ADR claims it is.
- If JSONB fields start appearing in `WHERE` clauses on the planner's hot path,
  that is the signal that the relational/document line has been drawn in the
  wrong place, and the affected fields get promoted to columns.
- If a query pattern emerges that Postgres genuinely cannot serve — full-text
  relevance ranking, say, or a graph traversal — that is a legitimate trigger to
  revisit, and it should produce a successor ADR rather than a quiet addition.

## Pros and cons of the options

### Option 1 — PostgreSQL only, with JSONB and PostGIS (chosen)

- Good, because one store means one consistency model, one transaction boundary,
  and no dual-write problem between stores.
- Good, because the exclusion constraint gives us database-enforced correctness on
  the invariant that matters most.
- Bad, because of the JSONB discipline risk and the single-point-of-failure
  concentration described above.

### Option 2 — PostgreSQL + MongoDB

- Good, because MongoDB's document ergonomics and aggregation pipeline are more
  pleasant than JSONB operators for deeply nested access, and horizontal sharding
  is a first-class capability rather than an extension.
- Bad, and decisively: writing an opportunity's relational row and its geometry
  document becomes a dual write across two systems with no shared transaction.
  We would have to invent a reconciliation mechanism for data that has no reason
  to be split in the first place.
- Bad, because two consistency models is a permanent cognitive tax on every
  developer, forever, in exchange for ergonomics on four fields.
- Bad, because it adds a container and an operational surface for capability
  Postgres already has.

### Option 3 — PostgreSQL, fully normalised, no JSONB

- Good, because everything is typed, constrained, and introspectable. Schema
  drift becomes impossible rather than merely discouraged.
- Good, because query plans over real columns are more predictable than over
  JSONB paths.
- Bad, because the geometry payload changes shape repeatedly during M1 and M2.
  Every refinement to the SAR geometry model would become a migration, and the
  cost lands exactly during the period of highest iteration.
- Bad, because mode parameters differ per mode. Fully normalising them means
  either a table per mode or a sparse table with mostly-null columns — the two
  classic bad answers to polymorphic attributes.

### Option 4 — PostgreSQL + a dedicated spatial store

- Good, because a specialised spatial engine would outperform PostGIS at large
  scale on complex geometry workloads.
- Bad, because PostGIS is not a compromise — it is one of the most capable
  spatial engines available, and it is *in the transaction* with everything else.
- Bad, because it reintroduces the dual-write problem for no gain at our data
  volumes.

## More information

- Invariant this decision exists to protect:
  [ADR-0003](0003-consistency-boundaries-and-cap-position.md)
- Deployment constraint on container count:
  [ADR-0005](0005-docker-compose-over-kubernetes.md)

# How the work was decomposed

## The central claim

**Contracts-first is what makes agent parallelism safe.** Everything else in
this document follows from that one sentence.

Once the event schemas and the OpenAPI documents are frozen and generating into
both languages with a drift check, `tasking-api`, `feasibility-service`, and
`web` can be built concurrently, in separate sessions, with no merge collisions —
because the interface between them already exists and is enforced by CI.

Without that, parallel agents produce integration debt faster than they produce
features. Three sessions each invent a plausible interpretation of "an
Opportunity", all three are internally coherent, and the cost is paid later at
integration time, in a form that is much harder to diagnose than a compile error:
each side works in isolation and the system does not.

That is the whole argument for spending an entire milestone on contracts before
writing a service. It is not process ceremony — it is the mechanism that turns
parallel work from a liability into a speedup.

## What "frozen" has to mean

Frozen does not mean "we agreed on it in a document". It means:

1. **Schemas are executable.** `make contracts-validate` — 32 checks including
   eight negative fixtures that prove the schemas actually reject things.
2. **Generated code is committed and gated.** `make contracts-verify` regenerates
   into a scratch tree and fails on any difference.
3. **Both bindings are round-trip tested.** `make contracts-smoke` proves Go and
   Python agree, and asserts exactly where they do not.

Only condition 3 is unusual, and it is the one that matters most for parallel
work. Two generators written by different people in different languages have no
reason to agree about what `prefixItems` means. **They do not** — both silently
drop it (see [02-verification.md](02-verification.md)). Discovering that at
integration time, in a service that emitted a swapped lat/lon and got a
geographically confident wrong answer, would have cost days.

## Decomposition

Work is decomposed by **contract boundary**, not by layer or by file. A unit of
agent work is one that:

- can be stated in terms of the contract it consumes and the contract it produces
- has a verification step that does not require another in-flight unit
- lands as one branch, one PR, one `Closes #N`

That maps directly onto the issue backlog. Each of the 70 issues has acceptance
criteria that are checkable without reference to unfinished work, which is the
same property that makes it safe to hand to a fresh session with no memory of the
previous one.

### What parallelises, and what does not

| Phase | Parallel? | Why |
| --- | --- | --- |
| M0 contracts | **No** | Everything downstream depends on them. Sequential and deliberate. |
| M1 services | **Yes** | Three services against frozen contracts, no shared code |
| M2 allocation policies | **Yes**, behind the `AllocationPolicy` interface | Each policy is a pure function; the interface is the contract |
| M2 planner core | **No** | The invariant lives here; single writer, single author |
| M3 hardening | Partly | Observability and load testing parallelise; chaos tests need the whole stack |
| M4 frontend | **Yes** | Views are independent given the read-model contract |

The pattern: **parallelise where a contract exists, serialise where an invariant
lives.** That is the same principle that placed the service boundaries in
[ADR-0003](../decisions/0003-consistency-boundaries-and-cap-position.md), applied
to the work rather than to the runtime. The planner is serialised for exactly the
reason its allocation is serialised — there is one invariant and it needs one
owner.

## The working agreement

`CLAUDE.md` at the repository root is loaded into every session. It is
deliberately short, because a long agreement is one that gets skimmed. Its
non-negotiables exist because each one prevents a specific observed failure:

| Rule | Failure it prevents |
| --- | --- |
| Contracts before implementations; never edit a published schema in place | Two services disagreeing after a "small" schema fix |
| Every non-obvious decision gets an ADR, or ask | Silent choices that cannot be defended later |
| Every consumer idempotent; every publish through the outbox | The dual-write problem, reintroduced by a helpful shortcut |
| No new dependency without stating what it buys and costs | Dependency accretion nobody can justify in review |
| Prefer boring explicit code over clever abstraction | Premature abstraction, the most common failure of generated code |

The last one earns its place. Left unconstrained, generated code reaches for
interfaces, generics, and indirection layers before there is a second
implementation to justify them. The instruction to prefer boring code is not
stylistic — it is a correction for a specific and consistent bias.

## Why the issue backlog is written the way it is

Every issue carries an **Engineering decisions** section, mandatory even when the
answer is "none". The point is that the question is always asked; optional
sections never get filled in.

That section is also the handoff mechanism. A session picking up issue #34 reads
the decisions already made and their reasoning, rather than re-deriving them and
arriving somewhere subtly different. It is the same function as an ADR, at a
smaller scale and a shorter half-life.

Issues are labelled `risk/high` where generated code is most likely to be
plausible and wrong — orbital geometry, concurrency safety, allocation
invariants. That label is not severity. It selects the verification strategy:
`risk/high` issues get property-based or golden-reference tests, not example
tests. See [02-verification.md](02-verification.md) and
[ADR-0010](../decisions/0010-test-strategy-and-coverage.md).

## Honest limits of this methodology

- **It has been exercised through M0 only.** M0 is the sequential milestone. The
  parallelism claim is well-founded but not yet demonstrated at scale, and
  saying otherwise would be overclaiming.
- **The 70 issues were generated in one pass** and will be wrong in places.
  Estimates are absent on purpose — they would be invented.
- **Contracts will need to change.** The versioning rules exist precisely because
  "frozen" is a statement about process, not about omniscience. What must not
  happen is a schema edited in place; what will certainly happen is a `.v2`.

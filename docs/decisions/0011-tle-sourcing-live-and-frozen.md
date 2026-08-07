# 0011 — Fetch TLEs live from Celestrak at seed time, and test orbital math against a frozen snapshot

- **Status:** accepted
- **Date:** 2026-08-07
- **Deciders:** Mhayk Whandson

## Context and problem statement

Every access window this system computes traces back to a two-line element set.
Where those element sets come from decides two things that pull in opposite
directions: whether the demo shows real orbital data, and whether the orbital
math can be regression-tested at all.

Live data makes the demo honest. A reviewer running `make demo` sees Sentinel-1A
where Sentinel-1A actually is, and the `tle_epoch` staleness logic — FRESH,
AGING, STALE, and the refusal to compute on stale data — exercises itself
against real ages rather than against ages we invented.

Live data also makes results non-deterministic. Celestrak publishes a new
element set for an active LEO satellite roughly daily, and each one moves every
computed window by seconds to minutes. A golden-reference test written against
today's TLE fails tomorrow, for a reason that has nothing to do with the code
having changed.

Both properties are wanted, and one source cannot have both.

The question: **where do TLEs come from, given that the demo needs current data
and the tests need frozen data?**

## Decision drivers

1. **Golden-reference tests must be possible.** [ADR-0010](0010-test-strategy-and-coverage.md)
   makes the test suite the verification harness for generated code, and M1-12
   specifies golden references for orbital math. A test whose expected values
   drift daily is not a regression test, it is a weather report.
2. **The staleness path must be genuinely exercised.** FRESH/AGING/STALE and the
   `TLE_STALE` refusal are real behaviour with real consequences, and the
   temptation with frozen data is to fake the ages and never run the real thing.
3. **`docker compose up` must work on a machine with no network guarantees.** The
   definition of done is a working system in five minutes on someone else's
   laptop, and a hard dependency on an external HTTP endpoint at startup is a
   way for that to fail in front of a reviewer.
4. **The demo should show real satellites.** A portfolio project whose orbital
   data is invented is a simulation of a simulation, and a reviewer can tell.
5. **Determinism is a hard requirement for the propagation layer**, not a
   preference — see the confirmation section.

## Considered options

1. **Live fetch only**, at seed time and in tests
2. **Frozen snapshot only**, committed and used everywhere
3. **Both, with distinct roles** — live at seed time, frozen for anything that
   must be reproducible
4. **Live fetch with a committed snapshot as a fallback** on network failure

## Decision outcome

Chosen: **Option 3 — two sources with two jobs, and the split is explicit.**

- **Seeding fetches live**, from Celestrak's GP endpoint. This is what the demo
  runs on and what makes the staleness classification meaningful.
- **Tests use `testdata/tle/sar-constellation.2026-08-07.tle`**, nine real
  element sets across Sentinel-1, ICEYE and Capella, committed with a header
  saying why they must not be casually regenerated.

The distinction that makes this work rather than being a fudge: the frozen file
is not a *fallback* for the live one, and the live one is not a *default* for
the frozen one. They answer different questions. "What is the constellation
doing right now" is only answerable live. "Does the propagator still produce the
window it produced last month" is only answerable frozen.

Option 4 — live with the snapshot as a fallback — is the one that sounds most
robust and is rejected in the confirmation section below, because a fallback
that engages silently is worse than a failure.

### What this costs, concretely

A test suite that never fetches cannot catch Celestrak changing its response
format. That is a real gap and it is accepted deliberately: the Celestrak
adapter is tested against recorded responses, including the one that matters
most — an unmatched query answers `200 OK` with a body reading "No GP data
found", not a `404`. Treating that as an empty catalogue would seed a
constellation with no satellites in it, and the first symptom would surface
three services downstream.

### Consequences

**Good**

- Golden-reference tests become possible at all, which is what M1-12 needs.
- The demo shows real satellites at their real positions.
- The staleness classification runs against real TLE ages in the demo path, so
  AGING and STALE are reachable states rather than theoretical ones.
- No test touches the network, so the suite runs identically on a plane, in CI,
  and on a reviewer's laptop.

**Bad**

- **Two sources is two things to keep straight**, and the failure mode is a test
  that quietly starts using live data and becomes flaky months later. Mitigated
  by the frozen file carrying a header that says so, and by the adapter being
  the only code that can reach the network.
- The frozen snapshot ages. Its element sets are already stale by the staleness
  policy's own definition, which is fine for propagation regression testing and
  would be wrong for anything that asserted on freshness. That distinction has
  to be remembered.
- Format drift at Celestrak is invisible to the test suite until seeding fails.
- Refreshing the snapshot invalidates every golden expectation derived from it.
  That is the point, but it makes the file expensive to touch, and someone will
  eventually want to touch it.

**Neutral**

- The snapshot is nine satellites, not a full catalogue. Enough for a
  constellation, small enough to read in a pull request.

### Confirmation

- **Determinism:** propagating the frozen snapshot twice produces identical
  windows, asserted in `test_access.py::test_determinism`. Skyfield's timescale
  is loaded with `builtin=True` specifically so no downloaded Earth-orientation
  table can shift results under us — a fetched table would make the physics
  depend on when the machine last had internet.
- **The frozen data is real:** every element set in the snapshot passes its own
  mod-10 checksum, asserted across all nine.
- **No test reaches the network:** the Celestrak adapter's tests run against
  `respx`. A test that started making real requests would show up as a suite
  that fails offline.
- **This decision is wrong** if the snapshot is ever used as a silent fallback
  for a failed live fetch. That would make the demo's data source depend on
  network conditions, and a reviewer could see stale positions with no
  indication why — the seeding path must fail loudly instead, which is why
  `CelestrakClient` raises rather than returning an empty list.

## Pros and cons of the options

### Option 1 — Live fetch only

- Good, because there is one source, one code path, and nothing to keep straight.
- Good, because the data is always current.
- Bad, because golden-reference tests become impossible. Expected windows would
  have to be recomputed on every run, which means comparing the code against
  itself — the exact failure ADR-0010 exists to prevent.
- Bad, because the test suite would not run offline or in a sandboxed CI job.

### Option 2 — Frozen snapshot only

- Good, because everything is deterministic and nothing depends on the network.
- Good, because it is the simplest thing that could work.
- Bad, because the staleness classification would never see a real age. Every
  TLE would be years old and permanently STALE, so the demo would refuse to
  compute anything, and the honest fix — pretending the frozen epoch is recent —
  means the one piece of logic about data freshness never runs against real
  freshness.
- Bad, because the demo would show satellites where they were, not where they
  are, and the discrepancy is visible on a globe.

### Option 3 — Both, with distinct roles (chosen)

- Good, because each source does the job it is actually good at.
- Good, because the boundary is enforceable: only the adapter can reach the
  network, and only tests read the snapshot.
- Bad, because it is two things rather than one, as covered above.

### Option 4 — Live fetch with the snapshot as a fallback

- Good, because seeding would never fail, which is superficially attractive for
  a demo that has to work on an unfamiliar machine.
- **Bad, because a silent fallback is a lie.** The reviewer sees satellites at
  positions that are months out of date, the staleness flags say STALE, and
  nothing on screen explains that the network was the reason. A loud failure
  with "could not reach Celestrak" is more useful than a working demo showing
  wrong data.
- Bad, because it would couple the two sources, and the frozen file would drift
  from being a test fixture to being production data — at which point refreshing
  it for a test reason changes the demo.
- **This is the tempting option and it is rejected on the principle that a
  fallback nobody can see is worse than a failure everybody can.**

## More information

- The adapter and the parser: `services/feasibility/src/feasibility/tle/`
- The frozen snapshot and its header: `testdata/tle/sar-constellation.2026-08-07.tle`
- Staleness thresholds, and why they are configuration:
  `element_set.py::StalenessPolicy`
- Golden-reference strategy this enables: [ADR-0010](0010-test-strategy-and-coverage.md), M1-12 (#24)
- Provenance carried on every result: `contracts/common/sar.v1.schema.json#/$defs/TleReference`

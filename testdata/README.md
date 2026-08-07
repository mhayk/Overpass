# Test data

## `tle/`

**Frozen** TLE snapshots at a fixed epoch, used by golden-reference tests only.

This is deliberately *not* the same source the running system uses. At seed time
Overpass fetches live from Celestrak so that TLE staleness is exercised for real
rather than simulated. But tests have to be deterministic, and nothing about
orbital mechanics is provable against input that changes daily — so the golden
tests run against these committed fixtures and never touch the network.

Two sources for the same data, with two different jobs. The cost is that they can
diverge, and that is written up in ADR-0011.

## `scenarios/`

Generated allocation scenarios with fixed seeds, for the policy benchmark. Fixed
seeds because comparing four policies on different inputs measures nothing.

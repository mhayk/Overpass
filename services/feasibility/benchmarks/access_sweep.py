"""How long a full-constellation access sweep actually takes.

The number this produces is not decoration. `feasibility-service` consumes
`tasking.request.received.v1` from a durable NATS consumer with an `ack_wait`,
and a sweep that outruns it gets redelivered mid-flight — so the same work
starts again while the first attempt is still running, which under load is how a
queue goes from busy to unrecoverable.

So the rule this exists to inform: ack_wait must comfortably exceed the p99 of a
realistic sweep. Run this, read the p99, and set the consumer accordingly.

    uv run python benchmarks/access_sweep.py
"""

from __future__ import annotations

import statistics
import time
from datetime import UTC, datetime, timedelta
from pathlib import Path

from feasibility.orbit import GroundPoint, Propagator, sweep
from feasibility.tle import parse_catalogue

# Deliberately fixed rather than "now": a benchmark whose inputs move cannot be
# compared against a previous run.
EPOCH = datetime(2026, 8, 7, tzinfo=UTC)
REPEATS = 5

# A spread of latitudes, because pass count and duration depend strongly on how
# far a site is from the orbital inclination. Timing only at one site would
# report a best case and call it typical.
SITES = {
    "Lisbon (39N)": GroundPoint(38.7223, -9.1393),
    "Singapore (1N)": GroundPoint(1.3521, 103.8198),
    "Longyearbyen (78N)": GroundPoint(78.2232, 15.6267),
}


def repo_root() -> Path:
    for parent in Path(__file__).resolve().parents:
        if (parent / "testdata").is_dir() and (parent / "contracts").is_dir():
            return parent
    msg = "could not locate the repository root"
    raise RuntimeError(msg)


def main() -> None:
    snapshot = repo_root() / "testdata" / "tle" / "sar-constellation.2026-08-07.tle"
    element_sets = parse_catalogue(snapshot.read_text())

    build_start = time.perf_counter()
    propagators = [Propagator(es) for es in element_sets]
    build_ms = (time.perf_counter() - build_start) * 1000

    print(f"constellation: {len(propagators)} satellites")
    print(f"propagator construction: {build_ms:.1f} ms total\n")

    print(f"{'site':<22} {'horizon':>8} {'windows':>8} {'median':>10} {'p99':>10}")
    print("-" * 62)

    worst_p99 = 0.0
    for hours in (24, 72):
        for name, site in SITES.items():
            timings: list[float] = []
            window_count = 0
            for _ in range(REPEATS):
                start = time.perf_counter()
                result = sweep(propagators, site, EPOCH, EPOCH + timedelta(hours=hours))
                timings.append((time.perf_counter() - start) * 1000)
                window_count = result.window_count

            median = statistics.median(timings)
            # With five samples this is the maximum, which is the honest reading
            # of "p99" at this sample size. Saying p99 and computing a quantile
            # from five points would be dressing up a max.
            p99 = max(timings)
            worst_p99 = max(worst_p99, p99)
            print(f"{name:<22} {hours:>6}h {window_count:>8} {median:>8.0f}ms {p99:>8.0f}ms")

    print("-" * 62)
    print(f"\nworst p99 across all cases: {worst_p99:.0f} ms")
    print("Consumer ack_wait should exceed this by a wide margin - see M1-13.")


if __name__ == "__main__":
    main()

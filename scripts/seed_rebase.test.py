"""The seeder's epoch rebase, tested without a database.

#187 was a time bomb: the frozen snapshot in testdata/ aged past feasibility's
72-hour refusal threshold and every seeded stack began refusing every request
as TLE_STALE. It broke `make demo` and the integration suite simultaneously,
and it did so silently on any long-running instance, because accumulated
opportunities from earlier days kept the dashboards populated.

These assert the property that stops it recurring: what the seeder writes is
always FRESH, whatever the file's date, and it is still a valid TLE.

Run: python3 scripts/seed_rebase.test.py
"""

from __future__ import annotations

import importlib.util
import sys
import types
from datetime import datetime, timedelta, timezone
from pathlib import Path

UTC = timezone.utc
ROOT = Path(__file__).resolve().parents[1]

# psycopg is the seeder's only third-party import and none of this needs it.
sys.modules.setdefault("psycopg", types.ModuleType("psycopg"))
spec = importlib.util.spec_from_file_location("seed", ROOT / "scripts" / "seed.py")
seed = importlib.util.module_from_spec(spec)
sys.modules["seed"] = seed
spec.loader.exec_module(seed)

failures: list[str] = []


def check(condition: bool, message: str) -> None:
    if not condition:
        failures.append(message)


elements = seed.read_snapshot(seed.SNAPSHOT)
check(len(elements) > 0, "the snapshot parsed to no element sets at all")

at = datetime.now(UTC) - timedelta(hours=1)
stale_threshold_hours = 72.0

for element in elements:
    rebased = seed.rebase_epoch(element, at)

    age_hours = (datetime.now(UTC) - rebased.epoch).total_seconds() / 3600
    check(
        age_hours < stale_threshold_hours,
        f"{element.satellite_id}: rebased epoch is {age_hours:.1f}h old, past the "
        f"{stale_threshold_hours}h refusal threshold — feasibility will refuse it",
    )
    check(
        age_hours >= 0,
        f"{element.satellite_id}: rebased epoch is in the future by "
        f"{-age_hours:.1f}h; a predicted element set only confuses a demo",
    )

    # Still a valid TLE. A line whose checksum no longer matches is rejected on
    # the way back in, which would turn the fix into a different failure.
    check(
        seed._checksum(rebased.line1) == int(rebased.line1[68]),
        f"{element.satellite_id}: rebased line 1 checksum does not validate",
    )
    check(
        len(rebased.line1) == len(element.line1),
        f"{element.satellite_id}: rebased line 1 changed length",
    )

    # The ORBIT is untouched. Line 2 carries inclination, RAAN, eccentricity,
    # argument of perigee, mean anomaly and mean motion — rebasing must move
    # the observation time and nothing else.
    check(
        rebased.line2 == element.line2,
        f"{element.satellite_id}: line 2 changed; rebasing altered the orbit itself",
    )

    # And the decoded epoch agrees with the line it was written into. store.py
    # refuses a row whose epoch column contradicts its own element set.
    check(
        seed._epoch_from_line1(rebased.line1) == rebased.epoch,
        f"{element.satellite_id}: epoch column and line 1 disagree",
    )

# The snapshot itself is expected to be stale by now. If it ever stops being
# so, this test has become vacuous and would pass with rebasing removed.
oldest = min(e.epoch for e in elements)
snapshot_age = (datetime.now(UTC) - oldest).total_seconds() / 3600
if snapshot_age < stale_threshold_hours:
    print(
        f"note: the frozen snapshot is only {snapshot_age:.0f}h old, still inside the "
        f"{stale_threshold_hours:.0f}h threshold. These assertions cannot fail today; "
        "they will once it ages, which is exactly what #187 was."
    )

if failures:
    print(f"FAIL: {len(failures)} problem(s)")
    for failure in failures:
        print(f"  - {failure}")
    raise SystemExit(1)

print(f"ok: {len(elements)} element sets rebase fresh, valid, and orbit-preserving")

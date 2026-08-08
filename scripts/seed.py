"""Seed the constellation, sensor modes and sample customers.

Run via ``scripts/seed.sh`` or ``make seed``.

Idempotent by construction: every insert is an upsert keyed on the natural id,
so running it twice is running it once. That is not politeness — ``make up`` is
expected to be run repeatedly on a laptop, and a seeder that duplicates rows on
the second run turns the demo into a debugging session.

**Offline.** The element sets come from the frozen snapshot in
``testdata/tle/``, never from the network. ADR-0011 splits this deliberately:
live TLEs at seed time are a nice property for a deployment and a liability for
a demo, because a reviewer on a train with no signal gets a broken clone. The
same snapshot backs the golden orbital tests, so what the demo propagates is
exactly what the test suite pins.
"""

from __future__ import annotations

import json
import os
import re
import sys
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path

import psycopg

ROOT = Path(__file__).resolve().parent.parent
SNAPSHOT = ROOT / "testdata" / "tle" / "sar-constellation.2026-08-07.tle"

GREEN, YELLOW, DIM, RESET = "\033[32m", "\033[33m", "\033[90m", "\033[0m"

# reference.satellites enforces ^[A-Z0-9][A-Z0-9_-]{0,31}$, so the parenthetical
# in "CAPELLA-11 (ACADIA-1)" cannot be part of the id. The Celestrak name is a
# display string; the id is derived from it and must stay stable across
# snapshots or every foreign key in the system moves.
_ID_STRIP = re.compile(r"\s*\(.*\)\s*$")


@dataclass(frozen=True)
class ElementSet:
    satellite_id: str
    display_name: str
    norad_id: int
    line1: str
    line2: str
    epoch: datetime


def _epoch_from_line1(line1: str) -> datetime:
    """Decode the TLE epoch: two-digit year and fractional day-of-year.

    Decoded from the element set rather than taken from the fetch time. They
    differ by hours to days, and using the wrong one shifts every computed
    access window — quietly, because both are plausible timestamps.

    The two-digit year pivots at 57, which is the TLE convention rather than a
    choice: 57-99 mean 1957-1999, 00-56 mean 2000-2056. Spelled out because a
    bare ``2000 + yy`` is correct until 2057 and wrong forever after.

    Day-of-year is 1-based, so day 1.0 is midnight on 1 January.
    """
    yy = int(line1[18:20])
    year = 1900 + yy if yy >= 57 else 2000 + yy
    day_of_year = float(line1[20:32])
    return datetime(year, 1, 1, tzinfo=UTC) + timedelta(days=day_of_year - 1)


def read_snapshot(path: Path) -> list[ElementSet]:
    lines = [
        line.rstrip("\n")
        for line in path.read_text().splitlines()
        if line.strip() and not line.startswith("#")
    ]
    if len(lines) % 3 != 0:
        message = f"{path} has {len(lines)} non-comment lines, not a multiple of 3"
        raise ValueError(message)

    out: list[ElementSet] = []
    for i in range(0, len(lines), 3):
        name, line1, line2 = lines[i].strip(), lines[i + 1], lines[i + 2]
        satellite_id = _ID_STRIP.sub("", name).upper().replace(" ", "-")
        out.append(
            ElementSet(
                satellite_id=satellite_id,
                display_name=name,
                norad_id=int(line1[2:7]),
                line1=line1,
                line2=line2,
                epoch=_epoch_from_line1(line1),
            )
        )
    return out


# One mode table for every satellite in the snapshot.
#
# Not per-satellite parameters, and that is a deliberate simplification worth
# admitting: real SAR platforms differ, and the numbers below are
# representative of X-band SAR rather than measured from any operator's
# datasheet. What matters for the demo is that the three modes TRADE resolution
# against swath, because that trade is what makes one request contend with
# another. Per-satellite fidelity is a seed-data change, not a code change.
SENSOR_MODES = {
    "SPOTLIGHT": {
        "mode": "SPOTLIGHT",
        "resolution_m": 0.5,
        "swath_width_km": 5.0,
        "min_dwell_s": 8.0,
        "max_dwell_s": 30.0,
        "min_incidence_deg": 20.0,
        "max_incidence_deg": 50.0,
        "permitted_look_sides": ["LEFT", "RIGHT"],
    },
    "STRIPMAP": {
        "mode": "STRIPMAP",
        "resolution_m": 1.0,
        "swath_width_km": 30.0,
        "min_dwell_s": 10.0,
        "max_dwell_s": 60.0,
        "min_incidence_deg": 20.0,
        "max_incidence_deg": 45.0,
        "permitted_look_sides": ["LEFT", "RIGHT"],
    },
    "SCAN": {
        "mode": "SCAN",
        "resolution_m": 5.0,
        "swath_width_km": 100.0,
        "min_dwell_s": 15.0,
        "max_dwell_s": 120.0,
        "min_incidence_deg": 25.0,
        "max_incidence_deg": 45.0,
        "permitted_look_sides": ["RIGHT"],
    },
}

# Ten minutes of imaging per orbit. A duty cycle exists because a SAR is
# power-limited, and without one the planner has no reason to refuse anything —
# which would make every demo request win and show nothing.
DUTY_CYCLE_BUDGET_S = 600

# Customers spanning all four priority tiers.
#
# All four, so the fairness model is visible on the first run rather than
# needing to be explained. The tier lives on the REQUEST rather than the
# customer — a government agency can file a best-effort request — so these are
# the customers whose demo requests exercise each tier.
CUSTOMERS = [
    ("nl-coastguard", "Netherlands Coastguard", "GOVERNMENT"),
    ("eu-civil-protection", "EU Civil Protection Mechanism", "CIVIL_PROTECTION"),
    ("port-authority-nl", "Port of Rotterdam Authority", "COMMERCIAL"),
    ("acme-imaging", "Acme Imaging BV", "BEST_EFFORT"),
]


def seed(dsn: str) -> int:
    element_sets = read_snapshot(SNAPSHOT)
    modes = json.dumps(SENSOR_MODES)

    with psycopg.connect(dsn) as connection, connection.cursor() as cursor:
        # One transaction. A half-seeded database is worse than an empty one:
        # the foreign keys hold, so it looks populated and the demo fails later
        # and elsewhere.
        for customer_id, display_name, tier in CUSTOMERS:
            cursor.execute(
                """
                INSERT INTO reference.customers (customer_id, display_name)
                VALUES (%s, %s)
                ON CONFLICT (customer_id) DO UPDATE SET display_name = EXCLUDED.display_name
                """,
                (customer_id, display_name),
            )
            print(f"  {GREEN}ok{RESET}   customer {customer_id} {DIM}({tier}){RESET}")

        for element in element_sets:
            cursor.execute(
                """
                INSERT INTO reference.satellites
                    (satellite_id, norad_id, display_name, sensor_modes, duty_cycle_budget_s)
                VALUES (%s, %s, %s, %s::jsonb, %s)
                ON CONFLICT (satellite_id) DO UPDATE SET
                    norad_id            = EXCLUDED.norad_id,
                    display_name        = EXCLUDED.display_name,
                    sensor_modes        = EXCLUDED.sensor_modes,
                    duty_cycle_budget_s = EXCLUDED.duty_cycle_budget_s
                """,
                (
                    element.satellite_id,
                    element.norad_id,
                    element.display_name,
                    modes,
                    DUTY_CYCLE_BUDGET_S,
                ),
            )
            # Keyed on (satellite_id, epoch), so re-seeding the same snapshot
            # updates one row while a genuinely newer element set adds one. The
            # history is what makes "which TLE produced this window" answerable.
            cursor.execute(
                """
                INSERT INTO reference.tle_sets
                    (satellite_id, norad_id, line1, line2, epoch, source, fetched_at)
                VALUES (%s, %s, %s, %s, %s, %s::jsonb, %s)
                ON CONFLICT (satellite_id, epoch) DO UPDATE SET
                    line1 = EXCLUDED.line1,
                    line2 = EXCLUDED.line2
                """,
                (
                    element.satellite_id,
                    element.norad_id,
                    element.line1,
                    element.line2,
                    element.epoch,
                    # jsonb, not a string: the column carries fetch provenance,
                    # and the whole point of ADR-0011's frozen snapshot is being
                    # able to answer "where did this element set come from"
                    # months later without guessing.
                    json.dumps(
                        {
                            "kind": "frozen-snapshot",
                            "file": SNAPSHOT.name,
                            "reason": "offline seed — see ADR-0011",
                        }
                    ),
                    datetime.now(UTC),
                ),
            )
            print(
                f"  {GREEN}ok{RESET}   satellite {element.satellite_id} "
                f"{DIM}(NORAD {element.norad_id}, epoch {element.epoch:%Y-%m-%d %H:%M}Z){RESET}"
            )

    return len(element_sets)


def main() -> int:
    dsn = os.getenv(
        "DATABASE_URL", "postgres://overpass:overpass@localhost:5433/overpass"
    )
    print(f"\n{YELLOW}Seeding from {SNAPSHOT.relative_to(ROOT)}{RESET}")
    print(f"{DIM}offline by design — see ADR-0011{RESET}\n")

    count = seed(dsn)
    print(
        f"\n{GREEN}seeded{RESET} {len(CUSTOMERS)} customers across four tiers "
        f"and {count} satellites\n"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())

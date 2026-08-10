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
from dataclasses import dataclass, replace
from datetime import UTC, datetime, timedelta
from pathlib import Path

import psycopg

ROOT = Path(__file__).resolve().parent.parent
SNAPSHOT = ROOT / "testdata" / "tle" / "sar-constellation.2026-08-07.tle"
# The second observation of the same nine spacecraft, three days later. What the
# simulator treats as where they ACTUALLY were (#62).
TRUTH_SNAPSHOT = ROOT / "testdata" / "tle" / "sar-constellation.2026-08-10.tle"

# Where the OLDEST planning element set is placed relative to now.
#
# 70 hours, deliberately just inside StalenessPolicy's 72. Every other planning
# set is newer than this one by its real margin, so the whole constellation
# stays usable while keeping the real spread between satellites — and the demo
# runs close enough to the threshold that TLE_DRIFT_MISS is a live possibility
# rather than a theoretical one.
OLDEST_PLANNING_AGE_HOURS = 70.0

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


def _checksum(line: str) -> int:
    """The mod-10 TLE checksum over columns 1-68.

    Digits count as themselves, a minus sign counts as 1, everything else
    counts as zero. Duplicated from feasibility's tle_checksum rather than
    imported: this script depends on the standard library only, and reaching
    into a service package for eleven lines would give the seeder a dependency
    on the thing it seeds.
    """
    return sum(int(c) if c.isdigit() else 1 if c == "-" else 0 for c in line[:68]) % 10


def rebase_epoch(element: ElementSet, at: datetime) -> ElementSet:
    """Return the same orbit, stamped with a current epoch.

    THE FROZEN SNAPSHOT AGES, AND FEASIBILITY REFUSES STALE ELEMENT SETS.

    testdata/tle/ is frozen on purpose — the golden orbital tests assert
    against those exact element sets, and refreshing the file turns a
    regression suite into a snapshot of whatever the code did that day. But
    `StalenessPolicy` refuses to compute against anything 72 hours old, so a
    snapshot that is correct as a test fixture becomes unusable as seed data
    about three days after it is taken. Left alone, `make demo` refuses every
    request as TLE_STALE and the integration suite times out waiting for
    opportunities that can never arrive (#187).

    Rebasing separates the two uses instead of trading one off against the
    other. The golden tests keep reading the file directly and are untouched;
    the DATABASE gets the same mean elements stamped with a current epoch —
    the same orbit, observed now.

    What this does and does not preserve, stated plainly: inclination, RAAN,
    eccentricity, argument of perigee, mean anomaly and mean motion are all
    carried over unchanged, so the orbit's SHAPE is identical. Its PHASE
    relative to wall-clock time is not — the satellite is where it would be if
    those elements had been observed now rather than on the snapshot date. For
    a demo constellation that is the intent; for the golden tests it would be
    wrong, which is exactly why they read the file and this does not touch it.
    """
    day_of_year = (at - datetime(at.year, 1, 1, tzinfo=UTC)).total_seconds() / 86400.0 + 1.0
    stamped = f"{at.year % 100:02d}{day_of_year:012.8f}"
    line1 = element.line1[:18] + stamped + element.line1[32:]
    # Column 69 is the checksum, and it is over the columns just rewritten. A
    # line whose checksum no longer matches is rejected by the parser on the
    # way back in, which would turn this fix into a different failure.
    line1 = line1[:68] + str(_checksum(line1))
    return replace(element, line1=line1, epoch=_epoch_from_line1(line1))


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

    # Stamp the frozen orbits with a current epoch unless told not to. An hour
    # ago rather than exactly now, so the constellation reads as freshly
    # observed rather than as a set of predictions — StalenessPolicy calls
    # anything under 24h fresh, and a future epoch is a real thing that only
    # confuses a demo.
    #
    # OVERPASS_SEED_REBASE_EPOCH=0 opts out, for anyone who wants the database
    # to hold exactly what the file holds. The Python unit suite sets it,
    # because those tests pin their clock to the snapshot's era — 2026-08-07 in
    # test_ephemeris_sweep.py — and `newest_element_sets` filters `epoch <= at`,
    # so a rebased epoch would sit in their future and read as unseeded.
    #
    # Tests that assert against frozen physics seed frozen data. The demo,
    # which asserts nothing and has to actually work, gets the rebase.
    truth_sets = read_snapshot(TRUTH_SNAPSHOT)

    if os.getenv("OVERPASS_SEED_REBASE_EPOCH", "1") != "0":
        # ONE SHARED SHIFT, NOT A REBASE PER ELEMENT SET.
        #
        # Rebasing each set independently to "now" would destroy the very thing
        # the second snapshot exists to provide. Two element sets stamped with
        # the same epoch have no drift between them, and rebasing them to
        # DIFFERENT epochs is worse: the mean anomaly is carried over unchanged,
        # so shifting an epoch moves the satellite along its orbit by the shift.
        # Measured before this was built — a six-hour independent shift on a
        # 95-minute orbit separated the two propagations by 700 to 13,000 km,
        # which is a different point on the orbit rather than a drifted one. The
        # rebase_epoch docstring says the same thing about phase; this is what
        # it costs when two sets have to be compared.
        #
        # Sliding both files by one delta preserves every real relationship:
        # each satellite's age relative to the others, and the 90-to-146-hour
        # separation between the planning observation and the truth one. The
        # drift the simulator sees is then the drift Celestrak actually
        # recorded.
        oldest = min(element.epoch for element in element_sets)
        shift = (datetime.now(UTC) - timedelta(hours=OLDEST_PLANNING_AGE_HOURS)) - oldest
        element_sets = [rebase_epoch(element, element.epoch + shift) for element in element_sets]
        truth_sets = [rebase_epoch(element, element.epoch + shift) for element in truth_sets]

        newest = max(element.epoch for element in element_sets)
        print(
            f"  {DIM}planning epochs shifted by {shift.total_seconds() / 3600:+.1f}h "
            f"(ages {OLDEST_PLANNING_AGE_HOURS:.0f}h to "
            f"{(datetime.now(UTC) - newest).total_seconds() / 3600:.0f}h){RESET}"
        )
        # The truth epochs land in the FUTURE, and that is what makes them
        # invisible to planning. newest_element_sets selects
        # `WHERE epoch <= at ORDER BY epoch DESC`, so feasibility keeps using the
        # planning observation while the simulator, asking after the fact, gets
        # the one that superseded it. No change to feasibility is needed for
        # this, which is the point of doing it here.
        print(
            f"  {DIM}truth epochs {(min(e.epoch for e in truth_sets) - datetime.now(UTC)).total_seconds() / 3600:+.0f}h "
            f"to {(max(e.epoch for e in truth_sets) - datetime.now(UTC)).total_seconds() / 3600:+.0f}h "
            f"— future, so planning cannot see them{RESET}"
        )
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

        # THE SNAPSHOT ROWS ARE REPLACED, NOT ACCUMULATED.
        #
        # seed.py calls itself idempotent by construction, and for element sets
        # that was only true before rebasing existed. The upsert is keyed on
        # (satellite_id, epoch), and every run stamps a different epoch — so
        # each re-seed left the previous run's rows behind. Harmless-looking,
        # and not harmless: newest_element_sets picks the newest row at or
        # before now, so yesterday's leftovers silently shadowed today's
        # planning sets. Measured after adding the truth snapshot — three sets
        # per satellite, and the one feasibility selected was 5 hours old
        # instead of the intended 70.
        #
        # Only rows this script wrote are removed. Anything fetched live keeps
        # its history, which is what the source column is for.
        cursor.execute(
            """
            DELETE FROM reference.tle_sets
            WHERE source->>'kind' = 'frozen-snapshot'
            """
        )

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
            # history is what makes "which TLE produced this window" answerable
            # — and what lets the truth snapshot sit alongside the planning one
            # rather than replacing it.
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

        # The truth observation, stored beside the planning one rather than
        # instead of it. No reference.satellites row is written from here: the
        # spacecraft are the same spacecraft, and writing them twice would let
        # the two files disagree about a duty-cycle budget.
        for element in truth_sets:
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
                    json.dumps(
                        {
                            "kind": "frozen-snapshot",
                            "file": TRUTH_SNAPSHOT.name,
                            "reason": "post-pass orbit determination — the truth set for #62",
                        }
                    ),
                    datetime.now(UTC),
                ),
            )
        print(
            f"  {GREEN}ok{RESET}   {len(truth_sets)} truth element sets "
            f"{DIM}(from {TRUTH_SNAPSHOT.name}){RESET}"
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

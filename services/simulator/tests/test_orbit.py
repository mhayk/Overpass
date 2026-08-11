"""Golden-reference tests for the drift geometry.

AGAINST FROZEN FIXTURES AT A PINNED INSTANT, so these assert physics rather than
asserting that the code agrees with itself. Both element sets are committed and
never regenerated, and the instant is a constant — so every number below is
reproducible forever and a change to any of them means the propagation changed,
not that the world moved.

This is also the check that keeps two SGP4 code paths honest. feasibility owns
the propagation this system plans against; simulator/orbit.py owns a second,
smaller one for asking where a spacecraft ended up. A divergence between them
shows up here as a changed number.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

from simulator.execution import GroundPoint
from simulator.orbit import ElementSet, Propagator, closest_approach_km, separation_km

REPO = Path(__file__).resolve().parents[3]
PLANNING = REPO / "testdata" / "tle" / "sar-constellation.2026-08-07.tle"
TRUTH = REPO / "testdata" / "tle" / "sar-constellation.2026-08-10.tle"

#: A constant, not "now". The whole point of a golden test is that it does not
#: move.
AT = datetime(2026, 8, 11, 12, 0, 0, tzinfo=UTC)

#: Measured separations between the two real observations, in km, at AT.
#:
#: These are the numbers ADR-0021 is built on: the drift grows with the age of
#: the planning element set, and it is large enough to matter against a
#: SPOTLIGHT half-swath of 2.5 km while mostly harmless against SCAN's 50 km.
GOLDEN_SEPARATION_KM = {
    "SENTINEL-1A": 243.4,
    "SENTINEL-1B": 0.8,
    "SENTINEL-1C": 3.0,
    "ICEYE-X2": 8.4,
    "ICEYE-X5": 6.5,
    "ICEYE-X4": 1.6,
    "CAPELLA-11": 224.0,
    "CAPELLA-14": 911.6,
    "CAPELLA-13": 33.6,
}


def read_snapshot(path: Path) -> dict[str, ElementSet]:
    lines = [
        line.rstrip()
        for line in path.read_text().splitlines()
        if line.strip() and not line.startswith("#")
    ]
    out: dict[str, ElementSet] = {}
    for i in range(0, len(lines) - 2, 3):
        name, line1, line2 = lines[i].strip(), lines[i + 1], lines[i + 2]
        year = 2000 + int(line1[18:20])
        day_of_year = float(line1[20:32])
        epoch = datetime(year, 1, 1, tzinfo=UTC) + timedelta(days=day_of_year - 1)
        # Keyed on the bare satellite id: Celestrak's display strings differ
        # between the two files — "CAPELLA-11 (ACADIA-1)" against "CAPELLA-11" —
        # and joining on them would silently pair nothing.
        key = name.split(" ")[0]
        out[key] = ElementSet(satellite_id=key, epoch=epoch, line1=line1, line2=line2)
    return out


@pytest.fixture(scope="module")
def planning() -> dict[str, ElementSet]:
    return read_snapshot(PLANNING)


@pytest.fixture(scope="module")
def truth() -> dict[str, ElementSet]:
    return read_snapshot(TRUTH)


def test_both_snapshots_cover_the_same_constellation(
    planning: dict[str, ElementSet], truth: dict[str, ElementSet]
) -> None:
    # A satellite in one file and not the other would silently lose its drift —
    # the simulator would find no truth set and report a clean pass.
    assert set(planning) == set(truth)
    assert len(planning) == 9


@pytest.mark.parametrize(("satellite_id", "expected_km"), GOLDEN_SEPARATION_KM.items())
def test_separation_matches_the_frozen_measurement(
    planning: dict[str, ElementSet],
    truth: dict[str, ElementSet],
    satellite_id: str,
    expected_km: float,
) -> None:
    got = separation_km(Propagator(planning[satellite_id]), Propagator(truth[satellite_id]), AT)
    # 1% or 100 m, whichever is larger. Tight enough that a propagation change
    # fails, loose enough to survive a skyfield patch release rounding
    # differently in the last digit.
    assert got == pytest.approx(expected_km, rel=0.01, abs=0.1)


def test_drift_is_larger_for_older_planning_sets(
    planning: dict[str, ElementSet], truth: dict[str, ElementSet]
) -> None:
    # THE CORRELATION THE ACCEPTANCE CRITERION ASKS FOR, asserted against the
    # data rather than against a formula. Nothing here configures it: the three
    # spacecraft whose planning observation is oldest are the three that drift
    # by hundreds of kilometres.
    ages = {satellite_id: element.age_hours(AT) for satellite_id, element in planning.items()}
    oldest = sorted(ages, key=lambda s: ages[s], reverse=True)[:3]
    newest = sorted(ages, key=lambda s: ages[s])[:3]

    worst_of_newest = max(GOLDEN_SEPARATION_KM[s] for s in newest)
    best_of_oldest = min(GOLDEN_SEPARATION_KM[s] for s in oldest)

    assert best_of_oldest > worst_of_newest, (
        f"oldest {oldest} drifted {best_of_oldest:.1f} km at best, "
        f"newest {newest} drifted {worst_of_newest:.1f} km at worst — "
        "the correlation with staleness is not in this data"
    )


def test_a_naive_datetime_is_refused(planning: dict[str, ElementSet]) -> None:
    propagator = Propagator(planning["SENTINEL-1B"])
    with pytest.raises(ValueError, match="naive datetime"):
        propagator.subpoint(datetime(2026, 8, 11, 12, 0, 0))


class TestClosestApproach:
    def test_the_subpoint_is_zero_from_itself(self, planning: dict[str, ElementSet]) -> None:
        propagator = Propagator(planning["SENTINEL-1B"])
        beneath = propagator.subpoint(AT)
        approach = closest_approach_km(propagator, beneath, (AT, AT + timedelta(seconds=1)))
        assert approach == pytest.approx(0.0, abs=1.0)

    def test_a_target_off_the_track_is_far(self, planning: dict[str, ElementSet]) -> None:
        propagator = Propagator(planning["SENTINEL-1B"])
        beneath = propagator.subpoint(AT)
        # A degree of latitude is about 111 km, and the pass cannot close that
        # inside a fifteen-second window.
        offset = GroundPoint(beneath.latitude_deg + 5.0, beneath.longitude_deg)
        approach = closest_approach_km(propagator, offset, (AT, AT + timedelta(seconds=15)))
        assert approach > 100.0

    def test_an_empty_window_is_refused(self, planning: dict[str, ElementSet]) -> None:
        propagator = Propagator(planning["SENTINEL-1B"])
        with pytest.raises(ValueError, match="window must be non-empty"):
            closest_approach_km(propagator, GroundPoint(0.0, 0.0), (AT, AT))

    def test_the_end_of_the_window_is_always_sampled(self, planning: dict[str, ElementSet]) -> None:
        # A window shorter than one step would otherwise be measured only at its
        # start, and the closest approach can be at either end.
        propagator = Propagator(planning["SENTINEL-1B"])
        end = AT + timedelta(milliseconds=200)
        beneath_end = propagator.subpoint(end)
        approach = closest_approach_km(propagator, beneath_end, (AT, end))
        assert approach == pytest.approx(0.0, abs=1.0)

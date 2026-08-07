"""Golden-reference tests for the orbital math.

Physics needs an oracle. A snapshot of our own output asserts only that the code
still does what it did yesterday, including if yesterday was wrong — so every
test here compares against something we did not write.

Three oracles, each covering a different layer:

  PROPAGATION is checked in `test_vallado_vectors.py`, against the output of the
  reference C++ implementation that ships with the `sgp4` package.

  THE ACCESS SEARCH is checked here against Skyfield's own `find_events`, which
  solves the same problem by a different method — root-finding on its own
  altitude function rather than our coarse sample plus bisection. Two
  independent algorithms agreeing on a rise time is evidence; our algorithm
  agreeing with itself is not.

  THE FRAME CHAIN is checked here against a hand-rolled TEME-to-geodetic path
  built from raw `sgp4` output, a GMST rotation and an iterative geodetic
  solution — deliberately NOT Skyfield's, so a fault in Skyfield's ITRS handling
  would show up rather than cancel out.

THE GOLDEN FILE CANNOT BE LAUNDERED. `testdata/scenarios/golden-access-windows.json`
records expected windows, and regenerating it from broken code would be the
obvious way to make a failing test pass. So the file is checked twice: current
code must match it, AND it must match Skyfield's independent finder. Anyone who
regenerates it from a broken propagator fails the second check.

Every tolerance below is derived from something, and the derivation is written
next to it. A tolerance tuned until the test passes is a test that asserts
nothing.
"""

from __future__ import annotations

import json
import math
from datetime import UTC, datetime, timedelta
from itertools import pairwise
from pathlib import Path

import pytest
from sgp4.api import Satrec, jday
from skyfield.api import wgs84

from feasibility.orbit import (
    AccessSearchPolicy,
    GroundPoint,
    Propagator,
    search,
    timescale,
)
from feasibility.tle.element_set import ElementSet, parse_catalogue


def _repo_root() -> Path:
    for parent in Path(__file__).resolve().parents:
        if (parent / "testdata").is_dir() and (parent / "contracts").is_dir():
            return parent
    msg = "could not locate the repository root"
    raise RuntimeError(msg)


ROOT = _repo_root()
SNAPSHOT = ROOT / "testdata" / "tle" / "sar-constellation.2026-08-07.tle"
GOLDEN = ROOT / "testdata" / "scenarios" / "golden-access-windows.json"

# ---------------------------------------------------------------------------
# Tolerances, and where each one comes from
# ---------------------------------------------------------------------------

# Our bisection converges to `refine_tolerance_s` (1.0 s) and deliberately
# returns the edge INSIDE the window, so our boundaries are conservative by up
# to a second against any oracle that returns the true crossing. Skyfield's
# find_events uses its own root finder with no such bias. The permitted
# disagreement is therefore that one second, plus a little for the two methods
# evaluating altitude at slightly different points.
#
# Measured on the frozen snapshot: our starts run +0.49 to +0.66 s late and our
# ends -0.35 to -0.84 s early. Every observation is inside one second AND in the
# conservative direction, which is what the convention predicts — so this bound
# is a prediction that held, not a number fitted to the data.
WINDOW_BOUNDARY_TOLERANCE_S = 1.5

# Our subpoint goes through Skyfield's full ITRS chain: precession, nutation,
# polar motion. The independent path below uses the classical TEME rotation by
# GMST alone. The difference between them is dominated by polar motion, which is
# a few tenths of an arcsecond — tens of metres at the surface — plus the
# equation of the equinoxes.
#
# One kilometre is roughly thirty times the largest separation actually observed
# (30 m over 24 hours). It is set well above the correction terms so that it can
# only fail for a real fault — a wrong frame, a sign error in the rotation, a
# swapped latitude and longitude — and not for a missing refinement.
SUBPOINT_TOLERANCE_KM = 1.0

# WGS84, from the defining constants rather than a rounded copy.
_WGS84_A_KM = 6378.137
_WGS84_F = 1.0 / 298.257223563
_WGS84_E2 = _WGS84_F * (2.0 - _WGS84_F)


def _gmst_rad(jd_ut1: float) -> float:
    """Greenwich Mean Sidereal Time, IAU 1982 series.

    The classical rotation TEME is defined against. Independent of Skyfield's
    implementation on purpose — that is the whole point of this cross-check.
    """
    t = (jd_ut1 - 2451545.0) / 36525.0
    seconds = (
        67310.54841 + (876600.0 * 3600.0 + 8640184.812866) * t + 0.093104 * t * t - 6.2e-6 * t**3
    )
    return math.radians((seconds % 86400.0) / 240.0)


def _ecef_to_geodetic(x: float, y: float, z: float) -> tuple[float, float, float]:
    """Bowring-style iteration to geodetic latitude, longitude and height."""
    longitude = math.atan2(y, x)
    equatorial = math.hypot(x, y)
    latitude = math.atan2(z, equatorial * (1.0 - _WGS84_E2))
    height_km = 0.0
    for _ in range(8):
        prime_vertical = _WGS84_A_KM / math.sqrt(1.0 - _WGS84_E2 * math.sin(latitude) ** 2)
        height_km = equatorial / math.cos(latitude) - prime_vertical
        latitude = math.atan2(
            z, equatorial * (1.0 - _WGS84_E2 * prime_vertical / (prime_vertical + height_km))
        )
    return math.degrees(latitude), math.degrees(longitude), height_km * 1000.0


def independent_subpoint(element_set: ElementSet, when: datetime) -> tuple[float, float, float]:
    """Sub-satellite point via raw sgp4, a GMST rotation, and iteration.

    Shares no code with `Propagator.subpoint` beyond the element set itself.
    """
    satrec = Satrec.twoline2rv(element_set.line1, element_set.line2)
    jd, fr = jday(
        when.year,
        when.month,
        when.day,
        when.hour,
        when.minute,
        when.second + when.microsecond / 1e6,
    )
    error, teme_position, _velocity = satrec.sgp4(jd, fr)
    assert error == 0, f"propagation failed with code {error}"

    theta = _gmst_rad(jd + fr)
    cos_t, sin_t = math.cos(theta), math.sin(theta)
    x = cos_t * teme_position[0] + sin_t * teme_position[1]
    y = -sin_t * teme_position[0] + cos_t * teme_position[1]
    return _ecef_to_geodetic(x, y, teme_position[2])


@pytest.fixture(scope="module")
def catalogue() -> dict[str, ElementSet]:
    return {es.name: es for es in parse_catalogue(SNAPSHOT.read_text())}


@pytest.fixture(scope="module")
def golden() -> dict[str, object]:
    return json.loads(GOLDEN.read_text())  # type: ignore[no-any-return]


class TestFrozenFixtures:
    def test_the_snapshot_is_the_only_source(self, catalogue: dict[str, ElementSet]) -> None:
        # Never a live fetch. If this file ever imported the Celestrak client,
        # the suite would stop being reproducible and would fail offline.
        assert len(catalogue) == 9
        assert "SENTINEL-1A" in catalogue

    def test_the_golden_file_names_the_snapshot_it_came_from(
        self, golden: dict[str, object]
    ) -> None:
        assert golden["generated_from"] == "testdata/tle/sar-constellation.2026-08-07.tle"


class TestAccessWindowsAgainstAnIndependentFinder:
    """Skyfield's find_events solves the same problem by a different method."""

    @staticmethod
    def _skyfield_windows(
        propagator: Propagator, lat: float, lon: float, start: datetime, end: datetime, mask: float
    ) -> list[tuple[datetime, datetime]]:
        ts = timescale()
        site = wgs84.latlon(lat, lon)
        times, events = propagator.satellite.find_events(
            site, ts.from_datetime(start), ts.from_datetime(end), altitude_degrees=mask
        )
        windows: list[tuple[datetime, datetime]] = []
        rise: datetime | None = None
        for t, event in zip(times, events, strict=True):
            if event == 0:
                rise = t.utc_datetime()
            elif event == 2 and rise is not None:
                windows.append((rise, t.utc_datetime()))
                rise = None
        return windows

    def test_every_golden_case_matches_skyfield(
        self, catalogue: dict[str, ElementSet], golden: dict[str, object]
    ) -> None:
        start = datetime.fromisoformat(str(golden["horizon_start"]))
        hours = float(golden["horizon_hours"])  # type: ignore[arg-type]
        mask = float(golden["elevation_mask_deg"])  # type: ignore[arg-type]
        cases = golden["cases"]
        assert isinstance(cases, list)

        compared = 0
        for case in cases:
            propagator = Propagator(catalogue[case["satellite"]])
            expected = self._skyfield_windows(
                propagator, case["lat"], case["lon"], start, start + timedelta(hours=hours), mask
            )
            recorded = case["windows"]
            assert len(recorded) == len(expected), (
                f"{case['satellite']} over {case['site']}: golden has {len(recorded)} windows, "
                f"Skyfield finds {len(expected)}"
            )
            for row, (rise, set_) in zip(recorded, expected, strict=True):
                assert (
                    abs((datetime.fromisoformat(row["start"]) - rise).total_seconds())
                    <= WINDOW_BOUNDARY_TOLERANCE_S
                )
                assert (
                    abs((datetime.fromisoformat(row["end"]) - set_).total_seconds())
                    <= WINDOW_BOUNDARY_TOLERANCE_S
                )
                compared += 1

        assert compared >= 40, f"only {compared} windows compared — the golden file looks thin"

    def test_our_boundaries_are_conservative_not_merely_close(
        self, catalogue: dict[str, ElementSet], golden: dict[str, object]
    ) -> None:
        """A sharper claim than "within a second".

        Our bisection returns the edge inside the window, so every start should
        be at or after Skyfield's rise and every end at or before its set. A
        boundary that landed on the wrong side would still pass a symmetric
        tolerance while reporting visibility the geometry does not support.
        """
        start = datetime.fromisoformat(str(golden["horizon_start"]))
        cases = golden["cases"]
        assert isinstance(cases, list)
        case = cases[0]
        propagator = Propagator(catalogue[case["satellite"]])
        expected = self._skyfield_windows(
            propagator,
            case["lat"],
            case["lon"],
            start,
            start + timedelta(hours=float(golden["horizon_hours"])),  # type: ignore[arg-type]
            float(golden["elevation_mask_deg"]),  # type: ignore[arg-type]
        )
        ours = search(
            propagator,
            GroundPoint(case["lat"], case["lon"]),
            start,
            start + timedelta(hours=float(golden["horizon_hours"])),  # type: ignore[arg-type]
        )
        for w, (rise, set_) in zip(ours, expected, strict=True):
            assert w.start >= rise - timedelta(milliseconds=50)
            assert w.end <= set_ + timedelta(milliseconds=50)


class TestGoldenRegression:
    def test_current_code_reproduces_the_golden_windows(
        self, catalogue: dict[str, ElementSet], golden: dict[str, object]
    ) -> None:
        """The regression half. Meaningless alone, load-bearing next to the above.

        If someone regenerates the golden file from broken code this passes and
        `test_every_golden_case_matches_skyfield` fails, which is the point of
        having both.
        """
        start = datetime.fromisoformat(str(golden["horizon_start"]))
        horizon = timedelta(hours=float(golden["horizon_hours"]))  # type: ignore[arg-type]
        policy = AccessSearchPolicy(
            elevation_mask_deg=float(golden["elevation_mask_deg"])  # type: ignore[arg-type]
        )
        cases = golden["cases"]
        assert isinstance(cases, list)

        for case in cases:
            propagator = Propagator(catalogue[case["satellite"]])
            windows = search(
                propagator,
                GroundPoint(case["lat"], case["lon"]),
                start,
                start + horizon,
                policy,
            )
            assert len(windows) == len(case["windows"]), f"{case['satellite']}/{case['site']}"
            for w, row in zip(windows, case["windows"], strict=True):
                assert w.start == datetime.fromisoformat(row["start"])
                assert w.end == datetime.fromisoformat(row["end"])
                assert w.peak_elevation_deg == pytest.approx(row["peak_elevation_deg"], abs=1e-4)
                assert w.orbit_number == row["orbit_number"]


class TestSubpointAgainstAnIndependentPropagator:
    def test_agrees_across_a_full_day(self, catalogue: dict[str, ElementSet]) -> None:
        element_set = catalogue["SENTINEL-1A"]
        propagator = Propagator(element_set)
        base = datetime(2026, 8, 7, tzinfo=UTC)

        worst_km = 0.0
        for minutes in range(0, 24 * 60, 37):
            when = base + timedelta(minutes=minutes)
            lat, lon, _height = independent_subpoint(element_set, when)
            ours = propagator.subpoint(when)

            delta_lon = (lon - ours.longitude_deg + 180.0) % 360.0 - 180.0
            separation_km = math.hypot(
                (lat - ours.latitude_deg) * 111.32,
                delta_lon * 111.32 * math.cos(math.radians(lat)),
            )
            worst_km = max(worst_km, separation_km)

        assert worst_km <= SUBPOINT_TOLERANCE_KM, (
            f"subpoint disagrees with the independent path by {worst_km:.3f} km"
        )

    def test_the_cross_check_would_notice_a_swapped_latitude_and_longitude(
        self, catalogue: dict[str, ElementSet]
    ) -> None:
        # Guards the guard: a comparison this loose could hide a real fault if
        # the tolerance were far too generous. Swapping the two coordinates is
        # the classic version of that fault, and it must be caught.
        element_set = catalogue["SENTINEL-1A"]
        when = datetime(2026, 8, 7, 3, 14, tzinfo=UTC)
        lat, lon, _height = independent_subpoint(element_set, when)
        ours = Propagator(element_set).subpoint(when)

        swapped_delta_lon = (lat - ours.longitude_deg + 180.0) % 360.0 - 180.0
        swapped_km = math.hypot(
            (lon - ours.latitude_deg) * 111.32,
            swapped_delta_lon * 111.32 * math.cos(math.radians(lon)),
        )
        assert swapped_km > SUBPOINT_TOLERANCE_KM * 100

    def test_altitude_is_physically_plausible(self, catalogue: dict[str, ElementSet]) -> None:
        # Sentinel-1A flies a ~693 km sun-synchronous orbit. A frame error that
        # left the latitude and longitude looking fine would still move this.
        element_set = catalogue["SENTINEL-1A"]
        for minutes in (0, 300, 900):
            _lat, _lon, height_m = independent_subpoint(
                element_set, datetime(2026, 8, 7, tzinfo=UTC) + timedelta(minutes=minutes)
            )
            assert 650_000 < height_m < 750_000


class TestPhysicalInvariants:
    def test_pass_spacing_matches_the_orbital_period(
        self, catalogue: dict[str, ElementSet]
    ) -> None:
        """An oracle that needs no library at all.

        Consecutive passes over one site come one orbit apart, and the orbital
        period follows from the mean motion recorded in the element set. If the
        search invented or dropped a pass, the spacing would not be a whole
        number of periods.
        """
        propagator = Propagator(catalogue["SENTINEL-1A"])
        period_minutes = 1440.0 / propagator.revolutions_per_day()
        base = datetime(2026, 8, 7, tzinfo=UTC)
        windows = search(
            propagator, GroundPoint(78.2232, 15.6267), base, base + timedelta(hours=24)
        )
        assert len(windows) >= 8, "expected frequent passes at high latitude"

        for earlier, later in pairwise(windows):
            gap_minutes = (later.peak_at - earlier.peak_at).total_seconds() / 60.0
            orbits = gap_minutes / period_minutes
            assert abs(orbits - round(orbits)) < 0.15, (
                f"passes {gap_minutes:.1f} minutes apart is {orbits:.2f} orbits, not a whole number"
            )

    def test_orbit_number_advances_with_the_pass_spacing(
        self, catalogue: dict[str, ElementSet]
    ) -> None:
        propagator = Propagator(catalogue["SENTINEL-1A"])
        period_minutes = 1440.0 / propagator.revolutions_per_day()
        base = datetime(2026, 8, 7, tzinfo=UTC)
        windows = search(
            propagator, GroundPoint(78.2232, 15.6267), base, base + timedelta(hours=24)
        )

        for earlier, later in pairwise(windows):
            orbits = (later.peak_at - earlier.peak_at).total_seconds() / 60.0 / period_minutes
            assert later.orbit_number - earlier.orbit_number == pytest.approx(round(orbits), abs=1)

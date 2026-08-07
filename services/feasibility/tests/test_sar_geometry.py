"""SAR acquisition geometry.

The first test in this file is the one that matters. Everything else can pass
while the instrument being modelled is a nadir-pointing camera rather than a
side-looking radar; only the nadir canary tells them apart.
"""

from __future__ import annotations

import math
from datetime import UTC, datetime, timedelta

import pytest
from pyproj import Geod

from feasibility.orbit import GroundPoint, Propagator, search
from feasibility.sar import (
    AcquisitionConstraints,
    ImagingMode,
    LookSide,
    SensorMode,
    compute,
    effective_limits,
    quality_score,
    satisfies,
)
from feasibility.tle.element_set import parse

S1A_L1 = "1 39634U 14016A   26217.94446112  .00000085  00000+0  27560-4 0  9992"
S1A_L2 = "2 39634  98.1585 224.7210 0001387  84.4412 275.6946 14.59278904657278"

LISBON = GroundPoint(latitude_deg=38.7223, longitude_deg=-9.1393)
T0 = datetime(2026, 8, 7, tzinfo=UTC)

EARTH_RADIUS_KM = 6371.0
GEOD = Geod(ellps="WGS84")


@pytest.fixture
def propagator() -> Propagator:
    return Propagator(parse("SENTINEL-1A", S1A_L1, S1A_L2))


@pytest.fixture
def stripmap() -> SensorMode:
    return SensorMode(
        mode=ImagingMode.STRIPMAP,
        swath_width_km=80.0,
        resolution_m=5.0,
        min_dwell_s=3.0,
        max_dwell_s=30.0,
        max_squint_deg=5.0,
    )


@pytest.fixture
def a_pass(propagator: Propagator) -> tuple[datetime, datetime]:
    w = search(propagator, LISBON, T0, T0 + timedelta(hours=24))[0]
    return w.start, w.end


class TestNadirCanary:
    """The single most important domain fact: SAR is side-looking."""

    def test_a_target_at_nadir_produces_no_opportunity(
        self, propagator: Propagator, stripmap: SensorMode, a_pass: tuple[datetime, datetime]
    ) -> None:
        # Put the target exactly beneath the satellite. A nadir-pointing sensor
        # would call this the best possible geometry; a side-looking radar
        # cannot image it at all.
        start, end = a_pass
        peak = start + (end - start) / 2
        sub = propagator.subpoint(peak)
        nadir_target = GroundPoint(sub.latitude_deg, sub.longitude_deg)

        geometry = compute(propagator, nadir_target, peak)

        assert geometry.incidence_angle_deg == pytest.approx(0.0, abs=0.5)
        assert not satisfies(geometry, stripmap)
        assert quality_score(geometry, stripmap) == 0.0

    def test_the_canary_would_notice_a_nadir_pointing_sensor(self) -> None:
        # If someone "fixed" the band to start at zero, the canary above must
        # still refuse — because a mode whose incidence floor is zero is not a
        # SAR mode. This guards the guard.
        with pytest.raises(ValueError, match="incidence band"):
            SensorMode(
                mode=ImagingMode.STRIPMAP,
                swath_width_km=80.0,
                resolution_m=5.0,
                min_dwell_s=3.0,
                max_dwell_s=30.0,
                min_incidence_deg=50.0,
                max_incidence_deg=50.0,
            )


class TestLookSide:
    """Flying north, east is on your right. That is the whole oracle.

    It catches a cross-product sign error, which would otherwise mirror every
    acquisition in the system onto the wrong side of the ground track — and
    nothing else here would notice, because every angle would still be
    perfectly plausible.

    The instant has to be chosen carefully. A first attempt at this test picked
    a moment at 82 degrees south, where the orbit is turning over and "east"
    spans 46 km rather than 250 — the dot products came out at 0.004, which is
    noise, and the test failed against correct code. Mid latitude, and a
    geodesic offset in kilometres rather than degrees of longitude.
    """

    @staticmethod
    def _instant_heading(propagator: Propagator, *, north: bool) -> tuple[datetime, GroundPoint]:
        for i in range(0, 12000, 10):
            t = T0 + timedelta(seconds=i)
            here = propagator.subpoint(t)
            later = propagator.subpoint(t + timedelta(seconds=10))
            if abs(here.latitude_deg) > 50.0:
                continue
            rising = later.latitude_deg > here.latitude_deg
            if rising is north and abs(later.latitude_deg - here.latitude_deg) > 0.03:
                return t, here
        pytest.fail("no well-conditioned mid-latitude instant found")

    @staticmethod
    def _offset(point: GroundPoint, azimuth_deg: float, km: float) -> GroundPoint:
        lon, lat, _back = GEOD.fwd(
            point.longitude_deg, point.latitude_deg, azimuth_deg, km * 1000.0
        )
        return GroundPoint(lat, lon)

    def test_northbound_sees_east_on_the_right_and_west_on_the_left(
        self, propagator: Propagator
    ) -> None:
        when, sub = self._instant_heading(propagator, north=True)
        east = self._offset(sub, 90.0, 300.0)
        west = self._offset(sub, 270.0, 300.0)
        assert compute(propagator, east, when).look_side is LookSide.RIGHT
        assert compute(propagator, west, when).look_side is LookSide.LEFT

    def test_southbound_reverses_it(self, propagator: Propagator) -> None:
        when, sub = self._instant_heading(propagator, north=False)
        east = self._offset(sub, 90.0, 300.0)
        west = self._offset(sub, 270.0, 300.0)
        assert compute(propagator, east, when).look_side is LookSide.LEFT
        assert compute(propagator, west, when).look_side is LookSide.RIGHT


class TestRoll:
    def test_roll_matches_the_spherical_relation(
        self, propagator: Propagator, a_pass: tuple[datetime, datetime]
    ) -> None:
        """An independent derivation, not a snapshot of our own output.

        On a spherical Earth the off-nadir angle and the incidence angle are
        related by sin(roll) = Re / (Re + h) * sin(incidence). Our roll is
        computed directly from the vectors on the ellipsoid, so the two should
        agree to within the sphere-versus-ellipsoid difference — about a degree.
        Agreement to that level is strong evidence the direct computation is
        measuring the angle it claims to.
        """
        start, end = a_pass
        for frac in (0.3, 0.5, 0.7):
            when = start + (end - start) * frac
            geometry = compute(propagator, LISBON, when)
            altitude_km = propagator.subpoint(when).elevation_m / 1000.0

            ratio = EARTH_RADIUS_KM / (EARTH_RADIUS_KM + altitude_km)
            expected = math.degrees(
                math.asin(ratio * math.sin(math.radians(geometry.incidence_angle_deg)))
            )
            assert abs(geometry.roll_angle_deg) == pytest.approx(expected, abs=1.0)

    def test_roll_is_always_smaller_than_incidence(
        self, propagator: Propagator, a_pass: tuple[datetime, datetime]
    ) -> None:
        # Earth curvature guarantees it. If roll ever exceeded incidence, the
        # two are being measured from the same origin, which means one of them
        # is wrong.
        start, end = a_pass
        for frac in (0.1, 0.3, 0.5, 0.7, 0.9):
            g = compute(propagator, LISBON, start + (end - start) * frac)
            assert abs(g.roll_angle_deg) < g.incidence_angle_deg

    def test_roll_sign_follows_look_side(
        self, propagator: Propagator, a_pass: tuple[datetime, datetime]
    ) -> None:
        # Roll feeds slew_time(a, b) in M2. A sign that did not track the look
        # side would make the slew between a left-looking and a right-looking
        # acquisition look free.
        start, end = a_pass
        g = compute(propagator, LISBON, start + (end - start) / 2)
        if g.look_side is LookSide.RIGHT:
            assert g.roll_angle_deg > 0
        else:
            assert g.roll_angle_deg < 0


class TestSquint:
    def test_squint_runs_fore_to_aft_across_a_pass(
        self, propagator: Propagator, a_pass: tuple[datetime, datetime]
    ) -> None:
        # Approaching, the target is ahead of broadside (positive). Departing,
        # behind it (negative). Crossing zero near closest approach is what
        # broadside means.
        start, end = a_pass
        early = compute(propagator, LISBON, start + (end - start) * 0.2).squint_angle_deg
        middle = compute(propagator, LISBON, start + (end - start) * 0.5).squint_angle_deg
        late = compute(propagator, LISBON, start + (end - start) * 0.8).squint_angle_deg

        assert early > 0
        assert late < 0
        assert abs(middle) < abs(early)
        assert abs(middle) < abs(late)

    def test_slant_range_is_least_near_broadside(
        self, propagator: Propagator, a_pass: tuple[datetime, datetime]
    ) -> None:
        start, end = a_pass
        ranges = [
            compute(propagator, LISBON, start + (end - start) * f).slant_range_km
            for f in (0.1, 0.5, 0.9)
        ]
        assert ranges[1] < ranges[0]
        assert ranges[1] < ranges[2]


class TestIncidence:
    def test_incidence_and_elevation_are_complementary(
        self, propagator: Propagator, a_pass: tuple[datetime, datetime]
    ) -> None:
        start, end = a_pass
        g = compute(propagator, LISBON, start + (end - start) / 2)
        assert g.incidence_angle_deg + g.elevation_angle_deg == pytest.approx(90.0)

    def test_incidence_is_least_at_closest_approach(
        self, propagator: Propagator, a_pass: tuple[datetime, datetime]
    ) -> None:
        start, end = a_pass
        values = [
            compute(propagator, LISBON, start + (end - start) * f).incidence_angle_deg
            for f in (0.1, 0.5, 0.9)
        ]
        assert values[1] < values[0]
        assert values[1] < values[2]


class TestConstraints:
    def test_customer_constraints_only_narrow(self, stripmap: SensorMode) -> None:
        # A customer asking for incidence down to 5 degrees on a sensor whose
        # floor is 15 must get 15. Widening would promise an image the
        # instrument cannot take.
        widened = effective_limits(
            stripmap,
            AcquisitionConstraints(
                min_incidence_deg=5.0, max_incidence_deg=80.0, max_squint_deg=45.0
            ),
        )
        assert widened is not None
        assert widened.min_incidence_deg == stripmap.min_incidence_deg
        assert widened.max_incidence_deg == stripmap.max_incidence_deg
        assert widened.max_squint_deg == stripmap.max_squint_deg

    def test_customer_constraints_do_narrow(self, stripmap: SensorMode) -> None:
        narrowed = effective_limits(
            stripmap,
            AcquisitionConstraints(
                min_incidence_deg=25.0, max_incidence_deg=35.0, max_squint_deg=2.0
            ),
        )
        assert narrowed is not None
        assert narrowed.min_incidence_deg == 25.0
        assert narrowed.max_incidence_deg == 35.0
        assert narrowed.max_squint_deg == 2.0

    def test_a_look_side_constraint_restricts_the_permitted_sides(
        self, stripmap: SensorMode
    ) -> None:
        limited = effective_limits(stripmap, AcquisitionConstraints(look_side=LookSide.LEFT))
        assert limited is not None
        assert limited.permitted_look_sides == (LookSide.LEFT,)

    def test_an_impossible_narrowing_satisfies_nothing(
        self, propagator: Propagator, stripmap: SensorMode, a_pass: tuple[datetime, datetime]
    ) -> None:
        # Narrowing that leaves an empty band is a request that cannot be met,
        # not a crash.
        start, end = a_pass
        g = compute(propagator, LISBON, start + (end - start) / 2)
        impossible = effective_limits(
            stripmap, AcquisitionConstraints(min_incidence_deg=44.0, max_incidence_deg=16.0)
        )
        assert impossible is None
        assert not satisfies(g, impossible)
        assert quality_score(g, impossible) == 0.0

    def test_excluding_the_only_permitted_side_is_impossible_not_an_error(self) -> None:
        one_sided = SensorMode(
            mode=ImagingMode.STRIPMAP,
            swath_width_km=80.0,
            resolution_m=5.0,
            min_dwell_s=3.0,
            max_dwell_s=30.0,
            permitted_look_sides=(LookSide.RIGHT,),
        )
        assert effective_limits(one_sided, AcquisitionConstraints(look_side=LookSide.LEFT)) is None

    def test_a_mode_restricted_to_one_side_rejects_the_other(
        self, propagator: Propagator, stripmap: SensorMode, a_pass: tuple[datetime, datetime]
    ) -> None:
        start, end = a_pass
        g = compute(propagator, LISBON, start + (end - start) / 2)
        wrong_side = LookSide.LEFT if g.look_side is LookSide.RIGHT else LookSide.RIGHT
        restricted = effective_limits(stripmap, AcquisitionConstraints(look_side=wrong_side))
        assert not satisfies(g, restricted)

    @pytest.mark.parametrize(
        "kwargs",
        [
            {"min_incidence_deg": 45.0, "max_incidence_deg": 15.0},
            {"min_incidence_deg": 30.0, "max_incidence_deg": 30.0},
            {"max_squint_deg": 91.0},
            {"swath_width_km": 0.0},
            {"min_dwell_s": 10.0, "max_dwell_s": 5.0},
            {"permitted_look_sides": ()},
        ],
    )
    def test_rejects_an_incoherent_sensor_mode(self, kwargs: dict[str, object]) -> None:
        base = {
            "mode": ImagingMode.STRIPMAP,
            "swath_width_km": 80.0,
            "resolution_m": 5.0,
            "min_dwell_s": 3.0,
            "max_dwell_s": 30.0,
        }
        with pytest.raises(ValueError):
            SensorMode(**{**base, **kwargs})  # type: ignore[arg-type]


class TestQualityScore:
    def test_is_zero_when_the_geometry_is_not_imageable(
        self, propagator: Propagator, stripmap: SensorMode, a_pass: tuple[datetime, datetime]
    ) -> None:
        # Horizon-grazing: incidence near 90, far outside the band.
        start, _end = a_pass
        assert quality_score(compute(propagator, LISBON, start), stripmap) == 0.0

    def test_peaks_at_band_centre_and_broadside(self, stripmap: SensorMode) -> None:
        from feasibility.sar.geometry import AccessGeometry

        centre = (stripmap.min_incidence_deg + stripmap.max_incidence_deg) / 2
        best = AccessGeometry(
            incidence_angle_deg=centre,
            look_side=LookSide.RIGHT,
            squint_angle_deg=0.0,
            slant_range_km=900.0,
            elevation_angle_deg=90.0 - centre,
            ground_azimuth_deg=100.0,
            roll_angle_deg=25.0,
        )
        assert quality_score(best, stripmap) == pytest.approx(1.0)

    def test_falls_off_towards_the_band_edges(self, stripmap: SensorMode) -> None:
        from feasibility.sar.geometry import AccessGeometry

        def score(incidence: float) -> float:
            return quality_score(
                AccessGeometry(
                    incidence_angle_deg=incidence,
                    look_side=LookSide.RIGHT,
                    squint_angle_deg=0.0,
                    slant_range_km=900.0,
                    elevation_angle_deg=90.0 - incidence,
                    ground_azimuth_deg=100.0,
                    roll_angle_deg=25.0,
                ),
                stripmap,
            )

        assert score(30.0) > score(22.0) > score(16.0)

    def test_squint_degrades_the_score(self, stripmap: SensorMode) -> None:
        from feasibility.sar.geometry import AccessGeometry

        def score(squint: float) -> float:
            return quality_score(
                AccessGeometry(
                    incidence_angle_deg=30.0,
                    look_side=LookSide.RIGHT,
                    squint_angle_deg=squint,
                    slant_range_km=900.0,
                    elevation_angle_deg=60.0,
                    ground_azimuth_deg=100.0,
                    roll_angle_deg=25.0,
                ),
                stripmap,
            )

        assert score(0.0) > score(2.5) > score(4.9)

    def test_is_bounded_to_the_unit_interval(
        self, propagator: Propagator, stripmap: SensorMode, a_pass: tuple[datetime, datetime]
    ) -> None:
        start, end = a_pass
        for i in range(30):
            when = start + (end - start) * (i / 29)
            assert 0.0 <= quality_score(compute(propagator, LISBON, when), stripmap) <= 1.0


class TestFilterAdmitsRealOpportunities:
    def test_some_instant_in_the_constellation_is_imageable(self, stripmap: SensorMode) -> None:
        """Guards against a filter that rejects everything.

        Every other test here checks that something is refused. A filter with an
        inverted comparison would pass all of them and quietly make the whole
        system return no opportunities ever, which looks like a quiet system
        rather than a broken one.
        """
        propagator = Propagator(parse("SENTINEL-1A", S1A_L1, S1A_L2))
        found = False
        for window in search(propagator, LISBON, T0, T0 + timedelta(hours=72)):
            steps = int(window.duration_s // 5)
            for i in range(steps + 1):
                when = window.start + timedelta(seconds=5 * i)
                if satisfies(compute(propagator, LISBON, when), stripmap):
                    found = True
                    break
            if found:
                break
        assert found, "no imageable instant in 72 hours — the filter admits nothing"

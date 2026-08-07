"""Geodesic footprint polygons.

The claim under test is that these are geodesic rather than planar. That is easy
to assert in a docstring and easy to get wrong in a way nothing notices at mid
latitude, so the tests that matter here compare against a planar construction
and require them to DISAGREE where they should — and the constellation is
sun-synchronous, so where they should is where it spends its time.
"""

from __future__ import annotations

import math

import pytest
from pyproj import Geod
from shapely.geometry import Point, Polygon

from feasibility.orbit import GroundPoint
from feasibility.sar import (
    AccessGeometry,
    ImagingMode,
    LookSide,
    SensorMode,
    area_km2,
    contains_target,
    ground_footprint,
)

GEOD = Geod(ellps="WGS84")

EQUATOR = GroundPoint(latitude_deg=0.0, longitude_deg=10.0)
MID = GroundPoint(latitude_deg=38.7, longitude_deg=-9.1)
ARCTIC = GroundPoint(latitude_deg=78.2, longitude_deg=15.6)

DWELL_S = 10.0
GROUND_SPEED_KM_S = 6.8


@pytest.fixture
def stripmap() -> SensorMode:
    return SensorMode(
        mode=ImagingMode.STRIPMAP,
        swath_width_km=80.0,
        resolution_m=5.0,
        min_dwell_s=3.0,
        max_dwell_s=30.0,
    )


def geometry(azimuth_deg: float = 100.0) -> AccessGeometry:
    return AccessGeometry(
        incidence_angle_deg=30.0,
        look_side=LookSide.RIGHT,
        squint_angle_deg=0.0,
        slant_range_km=900.0,
        elevation_angle_deg=60.0,
        ground_azimuth_deg=azimuth_deg,
        roll_angle_deg=27.0,
    )


def planar_footprint(
    aim: GroundPoint, azimuth_deg: float, along_km: float, across_km: float
) -> Polygon:
    """The naive construction this module exists to avoid.

    Kilometres converted to degrees with a fixed 111 km per degree and a single
    cos(latitude) factor. Correct near the equator, visibly wrong towards the
    poles — which is the point of comparing against it.
    """
    km_per_deg_lat = 111.32
    km_per_deg_lon = 111.32 * math.cos(math.radians(aim.latitude_deg))
    theta = math.radians(azimuth_deg)
    corners = []
    for along_sign, across_sign in ((1, -1), (1, 1), (-1, 1), (-1, -1)):
        north_km = along_sign * (along_km / 2) * math.cos(theta) - across_sign * (
            across_km / 2
        ) * math.sin(theta)
        east_km = along_sign * (along_km / 2) * math.sin(theta) + across_sign * (
            across_km / 2
        ) * math.cos(theta)
        corners.append(
            (
                aim.longitude_deg + east_km / km_per_deg_lon,
                aim.latitude_deg + north_km / km_per_deg_lat,
            )
        )
    return Polygon(corners)


class TestShape:
    def test_contains_its_own_aim_point(self, stripmap: SensorMode) -> None:
        fp = ground_footprint(MID, geometry(), stripmap, DWELL_S, GROUND_SPEED_KM_S)
        assert contains_target(fp, MID)

    def test_is_a_valid_closed_polygon(self, stripmap: SensorMode) -> None:
        fp = ground_footprint(MID, geometry(), stripmap, DWELL_S, GROUND_SPEED_KM_S)
        assert fp.is_valid
        assert not fp.is_empty
        assert fp.exterior is not None

    def test_area_matches_swath_times_along_track_length(self, stripmap: SensorMode) -> None:
        # An independent check on the construction: the patch should be about
        # swath_width x (ground_speed x dwell). Agreement within a few percent
        # is what a correctly built rectangle looks like; a factor of two means
        # a half-width was used as a full width somewhere.
        fp = ground_footprint(MID, geometry(), stripmap, DWELL_S, GROUND_SPEED_KM_S)
        expected = stripmap.swath_width_km * GROUND_SPEED_KM_S * DWELL_S
        assert area_km2(fp) == pytest.approx(expected, rel=0.05)

    def test_a_longer_dwell_makes_a_longer_footprint(self, stripmap: SensorMode) -> None:
        short = area_km2(ground_footprint(MID, geometry(), stripmap, 5.0, GROUND_SPEED_KM_S))
        long_ = area_km2(ground_footprint(MID, geometry(), stripmap, 20.0, GROUND_SPEED_KM_S))
        assert long_ == pytest.approx(short * 4.0, rel=0.05)

    def test_orientation_follows_the_ground_azimuth_not_north(self, stripmap: SensorMode) -> None:
        # A north-aligned box would be wrong by the orbit's inclination, which
        # for a sun-synchronous satellite is most of a right angle.
        along_track = ground_footprint(MID, geometry(0.0), stripmap, DWELL_S, GROUND_SPEED_KM_S)
        rotated = ground_footprint(MID, geometry(90.0), stripmap, DWELL_S, GROUND_SPEED_KM_S)
        assert not along_track.equals(rotated)
        # Same patch of ground, rotated: the areas must still match.
        assert area_km2(along_track) == pytest.approx(area_km2(rotated), rel=0.02)

    @pytest.mark.parametrize(
        ("dwell", "speed"), [(0.0, 6.8), (-1.0, 6.8), (10.0, 0.0), (10.0, -1.0)]
    )
    def test_rejects_impossible_inputs(
        self, stripmap: SensorMode, dwell: float, speed: float
    ) -> None:
        with pytest.raises(ValueError):
            ground_footprint(MID, geometry(), stripmap, dwell, speed)


class TestGeodesicVersusPlanar:
    """The claim: geodesic, not planar. These tests make it falsifiable."""

    def test_the_two_agree_near_the_equator(self, stripmap: SensorMode) -> None:
        # If they disagreed here, the comparison itself would be broken and the
        # high-latitude test below would prove nothing.
        along_km = GROUND_SPEED_KM_S * DWELL_S
        geodesic = ground_footprint(EQUATOR, geometry(), stripmap, DWELL_S, GROUND_SPEED_KM_S)
        planar = planar_footprint(EQUATOR, 100.0, along_km, stripmap.swath_width_km)
        assert area_km2(geodesic) == pytest.approx(area_km2(planar), rel=0.02)

    @staticmethod
    def _edge_lengths_km(polygon: Polygon) -> list[float]:
        coords = list(polygon.exterior.coords)[:-1]
        step = len(coords) // 4
        lengths = []
        for i in range(0, len(coords), step):
            a = coords[i]
            b = coords[(i + step) % len(coords)]
            _fwd, _back, metres = GEOD.inv(a[0], a[1], b[0], b[1])
            lengths.append(metres / 1000.0)
        return lengths[:4]

    def test_opposite_edges_match_at_high_latitude(self) -> None:
        """The test that caught a real bug, and the reason it is written this way.

        Two earlier attempts at proving "geodesic, not planar" measured the
        wrong thing. Intersection-over-union at an 80 km swath differed by 0.8%,
        below any threshold worth asserting. Distance to the corners differed by
        1.1%, also too blunt.

        Measuring the EDGES found it immediately — and found that our own
        construction was skewed, not just the planar strawman. Building each
        corner by going along-track and then across-track uses an across-track
        azimuth computed at the aim point, and meridians converge, so at 78N
        with a 400 km swath the two along-track edges came out at 78 km and 58
        km on a nominal 68. A 30 percent asymmetry, in a polygon that still
        looked like a perfectly reasonable rectangle on a map.

        One geodesic hop per corner fixes it. This test is what stops it coming
        back.
        """
        wide = SensorMode(
            mode=ImagingMode.SCAN,
            swath_width_km=400.0,
            resolution_m=50.0,
            min_dwell_s=3.0,
            max_dwell_s=30.0,
        )
        along_km = GROUND_SPEED_KM_S * DWELL_S

        for site in (EQUATOR, ARCTIC):
            across_a, along_a, across_b, along_b = self._edge_lengths_km(
                ground_footprint(site, geometry(), wide, DWELL_S, GROUND_SPEED_KM_S)
            )
            assert across_a == pytest.approx(across_b, rel=0.01), f"skewed at {site.latitude_deg}N"
            assert along_a == pytest.approx(along_b, rel=0.01), f"skewed at {site.latitude_deg}N"
            assert across_a == pytest.approx(wide.swath_width_km, rel=0.01)
            assert along_a == pytest.approx(along_km, rel=0.01)

    def test_diverges_from_a_planar_box_where_it_should(self) -> None:
        # With a wide swath at 78N the planar construction is visibly the wrong
        # shape. If ours matched it, ours would be planar too.
        wide = SensorMode(
            mode=ImagingMode.SCAN,
            swath_width_km=400.0,
            resolution_m=50.0,
            min_dwell_s=3.0,
            max_dwell_s=30.0,
        )
        along_km = GROUND_SPEED_KM_S * DWELL_S
        geodesic = ground_footprint(ARCTIC, geometry(), wide, DWELL_S, GROUND_SPEED_KM_S)
        planar = planar_footprint(ARCTIC, 100.0, along_km, wide.swath_width_km)

        overlap = geodesic.intersection(planar).area / geodesic.union(planar).area
        assert overlap < 0.97, (
            "the geodesic and planar footprints are indistinguishable at 78N with a "
            "400 km swath, which means the geodesic construction is not doing anything"
        )

    def test_area_is_preserved_across_latitudes(self, stripmap: SensorMode) -> None:
        # The same acquisition images the same amount of ground wherever it
        # happens. A construction that shrank towards the poles would be using
        # degrees as if they were distances.
        areas = [
            area_km2(ground_footprint(site, geometry(), stripmap, DWELL_S, GROUND_SPEED_KM_S))
            for site in (EQUATOR, MID, ARCTIC)
        ]
        assert max(areas) / min(areas) < 1.05


class TestContainment:
    def test_a_point_target_inside_is_covered(self, stripmap: SensorMode) -> None:
        fp = ground_footprint(MID, geometry(), stripmap, DWELL_S, GROUND_SPEED_KM_S)
        nearby = GroundPoint(MID.latitude_deg + 0.05, MID.longitude_deg + 0.05)
        assert contains_target(fp, nearby)

    def test_a_point_target_outside_is_not(self, stripmap: SensorMode) -> None:
        fp = ground_footprint(MID, geometry(), stripmap, DWELL_S, GROUND_SPEED_KM_S)
        far = GroundPoint(MID.latitude_deg + 5.0, MID.longitude_deg)
        assert not contains_target(fp, far)

    def test_a_polygon_target_needs_full_containment(self, stripmap: SensorMode) -> None:
        """Half an area of interest is not the image the customer asked for.

        Intersection would be the easy check and the wrong one: it would report
        an opportunity that delivers part of the target, and partial success
        presented as success is worse than a refusal the customer can act on.
        """
        fp = ground_footprint(MID, geometry(), stripmap, DWELL_S, GROUND_SPEED_KM_S)

        # A small polygon well inside.
        inside = Point(MID.longitude_deg, MID.latitude_deg).buffer(0.02)
        assert contains_target(fp, inside)

        # A large polygon that overlaps but extends beyond.
        straddling = Point(MID.longitude_deg, MID.latitude_deg).buffer(3.0)
        assert straddling.intersects(fp), "the fixture must actually overlap"
        assert not contains_target(fp, straddling)

    def test_a_polygon_target_entirely_outside_is_not_covered(self, stripmap: SensorMode) -> None:
        fp = ground_footprint(MID, geometry(), stripmap, DWELL_S, GROUND_SPEED_KM_S)
        elsewhere = Point(MID.longitude_deg + 20.0, MID.latitude_deg).buffer(0.1)
        assert not contains_target(fp, elsewhere)

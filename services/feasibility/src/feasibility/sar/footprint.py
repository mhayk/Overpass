"""Geodesic footprint polygons on the WGS84 ellipsoid.

Geodesic, not planar. A planar approximation — offset the centre by
kilometres-to-degrees and draw a rectangle — is visibly wrong at high latitude,
where a degree of longitude is a fraction of a degree of latitude. The
constellation is sun-synchronous and therefore spends its time exactly there, so
the approximation would be wrong precisely where the system is used most.

`pyproj.Geod` solves the forward geodesic problem on the ellipsoid: given a
point, an azimuth and a distance, where do you end up. Corners built that way are
correct at any latitude, including across the poles and the antimeridian, both of
which break naive degree arithmetic.

This is where pyproj earns its place. M1-10 deliberately did NOT use it — Skyfield
already does ECI to ECEF to geodetic on a path validated against Vallado's
reference vectors, and a second implementation of the same transform is two
answers that can drift. Footprint polygons are a different problem that Skyfield
does not solve, so a second library is buying something rather than duplicating.
"""

from __future__ import annotations

import math
from typing import TYPE_CHECKING

from pyproj import Geod
from shapely.geometry import Point, Polygon

if TYPE_CHECKING:
    from feasibility.orbit.propagation import GroundPoint
    from feasibility.sar.geometry import AccessGeometry
    from feasibility.sar.sensor import SensorMode

# WGS84, matching the SRID 4326 the database columns use and the frame every
# contract states.
_GEOD = Geod(ellps="WGS84")

# Corners per edge. Four would make a quadrilateral whose sides are straight in
# lon/lat space rather than geodesic, which is the same planar error this module
# exists to avoid — just moved from the corners to the edges. Densifying the
# edges keeps them on the ellipsoid.
_POINTS_PER_EDGE = 8


def _offset(lat: float, lon: float, azimuth_deg: float, distance_km: float) -> tuple[float, float]:
    """Forward geodesic: where you arrive travelling `distance_km` at `azimuth_deg`."""
    lon2, lat2, _back_azimuth = _GEOD.fwd(lon, lat, azimuth_deg, distance_km * 1000.0)
    return lat2, lon2


def ground_footprint(
    aim_point: GroundPoint,
    geometry: AccessGeometry,
    mode: SensorMode,
    dwell_s: float,
    ground_speed_km_s: float,
) -> Polygon:
    """The ground patch an acquisition would collect, as a WGS84 polygon.

    A rectangle centred on the aim point: `swath_width_km` across track, and
    along track however far the ground trace moves during the dwell. The
    along-track axis follows the ground azimuth of the acquisition, so the
    rectangle is oriented with the pass rather than with north — a
    north-aligned box would be wrong by the orbit's inclination, which for a
    sun-synchronous satellite is most of a right angle.
    """
    if dwell_s <= 0:
        msg = f"dwell must be positive, got {dwell_s}"
        raise ValueError(msg)
    if ground_speed_km_s <= 0:
        msg = f"ground speed must be positive, got {ground_speed_km_s}"
        raise ValueError(msg)

    half_along_km = ground_speed_km_s * dwell_s / 2.0
    half_across_km = mode.swath_width_km / 2.0

    # The beam points across track, so the swath runs perpendicular to the
    # direction of travel.
    along_azimuth = geometry.ground_azimuth_deg % 360.0

    # ONE geodesic hop per corner, from the aim point, at the corner's own
    # bearing and distance.
    #
    # The obvious construction — go half_along forward, then half_across
    # sideways — is wrong on an ellipsoid, and wrong in a way that looks fine.
    # The across-track azimuth has to be recomputed at the intermediate point,
    # because meridians converge; using the aim point's azimuth for all four
    # corners skews the polygon. Measured at 78N with a 400 km swath, it made
    # the two along-track edges 78 km and 58 km on a nominal 68 — a 30 percent
    # asymmetry in a shape that still looked like a plausible rectangle.
    #
    # Going straight to each corner has no intermediate point to be wrong
    # about, and is symmetric by construction.
    diagonal_km = math.hypot(half_along_km, half_across_km)
    corner_offset_deg = math.degrees(math.atan2(half_across_km, half_along_km))

    def corner(along_sign: float, across_sign: float) -> tuple[float, float]:
        # Bearing measured from the along-track direction, positive across-track.
        bearing = along_azimuth if along_sign > 0 else (along_azimuth + 180.0) % 360.0
        turn = corner_offset_deg * across_sign * (1.0 if along_sign > 0 else -1.0)
        return _offset(
            aim_point.latitude_deg,
            aim_point.longitude_deg,
            (bearing + turn) % 360.0,
            diagonal_km,
        )

    corners = [corner(+1, -1), corner(+1, +1), corner(-1, +1), corner(-1, -1)]

    ring: list[tuple[float, float]] = []
    for i in range(4):
        start_lat, start_lon = corners[i]
        end_lat, end_lon = corners[(i + 1) % 4]
        # npts returns the intermediate points only, so the edge is start plus
        # the interior points; the next iteration contributes the end.
        ring.append((start_lon, start_lat))
        for lon, lat in _GEOD.npts(start_lon, start_lat, end_lon, end_lat, _POINTS_PER_EDGE):
            ring.append((lon, lat))

    # Shapely closes the ring itself; GeoJSON is (lon, lat) and so is this.
    return Polygon(ring)


def contains_target(footprint: Polygon, target: GroundPoint | Polygon) -> bool:
    """Whether the footprint covers the target.

    A point target is satisfied by containment. A polygon target requires FULL
    containment, not intersection: half an area of interest is not the image the
    customer asked for, and delivering it as though it were would be the kind of
    quiet partial success that is worse than a refusal.
    """
    if isinstance(target, Polygon):
        return bool(footprint.contains(target))
    return bool(footprint.contains(Point(target.longitude_deg, target.latitude_deg)))


def area_km2(footprint: Polygon) -> float:
    """Geodesic area, for sanity checks and for the coverage view in M4."""
    lons, lats = zip(*footprint.exterior.coords, strict=True)
    area_m2, _perimeter = _GEOD.polygon_area_perimeter(lons, lats)
    return abs(area_m2) / 1e6

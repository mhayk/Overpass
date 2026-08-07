"""SAR acquisition geometry: incidence, look side, squint, roll.

THE DOMAIN FACT THIS FILE EXISTS FOR: **SAR is side-looking.** A target directly
beneath the satellite is not imageable. Anything that models this as a
nadir-pointing sensor passes every plausible-looking test and fails the one in
`test_sar_geometry.py::test_a_target_at_nadir_produces_no_opportunity` — which is
why that test is the canary and why it is written first.

Everything here is computed in GCRS, the inertial frame, at a single instant.
That choice matters for look side and roll, and the reason is physical rather
than convenient: the instrument is rolled about the spacecraft's velocity
vector, and spacecraft attitude is referenced to the orbital frame, which is
built from inertial position and velocity. Using the Earth-relative ground-track
direction instead would differ by the ~3.5 degrees that Earth rotation
contributes to the velocity direction — enough to flip the reported look side
for a target almost exactly on the ground track, and those are filtered out by
the incidence band anyway.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from enum import StrEnum
from typing import TYPE_CHECKING

import numpy as np
from skyfield.api import wgs84

if TYPE_CHECKING:
    from datetime import datetime

    from feasibility.orbit.propagation import GroundPoint, Propagator


class LookSide(StrEnum):
    """Maps to sar.v1.schema.json#/$defs/LookSide."""

    LEFT = "LEFT"
    RIGHT = "RIGHT"


class ImagingMode(StrEnum):
    """Maps to sar.v1.schema.json#/$defs/ImagingMode."""

    SPOTLIGHT = "SPOTLIGHT"
    STRIPMAP = "STRIPMAP"
    SCAN = "SCAN"


@dataclass(frozen=True)
class AccessGeometry:
    """One instant of acquisition geometry. Mirrors the contract's AccessGeometry."""

    incidence_angle_deg: float
    look_side: LookSide
    squint_angle_deg: float
    slant_range_km: float
    elevation_angle_deg: float
    ground_azimuth_deg: float
    roll_angle_deg: float


def _unit(v: np.ndarray) -> np.ndarray:
    return v / float(np.linalg.norm(v))


def compute(propagator: Propagator, target: GroundPoint, when: datetime) -> AccessGeometry:
    """Full acquisition geometry for one satellite, one target, one instant."""
    t = propagator.time(when)

    geocentric = propagator.satellite.at(t)
    sat_position = np.asarray(geocentric.position.km, dtype=float)
    sat_velocity = np.asarray(geocentric.velocity.km_per_s, dtype=float)

    site = wgs84.latlon(target.latitude_deg, target.longitude_deg, elevation_m=target.elevation_m)
    target_position = np.asarray(site.at(t).position.km, dtype=float)

    # Satellite to target.
    to_target = target_position - sat_position
    slant_range_km = float(np.linalg.norm(to_target))
    line_of_sight = _unit(to_target)

    # Topocentric elevation, from the validated Skyfield path rather than
    # recomputed here. Incidence is its complement — both are measured at the
    # target, one from the local horizontal and one from the local vertical, so
    # they sum to 90 by construction. Deriving incidence from elevation rather
    # than computing it separately means the two can never disagree.
    topo = propagator.topocentric(target, when)
    elevation_angle_deg = topo.elevation_deg
    incidence_angle_deg = topo.incidence_deg

    # Look side. With forward = velocity and up = radial, the right-hand side is
    # forward x up: in a frame where forward is +x and up is +z, x cross z is
    # -y, and +y is left. Verified against the intuitive case in the tests — a
    # northbound satellite sees a target to its east on the RIGHT.
    forward = _unit(sat_velocity)
    up = _unit(sat_position)
    right = _unit(np.cross(forward, up))
    cross_track = float(np.dot(line_of_sight, right))
    look_side = LookSide.RIGHT if cross_track > 0 else LookSide.LEFT

    # Squint: how far the beam is fore (+) or aft (-) of broadside. Broadside is
    # perpendicular to the velocity, so a line of sight exactly perpendicular
    # gives zero.
    along_track = float(np.dot(line_of_sight, forward))
    squint_angle_deg = math.degrees(math.asin(max(-1.0, min(1.0, along_track))))

    # Roll: the off-nadir angle the spacecraft must take to point at the target,
    # signed by look side. Computed directly from the vectors rather than from
    # the spherical-Earth relation asin(Re/(Re+h) * sin(incidence)) — the direct
    # form is exact on the ellipsoid, and the spherical relation is used in the
    # tests as an INDEPENDENT cross-check rather than as the implementation.
    nadir = -up
    off_nadir_rad = math.acos(max(-1.0, min(1.0, float(np.dot(line_of_sight, nadir)))))
    roll_magnitude_deg = math.degrees(off_nadir_rad)
    roll_angle_deg = roll_magnitude_deg if look_side is LookSide.RIGHT else -roll_magnitude_deg

    return AccessGeometry(
        incidence_angle_deg=incidence_angle_deg,
        look_side=look_side,
        squint_angle_deg=squint_angle_deg,
        slant_range_km=slant_range_km,
        elevation_angle_deg=elevation_angle_deg,
        ground_azimuth_deg=float(topo.azimuth_deg),
        roll_angle_deg=roll_angle_deg,
    )

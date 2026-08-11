"""Where the satellite actually was, as opposed to where the plan expected it.

A DELIBERATE SECOND PROPAGATION PATH, and a small one.

feasibility owns the orbital mechanics this system plans against: access search,
SAR geometry, ephemeris sweeps. This module does one thing that service has no
reason to do — ask where a spacecraft ended up, using an element set observed
AFTER the plan was committed — and it is kept to the smallest surface that can
answer that. Sharing feasibility's propagator would have meant extracting a
lib/python package and touching a second service to build this one; the two
services stay independently deployable instead, at the cost of skyfield being
declared twice.

What that costs is real and worth naming: two code paths that call SGP4. The
mitigation is that this one computes a single scalar — how close the ground
track came to a target — and the golden test pins it against both frozen
fixtures, so a divergence between the two paths shows up as a changed number
rather than as a subtly different plan.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from datetime import datetime, timedelta

from skyfield.api import EarthSatellite, load, wgs84

from simulator.execution import GroundPoint, great_circle_km

# One timescale for the process. Skyfield builds ephemeris tables on first use,
# and rebuilding them per call turns a millisecond into a second.
_TS = load.timescale()


@dataclass(frozen=True)
class ElementSet:
    """One two-line element set, with the epoch already decoded."""

    satellite_id: str
    epoch: datetime
    line1: str
    line2: str

    def age_hours(self, at: datetime) -> float:
        """Hours between this element set's epoch and `at`.

        Negative for an element set observed after `at`, which is exactly what
        the truth snapshot is — and is not an error. The caller decides what a
        negative age means; here it is the normal case.
        """
        return (at - self.epoch).total_seconds() / 3600.0


class Propagator:
    """Propagates one element set. Construction is the expensive part."""

    def __init__(self, element_set: ElementSet) -> None:
        self.element_set = element_set
        self._satellite = EarthSatellite(
            element_set.line1, element_set.line2, element_set.satellite_id, _TS
        )

    def subpoint(self, when: datetime) -> GroundPoint:
        """The geodetic point directly beneath the satellite."""
        if when.tzinfo is None:
            msg = "refusing to propagate against a naive datetime"
            raise ValueError(msg)
        geocentric = self._satellite.at(_TS.from_datetime(when))
        point = wgs84.subpoint(geocentric)
        return GroundPoint(
            latitude_deg=float(point.latitude.degrees),
            longitude_deg=float(point.longitude.degrees),
        )

    def position_km(self, when: datetime) -> tuple[float, float, float]:
        """Geocentric position, for comparing two element sets directly."""
        if when.tzinfo is None:
            msg = "refusing to propagate against a naive datetime"
            raise ValueError(msg)
        x, y, z = self._satellite.at(_TS.from_datetime(when)).position.km
        return float(x), float(y), float(z)


def separation_km(a: Propagator, b: Propagator, when: datetime) -> float:
    """How far apart two element sets say the same satellite is.

    The raw measure of drift between a planning observation and the one that
    superseded it. Not what decides a miss — that is closest_approach_km below,
    which is about the target rather than the spacecraft — but it is the number
    that makes drift legible in a log line.
    """
    ax, ay, az = a.position_km(when)
    bx, by, bz = b.position_km(when)
    return math.sqrt((ax - bx) ** 2 + (ay - by) ** 2 + (az - bz) ** 2)


def closest_approach_km(
    propagator: Propagator,
    target: GroundPoint,
    window: tuple[datetime, datetime],
    *,
    step: timedelta = timedelta(seconds=1),
) -> float:
    """How near the ground track came to the target, in kilometres.

    SAMPLED, NOT SOLVED. The exact perpendicular distance from a point to a
    ground track is a minimisation over a curve that is itself only defined by
    propagation; sampling it at one-second steps over an acquisition window of
    ten to thirty seconds costs a few dozen evaluations and is accurate to well
    under the swath half-widths this feeds — 2.5 km at the narrowest, against a
    ground speed near 7.5 km/s, so a one-second step resolves the approach to
    better than the sub-kilometre scale that matters.

    A closed-form solve would be more precise about a quantity whose inputs are
    element sets with kilometres of uncertainty in them, which is precision
    spent in the wrong place.
    """
    start, end = window
    if end <= start:
        msg = f"window must be non-empty, got {start.isoformat()}..{end.isoformat()}"
        raise ValueError(msg)

    best = float("inf")
    when = start
    while when <= end:
        best = min(best, great_circle_km(propagator.subpoint(when), target))
        when += step
    # The end instant, always — a window shorter than one step would otherwise
    # be measured only at its start.
    return min(best, great_circle_km(propagator.subpoint(end), target))

"""SGP4 propagation and topocentric geometry.

No orbital mechanics is implemented here. Every number comes out of Skyfield,
which wraps the `sgp4` package, which is a direct port of Vallado's reference
C++ implementation. This module's job is to hold that library correctly — the
right time scale, the right frame, the right sign convention — and nothing else.

That restraint is the point. This is the highest-risk code in the project for
output that is plausible and wrong, and the mitigation named in the issue is to
use code thousands of people have validated against real ephemerides. The tests
check the *wiring*, against the official Vallado verification vectors.

DETERMINISM IS A HARD REQUIREMENT. Given the same element set and the same
instants, this must produce byte-identical answers forever, or the golden
reference tests in M1-12 are impossible. Two things could break it and both are
handled: Skyfield's timescale is built in rather than downloaded, so no network
fetch can shift UT1 under us; and nothing here samples wall-clock time.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from datetime import datetime
from typing import TYPE_CHECKING

from skyfield.api import EarthSatellite, load, wgs84

from feasibility.tle.element_set import ElementSet

if TYPE_CHECKING:
    from collections.abc import Sequence

    from skyfield.timelib import Time, Timescale

# One shared timescale. `builtin=True` is the load-bearing argument: it uses the
# leap-second and Earth-orientation tables shipped inside Skyfield instead of
# fetching finals2000A.all from the network. A downloaded table would make the
# physics depend on the day the machine last had internet, which is the opposite
# of a golden reference.
_TS: Timescale = load.timescale(builtin=True)


def timescale() -> Timescale:
    """The shared, built-in timescale. Exposed so tests use the same one."""
    return _TS


@dataclass(frozen=True)
class Topocentric:
    """Where a satellite is, as seen from a point on the ground.

    `elevation_deg` is measured up from the local horizon and `incidence_deg`
    down from the local vertical, both at the TARGET. They are complementary by
    construction — see `incidence_deg` — and that identity is asserted in the
    tests as an internal consistency check.
    """

    elevation_deg: float
    azimuth_deg: float
    slant_range_km: float

    @property
    def incidence_deg(self) -> float:
        """Angle between the line of sight and the local vertical at the target.

        Elevation is measured from the local horizontal and incidence from the
        local vertical, at the same point, so they sum to 90 degrees exactly.
        This is the bridge between the access search here, which thinks in
        elevation, and the SAR geometry filter in M1-11, whose valid band is
        stated in incidence — 15 to 45 degrees of incidence is 45 to 75 degrees
        of elevation.

        Stated as a derived property rather than a stored field so the two can
        never disagree.
        """
        return 90.0 - self.elevation_deg


@dataclass(frozen=True)
class GroundPoint:
    """A target on the WGS84 ellipsoid."""

    latitude_deg: float
    longitude_deg: float
    elevation_m: float = 0.0

    def __post_init__(self) -> None:
        if not -90.0 <= self.latitude_deg <= 90.0:
            msg = f"latitude out of range: {self.latitude_deg}"
            raise ValueError(msg)
        if not -180.0 <= self.longitude_deg <= 180.0:
            msg = f"longitude out of range: {self.longitude_deg}"
            raise ValueError(msg)


class Propagator:
    """Propagates one element set and answers geometry questions about it.

    Construction is the expensive part — building the SGP4 record and the
    Skyfield wrapper — so a propagator is built once per satellite per sweep and
    reused across the thousands of instants an access search samples.
    """

    def __init__(self, element_set: ElementSet) -> None:
        self._element_set = element_set
        self._satellite = EarthSatellite(
            element_set.line1,
            element_set.line2,
            element_set.name,
            _TS,
        )

    @property
    def element_set(self) -> ElementSet:
        return self._element_set

    @property
    def satellite(self) -> EarthSatellite:
        """The underlying Skyfield object. Exposed for tests and for M1-11."""
        return self._satellite

    def time(self, when: datetime) -> Time:
        """Convert an aware UTC datetime into a Skyfield time."""
        if when.tzinfo is None:
            msg = "refusing to propagate against a naive datetime"
            raise ValueError(msg)
        return _TS.from_datetime(when)

    def times(self, whens: Sequence[datetime]) -> Time:
        """Vectorised form of `time`. One Skyfield call beats N."""
        for when in whens:
            if when.tzinfo is None:
                msg = "refusing to propagate against a naive datetime"
                raise ValueError(msg)
        return _TS.from_datetimes(list(whens))

    def subpoint(self, when: datetime) -> GroundPoint:
        """The geodetic point directly beneath the satellite."""
        geocentric = self._satellite.at(self.time(when))
        sub = wgs84.geographic_position_of(geocentric)
        return GroundPoint(
            latitude_deg=float(sub.latitude.degrees),
            longitude_deg=float(sub.longitude.degrees),
            elevation_m=float(sub.elevation.m),
        )

    def topocentric(self, target: GroundPoint, when: datetime) -> Topocentric:
        """Elevation, azimuth and slant range from `target` to the satellite."""
        elevations, azimuths, ranges = self._altaz(target, self.times([when]))
        return Topocentric(
            elevation_deg=float(elevations[0]),
            azimuth_deg=float(azimuths[0]),
            slant_range_km=float(ranges[0]),
        )

    def elevations_deg(self, target: GroundPoint, whens: Sequence[datetime]) -> list[float]:
        """Elevation at many instants, in one propagation call.

        The access search samples the horizon at a coarse step and then bisects.
        Doing that one instant at a time is the difference between a sweep that
        finishes and one that does not, so the vector path exists and the scalar
        path is built on top of it rather than the other way round.
        """
        if not whens:
            return []
        elevations, _, _ = self._altaz(target, self.times(whens))
        return [float(e) for e in elevations]

    def _altaz(self, target: GroundPoint, t: Time) -> tuple[list[float], list[float], list[float]]:
        site = wgs84.latlon(
            target.latitude_deg,
            target.longitude_deg,
            elevation_m=target.elevation_m,
        )
        # Satellite minus observer gives the topocentric vector. Skyfield
        # handles TEME to ITRF, Earth rotation, and the geodetic site position;
        # doing any of that by hand here would be a second implementation of
        # something already validated, and two implementations disagree.
        alt, az, distance = (self._satellite - site).at(t).altaz()
        degrees_alt = alt.degrees
        degrees_az = az.degrees
        km = distance.km
        # Skyfield returns scalars for a scalar Time and arrays otherwise.
        if not hasattr(degrees_alt, "__len__"):
            return [float(degrees_alt)], [float(degrees_az)], [float(km)]
        return (
            [float(v) for v in degrees_alt],
            [float(v) for v in degrees_az],
            [float(v) for v in km],
        )

    def revolutions_per_day(self) -> float:
        """Mean motion in revolutions per day, from the element set itself."""
        # satrec.no_kozai is radians per minute.
        return float(self._satellite.model.no_kozai) * 1440.0 / (2.0 * math.pi)

    def orbit_number(self, when: datetime) -> int:
        """Revolution number at `when`.

        Extrapolated from the revolution count at epoch, which the element set
        carries, using mean motion. This is an APPROXIMATION and the reason
        matters: the true count is ascending-node crossings, and mean motion
        drifts across the horizon, so this can be off by one near a node several
        days from epoch.

        That is acceptable for what it is used for — bucketing acquisitions into
        per-orbit duty-cycle budgets (M2-03) — where an occasional boundary
        disagreement moves one acquisition between adjacent buckets and does not
        break the budget. It would NOT be acceptable as a published orbit
        identifier, and if it ever becomes one this needs replacing with real
        node crossing detection.
        """
        if when.tzinfo is None:
            msg = "refusing to compute an orbit number against a naive datetime"
            raise ValueError(msg)
        elapsed_days = (when - self._element_set.epoch).total_seconds() / 86400.0
        at_epoch = int(self._satellite.model.revnum)
        return at_epoch + int(math.floor(elapsed_days * self.revolutions_per_day()))

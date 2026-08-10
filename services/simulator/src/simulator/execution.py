"""What actually happened to a committed acquisition.

The domain of the execution simulator, kept free of NATS, Postgres and the clock
so the physics can be tested without any of them.

ONE OUTCOME IS COMPUTED AND SIX ARE INJECTED, and the asymmetry is the design
(ADR-0021). `TLE_DRIFT_MISS` is computed from two real element sets, because
this system has the information to compute it and computing it is the whole
point: it turns the staleness threshold from a number in a config file into a
visible physical consequence. The other six failure reasons are spacecraft
internals — an attitude control error, a thermal limit — that the system holds
no state for, so inventing a thermal model to roll against would be simulating a
simulation. They are injected from a seeded generator instead, and the code says
which is which rather than blurring the two.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from datetime import datetime, timedelta
from enum import StrEnum
from random import Random

EARTH_RADIUS_KM = 6371.0088


class Status(StrEnum):
    """Mirrors acquisition.executed.v1's `status`."""

    SUCCEEDED = "SUCCEEDED"
    PARTIAL = "PARTIAL"
    FAILED = "FAILED"
    SKIPPED = "SKIPPED"


class FailureReason(StrEnum):
    """Mirrors acquisition.executed.v1's `failure_reason`."""

    ATTITUDE_ERROR = "ATTITUDE_ERROR"
    SLEW_OVERRUN = "SLEW_OVERRUN"
    POWER_BUDGET_EXCEEDED = "POWER_BUDGET_EXCEEDED"
    THERMAL_LIMIT = "THERMAL_LIMIT"
    SENSOR_FAULT = "SENSOR_FAULT"
    GROUND_ABORT = "GROUND_ABORT"
    TLE_DRIFT_MISS = "TLE_DRIFT_MISS"


#: The failure reasons this system cannot compute and therefore injects.
#:
#: TLE_DRIFT_MISS is deliberately absent: it is the one outcome derived from
#: real geometry, and putting it here would make it a coin flip like the rest.
INJECTABLE = (
    FailureReason.ATTITUDE_ERROR,
    FailureReason.SLEW_OVERRUN,
    FailureReason.POWER_BUDGET_EXCEEDED,
    FailureReason.THERMAL_LIMIT,
    FailureReason.SENSOR_FAULT,
    FailureReason.GROUND_ABORT,
)


@dataclass(frozen=True)
class InjectionRates:
    """How often each uncomputable failure happens, per acquisition.

    Defaults are deliberately small and deliberately non-zero. Zero would mean
    the failure paths never run, and the read model handling an acquisition that
    was committed and then did not happen is a materially different code path —
    the one most demo systems never execute. Large would drown the computed
    drift misses in noise, which are the interesting ones.

    Every value is a probability in [0, 1]. They are checked rather than
    trusted: a rate of 1.5 means whoever set it believes something untrue about
    this knob, and clamping would leave that belief in place.
    """

    attitude_error: float = 0.01
    slew_overrun: float = 0.01
    power_budget_exceeded: float = 0.005
    thermal_limit: float = 0.005
    sensor_fault: float = 0.005
    ground_abort: float = 0.005
    #: PARTIAL rather than FAILED, when the collection happened but fell short.
    partial: float = 0.03

    def __post_init__(self) -> None:
        for name, value in self.as_map().items():
            if not 0.0 <= value <= 1.0:
                msg = f"injection rate {name} must be in [0, 1], got {value}"
                raise ValueError(msg)
        total = sum(self.as_map()[reason.value.lower()] for reason in INJECTABLE)
        if total > 1.0:
            msg = f"injected failure rates sum to {total:.3f}; that leaves no successes"
            raise ValueError(msg)

    def as_map(self) -> dict[str, float]:
        return {
            "attitude_error": self.attitude_error,
            "slew_overrun": self.slew_overrun,
            "power_budget_exceeded": self.power_budget_exceeded,
            "thermal_limit": self.thermal_limit,
            "sensor_fault": self.sensor_fault,
            "ground_abort": self.ground_abort,
            "partial": self.partial,
        }


@dataclass(frozen=True)
class GroundPoint:
    latitude_deg: float
    longitude_deg: float


@dataclass(frozen=True)
class Outcome:
    """What the simulator concluded, before it is shaped into an event."""

    status: Status
    failure_reason: FailureReason | None
    actual_window: tuple[datetime, datetime] | None
    target_coverage_ratio: float
    #: How far the target sat from the actual ground track, in km. Carried even
    #: on success, because "we made it by 400 metres" and "we made it by 40 km"
    #: are different facts about the same SUCCEEDED.
    cross_track_km: float
    #: Age of the element set the plan was computed against, in hours. The
    #: number the correlation is against, carried so it never has to be
    #: recovered by joining back to the plan.
    planning_tle_age_hours: float


def great_circle_km(a: GroundPoint, b: GroundPoint) -> float:
    """Haversine distance, in kilometres.

    Spherical, not WGS84. The quantity this feeds is a comparison against a
    swath half-width of 2.5 to 50 km, where the ellipsoidal correction is well
    under a percent — far below the drift being measured. Stated because the
    geometry elsewhere in this system IS ellipsoidal, and an inconsistency that
    looks accidental invites someone to "fix" it.
    """
    lat1, lat2 = math.radians(a.latitude_deg), math.radians(b.latitude_deg)
    delta_lat = lat2 - lat1
    delta_lon = math.radians(b.longitude_deg - a.longitude_deg)
    h = (
        math.sin(delta_lat / 2) ** 2
        + math.cos(lat1) * math.cos(lat2) * math.sin(delta_lon / 2) ** 2
    )
    return 2 * EARTH_RADIUS_KM * math.asin(math.sqrt(min(1.0, h)))


def coverage_ratio(cross_track_km: float, swath_half_km: float) -> float:
    """How much of the target the swath still caught, in [0, 1].

    Linear in the miss distance rather than a step, because the interesting case
    is the edge: a target one kilometre outside a 2.5 km half-swath is a partial
    collection someone might still accept, and a step function would call it a
    total loss. Zero once the target is a full swath-width away.
    """
    if swath_half_km <= 0:
        msg = f"swath half-width must be positive, got {swath_half_km}"
        raise ValueError(msg)
    if cross_track_km <= swath_half_km:
        return 1.0
    overshoot = cross_track_km - swath_half_km
    return max(0.0, 1.0 - overshoot / swath_half_km)


def decide(
    *,
    cross_track_km: float,
    swath_half_km: float,
    planning_tle_age_hours: float,
    scheduled_window: tuple[datetime, datetime],
    rates: InjectionRates,
    random: Random,
    partial_floor: float = 0.6,
) -> Outcome:
    """Decide what happened, computed first and injected second.

    ORDER MATTERS AND IS NOT ARBITRARY. The drift check runs BEFORE the injected
    failures, so a genuine geometric miss is never relabelled as a sensor fault
    that happened to roll first. The computed answer is the one this system can
    defend; the injected ones fill in around it.
    """
    covered = coverage_ratio(cross_track_km, swath_half_km)

    if covered <= 0.0:
        # The target fell outside the swath entirely. Nothing was collected, so
        # there is no actual_window to report — the contract says null for that
        # case and means it.
        return Outcome(
            status=Status.FAILED,
            failure_reason=FailureReason.TLE_DRIFT_MISS,
            actual_window=None,
            target_coverage_ratio=0.0,
            cross_track_km=cross_track_km,
            planning_tle_age_hours=planning_tle_age_hours,
        )

    if covered < partial_floor:
        # Clipped, not missed. Some usable data exists and the customer may or
        # may not be satisfied — which the contract calls genuinely ambiguous
        # and is the reason PARTIAL exists as a status at all.
        return Outcome(
            status=Status.PARTIAL,
            failure_reason=FailureReason.TLE_DRIFT_MISS,
            actual_window=scheduled_window,
            target_coverage_ratio=covered,
            cross_track_km=cross_track_km,
            planning_tle_age_hours=planning_tle_age_hours,
        )

    for reason in INJECTABLE:
        if random.random() < rates.as_map()[reason.value.lower()]:
            # GROUND_ABORT is the only one that means the acquisition never
            # started; the rest fail during or after it.
            skipped = reason is FailureReason.GROUND_ABORT
            return Outcome(
                status=Status.SKIPPED if skipped else Status.FAILED,
                failure_reason=reason,
                actual_window=None if skipped else scheduled_window,
                target_coverage_ratio=0.0,
                cross_track_km=cross_track_km,
                planning_tle_age_hours=planning_tle_age_hours,
            )

    if random.random() < rates.partial:
        return Outcome(
            status=Status.PARTIAL,
            failure_reason=FailureReason.SENSOR_FAULT,
            actual_window=scheduled_window,
            target_coverage_ratio=round(random.uniform(0.4, 0.9), 3),
            cross_track_km=cross_track_km,
            planning_tle_age_hours=planning_tle_age_hours,
        )

    return Outcome(
        status=Status.SUCCEEDED,
        failure_reason=None,
        actual_window=scheduled_window,
        target_coverage_ratio=covered,
        cross_track_km=cross_track_km,
        planning_tle_age_hours=planning_tle_age_hours,
    )


def drift_actual_window(
    scheduled: tuple[datetime, datetime], cross_track_km: float
) -> tuple[datetime, datetime]:
    """Nudge the executed window, proportionally to how far off the pass was.

    A satellite that is not where the propagation said it would be also does not
    cross the target when it was expected to. The shift is derived from the
    cross-track error rather than drawn from a distribution, so plan-versus-
    actual drift stays a consequence of the same geometry as the miss — one
    cause, two visible effects, rather than two independent knobs that can
    disagree.

    Roughly 7.5 km/s of ground speed for a LEO SAR platform, capped so a
    hundred-kilometre miss does not report an acquisition a minute early and
    make the timeline unreadable.
    """
    seconds = max(-20.0, min(20.0, cross_track_km / 7.5))
    shift = timedelta(seconds=seconds)
    return scheduled[0] + shift, scheduled[1] + shift

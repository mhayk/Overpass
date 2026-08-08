"""The feasibility sweep, end to end, as a pure function of its inputs.

Deliberately knows nothing about NATS or Postgres. It takes a request and a
constellation and returns either opportunities or a refusal; the worker is what
puts it in a transaction and the outbox. Keeping the physics free of the
plumbing is what lets the whole sweep be tested without a broker.

Two things this closes that earlier issues left open:

  GROUND SPEED is derived here, from the propagation, rather than passed in.
  M1-11 took it as an argument because nothing yet produced it, and a footprint
  scaled by a guessed speed is the wrong size in a way nothing would flag.

  THE REFUSALS are distinguished. `NO_ACCESS_IN_HORIZON` and
  `CONSTRAINTS_TOO_NARROW` are different answers to the customer — the first
  says the geometry never permits it, the second says their own constraints
  eliminated access that existed. Both are correct negative answers and neither
  is retryable.
"""

from __future__ import annotations

import math
import uuid
from dataclasses import dataclass
from datetime import datetime, timedelta
from typing import TYPE_CHECKING

from feasibility.orbit import AccessSearchPolicy, GroundPoint, HorizonPolicy, Propagator, sweep
from feasibility.sar import (
    AccessGeometry,
    SensorMode,
    compute,
    contains_target,
    effective_limits,
    ground_footprint,
    quality_score,
    satisfies,
)
from feasibility.tle.element_set import Staleness, StalenessPolicy

if TYPE_CHECKING:
    from collections.abc import Sequence

    from shapely.geometry import Polygon

    from feasibility.sar import AcquisitionConstraints

# How finely each access window is sampled looking for an imageable instant.
# The SAR filter admits roughly half a percent of a pass — a narrow band around
# broadside — so a coarse sample here would step straight over it. Five seconds
# against passes of several minutes gives tens of samples inside the usable
# part, which is enough to find its edges without another bisection.
_GEOMETRY_SAMPLE_S = 5.0

# The namespace for derived opportunity ids. Arbitrary but FROZEN: changing it
# renames every opportunity ever computed, so a replayed request would produce a
# set the planner cannot reconcile with the one it already allocated over.
_OPPORTUNITY_NAMESPACE = uuid.UUID("8c1d5e2f-4a3b-4c6d-9e7f-0a1b2c3d4e5f")


def opportunity_id(request_id: str, index: int, mode: str) -> str:
    """The stable identity of one opportunity.

    A UUID because the contract says `format: uuid` and
    `feasibility.opportunities.opportunity_id` is a `uuid` column. The first
    version of this was `f"{request_id}:{index}:{mode}"`, which satisfied
    neither — so a sweep that found an opportunity could not have published it
    and could not have stored it. Nothing noticed, because nothing had ever
    tried: the worker never ran the sweep (#131).

    Derived rather than random, because a replay from the stream is a supported
    operation. Random ids would fan one request out into two disjoint sets of
    opportunities on the second pass, and the planner would have allocated over
    the first.
    """
    return str(uuid.uuid5(_OPPORTUNITY_NAMESPACE, f"{request_id}|{index}|{mode}"))


@dataclass(frozen=True)
class Opportunity:
    """A candidate acquisition. NOT a commitment — allocation makes it one."""

    opportunity_id: str
    satellite_id: str
    mode: str
    access_start: datetime
    access_end: datetime
    acquisition_duration_s: float
    orbit_number: int
    geometry: AccessGeometry
    footprint: Polygon
    duty_cycle_cost_s: float
    quality_score: float


@dataclass(frozen=True)
class Refusal:
    """A correct negative answer. Maps to feasibility.failed.v1."""

    reason_code: str
    retryable: bool
    detail: str


@dataclass(frozen=True)
class SweepOutcome:
    opportunities: list[Opportunity]
    refusal: Refusal | None
    satellites_evaluated: int
    truncated: bool
    horizon_start: datetime
    horizon_end: datetime


def ground_speed_km_s(propagator: Propagator, when: datetime) -> float:
    """Speed of the sub-satellite point across the ground, from the propagation.

    Not the orbital speed. The ground trace moves more slowly than the satellite
    — by roughly the ratio of Earth's radius to the orbital radius — and it is
    the ground trace that sweeps the footprint. Using orbital speed would make
    every along-track footprint about 12 percent too long at LEO.

    Measured over a short symmetric interval rather than derived analytically,
    because the analytic form needs the Earth-rotation term and the numerical
    one gets it for free from the geodetic subpoints.
    """
    step = timedelta(seconds=1)
    before = propagator.subpoint(when - step)
    after = propagator.subpoint(when + step)

    mean_lat = math.radians((before.latitude_deg + after.latitude_deg) / 2.0)
    delta_lat_km = (after.latitude_deg - before.latitude_deg) * 111.32
    delta_lon = (after.longitude_deg - before.longitude_deg + 180.0) % 360.0 - 180.0
    delta_lon_km = delta_lon * 111.32 * math.cos(mean_lat)

    return math.hypot(delta_lat_km, delta_lon_km) / (2.0 * step.total_seconds())


def evaluate(
    request_id: str,
    target: GroundPoint,
    horizon_start: datetime,
    horizon_end: datetime,
    propagators: Sequence[Propagator],
    modes: dict[str, SensorMode],
    constraints: AcquisitionConstraints,
    now: datetime,
    staleness: StalenessPolicy | None = None,
    search_policy: AccessSearchPolicy | None = None,
    horizon_policy: HorizonPolicy | None = None,
) -> SweepOutcome:
    """Propagate the constellation and return every imageable opportunity.

    `now` is passed rather than read from the clock, because a sweep that
    consults wall time is not reproducible and the golden tests in M1-12 depend
    on it being.
    """
    staleness = staleness or StalenessPolicy()

    # Refuse before computing, not after. A stale element set produces a
    # confidently wrong access window, and publishing one is worse than
    # publishing nothing — the planner cannot tell the difference.
    usable = [
        p for p in propagators if p.element_set.staleness(now, staleness) is not Staleness.STALE
    ]
    if not usable:
        return SweepOutcome(
            opportunities=[],
            refusal=Refusal(
                "TLE_STALE",
                retryable=True,
                detail=(
                    f"all {len(propagators)} element sets exceeded the staleness threshold; "
                    "a fresh fetch would change this"
                ),
            ),
            satellites_evaluated=0,
            truncated=False,
            horizon_start=horizon_start,
            horizon_end=horizon_end,
        )

    excluded = constraints.excluded_satellite_ids
    usable = [p for p in usable if p.element_set.name not in excluded]

    result = sweep(usable, target, horizon_start, horizon_end, search_policy, horizon_policy)

    if not result.windows:
        return SweepOutcome(
            opportunities=[],
            refusal=Refusal(
                "NO_ACCESS_IN_HORIZON",
                retryable=False,
                detail="no satellite rises above the elevation mask over this horizon",
            ),
            satellites_evaluated=result.satellites_evaluated,
            truncated=result.truncated,
            horizon_start=result.horizon_start,
            horizon_end=result.horizon_end,
        )

    by_name = {p.element_set.name: p for p in usable}
    opportunities: list[Opportunity] = []
    geometric_access_existed = False

    for index, window in enumerate(result.windows):
        propagator = by_name[window.satellite_id]
        for mode_name, mode in modes.items():
            limits = effective_limits(mode, constraints)
            if limits is None:
                continue
            found = _best_instant(propagator, target, window.start, window.end, limits)
            if found is None:
                continue
            geometric_access_existed = True
            when, geometry, score = found

            dwell_s = mode.min_dwell_s
            speed = ground_speed_km_s(propagator, when)
            footprint = ground_footprint(target, geometry, mode, dwell_s, speed)

            # A footprint that does not actually cover the target is not an
            # opportunity, however good the angles look.
            if not contains_target(footprint, target):
                continue

            opportunities.append(
                Opportunity(
                    opportunity_id=opportunity_id(request_id, index, mode_name),
                    satellite_id=window.satellite_id,
                    mode=mode_name,
                    access_start=window.start,
                    access_end=window.end,
                    acquisition_duration_s=dwell_s,
                    orbit_number=window.orbit_number,
                    geometry=geometry,
                    footprint=footprint,
                    duty_cycle_cost_s=dwell_s,
                    quality_score=score,
                )
            )

    if not opportunities:
        # The distinction the contract asks for. Access existed and the
        # customer's own narrowing removed it, versus the geometry never
        # permitting it at all — different answers, and the first one is
        # actionable by the customer.
        reason = (
            "CONSTRAINTS_TOO_NARROW"
            if geometric_access_existed or _constraints_are_narrowing(constraints)
            else "NO_ACCESS_IN_HORIZON"
        )
        return SweepOutcome(
            opportunities=[],
            refusal=Refusal(
                reason,
                retryable=False,
                detail=(
                    f"{len(result.windows)} access windows found, none satisfying the "
                    "sensor and customer geometry constraints"
                ),
            ),
            satellites_evaluated=result.satellites_evaluated,
            truncated=result.truncated,
            horizon_start=result.horizon_start,
            horizon_end=result.horizon_end,
        )

    return SweepOutcome(
        opportunities=opportunities,
        refusal=None,
        satellites_evaluated=result.satellites_evaluated,
        truncated=result.truncated,
        horizon_start=result.horizon_start,
        horizon_end=result.horizon_end,
    )


def _constraints_are_narrowing(constraints: AcquisitionConstraints) -> bool:
    return any(
        (
            constraints.look_side is not None,
            constraints.min_incidence_deg is not None,
            constraints.max_incidence_deg is not None,
            constraints.max_squint_deg is not None,
        )
    )


def _best_instant(
    propagator: Propagator,
    target: GroundPoint,
    start: datetime,
    end: datetime,
    limits: SensorMode,
) -> tuple[datetime, AccessGeometry, float] | None:
    """The highest-scoring imageable instant in one window, or None."""
    best: tuple[datetime, AccessGeometry, float] | None = None
    steps = max(1, int((end - start).total_seconds() // _GEOMETRY_SAMPLE_S))
    for i in range(steps + 1):
        when = start + timedelta(seconds=_GEOMETRY_SAMPLE_S * i)
        if when > end:
            break
        geometry = compute(propagator, target, when)
        if not satisfies(geometry, limits):
            continue
        score = quality_score(geometry, limits)
        if best is None or score > best[2]:
            best = (when, geometry, score)
    return best

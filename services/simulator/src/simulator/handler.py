"""Turning a committed plan into what actually happened.

The seam between the physics and the pipeline: it reads a
`planning.plan.committed.v1`, decides an outcome per acquisition, and writes
`acquisition.executed.v1` to the outbox — inside the caller's transaction, so an
execution and its event commit together or not at all.

EXECUTION IS COMPRESSED, NOT SCHEDULED (ADR-0021). An acquisition is executed
when its plan commits, not when its window opens. A demo that waited for a real
pass would show nothing, and a scheduler holding acquisitions for hours would be
a second timing system to get wrong for no demonstrative gain. What is simulated
away is the waiting; the divergence is still computed, and `actual_window` still
carries it.
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass
from datetime import datetime
from random import Random
from typing import Any

from simulator.execution import (
    GroundPoint,
    InjectionRates,
    Outcome,
    Status,
    decide,
    drift_actual_window,
)
from simulator.orbit import ElementSet, Propagator, closest_approach_km

SUBJECT = "acquisition.executed.v1"
SCHEMA_VERSION = "1.0.0"

#: The producer name the contract requires.
#:
#: Taken from the schema's enum rather than from this service's directory name.
#: "simulator-service" was the obvious guess and the schema rejected it — which
#: is the whole reason the emitted event is validated against the committed
#: contract instead of against what this module believes.
PRODUCER = "acquisition-simulator"

#: Swath half-widths, in km, by imaging mode.
#:
#: Half of reference.satellites' sensor_modes swath_width_km. Duplicated here
#: rather than read from the database because it is what the miss is compared
#: against and a silent disagreement would move every outcome — the golden test
#: pins the consequence, and a mode this map does not know is refused rather
#: than guessed at.
SWATH_HALF_KM = {
    "SPOTLIGHT": 2.5,
    "STRIPMAP": 15.0,
    "SCAN": 50.0,
}


@dataclass(frozen=True)
class Acquisition:
    """One acquisition out of a committed plan."""

    acquisition_id: str
    request_id: str
    customer_id: str | None
    mode: str
    window: tuple[datetime, datetime]
    target: GroundPoint
    duty_cycle_cost_s: float


@dataclass(frozen=True)
class Plan:
    """The parts of planning.plan.committed.v1 this service needs."""

    plan_id: str
    satellite_id: str
    committed_at: datetime
    acquisitions: tuple[Acquisition, ...]


def centroid(ring: list[list[float]]) -> GroundPoint:
    """The middle of a footprint ring, as a stand-in for the target.

    THE FOOTPRINT IS WHAT THE EVENT CARRIES, and it is centred on the target the
    plan was built around. Reading tasking.requests for the true target instead
    would put this service across another service's schema boundary to recover
    something the contract already hands it.

    An unweighted mean of the ring's vertices, which for the near-rectangular
    swath footprints this system produces sits within a few hundred metres of
    the polygon's centroid — well inside the kilometre scale that decides a
    miss. Longitude is averaged naively, so a footprint spanning the
    antimeridian would land in the wrong hemisphere; none do, because a swath is
    at most 100 km wide, and the guard below refuses an empty ring rather than
    dividing by zero.
    """
    if not ring:
        msg = "a footprint with no vertices cannot have a centroid"
        raise ValueError(msg)
    longitudes = [point[0] for point in ring]
    latitudes = [point[1] for point in ring]
    return GroundPoint(
        latitude_deg=sum(latitudes) / len(latitudes),
        longitude_deg=sum(longitudes) / len(longitudes),
    )


def execute(
    *,
    acquisition: Acquisition,
    planning_elements: ElementSet,
    truth_elements: ElementSet,
    rates: InjectionRates,
    random: Random,
) -> Outcome:
    """Decide what happened to one acquisition.

    The truth element set is propagated, not the planning one: the question is
    where the satellite ACTUALLY was, and the planning set is by construction
    the one that said the target would be in the swath.
    """
    swath_half_km = SWATH_HALF_KM.get(acquisition.mode)
    if swath_half_km is None:
        # Refused rather than defaulted. A mode this map does not know would
        # otherwise silently take some other mode's swath, and every outcome for
        # it would be wrong in a way nothing reports.
        msg = f"no swath half-width for mode {acquisition.mode!r}"
        raise ValueError(msg)

    cross_track_km = closest_approach_km(
        Propagator(truth_elements), acquisition.target, acquisition.window
    )

    outcome = decide(
        cross_track_km=cross_track_km,
        swath_half_km=swath_half_km,
        planning_tle_age_hours=planning_elements.age_hours(acquisition.window[0]),
        scheduled_window=acquisition.window,
        rates=rates,
        random=random,
    )

    if outcome.actual_window is None:
        return outcome
    # The satellite that was not where the propagation said also did not cross
    # when it was expected to. One cause, two visible effects.
    return Outcome(
        status=outcome.status,
        failure_reason=outcome.failure_reason,
        actual_window=drift_actual_window(outcome.actual_window, cross_track_km),
        target_coverage_ratio=outcome.target_coverage_ratio,
        cross_track_km=outcome.cross_track_km,
        planning_tle_age_hours=outcome.planning_tle_age_hours,
    )


def executed_event(
    *,
    plan: Plan,
    acquisition: Acquisition,
    outcome: Outcome,
    executed_at: datetime,
) -> dict[str, Any]:
    """Shape one outcome into acquisition.executed.v1's `data` object."""
    data: dict[str, Any] = {
        "acquisition_id": acquisition.acquisition_id,
        "plan_id": plan.plan_id,
        "satellite_id": plan.satellite_id,
        "request_id": acquisition.request_id,
        "mode": acquisition.mode,
        "scheduled_window": {
            "start": _iso(acquisition.window[0]),
            "end": _iso(acquisition.window[1]),
        },
        "status": str(outcome.status),
        "executed_at": _iso(executed_at),
    }
    if acquisition.customer_id is not None:
        data["customer_id"] = acquisition.customer_id
    if outcome.actual_window is not None:
        data["actual_window"] = {
            "start": _iso(outcome.actual_window[0]),
            "end": _iso(outcome.actual_window[1]),
        }
    if outcome.failure_reason is not None:
        data["failure_reason"] = str(outcome.failure_reason)
    if outcome.status is not Status.SKIPPED:
        # Nothing was collected on a SKIPPED acquisition, so reporting a
        # coverage ratio or a duty-cycle cost for one would be inventing detail
        # about an event that did not happen.
        data["target_coverage_ratio"] = round(outcome.target_coverage_ratio, 4)
        data["duty_cycle_consumed_s"] = acquisition.duty_cycle_cost_s
    return data


def event_id_for(acquisition_id: str) -> str:
    """A stable event id for one acquisition's execution.

    uuid5 in a fixed namespace: the same acquisition always produces the same
    id, so a redelivered plan is caught by the outbox's unique constraint rather
    than publishing a second execution for the same acquisition.
    """
    return str(uuid.uuid5(_NAMESPACE, acquisition_id))


#: A fixed namespace, so ids are reproducible across processes and restarts.
_NAMESPACE = uuid.UUID("6f1d2f4a-9a1e-4c3b-8d21-5b7f3c0a4e11")


def _iso(when: datetime) -> str:
    if when.tzinfo is None:
        msg = "refusing to serialise a naive datetime into a contract event"
        raise ValueError(msg)
    return when.isoformat().replace("+00:00", "Z")

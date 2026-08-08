"""Turning a sweep outcome into the event the planner allocates over.

Pure, like `failures.build_refusal` and `ephemeris.build_event`, and for the same
reason: the payload a producer really emits can be written to disk and shown to
the schema without a broker or a database in the room. That is the only thing
that would have caught #124, and it is the only thing that will catch the next
one.

THE CAP IS DECLARED, NOT SILENT. The contract bounds the array at 20000 items and
carries a `truncated` flag next to it. A sweep that quietly dropped the tail
would make the planner look worse than it is for a reason invisible in the data —
so when the cap bites, the LOWEST-QUALITY opportunities go and the flag says so.
Quality rather than time order, because dropping the tail of a horizon would
silently shorten the window the planner believes it has.
"""

from __future__ import annotations

import json
import uuid
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any

from feasibility.messaging.outbox import OutboxMessage

if TYPE_CHECKING:
    from collections.abc import Sequence

    from feasibility.messaging.idempotency import Delivery
    from feasibility.pipeline import Opportunity, SweepOutcome

OPPORTUNITIES_SUBJECT = "feasibility.opportunities.computed.v1"
SCHEMA_VERSION = "1.0.0"
PRODUCER = "feasibility-service"

# The contract's maxItems. Restated here because this is the only place that can
# enforce it before the event exists.
MAX_OPPORTUNITIES = 20000

# Frozen, like every derived-id namespace in this service.
_NAMESPACE = uuid.UUID("7a6b5c4d-3e2f-4a1b-8c9d-0e1f2a3b4c5d")


def build_event(
    delivery: Delivery,
    request_id: str,
    outcome: SweepOutcome,
    tle_references: Sequence[dict[str, Any]],
    now: datetime,
    compute_duration_ms: int,
) -> OutboxMessage:
    """Render a successful sweep as a contract event, ready for the outbox.

    Raises ValueError on an outcome with no opportunities. That is not this
    event: the contract sets `minItems: 1` and `opportunity_count` has a minimum
    of 1, because "no opportunities" is `feasibility.failed.v1` with a reason
    code and not an empty success.
    """
    if not outcome.opportunities:
        msg = (
            "an outcome with no opportunities is a refusal, not this event. "
            "Publish feasibility.failed.v1 with a reason code instead."
        )
        raise ValueError(msg)

    kept, truncated = _capped(outcome.opportunities)

    causing = delivery.event_id
    event_id = str(uuid.uuid5(_NAMESPACE, f"{request_id}|{causing}"))

    payload: dict[str, Any] = {
        "event_id": event_id,
        "event_type": OPPORTUNITIES_SUBJECT,
        "schema_version": SCHEMA_VERSION,
        "occurred_at": _rfc3339(now),
        "correlation_id": _correlation_of(delivery),
        # The request that caused this sweep. Correlation gives the tree,
        # causation gives the edges.
        "causation_id": causing,
        "producer": PRODUCER,
        "data": {
            "request_id": request_id,
            "computed_at": _rfc3339(now),
            "horizon": {
                "start": _rfc3339(outcome.horizon_start),
                "end": _rfc3339(outcome.horizon_end),
            },
            # Every satellite CONSIDERED, including the ones that produced
            # nothing. That is what makes a computed window auditable back to
            # the element set behind it, and what makes "the plan looked wrong
            # because the TLE was two days old" a diagnosable statement.
            "tle_references": list(tle_references),
            "satellites_evaluated": outcome.satellites_evaluated,
            "opportunity_count": len(kept),
            # Two ways a result can be short of the whole truth, and they are
            # different: `truncated` here is the per-request cap, and the sweep's
            # own `truncated` is the planning horizon having been clamped.
            "truncated": truncated or outcome.truncated,
            "compute_duration_ms": compute_duration_ms,
            "opportunities": [_opportunity(o) for o in kept],
        },
    }

    return OutboxMessage(
        event_id=event_id,
        event_type=OPPORTUNITIES_SUBJECT,
        schema_version=SCHEMA_VERSION,
        subject=OPPORTUNITIES_SUBJECT,
        payload=payload,
        occurred_at=now,
        headers=dict(delivery.headers),
    )


def _capped(opportunities: list[Opportunity]) -> tuple[list[Opportunity], bool]:
    """The best MAX_OPPORTUNITIES, and whether anything was dropped.

    Sorted by quality descending to choose, then restored to the sweep's own
    order — which is by access window — so a consumer reading the array in order
    reads the horizon in order.
    """
    if len(opportunities) <= MAX_OPPORTUNITIES:
        return list(opportunities), False

    best = sorted(opportunities, key=lambda o: o.quality_score, reverse=True)[:MAX_OPPORTUNITIES]
    keep = {o.opportunity_id for o in best}
    return [o for o in opportunities if o.opportunity_id in keep], True


def _opportunity(o: Opportunity) -> dict[str, Any]:
    """One candidate, in the contract's shape."""
    return {
        "opportunity_id": o.opportunity_id,
        "satellite_id": o.satellite_id,
        "mode": o.mode,
        "access_window": {
            "start": _rfc3339(o.access_start),
            "end": _rfc3339(o.access_end),
        },
        "acquisition_duration_s": o.acquisition_duration_s,
        "orbit_number": o.orbit_number,
        "geometry": {
            "incidence_angle_deg": o.geometry.incidence_angle_deg,
            "look_side": str(o.geometry.look_side),
            "squint_angle_deg": o.geometry.squint_angle_deg,
            "slant_range_km": o.geometry.slant_range_km,
            "elevation_angle_deg": o.geometry.elevation_angle_deg,
            "ground_azimuth_deg": o.geometry.ground_azimuth_deg,
            "roll_angle_deg": o.geometry.roll_angle_deg,
        },
        "footprint": _polygon(o),
        "duty_cycle_cost_s": o.duty_cycle_cost_s,
        "quality_score": o.quality_score,
    }


def _polygon(o: Opportunity) -> dict[str, Any]:
    """The footprint as GeoJSON, longitude first, rounded to six places.

    Six, the same precision the read layer asks PostGIS for on the way back out.
    A shapely ring carries full float64, and a fan-out event with a few thousand
    footprints is the largest payload this system produces — the digits beyond
    six describe a millimetre of a swath edge computed from a propagator whose
    own error is metres.
    """
    ring = [[round(x, 6), round(y, 6)] for x, y in o.footprint.exterior.coords]
    return {"type": "Polygon", "coordinates": [ring]}


def _correlation_of(delivery: Delivery) -> str:
    """The causing event's correlation id, or a fresh one.

    Propagated, so one customer request stays greppable by one value across
    every service it touches. The fallback exists because the envelope makes the
    field required and an upstream that omitted it must not make this event
    unpublishable.
    """
    try:
        correlation = json.loads(delivery.payload).get("correlation_id")
    except (json.JSONDecodeError, AttributeError, TypeError):
        correlation = None
    return correlation if isinstance(correlation, str) and correlation else str(uuid.uuid4())


def _rfc3339(when: datetime) -> str:
    """UTC, `Z`-suffixed, milliseconds only when there are any."""
    when = when.astimezone(UTC)
    if when.microsecond:
        return when.strftime("%Y-%m-%dT%H:%M:%S.") + f"{when.microsecond // 1000:03d}Z"
    return when.strftime("%Y-%m-%dT%H:%M:%SZ")

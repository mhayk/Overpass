"""The worker's real handler: a tasking request in, opportunities or a refusal out.

This is the piece that was missing. `pipeline.evaluate` has existed and been
golden-tested since M1-10; the worker loop, the ledger and the outbox have
existed since M1-13; and nothing joined them. The live handler was
`__main__._record_only`, whose own docstring said the real `evaluate()` was
somebody else's job — and that somebody's issue closed without it. So a submitted
request reached this service, was recorded as processed, and produced nothing.
See #131.

EVERYTHING HERE RUNS INSIDE THE CALLER'S TRANSACTION. `process_once` has already
inserted the dedup row on the cursor handed in; the opportunities and the outbox
event go in on the same one, so all three commit or none do. That is the whole
mechanism, and taking a connection instead of a cursor would quietly break it.

A REFUSAL IS A RESULT, NOT AN ERROR. `SweepOutcome.refusal` means the sweep ran
and the answer was no — the geometry does not permit it, or the customer's own
constraints removed the access that existed. It is published as
`feasibility.failed.v1` from the same committed transaction, and the message is
acked. Raising it as an exception would work, and would lose the horizon, the
satellite count and the element sets that make a negative answer auditable.
"""

from __future__ import annotations

import json
import logging
import time
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any

from feasibility import failures, metrics, opportunities
from feasibility.messaging.outbox import enqueue, enqueue_once
from feasibility.orbit import Propagator
from feasibility.pipeline import Refusal, SweepOutcome, evaluate
from feasibility.reference import sensor_modes
from feasibility.request import decode
from feasibility.tle.element_set import StalenessPolicy
from feasibility.tle.store import newest_element_sets

if TYPE_CHECKING:
    from collections.abc import Callable, Sequence

    import psycopg

    from feasibility.messaging.idempotency import Delivery
    from feasibility.pipeline import Opportunity
    from feasibility.request import SweepRequest
    from feasibility.tle.store import SatelliteElementSet

log = logging.getLogger(__name__)


def sweep_handler(
    now: Callable[[], datetime] | None = None,
    staleness: StalenessPolicy | None = None,
) -> Callable[[Delivery], Callable[[psycopg.Cursor[Any]], None]]:
    """Build the handler factory the worker loop expects.

    A factory of a factory because that is the worker's shape: it hands a
    Delivery in and gets back something it can call with a cursor inside the
    transaction. `now` is injectable for the same reason it is everywhere else
    here — a sweep that consults wall time cannot be tested against a frozen
    snapshot.
    """
    clock = now or (lambda: datetime.now(UTC))
    policy = staleness or StalenessPolicy()

    def factory(delivery: Delivery) -> Callable[[psycopg.Cursor[Any]], None]:
        def handler(cursor: psycopg.Cursor[Any]) -> None:
            _handle(cursor, delivery, clock(), policy)

        return handler

    return factory


def _handle(
    cursor: psycopg.Cursor[Any],
    delivery: Delivery,
    at: datetime,
    policy: StalenessPolicy,
) -> None:
    # Raises NonRetryableError with a contract reason code for anything the
    # request itself makes impossible. The worker turns that into a published
    # refusal and an ack.
    request = decode(delivery.payload)

    connection = cursor.connection
    entries = newest_element_sets(connection, at)
    if not entries:
        _refuse(
            cursor,
            delivery,
            request,
            "TLE_UNAVAILABLE",
            retryable=True,
            detail="no element sets are available; the constellation is not seeded",
            at=at,
        )
        return

    modes_by_satellite = sensor_modes(connection, [e.satellite_id for e in entries])

    started = time.perf_counter()
    outcome = _evaluate(request, entries, modes_by_satellite, at, policy)
    elapsed_ms = int((time.perf_counter() - started) * 1000)

    references = [_reference(entry, at, policy) for entry in entries]

    if outcome.refusal is not None:
        # The ingress-side sibling of the planner's unfulfilled-by-reason. A
        # request refused here never reaches a round and never becomes an
        # unfulfilment, so without this counter the two sides do not reconcile
        # and requests appear to vanish between the services.
        metrics.instruments().record_refusal(outcome.refusal.reason_code)
        # Zero IS an observation. Dropping it would leave the
        # opportunities-per-request panel describing successful sweeps only,
        # which is the half that never needs investigating.
        metrics.instruments().record_opportunities(0)
        _refuse(
            cursor,
            delivery,
            request,
            outcome.refusal.reason_code,
            retryable=outcome.refusal.retryable,
            detail=outcome.refusal.detail,
            at=at,
            outcome=outcome,
            references=references,
        )
        return

    metrics.instruments().record_opportunities(len(outcome.opportunities))

    _persist(cursor, request, outcome, at)
    # enqueue_once, not enqueue. The event id is DERIVED from the request, so a
    # collision means this exact event was already produced — not that the
    # idempotency ledger failed, which is what a collision means for a random
    # id and why plain `enqueue` raises. A read-model rebuild replays the
    # stream with the ledger cleared, and that must be a no-op here rather than
    # a constraint violation that rolls back a completed sweep.
    enqueue_once(
        cursor,
        opportunities.build_event(
            delivery, request.request_id, outcome, references, at, elapsed_ms
        ),
    )
    log.info(
        "request %s: %d opportunities across %d satellites in %d ms",
        request.request_id,
        len(outcome.opportunities),
        outcome.satellites_evaluated,
        elapsed_ms,
    )


def _evaluate(
    request: SweepRequest,
    entries: Sequence[SatelliteElementSet],
    modes_by_satellite: dict[str, dict[str, Any]],
    at: datetime,
    policy: StalenessPolicy,
) -> SweepOutcome:
    """Run the sweep, then hold it to the target's actual shape.

    `evaluate` searches access for a POINT — the centroid, for a polygon target
    — and checks that the footprint contains it. The contract asks for more than
    that: a Polygon target must be fully contained by a single acquisition
    footprint, because partial coverage across passes is mosaicking and is out
    of scope. So the polygon check happens here, after the sweep, against the
    shape `decode` kept for exactly this.
    """
    propagators = [Propagator(e.element_set) for e in entries]

    # ONE MODE TABLE FOR THE WHOLE SWEEP, because that is `evaluate`'s
    # signature: it takes `modes` once and applies it to every propagator.
    #
    # The schema allows per-satellite parameters and the seed writes the same
    # table to all of them, so today this is exactly right and tomorrow it might
    # not be. `_uniform_modes` therefore REFUSES a constellation whose requested
    # modes differ rather than silently imaging one satellite with another's
    # instrument — a wrong answer nothing downstream could detect. Supporting
    # divergence means changing evaluate's signature, which is #140.
    requested = set(request.requested_modes)
    modes = _uniform_modes(modes_by_satellite, requested)
    if not modes:
        return SweepOutcome(
            opportunities=[],
            refusal=Refusal(
                "CONSTRAINTS_TOO_NARROW",
                retryable=False,
                detail=(
                    f"none of the requested modes {sorted(requested)} exist on any "
                    "satellite in the constellation"
                ),
            ),
            satellites_evaluated=0,
            truncated=False,
            horizon_start=request.window_start,
            horizon_end=request.window_end,
        )

    outcome = evaluate(
        request.request_id,
        request.target,
        request.window_start,
        request.window_end,
        propagators,
        modes,
        request.constraints,
        now=at,
        staleness=policy,
    )

    if request.target_polygon is None or not outcome.opportunities:
        return outcome

    covering = [o for o in outcome.opportunities if o.footprint.contains(request.target_polygon)]
    if covering:
        return replace_opportunities(outcome, covering)

    return SweepOutcome(
        opportunities=[],
        refusal=Refusal(
            "CONSTRAINTS_TOO_NARROW",
            retryable=False,
            detail=(
                f"{len(outcome.opportunities)} acquisitions cover the target's centre but "
                "none contains the whole polygon; this system does not mosaic across passes"
            ),
        ),
        satellites_evaluated=outcome.satellites_evaluated,
        truncated=outcome.truncated,
        horizon_start=outcome.horizon_start,
        horizon_end=outcome.horizon_end,
    )


def _uniform_modes(
    modes_by_satellite: dict[str, dict[str, Any]], requested: set[str]
) -> dict[str, Any]:
    """The requested modes, once, or a loud refusal if the fleet disagrees."""
    tables = {
        satellite_id: {name: mode for name, mode in modes.items() if name in requested}
        for satellite_id, modes in modes_by_satellite.items()
    }
    distinct = {tuple(sorted((n, m) for n, m in table.items())) for table in tables.values()}
    if len(distinct) > 1:
        msg = (
            "the constellation declares different parameters for the requested modes "
            f"{sorted(requested)}, and the sweep applies one mode table to every "
            "satellite. Imaging one satellite with another's instrument would produce "
            "opportunities nothing downstream could detect as wrong. See #140."
        )
        raise ValueError(msg)
    return next(iter(tables.values()), {})


def replace_opportunities(outcome: SweepOutcome, kept: list[Opportunity]) -> SweepOutcome:
    """The same outcome with a narrowed candidate list.

    A function rather than `dataclasses.replace` inline, so the horizon and the
    satellite count are visibly carried through. Dropping either would make a
    filtered result claim a sweep that did not happen.
    """
    return SweepOutcome(
        opportunities=kept,
        refusal=None,
        satellites_evaluated=outcome.satellites_evaluated,
        truncated=outcome.truncated,
        horizon_start=outcome.horizon_start,
        horizon_end=outcome.horizon_end,
    )


def _persist(
    cursor: psycopg.Cursor[Any],
    request: SweepRequest,
    outcome: SweepOutcome,
    at: datetime,
) -> None:
    """Write the candidates to feasibility.opportunities.

    The table has existed since migration 00003 and had never been written to.
    It is not the event and does not replace it: the event is how the planner
    learns, this is how the sweep is inspectable afterwards — "which candidates
    did we actually compute for this request" is otherwise answerable only by
    replaying a stream with 72 hours of retention.

    ON CONFLICT DO NOTHING because the ids are derived: a replayed request
    recomputes the same opportunities, and the second write is a no-op rather
    than a constraint violation that would roll back the whole handler.
    """
    for o in outcome.opportunities:
        cursor.execute(
            """
            INSERT INTO feasibility.opportunities
                (opportunity_id, request_id, satellite_id, mode, access_window,
                 acquisition_duration_s, orbit_number, geometry, footprint,
                 duty_cycle_cost_s, quality_score, computed_at)
            VALUES (%s, %s, %s, %s, tstzrange(%s, %s, '[)'), %s, %s, %s::jsonb,
                    ST_SetSRID(ST_GeomFromGeoJSON(%s), 4326), %s, %s, %s)
            ON CONFLICT (opportunity_id) DO NOTHING
            """,
            (
                o.opportunity_id,
                request.request_id,
                o.satellite_id,
                o.mode,
                o.access_start,
                o.access_end,
                o.acquisition_duration_s,
                o.orbit_number,
                _geometry_json(o),
                _footprint_json(o),
                o.duty_cycle_cost_s,
                o.quality_score,
                at,
            ),
        )


def _geometry_json(o: Opportunity) -> str:
    return json.dumps(
        {
            "incidence_angle_deg": o.geometry.incidence_angle_deg,
            "look_side": str(o.geometry.look_side),
            "squint_angle_deg": o.geometry.squint_angle_deg,
            "slant_range_km": o.geometry.slant_range_km,
            "elevation_angle_deg": o.geometry.elevation_angle_deg,
            "ground_azimuth_deg": o.geometry.ground_azimuth_deg,
            "roll_angle_deg": o.geometry.roll_angle_deg,
        }
    )


def _footprint_json(o: Opportunity) -> str:
    ring = [[round(x, 6), round(y, 6)] for x, y in o.footprint.exterior.coords]
    return json.dumps({"type": "Polygon", "coordinates": [ring]})


def _reference(entry: SatelliteElementSet, at: datetime, policy: StalenessPolicy) -> dict[str, Any]:
    """Provenance for one satellite, in the contract's TleReference shape."""
    age_hours = max(0.0, entry.element_set.age_hours(at))
    return {
        "satellite_id": entry.satellite_id,
        "norad_id": entry.element_set.norad_id,
        "tle_epoch": _rfc3339(entry.element_set.epoch),
        "tle_age_hours": age_hours,
        "staleness": str(policy.classify(age_hours)),
    }


def _refuse(
    cursor: psycopg.Cursor[Any],
    delivery: Delivery,
    request: SweepRequest,
    reason_code: str,
    *,
    retryable: bool,
    detail: str,
    at: datetime,
    outcome: SweepOutcome | None = None,
    references: Sequence[dict[str, Any]] | None = None,
) -> None:
    """Publish a correct negative answer, with the context that makes it auditable."""
    enqueue(
        cursor,
        failures.build_refusal(
            delivery,
            reason_code,
            retryable=retryable,
            detail=detail,
            now=at,
            horizon=(
                (outcome.horizon_start, outcome.horizon_end)
                if outcome is not None
                else (request.window_start, request.window_end)
            ),
            satellites_evaluated=outcome.satellites_evaluated if outcome is not None else 0,
            tle_references=list(references) if references is not None else None,
        ),
    )
    log.info("request %s refused: %s", request.request_id, reason_code)


def _rfc3339(when: datetime) -> str:
    when = when.astimezone(UTC)
    if when.microsecond:
        return when.strftime("%Y-%m-%dT%H:%M:%S.") + f"{when.microsecond // 1000:03d}Z"
    return when.strftime("%Y-%m-%dT%H:%M:%SZ")

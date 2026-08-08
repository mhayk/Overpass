"""Turning a sampled track into the event the rest of the system sees.

Pure, like `pipeline.py`, and for the same reason: everything here is a function
of its arguments, so the whole publish path can be asserted without a broker or a
database in the room.

THE EVENT ID IS DERIVED, and that is the load-bearing part of this module.

Every other event in Overpass is published by a consumer that already holds an
upstream `event_id` to key deduplication on. This one is published by a timer.
There is no upstream anything, and a rolling sweep necessarily re-covers ground
it has already covered — so without a stable id, running the sweep every few
minutes would publish the same three-hour bucket over and over.

So the id is a UUIDv5 over `(satellite_id, bucket start, tle_epoch)`. The outbox
declares `event_id` UNIQUE, which turns "have I already published this bucket?"
from a question requiring state into an INSERT that does nothing. Note what is
IN the key: the element set. A fresher TLE means a genuinely different track for
the same bucket, and it must be publishable rather than swallowed as a duplicate.
"""

from __future__ import annotations

import uuid
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any

from feasibility.messaging.outbox import OutboxMessage
from feasibility.tle.element_set import StalenessPolicy

if TYPE_CHECKING:
    from feasibility.orbit.ephemeris import EphemerisTrack
    from feasibility.tle.element_set import ElementSet

EPHEMERIS_SUBJECT = "feasibility.ephemeris.computed.v1"
SCHEMA_VERSION = "1.0.0"
PRODUCER = "feasibility-service"

# One point is a position, not a path. Matches the schema's minItems.
_MINIMUM_SAMPLES = 2

# A fixed namespace for the derived ids. Arbitrary but FROZEN: changing it
# renames every ephemeris event ever published, which would republish the whole
# horizon exactly once and then never again — a confusing way to discover that
# somebody regenerated a constant.
_NAMESPACE = uuid.UUID("3f2b1a0e-9d8c-4b7a-a6f5-e4d3c2b1a098")


def derive_event_id(satellite_id: str, bucket_start: datetime, tle_epoch: datetime) -> str:
    """The stable identity of one satellite's track over one bucket.

    Formatted through `_rfc3339` rather than `isoformat()` so the key does not
    depend on whether a datetime happens to carry microseconds — two callers
    holding the same instant with different precision must derive the same id.
    """
    key = f"{satellite_id}|{_rfc3339(bucket_start)}|{_rfc3339(tle_epoch)}"
    return str(uuid.uuid5(_NAMESPACE, key))


def build_event(
    track: EphemerisTrack,
    element_set: ElementSet,
    now: datetime,
    staleness: StalenessPolicy | None = None,
    headers: dict[str, str] | None = None,
) -> OutboxMessage:
    """Render one track as a contract event, ready for the outbox.

    A STALE element set is published with its staleness stated, NOT refused.
    That is deliberately unlike `evaluate`, which refuses: an access window is a
    commitment the planner will act on, and a confidently wrong one is worse than
    none. A track is a drawing that carries its own age, and refusing to draw it
    empties the globe on the fourth day of a demo for a reason no viewer can see.
    """
    staleness = staleness or StalenessPolicy()
    # The contract's minItems, restated where the payload is built. Sampling
    # already refuses a track this short; this is the second gate, on the
    # boundary itself, because nothing guarantees a future caller went
    # through the sampler.
    if len(track.samples) < _MINIMUM_SAMPLES:
        msg = "refusing to publish a track of fewer than two samples"
        raise ValueError(msg)

    event_id = derive_event_id(track.satellite_id, track.horizon_start, element_set.epoch)
    age_hours = element_set.age_hours(now)

    payload: dict[str, Any] = {
        "event_id": event_id,
        "event_type": EPHEMERIS_SUBJECT,
        "schema_version": SCHEMA_VERSION,
        "occurred_at": _rfc3339(now),
        # Derived, and 1:1 with the event id. Correlation exists to tie a
        # fan-out together; a timer sweep has no fan-out and no customer
        # transaction, so inventing a shared random id would imply a
        # relationship between unrelated tracks that nothing else honours.
        "correlation_id": str(uuid.uuid5(_NAMESPACE, "correlation|" + event_id)),
        "causation_id": None,
        "producer": PRODUCER,
        "data": {
            "satellite_id": track.satellite_id,
            "computed_at": _rfc3339(now),
            "horizon": {
                "start": _rfc3339(track.horizon_start),
                "end": _rfc3339(track.horizon_end),
            },
            "tle_reference": {
                "satellite_id": track.satellite_id,
                "norad_id": element_set.norad_id,
                "tle_epoch": _rfc3339(element_set.epoch),
                # Negative ages are possible — a predicted element set has an
                # epoch in the future — and the contract's minimum is 0, so the
                # floor is applied here rather than emitting something the
                # schema rejects.
                "tle_age_hours": max(0.0, age_hours),
                "staleness": str(staleness.classify(max(0.0, age_hours))),
            },
            "epoch": _rfc3339(track.epoch),
            "sample_interval_s": track.interval_s,
            "sample_count": len(track.samples),
            "samples": [list(sample) for sample in track.samples],
        },
    }

    return OutboxMessage(
        event_id=event_id,
        event_type=EPHEMERIS_SUBJECT,
        schema_version=SCHEMA_VERSION,
        subject=EPHEMERIS_SUBJECT,
        payload=payload,
        occurred_at=now,
        headers=headers or {},
    )


def _rfc3339(when: datetime) -> str:
    """UTC, `Z`-suffixed, milliseconds only when there are any.

    One rule, stated once, because timestamps here are compared as STRINGS in
    the derived event id as well as read as instants by consumers. Python's
    `isoformat()` emits `+00:00` and either six fractional digits or none, so it
    would give two different renderings of the same instant depending on how it
    was constructed — and two renderings mean two event ids for one bucket.
    """
    when = when.astimezone(UTC)
    if when.microsecond:
        return when.strftime("%Y-%m-%dT%H:%M:%S.") + f"{when.microsecond // 1000:03d}Z"
    return when.strftime("%Y-%m-%dT%H:%M:%SZ")

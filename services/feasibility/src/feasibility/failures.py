"""The refusal event: a correct negative answer, addressed properly.

Pure, like `ephemeris.build_event` and for the same reason — the whole publish
path can be asserted without a broker or a database in the room, and the payload
a producer really emits can be written to disk and shown to the schema.

WHAT WAS WRONG. `worker._publish_refusal` enqueued six bare fields:

    {"request_id": ..., "reason_code": ..., "retryable": ..., "reason_detail":
     ..., "attempt": ..., "failed_at": ...}

The relay publishes the payload column verbatim, so that is what reached the
wire. `feasibility.failed.v1` requires the seven envelope fields with those six
nested under `data`, and sets `additionalProperties: false` — so the emitted
event was wrong in both directions at once: `data` missing, six undeclared
properties present. plan-gateway decodes with `DisallowUnknownFields` and would
have rejected it; this service's own `delivery_from` reads `event_id` from the
body and could not have deduplicated it on redelivery.

It survived because nothing had ever shown the schema what this producer emits.
That is #124 exactly, and the cure is the same: record the real payload under
`testdata/published-events/` and let `contracts-validate` be the gate.

CAUSATION IS NOT OPTIONAL HERE. A refusal has an antecedent — a customer's
request — and the envelope exists to carry that edge. Correlation is propagated
from the causing event rather than invented, because the entire point of the
field is that one customer transaction stays greppable by one value across every
service it touches.
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

FAILURE_SUBJECT = "feasibility.failed.v1"
SCHEMA_VERSION = "1.0.0"
PRODUCER = "feasibility-service"

# The contract's own enum, restated at the producer.
#
# Not defensive padding. The generated binding is not consulted at publish time
# and the schema is not either — `contracts-validate` runs in CI, against
# recorded fixtures, which is a gate on the SHAPES this producer is known to
# emit and not on every value it might compute. A reason code invented at a call
# site would sail past every check here and fail at the consumer.
REASON_CODES = frozenset(
    {
        "NO_ACCESS_IN_HORIZON",
        "CONSTRAINTS_TOO_NARROW",
        "TLE_STALE",
        "TLE_UNAVAILABLE",
        "PROPAGATION_ERROR",
        "UNSUPPORTED_TARGET_GEOMETRY",
        "HORIZON_EXHAUSTED",
        "INTERNAL_ERROR",
    }
)


def build_refusal(
    delivery: Delivery,
    reason_code: str,
    retryable: bool,
    detail: str,
    now: datetime,
    horizon: tuple[datetime, datetime] | None = None,
    satellites_evaluated: int | None = None,
    tle_references: Sequence[dict[str, Any]] | None = None,
) -> OutboxMessage:
    """Render one refusal as a contract event, ready for the outbox.

    `now` is passed rather than read from the clock, for the same reason every
    other builder here takes it: a payload that samples wall time cannot be
    recorded as a fixture and compared with anything.

    The optional trailing arguments are the sweep's context — which horizon was
    searched, how many satellites were evaluated, which element sets were used.
    They are absent from the payload when unknown rather than null: the schema
    types them, and null is not one of those types.
    """
    if reason_code not in REASON_CODES:
        msg = (
            f"{reason_code!r} is not a contract reason code. "
            f"feasibility.failed.v1 permits: {', '.join(sorted(REASON_CODES))}"
        )
        raise ValueError(msg)

    causing = _causing_envelope(delivery)
    request_id = causing.get("data", {}).get("request_id")
    if not request_id:
        # There is genuinely nothing to attribute this refusal to. Publishing it
        # with a null request_id — which is what the old code did — produces an
        # event the schema rejects and that no consumer can route. The caller
        # must handle an unattributable failure some other way; the message is
        # already terminal and will be acked.
        msg = (
            "cannot build a refusal with no request_id: the causing message carried "
            "no parseable envelope, so this failure cannot be attributed to a request"
        )
        raise ValueError(msg)

    data: dict[str, Any] = {
        "request_id": request_id,
        "reason_code": reason_code,
        "retryable": retryable,
        "attempt": delivery.delivered_count,
        "failed_at": _rfc3339(now),
    }
    if detail:
        # maxLength 1024 in the contract. Truncated here rather than at the
        # boundary, because a detail long enough to trip it is a stack trace
        # somebody pasted in and the first kilobyte is the useful part.
        data["reason_detail"] = detail[:1024]
    if horizon is not None:
        data["horizon"] = {"start": _rfc3339(horizon[0]), "end": _rfc3339(horizon[1])}
    if satellites_evaluated is not None:
        data["satellites_evaluated"] = satellites_evaluated
    if tle_references is not None:
        data["tle_references"] = list(tle_references)

    event_id = str(uuid.uuid4())
    payload = {
        "event_id": event_id,
        "event_type": FAILURE_SUBJECT,
        "schema_version": SCHEMA_VERSION,
        "occurred_at": _rfc3339(now),
        # Propagated, not invented. A fresh id here breaks the chain at exactly
        # the hop somebody following a failed request is trying to read.
        "correlation_id": _correlation_of(causing),
        # The event that caused this one. Never null: a refusal always has an
        # antecedent, and saying otherwise would make the causal graph lie.
        "causation_id": delivery.event_id,
        "producer": PRODUCER,
        "data": data,
    }

    return OutboxMessage(
        event_id=event_id,
        event_type=FAILURE_SUBJECT,
        schema_version=SCHEMA_VERSION,
        subject=FAILURE_SUBJECT,
        payload=payload,
        occurred_at=now,
        # The traceparent from the delivery, so the refusal stays on the trace
        # that produced it.
        headers=dict(delivery.headers),
    )


def _causing_envelope(delivery: Delivery) -> dict[str, Any]:
    """The causing event, or an empty envelope if it cannot be read.

    Returning `{}` rather than raising, so the missing-request_id case above is
    reported as the thing that is actually wrong. A JSONDecodeError here would
    say "the refusal failed to build", which is true and unhelpful.
    """
    try:
        decoded = json.loads(delivery.payload)
    except (json.JSONDecodeError, TypeError, ValueError):
        return {}
    return decoded if isinstance(decoded, dict) else {}


def _correlation_of(causing: dict[str, Any]) -> str:
    """The causing event's correlation id, or a fresh one.

    A fresh one is a last resort and is worse than propagating — it starts a new
    tree for an event that belongs to an existing one. It exists because
    `correlation_id` is REQUIRED by the envelope, so an upstream that omitted it
    must not be able to make this event unpublishable. Losing one edge of the
    graph beats losing the refusal entirely.
    """
    correlation = causing.get("correlation_id")
    if isinstance(correlation, str) and correlation:
        return correlation
    return str(uuid.uuid4())


def _rfc3339(when: datetime) -> str:
    """UTC, `Z`-suffixed, milliseconds only when there are any."""
    when = when.astimezone(UTC)
    if when.microsecond:
        return when.strftime("%Y-%m-%dT%H:%M:%S.") + f"{when.microsecond // 1000:03d}Z"
    return when.strftime("%Y-%m-%dT%H:%M:%SZ")

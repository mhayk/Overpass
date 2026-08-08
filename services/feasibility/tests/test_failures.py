"""The refusal event, against the contract rather than against itself.

#124's lesson, applied to the other producer. A test that builds its expected
payload with the same helper it asserts against will agree with itself forever
while emitting something the schema rejects — which is exactly what happened
here: `_publish_refusal` enqueued six bare fields with no envelope at all, the
worker test asserted `payload->>'reason_code'`, and both were wrong together.

So the real gate is not in this file. Every test below that matters writes the
payload the producer ACTUALLY emits into `testdata/published-events/`, where
`scripts/contracts_validate.py` validates it against the JSON Schema itself with
format assertion turned on. That is deliberately not the generated Pydantic
binding: `contracts/README.md` is explicit that the schema is the authority on
validity and the bindings are a convenience, and the binding is the weaker check
of the two.

What IS asserted here is the structure a reader would have to get right —
where the fields live, what causation and correlation carry, and what is omitted
rather than sent as null. Validity is the gate's job.
"""

from __future__ import annotations

import json
import uuid
from datetime import UTC, datetime
from pathlib import Path

import pytest

from feasibility.failures import FAILURE_SUBJECT, build_refusal
from feasibility.messaging.idempotency import Delivery

# The envelope, from the contract. Listed here rather than imported from the
# schema at runtime so that a field being dropped is a failing test rather than
# a check that quietly compares nothing.
ENVELOPE_FIELDS = frozenset(
    {
        "event_id",
        "event_type",
        "schema_version",
        "occurred_at",
        "correlation_id",
        "causation_id",
        "producer",
        "data",
    }
)

WHEN = datetime(2026, 8, 7, 9, 20, 31, 200000, tzinfo=UTC)

CAUSING_EVENT_ID = "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f"
CORRELATION_ID = "2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e"
REQUEST_ID = "8a9b0c1d-2e3f-4a5b-8c6d-7e8f9a0b1c2d"


def causing_delivery(*, delivered: int = 1, correlation: str | None = CORRELATION_ID) -> Delivery:
    """A tasking.request.received.v1 as it arrives, envelope and all."""
    envelope: dict[str, object] = {
        "event_id": CAUSING_EVENT_ID,
        "event_type": "tasking.request.received.v1",
        "schema_version": "1.0.0",
        "occurred_at": "2026-08-07T09:00:00Z",
        "causation_id": None,
        "producer": "tasking-api",
        "data": {"request_id": REQUEST_ID},
    }
    if correlation is not None:
        envelope["correlation_id"] = correlation

    return Delivery(
        event_id=CAUSING_EVENT_ID,
        subject="tasking.request.received.v1",
        payload=json.dumps(envelope).encode(),
        delivered_count=delivered,
        headers={"traceparent": "00-" + "a" * 32 + "-" + "b" * 16 + "-01"},
    )


class TestTheEnvelope:
    def test_the_payload_carries_the_envelope_and_nothing_else(self) -> None:
        # The assertion the old code could never have passed. It enqueued the
        # six data fields at the top level, so `data` was missing and six
        # undeclared properties were present — and `additionalProperties: false`
        # rejects both halves of that.
        message = build_refusal(
            causing_delivery(), "NO_ACCESS_IN_HORIZON", False, "nothing sees it", WHEN
        )

        assert set(message.payload) == ENVELOPE_FIELDS
        assert message.payload["event_type"] == "feasibility.failed.v1"
        assert message.payload["producer"] == "feasibility-service"
        assert message.payload["schema_version"] == "1.0.0"

    def test_the_six_data_fields_are_nested_under_data_not_at_the_top(self) -> None:
        message = build_refusal(causing_delivery(), "TLE_STALE", True, "79.2h old", WHEN)

        assert "reason_code" not in message.payload
        assert message.payload["data"]["reason_code"] == "TLE_STALE"
        assert message.payload["data"]["retryable"] is True
        assert message.payload["data"]["request_id"] == REQUEST_ID

    def test_it_is_addressed_and_versioned_for_the_outbox(self) -> None:
        message = build_refusal(causing_delivery(), "NO_ACCESS_IN_HORIZON", False, "", WHEN)

        assert message.subject == FAILURE_SUBJECT
        assert message.event_type == FAILURE_SUBJECT
        assert message.occurred_at == WHEN
        # The row's event_id and the envelope's must be the same value, or the
        # outbox deduplicates on one id while every consumer deduplicates on
        # another.
        assert message.event_id == message.payload["event_id"]

    def test_the_traceparent_survives_onto_the_published_message(self) -> None:
        # Dropping it is how one distributed trace silently becomes two, and
        # the refusal is the branch of the trace somebody is most likely to be
        # looking for.
        message = build_refusal(causing_delivery(), "NO_ACCESS_IN_HORIZON", False, "", WHEN)
        assert "traceparent" in message.headers


class TestCausationAndCorrelation:
    def test_causation_is_the_event_that_caused_the_refusal(self) -> None:
        # Correlation gives you the tree; causation gives you the edges. A null
        # causation here would say this refusal had no antecedent, which is
        # false — a customer's request is exactly what caused it.
        message = build_refusal(causing_delivery(), "NO_ACCESS_IN_HORIZON", False, "", WHEN)
        assert message.payload["causation_id"] == CAUSING_EVENT_ID

    def test_correlation_is_propagated_rather_than_invented(self) -> None:
        # One customer request stays greppable by one value across every
        # service it touches. A fresh uuid here would break the chain at the
        # exact hop somebody is trying to follow.
        message = build_refusal(causing_delivery(), "NO_ACCESS_IN_HORIZON", False, "", WHEN)
        assert message.payload["correlation_id"] == CORRELATION_ID

    def test_a_causing_event_with_no_correlation_still_produces_a_valid_one(self) -> None:
        # The envelope makes correlation_id required, so an upstream that
        # omitted it cannot be allowed to make this event unpublishable.
        message = build_refusal(
            causing_delivery(correlation=None), "INTERNAL_ERROR", False, "", WHEN
        )
        assert uuid.UUID(message.payload["correlation_id"])

    def test_the_event_id_is_not_the_causing_event_id(self) -> None:
        # They were the same value in an earlier draft, which made the refusal
        # look like a redelivery of the request to any consumer deduplicating
        # on event_id.
        message = build_refusal(causing_delivery(), "NO_ACCESS_IN_HORIZON", False, "", WHEN)
        assert message.payload["event_id"] != CAUSING_EVENT_ID


class TestTheDataItCarries:
    def test_the_attempt_is_the_delivery_count(self) -> None:
        message = build_refusal(causing_delivery(delivered=3), "INTERNAL_ERROR", False, "", WHEN)
        assert message.payload["data"]["attempt"] == 3

    def test_an_empty_detail_is_omitted_rather_than_sent_as_an_empty_string(self) -> None:
        message = build_refusal(causing_delivery(), "NO_ACCESS_IN_HORIZON", False, "", WHEN)
        assert "reason_detail" not in message.payload["data"]

    def test_the_sweep_context_is_carried_when_there_is_any(self) -> None:
        # `tle_references` on a failure is the whole point of TLE_STALE: it
        # shows which satellite's element set was too old and by how much.
        references = [
            {
                "satellite_id": "CAPELLA-14",
                "norad_id": 55555,
                "tle_epoch": "2026-08-04T02:00:00Z",
                "tle_age_hours": 79.2,
                "staleness": "STALE",
            }
        ]
        message = build_refusal(
            causing_delivery(),
            "TLE_STALE",
            True,
            "79.2h old",
            WHEN,
            horizon=(
                datetime(2026, 8, 7, 10, tzinfo=UTC),
                datetime(2026, 8, 8, 10, tzinfo=UTC),
            ),
            satellites_evaluated=6,
            tle_references=references,
        )

        data = message.payload["data"]
        assert data["horizon"] == {
            "start": "2026-08-07T10:00:00Z",
            "end": "2026-08-08T10:00:00Z",
        }
        assert data["satellites_evaluated"] == 6
        assert data["tle_references"][0]["staleness"] == "STALE"

    def test_the_optional_context_is_absent_rather_than_null_when_unknown(self) -> None:
        # `additionalProperties: false` tolerates an absent optional field and
        # not a null one — the schema types these, and null is not the type.
        message = build_refusal(causing_delivery(), "INTERNAL_ERROR", False, "", WHEN)
        data = message.payload["data"]
        for optional in ("horizon", "satellites_evaluated", "tle_references"):
            assert optional not in data

    def test_a_reason_code_outside_the_contract_is_refused_here(self) -> None:
        # Not left to the schema. The producer is the last place this can be
        # caught before it is a published event nobody can parse, and the
        # generated binding is not consulted at publish time.
        with pytest.raises(ValueError, match="not a contract reason code"):
            build_refusal(causing_delivery(), "SOMETHING_WENT_WRONG", False, "", WHEN)


class TestWhatItActuallyPublishes:
    def test_it_records_what_it_publishes_for_the_contract_gate(self) -> None:
        """Write the real payload where scripts/contracts_validate.py finds it.

        The gate that would have caught this defect on the day it shipped, and
        did not exist for this producer until now.
        """
        message = build_refusal(
            causing_delivery(),
            "TLE_STALE",
            True,
            "TLE for CAPELLA-14 is 79.2h old, exceeding the staleness threshold",
            WHEN,
            horizon=(
                datetime(2026, 8, 7, 10, tzinfo=UTC),
                datetime(2026, 8, 8, 10, tzinfo=UTC),
            ),
            satellites_evaluated=6,
            tle_references=[
                {
                    "satellite_id": "CAPELLA-14",
                    "norad_id": 55555,
                    "tle_epoch": "2026-08-04T02:00:00Z",
                    "tle_age_hours": 79.2,
                    "staleness": "STALE",
                }
            ],
        )

        destination = (
            Path(__file__).resolve().parents[1]
            / "testdata"
            / "published-events"
            / FAILURE_SUBJECT
            / "tle-stale.json"
        )
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(json.dumps(message.payload, indent=2) + "\n")

        assert json.loads(destination.read_text())["data"]["reason_code"] == "TLE_STALE"

    def test_a_bare_refusal_is_recorded_too(self) -> None:
        # The minimal shape, which is what most refusals actually look like:
        # no horizon, no element sets, because the sweep never got that far.
        message = build_refusal(
            causing_delivery(), "NO_ACCESS_IN_HORIZON", False, "nothing rises above the mask", WHEN
        )
        destination = (
            Path(__file__).resolve().parents[1]
            / "testdata"
            / "published-events"
            / FAILURE_SUBJECT
            / "no-access.json"
        )
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(json.dumps(message.payload, indent=2) + "\n")

        assert "horizon" not in json.loads(destination.read_text())["data"]


def test_a_payload_that_is_not_json_does_not_stop_the_refusal() -> None:
    """A refusal about an unparseable message must still be publishable.

    This is the case the old code handled with `contextlib.suppress` and a
    `request_id` of None — which the contract rejects, because request_id is
    required and is a uuid. There is genuinely no request id to report, so the
    honest answer is that this refusal cannot be attributed and must not be
    published as though it could.
    """
    delivery = Delivery(
        event_id=CAUSING_EVENT_ID,
        subject="tasking.request.received.v1",
        payload=b"not json at all",
        delivered_count=1,
        headers={},
    )
    with pytest.raises(ValueError, match="no request_id"):
        build_refusal(delivery, "INTERNAL_ERROR", False, "unparseable", WHEN)

"""The opportunities event, against the contract rather than against itself.

The sweep behind it is checked in test_pipeline and test_golden_orbital. What is
checked here is the rendering: the shape the planner receives, the provenance
that makes a window auditable, and the cap that must be declared rather than
applied quietly.

As with the refusal, the real gate is `contracts-validate` over the payload this
recorded under `testdata/published-events/`. A producer that only ever agrees
with its own tests is the failure #124 was.
"""

from __future__ import annotations

import json
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest
from shapely.geometry import Polygon

from feasibility.messaging.idempotency import Delivery
from feasibility.opportunities import (
    MAX_OPPORTUNITIES,
    OPPORTUNITIES_SUBJECT,
    build_event,
)
from feasibility.pipeline import Opportunity, SweepOutcome
from feasibility.sar import AccessGeometry, LookSide

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

WHEN = datetime(2026, 8, 7, 9, 14, 4, 870000, tzinfo=UTC)
HORIZON_START = datetime(2026, 8, 7, 10, tzinfo=UTC)
HORIZON_END = datetime(2026, 8, 8, 10, tzinfo=UTC)

CAUSING_EVENT_ID = "0f1d2c3b-4a59-4687-9c8d-1e2f3a4b5c6d"
CORRELATION_ID = "9b8a7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
REQUEST_ID = "3c4d5e6f-7a8b-4c9d-8e1f-2a3b4c5d6e7f"

TLE_REFERENCES = [
    {
        "satellite_id": "SENTINEL-1A",
        "norad_id": 39634,
        "tle_epoch": "2026-08-06T21:41:12Z",
        "tle_age_hours": 11.6,
        "staleness": "FRESH",
    }
]


def causing_delivery() -> Delivery:
    envelope = {
        "event_id": CAUSING_EVENT_ID,
        "event_type": "tasking.request.received.v1",
        "correlation_id": CORRELATION_ID,
        "data": {"request_id": REQUEST_ID},
    }
    return Delivery(
        event_id=CAUSING_EVENT_ID,
        subject="tasking.request.received.v1",
        payload=json.dumps(envelope).encode(),
        delivered_count=1,
        headers={"traceparent": "00-" + "c" * 32 + "-" + "d" * 16 + "-01"},
    )


def an_opportunity(index: int, quality: float = 0.9) -> Opportunity:
    """A candidate with a realistic swath, not a four-corner rectangle."""
    ring = [
        (-7.66 + index * 0.0001, 40.28),
        (-7.57 + index * 0.0001, 40.28),
        (-7.57 + index * 0.0001, 40.35),
        (-7.66 + index * 0.0001, 40.35),
        (-7.66 + index * 0.0001, 40.28),
    ]
    return Opportunity(
        opportunity_id=str(uuid.uuid5(uuid.NAMESPACE_OID, f"opportunity-{index}")),
        satellite_id="SENTINEL-1A",
        mode="SPOTLIGHT",
        access_start=HORIZON_START + timedelta(minutes=index),
        access_end=HORIZON_START + timedelta(minutes=index, seconds=96),
        acquisition_duration_s=12.5,
        orbit_number=61204 + index,
        geometry=AccessGeometry(
            incidence_angle_deg=33.7,
            look_side=LookSide.RIGHT,
            squint_angle_deg=-4.1,
            slant_range_km=812.4,
            elevation_angle_deg=54.9,
            ground_azimuth_deg=281.3,
            roll_angle_deg=29.2,
        ),
        footprint=Polygon(ring),
        duty_cycle_cost_s=18.5,
        quality_score=quality,
    )


def outcome_of(*opportunities: Opportunity, truncated: bool = False) -> SweepOutcome:
    return SweepOutcome(
        opportunities=list(opportunities),
        refusal=None,
        satellites_evaluated=6,
        truncated=truncated,
        horizon_start=HORIZON_START,
        horizon_end=HORIZON_END,
    )


class TestTheEnvelope:
    def test_the_payload_carries_the_envelope_and_nothing_else(self) -> None:
        message = build_event(
            causing_delivery(), REQUEST_ID, outcome_of(an_opportunity(0)), TLE_REFERENCES, WHEN, 742
        )

        assert set(message.payload) == ENVELOPE_FIELDS
        assert message.payload["event_type"] == OPPORTUNITIES_SUBJECT
        assert message.payload["producer"] == "feasibility-service"

    def test_it_names_the_request_that_caused_it(self) -> None:
        message = build_event(
            causing_delivery(), REQUEST_ID, outcome_of(an_opportunity(0)), TLE_REFERENCES, WHEN, 1
        )

        assert message.payload["causation_id"] == CAUSING_EVENT_ID
        assert message.payload["correlation_id"] == CORRELATION_ID

    def test_the_event_id_is_derived_so_a_replay_produces_the_same_event(self) -> None:
        # A replay from the stream is supported. Two runs of the same request
        # must be one event downstream, not two sets of opportunities the
        # planner has to reconcile.
        def build() -> str:
            return build_event(
                causing_delivery(),
                REQUEST_ID,
                outcome_of(an_opportunity(0)),
                TLE_REFERENCES,
                WHEN,
                1,
            ).event_id

        assert build() == build()
        assert uuid.UUID(build()).version == 5


class TestTheDataThePlannerReads:
    def test_an_opportunity_carries_its_geometry_and_footprint(self) -> None:
        message = build_event(
            causing_delivery(), REQUEST_ID, outcome_of(an_opportunity(0)), TLE_REFERENCES, WHEN, 1
        )
        (opportunity,) = message.payload["data"]["opportunities"]

        assert opportunity["mode"] == "SPOTLIGHT"
        assert opportunity["geometry"]["look_side"] == "RIGHT"
        assert opportunity["geometry"]["incidence_angle_deg"] == 33.7
        assert opportunity["footprint"]["type"] == "Polygon"
        # Longitude first, and a closed ring. Both are silent when wrong: the
        # first relocates the swath, the second makes PostGIS reject it.
        ring = opportunity["footprint"]["coordinates"][0]
        assert ring[0] == ring[-1]
        assert ring[0][0] == pytest.approx(-7.66, abs=1e-6)

    def test_footprint_coordinates_are_rounded_to_six_places(self) -> None:
        # The largest payload this system produces. Six places is ~0.1 m, the
        # same precision the read layer asks PostGIS for, and the digits past it
        # describe a millimetre of a swath edge derived from a propagator whose
        # own error is metres.
        message = build_event(
            causing_delivery(), REQUEST_ID, outcome_of(an_opportunity(7)), TLE_REFERENCES, WHEN, 1
        )
        ring = message.payload["data"]["opportunities"][0]["footprint"]["coordinates"][0]
        for longitude, latitude in ring:
            assert round(longitude, 6) == longitude
            assert round(latitude, 6) == latitude

    def test_the_count_is_the_length_of_the_array(self) -> None:
        # Redundant on purpose: consumers log and alert on it without
        # deserialising a multi-megabyte array. Redundancy that disagrees with
        # itself is worse than none.
        outcome = outcome_of(*(an_opportunity(i) for i in range(5)))
        message = build_event(causing_delivery(), REQUEST_ID, outcome, TLE_REFERENCES, WHEN, 1)

        data = message.payload["data"]
        assert data["opportunity_count"] == len(data["opportunities"]) == 5

    def test_provenance_covers_every_satellite_considered(self) -> None:
        message = build_event(
            causing_delivery(), REQUEST_ID, outcome_of(an_opportunity(0)), TLE_REFERENCES, WHEN, 1
        )
        assert message.payload["data"]["tle_references"] == TLE_REFERENCES
        assert message.payload["data"]["satellites_evaluated"] == 6

    def test_the_horizon_is_the_one_actually_searched(self) -> None:
        # Not the one requested. A customer asking for thirty days gets an
        # honest answer about the seven that were searched.
        message = build_event(
            causing_delivery(), REQUEST_ID, outcome_of(an_opportunity(0)), TLE_REFERENCES, WHEN, 1
        )
        assert message.payload["data"]["horizon"] == {
            "start": "2026-08-07T10:00:00Z",
            "end": "2026-08-08T10:00:00Z",
        }


class TestTheCap:
    def test_a_result_inside_the_cap_is_not_marked_truncated(self) -> None:
        outcome = outcome_of(*(an_opportunity(i) for i in range(3)))
        message = build_event(causing_delivery(), REQUEST_ID, outcome, TLE_REFERENCES, WHEN, 1)
        assert message.payload["data"]["truncated"] is False

    def test_a_clamped_horizon_is_reported_even_when_the_cap_did_not_bite(self) -> None:
        # Two different ways a result is short of the whole truth. The sweep's
        # own `truncated` means the planning horizon was clamped; conflating it
        # with the cap would hide either one.
        outcome = outcome_of(an_opportunity(0), truncated=True)
        message = build_event(causing_delivery(), REQUEST_ID, outcome, TLE_REFERENCES, WHEN, 1)
        assert message.payload["data"]["truncated"] is True

    def test_the_cap_drops_the_lowest_quality_and_says_so(self) -> None:
        # Quality order, not time order. Dropping the tail of the horizon would
        # silently shorten the window the planner believes it has.
        keep = [an_opportunity(i, quality=0.9) for i in range(MAX_OPPORTUNITIES)]
        drop = [an_opportunity(MAX_OPPORTUNITIES + i, quality=0.1) for i in range(5)]
        message = build_event(
            causing_delivery(), REQUEST_ID, outcome_of(*keep, *drop), TLE_REFERENCES, WHEN, 1
        )

        data = message.payload["data"]
        assert data["truncated"] is True
        assert data["opportunity_count"] == MAX_OPPORTUNITIES
        assert {o["quality_score"] for o in data["opportunities"]} == {0.9}

    def test_what_survives_the_cap_is_still_in_access_order(self) -> None:
        keep = [an_opportunity(i, quality=0.9) for i in range(MAX_OPPORTUNITIES + 3)]
        message = build_event(
            causing_delivery(), REQUEST_ID, outcome_of(*keep), TLE_REFERENCES, WHEN, 1
        )
        starts = [o["access_window"]["start"] for o in message.payload["data"]["opportunities"]]
        assert starts == sorted(starts)


class TestWhatItRefusesToBuild:
    def test_an_empty_outcome_is_not_this_event(self) -> None:
        # The contract sets minItems 1 and opportunity_count minimum 1, because
        # "no opportunities" is feasibility.failed.v1 with a reason code — an
        # empty success would tell the planner nothing and the customer less.
        with pytest.raises(ValueError, match="refusal, not this event"):
            build_event(causing_delivery(), REQUEST_ID, outcome_of(), TLE_REFERENCES, WHEN, 1)


def test_it_records_what_it_publishes_for_the_contract_gate() -> None:
    """The gate that would have caught #124 on the day it shipped."""
    outcome = outcome_of(an_opportunity(0), an_opportunity(1, quality=0.72))
    message = build_event(causing_delivery(), REQUEST_ID, outcome, TLE_REFERENCES, WHEN, 742)

    destination = (
        Path(__file__).resolve().parents[1]
        / "testdata"
        / "published-events"
        / OPPORTUNITIES_SUBJECT
        / "two-candidates.json"
    )
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(message.payload, indent=2) + "\n")

    assert json.loads(destination.read_text())["data"]["opportunity_count"] == 2

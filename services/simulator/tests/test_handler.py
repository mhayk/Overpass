"""The seam between the physics and the pipeline.

The load-bearing test here is the last one: the emitted event is validated
against the COMMITTED JSON SCHEMA, not against this service's idea of it. A
producer that agrees with itself proves nothing — M0's hard-won rule is that the
schema is the authority on validity, and both language bindings drop enough of
it that a structurally wrong payload can round-trip through generated types
cleanly.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime, timedelta
from pathlib import Path
from random import Random

import pytest

from simulator.execution import GroundPoint, InjectionRates, Status
from simulator.handler import (
    PRODUCER,
    SCHEMA_VERSION,
    SUBJECT,
    Acquisition,
    Plan,
    centroid,
    event_id_for,
    execute,
    executed_event,
)
from simulator.orbit import ElementSet

REPO = Path(__file__).resolve().parents[3]
SCHEMA = REPO / "contracts" / "events" / "acquisition.executed.v1.schema.json"

WINDOW = (
    datetime(2026, 8, 11, 12, 0, tzinfo=UTC),
    datetime(2026, 8, 11, 12, 0, 15, tzinfo=UTC),
)


def acquisition(mode: str = "SCAN", target: GroundPoint | None = None) -> Acquisition:
    return Acquisition(
        acquisition_id="44444444-4444-4444-4444-444444444444",
        request_id="11111111-1111-1111-1111-111111111111",
        customer_id="acme-imaging",
        mode=mode,
        window=WINDOW,
        target=target if target is not None else GroundPoint(51.9, 4.4),
        duty_cycle_cost_s=15.0,
    )


def plan() -> Plan:
    return Plan(
        plan_id="33333333-3333-3333-3333-333333333333",
        satellite_id="SENTINEL-1B",
        committed_at=datetime(2026, 8, 11, 11, 0, tzinfo=UTC),
        acquisitions=(acquisition(),),
    )


def elements(satellite_id: str, epoch: datetime) -> ElementSet:
    """A real element set, so propagation is real. SENTINEL-1B from the fixture."""
    lines = [
        line.rstrip()
        for line in (REPO / "testdata" / "tle" / "sar-constellation.2026-08-07.tle")
        .read_text()
        .splitlines()
        if line.strip() and not line.startswith("#")
    ]
    for i in range(0, len(lines) - 2, 3):
        if lines[i].strip().split(" ")[0] == satellite_id:
            return ElementSet(
                satellite_id=satellite_id,
                epoch=epoch,
                line1=lines[i + 1],
                line2=lines[i + 2],
            )
    msg = f"{satellite_id} is not in the fixture"
    raise AssertionError(msg)


class TestCentroid:
    def test_it_is_the_middle_of_the_ring(self) -> None:
        ring = [[4.0, 51.0], [5.0, 51.0], [5.0, 52.0], [4.0, 52.0]]
        point = centroid(ring)
        assert point.longitude_deg == pytest.approx(4.5)
        assert point.latitude_deg == pytest.approx(51.5)

    def test_longitude_comes_first(self) -> None:
        # GeoJSON is longitude-first, and a swap renders perfectly happily in
        # the wrong hemisphere — the exact mistake CLAUDE.md records from M0.
        point = centroid([[4.4, 51.9]])
        assert point.longitude_deg == 4.4
        assert point.latitude_deg == 51.9

    def test_an_empty_ring_is_refused(self) -> None:
        with pytest.raises(ValueError, match="no vertices"):
            centroid([])


class TestExecute:
    def test_an_unknown_mode_is_refused_rather_than_defaulted(self) -> None:
        # A mode this map does not know would otherwise silently borrow another
        # mode's swath, and every outcome for it would be wrong in a way nothing
        # reports.
        with pytest.raises(ValueError, match="no swath half-width"):
            execute(
                acquisition=acquisition(mode="INTERFEROMETRIC"),
                planning_elements=elements("SENTINEL-1B", WINDOW[0] - timedelta(hours=70)),
                truth_elements=elements("SENTINEL-1B", WINDOW[0]),
                rates=InjectionRates(),
                random=Random(1),
            )

    def test_the_planning_age_is_carried_through(self) -> None:
        planning = elements("SENTINEL-1B", WINDOW[0] - timedelta(hours=70))
        outcome = execute(
            acquisition=acquisition(),
            planning_elements=planning,
            truth_elements=elements("SENTINEL-1B", WINDOW[0]),
            rates=InjectionRates(),
            random=Random(1),
        )
        # The number the staleness correlation is against, carried so nothing
        # has to join back to the plan to recover it.
        assert outcome.planning_tle_age_hours == pytest.approx(70.0, abs=0.01)

    def test_a_target_under_the_track_succeeds(self) -> None:
        # Built from the propagation itself rather than a guessed coordinate:
        # the target is placed where the satellite actually passes, so this
        # asserts the plumbing rather than the weather.
        from simulator.orbit import Propagator

        truth = elements("SENTINEL-1B", WINDOW[0])
        beneath = Propagator(truth).subpoint(WINDOW[0])
        outcome = execute(
            acquisition=acquisition(target=beneath),
            planning_elements=elements("SENTINEL-1B", WINDOW[0] - timedelta(hours=70)),
            truth_elements=truth,
            rates=InjectionRates(
                attitude_error=0.0,
                slew_overrun=0.0,
                power_budget_exceeded=0.0,
                thermal_limit=0.0,
                sensor_fault=0.0,
                ground_abort=0.0,
                partial=0.0,
            ),
            random=Random(1),
        )
        assert outcome.status is Status.SUCCEEDED
        assert outcome.cross_track_km < 1.0

    def test_a_target_far_from_the_track_is_a_drift_miss(self) -> None:
        from simulator.execution import FailureReason
        from simulator.orbit import Propagator

        truth = elements("SENTINEL-1B", WINDOW[0])
        beneath = Propagator(truth).subpoint(WINDOW[0])
        far = GroundPoint(beneath.latitude_deg + 3.0, beneath.longitude_deg)
        outcome = execute(
            acquisition=acquisition(target=far),
            planning_elements=elements("SENTINEL-1B", WINDOW[0] - timedelta(hours=70)),
            truth_elements=truth,
            rates=InjectionRates(),
            random=Random(1),
        )
        assert outcome.status is Status.FAILED
        assert outcome.failure_reason is FailureReason.TLE_DRIFT_MISS


class TestEventIdentity:
    def test_the_id_is_stable_for_an_acquisition(self) -> None:
        # A REDELIVERED PLAN MUST NOT PUBLISH TWO EXECUTIONS. The id is derived
        # from the acquisition, so the outbox's unique constraint catches the
        # second one instead of the read model seeing an acquisition executed
        # twice.
        assert event_id_for("abc") == event_id_for("abc")

    def test_different_acquisitions_get_different_ids(self) -> None:
        assert event_id_for("abc") != event_id_for("abd")


class TestTheEmittedEvent:
    def _valid(self, outcome_status: Status) -> dict[str, object]:
        outcome = execute(
            acquisition=acquisition(),
            planning_elements=elements("SENTINEL-1B", WINDOW[0] - timedelta(hours=70)),
            truth_elements=elements("SENTINEL-1B", WINDOW[0]),
            rates=InjectionRates(),
            random=Random(1),
        )
        del outcome_status
        return executed_event(
            plan=plan(),
            acquisition=acquisition(),
            outcome=outcome,
            executed_at=datetime(2026, 8, 11, 11, 30, tzinfo=UTC),
        )

    def test_it_carries_what_the_contract_requires(self) -> None:
        schema = json.loads(SCHEMA.read_text())
        required = schema["properties"]["data"]["required"]
        data = self._valid(Status.SUCCEEDED)
        for field in required:
            assert field in data, f"{field} is required by {SUBJECT} and is missing"

    def test_timestamps_are_utc_and_zulu(self) -> None:
        data = self._valid(Status.SUCCEEDED)
        assert str(data["executed_at"]).endswith("Z")
        scheduled = data["scheduled_window"]
        assert isinstance(scheduled, dict)
        assert str(scheduled["start"]).endswith("Z")

    def test_a_naive_datetime_is_refused(self) -> None:
        naive = Acquisition(
            acquisition_id="a",
            request_id="r",
            customer_id=None,
            mode="SCAN",
            window=(datetime(2026, 8, 11, 12, 0), datetime(2026, 8, 11, 12, 0, 15)),
            target=GroundPoint(51.9, 4.4),
            duty_cycle_cost_s=15.0,
        )
        outcome = execute(
            acquisition=acquisition(),
            planning_elements=elements("SENTINEL-1B", WINDOW[0] - timedelta(hours=70)),
            truth_elements=elements("SENTINEL-1B", WINDOW[0]),
            rates=InjectionRates(),
            random=Random(1),
        )
        with pytest.raises(ValueError, match="naive datetime"):
            executed_event(
                plan=plan(),
                acquisition=naive,
                outcome=outcome,
                executed_at=datetime(2026, 8, 11, 11, 30, tzinfo=UTC),
            )

    def test_the_payload_validates_against_the_committed_schema(self) -> None:
        # THE SCHEMA IS THE AUTHORITY, not this service's idea of the shape.
        # Both generated bindings silently drop parts of it, so a payload that
        # round-trips through them cleanly can still be invalid — which is
        # exactly how a swapped lat/lon survived review in M0.
        jsonschema = pytest.importorskip("jsonschema")
        referencing = pytest.importorskip("referencing")

        # Registered by $id, then validated through a $ref to it. The schemas
        # declare an absolute $id and refer to siblings by RELATIVE path, which
        # is the only combination both toolchains accept (contracts/README.md).
        # Handing the raw dict to a validator leaves the base URI empty and the
        # relative refs resolve against nothing.
        resources = []
        for path in sorted((REPO / "contracts").rglob("*.schema.json")):
            schema = json.loads(path.read_text())
            if "$id" in schema:
                resources.append((schema["$id"], referencing.Resource.from_contents(schema)))
        registry = referencing.Registry().with_resources(resources)

        envelope = {
            "event_id": event_id_for(acquisition().acquisition_id),
            "event_type": SUBJECT,
            "schema_version": SCHEMA_VERSION,
            "occurred_at": "2026-08-11T11:30:00Z",
            "producer": PRODUCER,
            "correlation_id": "22222222-2222-4222-8222-222222222222",
            "causation_id": None,
            "data": self._valid(Status.SUCCEEDED),
        }

        validator = jsonschema.Draft202012Validator(
            {"$ref": json.loads(SCHEMA.read_text())["$id"]},
            registry=registry,
            # format as an ASSERTION, not an annotation — the whole reason the
            # contracts carry `format: uuid` without a redundant `pattern`.
            format_checker=jsonschema.FormatChecker(),
        )
        validator.validate(envelope)

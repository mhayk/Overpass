"""The handler, against a real database and the real seeded constellation.

This is where the vertical slice closes. Everything either side of it existed
and was tested — `evaluate` since M1-10, the ledger and the outbox since M1-13 —
and nothing joined them, so a submitted request produced nothing at all (#131).

Real Postgres, real element sets, real SGP4. A fake constellation here would
test that this file and the handler agree about a dictionary; what is in doubt is
whether a request the tasking API actually publishes turns into opportunities the
planner can actually read.

Every request is built from a CONTRACT EXAMPLE, patched. The target is moved to
somewhere the frozen snapshot's satellites pass over, because a sweep that finds
nothing exercises the refusal path and says nothing about the other one.
"""

from __future__ import annotations

import json
import os
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import TYPE_CHECKING, Any

import psycopg
import pytest

from feasibility.failures import FAILURE_SUBJECT
from feasibility.handler import sweep_handler
from feasibility.messaging.idempotency import Delivery, NonRetryableError
from feasibility.opportunities import OPPORTUNITIES_SUBJECT

if TYPE_CHECKING:
    from collections.abc import Iterator

DSN = os.environ.get("OVERPASS_TEST_DSN")

pytestmark = pytest.mark.skipif(not DSN, reason="set OVERPASS_TEST_DSN to run the handler tests")

ROOT = Path(__file__).resolve().parents[3]
EXAMPLE = ROOT / "contracts" / "examples" / "valid" / "tasking.request.received.v1" / "minimal.json"

# Inside the frozen snapshot's usable life, so the constellation is not STALE.
WHEN = datetime(2026, 8, 7, 10, tzinfo=UTC)

# Svalbard. A high-latitude target, because a near-polar constellation passes
# over it many times a day — so a 24-hour horizon reliably contains access and
# the happy path is actually exercised.
SVALBARD = [15.6267, 78.2232]


def dsn() -> str:
    assert DSN is not None
    return DSN


@pytest.fixture
def connection() -> Iterator[psycopg.Connection[Any]]:
    """Rolled back. The handler writes opportunities and outbox rows."""
    with psycopg.connect(dsn()) as conn, conn.transaction() as transaction:
        yield conn
        transaction.force_rollback = True


def delivery_for(**data: Any) -> Delivery:
    envelope = json.loads(EXAMPLE.read_text())
    envelope["event_id"] = str(uuid.uuid4())
    envelope["data"]["request_id"] = str(uuid.uuid4())
    envelope["data"]["target"] = {"type": "Point", "coordinates": SVALBARD}
    envelope["data"]["window"] = {
        "start": WHEN.isoformat().replace("+00:00", "Z"),
        "end": (WHEN + timedelta(hours=24)).isoformat().replace("+00:00", "Z"),
    }
    envelope["data"]["requested_modes"] = ["STRIPMAP", "SPOTLIGHT"]
    envelope["data"].update(data)

    return Delivery(
        event_id=envelope["event_id"],
        subject="tasking.request.received.v1",
        payload=json.dumps(envelope).encode(),
        delivered_count=1,
        headers={"traceparent": "00-" + "e" * 32 + "-" + "f" * 16 + "-01"},
    )


def run(connection: psycopg.Connection[Any], delivery: Delivery) -> None:
    """Invoke the handler the way the worker does: with a cursor, in a transaction."""
    with connection.cursor() as cursor:
        sweep_handler(now=lambda: WHEN)(delivery)(cursor)


def outbox(connection: psycopg.Connection[Any], subject: str) -> list[dict[str, Any]]:
    with connection.cursor() as cursor:
        cursor.execute(
            "SELECT payload FROM feasibility.outbox WHERE subject = %s ORDER BY id", (subject,)
        )
        return [row[0] for row in cursor.fetchall()]


class TestASweepThatFindsSomething:
    def test_it_publishes_opportunities_through_the_outbox(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        delivery = delivery_for()
        run(connection, delivery)

        published = outbox(connection, OPPORTUNITIES_SUBJECT)
        assert len(published) == 1, "the sweep published nothing; this is the #131 defect"

        data = published[0]["data"]
        assert data["opportunity_count"] >= 1
        assert data["opportunity_count"] == len(data["opportunities"])
        assert published[0]["causation_id"] == delivery.event_id

    def test_the_satellite_id_joins_to_the_reference_constellation(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # The failure this would otherwise have: `evaluate` takes satellite_id
        # from `element_set.name`, and a Celestrak display name joins to nothing
        # and violates the contract's own pattern.
        run(connection, delivery_for())
        published = outbox(connection, OPPORTUNITIES_SUBJECT)[0]

        with connection.cursor() as cursor:
            cursor.execute("SELECT satellite_id FROM reference.satellites")
            known = {row[0] for row in cursor.fetchall()}

        for o in published["data"]["opportunities"]:
            assert o["satellite_id"] in known

    def test_the_candidates_are_persisted_for_inspection(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # feasibility.opportunities has existed since migration 00003 and had
        # never been written to. The event is how the planner learns; this is
        # how the sweep stays answerable after the stream's 72 hours are up.
        delivery = delivery_for()
        run(connection, delivery)
        request_id = json.loads(delivery.payload)["data"]["request_id"]

        with connection.cursor() as cursor:
            cursor.execute(
                "SELECT count(*) FROM feasibility.opportunities WHERE request_id = %s",
                (request_id,),
            )
            row = cursor.fetchone()

        assert row is not None
        assert row[0] == outbox(connection, OPPORTUNITIES_SUBJECT)[0]["data"]["opportunity_count"]

    def test_running_it_twice_writes_the_candidates_once(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # The ids are derived, so a replay recomputes the same opportunities.
        # ON CONFLICT DO NOTHING makes the second write a no-op rather than a
        # constraint violation that would roll the whole handler back.
        delivery = delivery_for()
        run(connection, delivery)
        request_id = json.loads(delivery.payload)["data"]["request_id"]

        with connection.cursor() as cursor:
            cursor.execute(
                "SELECT count(*) FROM feasibility.opportunities WHERE request_id = %s",
                (request_id,),
            )
            first = cursor.fetchone()

        with connection.cursor() as cursor:
            sweep_handler(now=lambda: WHEN)(delivery)(cursor)
            cursor.execute(
                "SELECT count(*) FROM feasibility.opportunities WHERE request_id = %s",
                (request_id,),
            )
            second = cursor.fetchone()

        assert first is not None
        assert second is not None
        assert first[0] == second[0]

    def test_provenance_names_every_satellite_considered(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        run(connection, delivery_for())
        data = outbox(connection, OPPORTUNITIES_SUBJECT)[0]["data"]

        assert data["satellites_evaluated"] >= 1
        assert len(data["tle_references"]) >= 1
        for reference in data["tle_references"]:
            assert reference["staleness"] in ("FRESH", "AGING", "STALE")


class TestASweepThatFindsNothing:
    def test_a_target_nothing_can_see_publishes_a_refusal_not_an_empty_success(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # A one-minute window. Access exists over a day and not over a minute,
        # so this is a correct negative answer rather than a broken sweep.
        delivery = delivery_for(
            window={
                "start": WHEN.isoformat().replace("+00:00", "Z"),
                "end": (WHEN + timedelta(minutes=1)).isoformat().replace("+00:00", "Z"),
            }
        )
        run(connection, delivery)

        assert outbox(connection, OPPORTUNITIES_SUBJECT) == []
        refusals = outbox(connection, FAILURE_SUBJECT)
        assert len(refusals) == 1
        assert refusals[0]["data"]["reason_code"] in (
            "NO_ACCESS_IN_HORIZON",
            "CONSTRAINTS_TOO_NARROW",
        )
        assert refusals[0]["data"]["retryable"] is False

    def test_a_refusal_carries_the_context_that_makes_it_auditable(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # "No access" with no horizon and no element sets is an assertion. With
        # them it is evidence.
        delivery = delivery_for(
            window={
                "start": WHEN.isoformat().replace("+00:00", "Z"),
                "end": (WHEN + timedelta(minutes=1)).isoformat().replace("+00:00", "Z"),
            }
        )
        run(connection, delivery)

        data = outbox(connection, FAILURE_SUBJECT)[0]["data"]
        assert "horizon" in data
        assert data["satellites_evaluated"] >= 1
        assert len(data["tle_references"]) >= 1

    def test_a_customers_own_constraints_reach_the_sweep(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        """A narrowing must actually narrow.

        Asserted as a COMPARISON rather than as an empty result. The first
        version of this test asked for incidence 44.9-45.0 and expected nothing;
        that band intersects the sensor's own 20-45 and is satisfiable, so the
        sweep was right and the test was wrong. Comparing against the
        unconstrained run tests the thing the acceptance criterion actually
        names — that the constraint is applied — without depending on a guess
        about which angles a real orbit produces.
        """
        run(connection, delivery_for())
        unconstrained = outbox(connection, OPPORTUNITIES_SUBJECT)[0]["data"]["opportunity_count"]

        with connection.cursor() as cursor:
            cursor.execute("DELETE FROM feasibility.outbox")

        run(
            connection,
            delivery_for(constraints={"min_incidence_deg": 30.0, "max_incidence_deg": 31.0}),
        )
        published = outbox(connection, OPPORTUNITIES_SUBJECT)
        constrained = published[0]["data"]["opportunity_count"] if published else 0

        assert constrained < unconstrained, (
            "a one-degree incidence band produced as many opportunities as no "
            "constraint at all; the customer's narrowing is not reaching evaluate"
        )
        for o in published[0]["data"]["opportunities"] if published else []:
            assert 30.0 <= o["geometry"]["incidence_angle_deg"] <= 31.0

    def test_excluding_every_satellite_leaves_nothing_to_evaluate(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        with connection.cursor() as cursor:
            cursor.execute("SELECT satellite_id FROM reference.satellites")
            everything = sorted(row[0] for row in cursor.fetchall())

        run(connection, delivery_for(constraints={"excluded_satellite_ids": everything}))

        assert outbox(connection, OPPORTUNITIES_SUBJECT) == []
        assert len(outbox(connection, FAILURE_SUBJECT)) == 1


class TestAPolygonTarget:
    def test_a_polygon_larger_than_any_swath_is_refused_rather_than_half_covered(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # The contract is explicit: a Polygon must be fully contained by a
        # SINGLE acquisition footprint, because mosaicking across passes is out
        # of scope. A five-degree box is far wider than the widest SCAN swath,
        # so covering its centre must not be reported as covering it.
        delivery = delivery_for(
            target={
                "type": "Polygon",
                "coordinates": [
                    [
                        [13.0, 76.0],
                        [18.0, 76.0],
                        [18.0, 80.0],
                        [13.0, 80.0],
                        [13.0, 76.0],
                    ]
                ],
            }
        )
        run(connection, delivery)

        assert outbox(connection, OPPORTUNITIES_SUBJECT) == []
        (refusal,) = outbox(connection, FAILURE_SUBJECT)
        assert refusal["data"]["reason_code"] in (
            "CONSTRAINTS_TOO_NARROW",
            "NO_ACCESS_IN_HORIZON",
        )


class TestWhatItRefusesBeforeSweeping:
    def test_an_unusable_request_is_terminal_rather_than_retried(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # Raised out of the handler so `handle_one` publishes the refusal and
        # acks. Retrying a request whose geometry this system does not support
        # would burn the redelivery budget five times over.
        delivery = delivery_for(
            target={"type": "LineString", "coordinates": [[4.0, 51.0], [5.0, 52.0]]}
        )
        with pytest.raises(NonRetryableError) as caught, connection.cursor() as cursor:
            sweep_handler(now=lambda: WHEN)(delivery)(cursor)

        assert caught.value.reason_code == "UNSUPPORTED_TARGET_GEOMETRY"

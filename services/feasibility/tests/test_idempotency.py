"""Effectively-once processing, against a real Postgres.

Skipped unless `OVERPASS_TEST_DSN` points at a migrated database, because the
guarantee under test is a property of a transaction and a unique constraint. A
version of this with a fake connection would test the mock.

    OVERPASS_TEST_DSN=postgres://overpass:overpass@localhost:5433/overpass uv run pytest

The named acceptance test is `test_five_deliveries_produce_exactly_one_result`.
Everything else establishes the properties it depends on.
"""

from __future__ import annotations

import os
import uuid
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any

import psycopg
import pytest

from feasibility.messaging import (
    Delivery,
    NonRetryableError,
    OutboxMessage,
    Outcome,
    already_processed,
    claim_unpublished,
    enqueue,
    mark_published,
    process_once,
    record_failure,
)

if TYPE_CHECKING:
    from collections.abc import Iterator

DSN = os.environ.get("OVERPASS_TEST_DSN")

pytestmark = pytest.mark.skipif(
    DSN is None, reason="set OVERPASS_TEST_DSN to run the database integration tests"
)

CONSUMER = "feasibility-worker"


@pytest.fixture
def connection() -> Iterator[psycopg.Connection[Any]]:
    assert DSN is not None
    with psycopg.connect(DSN, autocommit=True) as conn:
        yield conn
        # Leave nothing behind. These tests write to the real tables on purpose
        # — that is the point — so they have to clean up after themselves.
        with conn.cursor() as cur:
            cur.execute("DELETE FROM feasibility.processed_events WHERE consumer LIKE 'test-%%'")
            cur.execute("DELETE FROM feasibility.outbox WHERE event_type LIKE 'test.%%'")
            cur.execute("DROP TABLE IF EXISTS feasibility._test_results")


@pytest.fixture
def results_table(connection: psycopg.Connection[Any]) -> str:
    """A stand-in for whatever the handler actually writes.

    The guarantee is about the transaction, not about opportunities
    specifically, so the test uses a table it controls. Using the real
    opportunities table would drag in foreign keys and satellite fixtures
    without testing anything more.
    """
    with connection.cursor() as cur:
        cur.execute(
            "CREATE TABLE IF NOT EXISTS feasibility._test_results "
            "(id bigserial PRIMARY KEY, event_id uuid NOT NULL, note text)"
        )
    return "feasibility._test_results"


def a_delivery(event_id: str | None = None, delivered: int = 1) -> Delivery:
    return Delivery(
        event_id=event_id or str(uuid.uuid4()),
        subject="tasking.request.received.v1",
        payload=b"{}",
        delivered_count=delivered,
        headers={"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
    )


def pending(
    connection: psycopg.Connection[Any],
) -> list[tuple[int, str, str, bytes, dict[str, str]]]:
    """claim_unpublished inside a transaction, which is the only legal way.

    The function refuses an autocommit connection on purpose: row locks live
    only as long as the transaction that took them, so an autocommit claim
    hands out rows it has not actually reserved.
    """
    with connection.transaction():
        return claim_unpublished(connection)


class TestEffectivelyOnce:
    def test_five_deliveries_produce_exactly_one_result(
        self, connection: psycopg.Connection[Any], results_table: str
    ) -> None:
        """The acceptance test. JetStream is at-least-once; this is what we add.

        Recomputing a sweep on redelivery is merely expensive. REPUBLISHING its
        opportunities is worse — the planner would allocate phantom candidates
        that no other part of the system believes in.
        """
        delivery = a_delivery()
        consumer = "test-five"
        outcomes = []

        for attempt in range(5):

            def handler(cursor: psycopg.Cursor[Any], attempt: int = attempt) -> None:
                cursor.execute(
                    f"INSERT INTO {results_table} (event_id, note) VALUES (%s, %s)",
                    (delivery.event_id, f"attempt {attempt}"),
                )

            outcomes.append(process_once(connection, consumer, delivery, handler))

        assert outcomes[0] is Outcome.PROCESSED
        assert all(o is Outcome.DUPLICATE for o in outcomes[1:])

        with connection.cursor() as cur:
            cur.execute(
                f"SELECT count(*), min(note) FROM {results_table} WHERE event_id = %s",
                (delivery.event_id,),
            )
            count, note = cur.fetchone()  # type: ignore[misc]
        assert count == 1, f"five deliveries wrote {count} rows"
        assert note == "attempt 0", "the surviving row must be from the first delivery"

    def test_the_dedup_row_and_the_result_commit_together(
        self, connection: psycopg.Connection[Any], results_table: str
    ) -> None:
        """The reason it is one transaction and not two.

        A handler that fails must leave NOTHING behind — not the result, and
        crucially not the processed_events row. A dedup row without its result
        would mark the work permanently done without it having happened, and
        nothing downstream could ever tell.
        """
        delivery = a_delivery()
        consumer = "test-atomic"

        def exploding_handler(cursor: psycopg.Cursor[Any]) -> None:
            cursor.execute(
                f"INSERT INTO {results_table} (event_id, note) VALUES (%s, 'doomed')",
                (delivery.event_id,),
            )
            msg = "handler blew up after writing"
            raise RuntimeError(msg)

        with pytest.raises(RuntimeError, match="blew up"):
            process_once(connection, consumer, delivery, exploding_handler)

        assert not already_processed(connection, consumer, delivery.event_id)
        with connection.cursor() as cur:
            cur.execute(
                f"SELECT count(*) FROM {results_table} WHERE event_id = %s",
                (delivery.event_id,),
            )
            assert cur.fetchone()[0] == 0  # type: ignore[index]

    def test_a_failed_delivery_can_be_retried_successfully(
        self, connection: psycopg.Connection[Any], results_table: str
    ) -> None:
        # The corollary of the rollback above: because nothing was recorded, the
        # redelivery is a first attempt and must be allowed to succeed.
        delivery = a_delivery()
        consumer = "test-retry"

        def failing(cursor: psycopg.Cursor[Any]) -> None:
            msg = "transient"
            raise RuntimeError(msg)

        def succeeding(cursor: psycopg.Cursor[Any]) -> None:
            cursor.execute(
                f"INSERT INTO {results_table} (event_id, note) VALUES (%s, 'ok')",
                (delivery.event_id,),
            )

        with pytest.raises(RuntimeError):
            process_once(connection, consumer, delivery, failing)
        assert process_once(connection, consumer, delivery, succeeding) is Outcome.PROCESSED
        assert already_processed(connection, consumer, delivery.event_id)

    def test_different_consumers_do_not_share_a_dedup_record(
        self, connection: psycopg.Connection[Any], results_table: str
    ) -> None:
        # The key is (consumer, event_id), not event_id alone. One service can
        # run several durable consumers, and a redelivery to one must not look
        # already-processed to another.
        delivery = a_delivery()

        def handler(cursor: psycopg.Cursor[Any]) -> None:
            cursor.execute(
                f"INSERT INTO {results_table} (event_id, note) VALUES (%s, 'x')",
                (delivery.event_id,),
            )

        assert process_once(connection, "test-a", delivery, handler) is Outcome.PROCESSED
        assert process_once(connection, "test-b", delivery, handler) is Outcome.PROCESSED

    def test_a_non_retryable_failure_propagates_rather_than_being_swallowed(
        self, connection: psycopg.Connection[Any], results_table: str
    ) -> None:
        # NO_ACCESS_IN_HORIZON is a correct negative answer. The caller has to
        # see it, to publish the refusal and ack — treating it as a generic
        # error would retry a physical impossibility until max_deliver.
        delivery = a_delivery()

        def refusing(cursor: psycopg.Cursor[Any]) -> None:
            raise NonRetryableError("NO_ACCESS_IN_HORIZON", "nothing sees the target")

        with pytest.raises(NonRetryableError) as caught:
            process_once(connection, "test-terminal", delivery, refusing)
        assert caught.value.reason_code == "NO_ACCESS_IN_HORIZON"
        # And it rolled back, so a deliberate replay is still possible.
        assert not already_processed(connection, "test-terminal", delivery.event_id)


class TestOutbox:
    def test_the_event_and_the_result_commit_together(
        self, connection: psycopg.Connection[Any], results_table: str
    ) -> None:
        """The dual-write problem, closed.

        The event exists if and only if the result does. This is the property
        that makes it impossible to publish an opportunity set that was rolled
        back.
        """
        delivery = a_delivery()
        event_id = str(uuid.uuid4())

        def handler(cursor: psycopg.Cursor[Any]) -> None:
            cursor.execute(
                f"INSERT INTO {results_table} (event_id, note) VALUES (%s, 'r')",
                (delivery.event_id,),
            )
            enqueue(
                cursor,
                OutboxMessage(
                    event_id=event_id,
                    event_type="test.opportunities.v1",
                    schema_version="1.0.0",
                    subject="feasibility.opportunities.computed.v1",
                    payload={"request_id": delivery.event_id},
                    occurred_at=datetime.now(UTC),
                    headers=delivery.headers,
                ),
            )

        assert process_once(connection, "test-outbox", delivery, handler) is Outcome.PROCESSED

        assert any(row[1] == event_id for row in pending(connection))

    def test_a_rolled_back_handler_leaves_no_event_behind(
        self, connection: psycopg.Connection[Any], results_table: str
    ) -> None:
        delivery = a_delivery()
        event_id = str(uuid.uuid4())

        def handler(cursor: psycopg.Cursor[Any]) -> None:
            enqueue(
                cursor,
                OutboxMessage(
                    event_id=event_id,
                    event_type="test.doomed.v1",
                    schema_version="1.0.0",
                    subject="feasibility.opportunities.computed.v1",
                    payload={},
                    occurred_at=datetime.now(UTC),
                    headers={},
                ),
            )
            msg = "after enqueue"
            raise RuntimeError(msg)

        with pytest.raises(RuntimeError):
            process_once(connection, "test-outbox-rollback", delivery, handler)

        assert all(row[1] != event_id for row in pending(connection))

    def test_the_traceparent_survives_the_hop(self, connection: psycopg.Connection[Any]) -> None:
        # Dropping it is how one distributed trace silently becomes two, and the
        # async boundary is exactly where that happens.
        event_id = str(uuid.uuid4())
        traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
        with connection.cursor() as cur:
            enqueue(
                cur,
                OutboxMessage(
                    event_id=event_id,
                    event_type="test.trace.v1",
                    schema_version="1.0.0",
                    subject="feasibility.opportunities.computed.v1",
                    payload={},
                    occurred_at=datetime.now(UTC),
                    headers={"traceparent": traceparent},
                ),
            )
        row = next(r for r in pending(connection) if r[1] == event_id)
        assert row[4]["traceparent"] == traceparent

    def test_publishing_marks_the_row_and_stops_claiming_it(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        event_id = str(uuid.uuid4())
        with connection.cursor() as cur:
            enqueue(
                cur,
                OutboxMessage(
                    event_id=event_id,
                    event_type="test.published.v1",
                    schema_version="1.0.0",
                    subject="feasibility.opportunities.computed.v1",
                    payload={},
                    occurred_at=datetime.now(UTC),
                    headers={},
                ),
            )
        row = next(r for r in pending(connection) if r[1] == event_id)
        with connection.cursor() as cur:
            mark_published(cur, row[0])
        assert all(r[1] != event_id for r in pending(connection))

    def test_a_failed_publish_keeps_the_row_and_counts_the_attempt(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # The relay must try again. A row dropped on the first network blip is
        # an event that never happened as far as everyone downstream is
        # concerned.
        event_id = str(uuid.uuid4())
        with connection.cursor() as cur:
            enqueue(
                cur,
                OutboxMessage(
                    event_id=event_id,
                    event_type="test.retry.v1",
                    schema_version="1.0.0",
                    subject="feasibility.opportunities.computed.v1",
                    payload={},
                    occurred_at=datetime.now(UTC),
                    headers={},
                ),
            )
        row = next(r for r in pending(connection) if r[1] == event_id)
        with connection.cursor() as cur:
            record_failure(cur, row[0], "connection refused")
            cur.execute(
                "SELECT attempts, last_error FROM feasibility.outbox WHERE id = %s", (row[0],)
            )
            attempts, last_error = cur.fetchone()  # type: ignore[misc]

        assert attempts == 1
        assert "refused" in last_error
        assert any(r[1] == event_id for r in pending(connection))

    def test_the_same_event_id_cannot_be_enqueued_twice(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # The unique constraint on event_id is the last line of defence against
        # a double publish if the dedup above were ever bypassed.
        event_id = str(uuid.uuid4())
        message = OutboxMessage(
            event_id=event_id,
            event_type="test.dup.v1",
            schema_version="1.0.0",
            subject="feasibility.opportunities.computed.v1",
            payload={},
            occurred_at=datetime.now(UTC),
            headers={},
        )
        with connection.cursor() as cur:
            enqueue(cur, message)
        with pytest.raises(psycopg.errors.UniqueViolation), connection.cursor() as cur:
            enqueue(cur, message)

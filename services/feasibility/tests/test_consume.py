"""The #48 hardening: poison decision, metrics, cleanup guard.

The decision and metrics tests are pure. The cleanup tests run against a real
Postgres — the guard protects a DELETE, and a version with a fake connection
would test the mock — so they skip unless `OVERPASS_TEST_DSN` is set, same as
`test_idempotency.py`.

Mirrors `lib/go/consume` case for case, so the Go and Python halves of the
design are held to the same bar.
"""

from __future__ import annotations

import os
import uuid
from datetime import timedelta
from typing import TYPE_CHECKING, Any

import psycopg
import pytest

from feasibility.messaging import consume
from feasibility.messaging.consume import Decision, Metrics, cleanup, on_failure
from feasibility.messaging.idempotency import Outcome

if TYPE_CHECKING:
    from collections.abc import Iterator

DSN = os.environ.get("OVERPASS_TEST_DSN")

requires_database = pytest.mark.skipif(
    DSN is None, reason="set OVERPASS_TEST_DSN to run the database integration tests"
)


# --- the poison decision -----------------------------------------------------


def test_a_permanent_failure_terminates_on_the_first_delivery() -> None:
    assert on_failure(permanent=True, delivered=1, max_deliver=5) is Decision.TERMINATE


def test_a_transient_failure_retries_until_the_last_delivery() -> None:
    for delivered in range(1, 5):
        assert on_failure(permanent=False, delivered=delivered, max_deliver=5) is Decision.RETRY


def test_a_transient_failure_terminates_deliberately_on_the_last_delivery() -> None:
    # The whole point of the decision: the broker's counter lapsing is a silent
    # drop, so the LAST attempt terms on purpose, with a log line and a metric.
    assert on_failure(permanent=False, delivered=5, max_deliver=5) is Decision.TERMINATE
    assert on_failure(permanent=False, delivered=6, max_deliver=5) is Decision.TERMINATE


def test_an_unbounded_consumer_never_invents_a_bound() -> None:
    for max_deliver in (0, -1):
        assert (
            on_failure(permanent=False, delivered=10_000, max_deliver=max_deliver) is Decision.RETRY
        )


# --- metrics -----------------------------------------------------------------


def test_metrics_count_each_outcome_where_an_operator_expects_it() -> None:
    m = Metrics()
    m.record(Outcome.PROCESSED, 1, 0.010)
    m.record(Outcome.DUPLICATE, 2, 0.020)
    m.record(Outcome.FAILED_TERMINAL, 1, 0.030)
    m.record(Outcome.FAILED_RETRYABLE, 3, 0.500)

    # A published refusal is completed, acked work — it counts as processed.
    assert m.processed == 2
    assert m.duplicates == 1
    # Every delivery the broker had to send again, whatever became of it.
    assert m.redeliveries == 2
    # Retries carry no ack latency: only the three acked deliveries count.
    assert m.ack_count == 3
    assert m.ack_latency_mean_s == pytest.approx(0.020)
    assert m.ack_latency_max_s == pytest.approx(0.030)


def test_metrics_mean_is_zero_before_any_ack_rather_than_a_crash() -> None:
    assert Metrics().ack_latency_mean_s == 0.0


# --- cleanup -----------------------------------------------------------------


@pytest.fixture
def connection() -> Iterator[psycopg.Connection[Any]]:
    assert DSN is not None
    with psycopg.connect(DSN, autocommit=True) as conn:
        yield conn
        with conn.cursor() as cur:
            cur.execute("DELETE FROM feasibility.processed_events WHERE consumer LIKE 'test-%%'")


def _insert(conn: psycopg.Connection[Any], consumer: str, age: timedelta) -> str:
    event_id = str(uuid.uuid4())
    with conn.cursor() as cur:
        cur.execute(
            "INSERT INTO feasibility.processed_events (consumer, event_id, processed_at) "
            "VALUES (%s, %s, now() - %s)",
            (consumer, event_id, age),
        )
    return event_id


def _held(conn: psycopg.Connection[Any], consumer: str) -> set[str]:
    with conn.cursor() as cur:
        cur.execute(
            "SELECT event_id::text FROM feasibility.processed_events WHERE consumer = %s",
            (consumer,),
        )
        return {row[0] for row in cur.fetchall()}


@requires_database
def test_cleanup_refuses_retention_inside_the_redelivery_horizon(
    connection: psycopg.Connection[Any],
) -> None:
    """A ledger row younger than the longest possible redelivery is still
    doing its job; deleting it reprocesses that redelivery as new."""
    inside = _insert(connection, "test-cleanup-guard", timedelta(hours=2))

    with pytest.raises(ValueError, match="redelivery horizon"):
        # horizon = 5 x 120s x 2 = 20 minutes; one minute is inside it.
        cleanup(
            connection,
            retention=timedelta(minutes=1),
            max_deliver=5,
            ack_wait=timedelta(seconds=120),
        )

    # The guard refused BEFORE touching the table.
    assert _held(connection, "test-cleanup-guard") == {inside}


@requires_database
def test_cleanup_deletes_old_rows_and_keeps_fresh_ones(
    connection: psycopg.Connection[Any],
) -> None:
    old = _insert(connection, "test-cleanup", timedelta(days=8))
    fresh = _insert(connection, "test-cleanup", timedelta(hours=1))

    deleted = cleanup(
        connection,
        retention=timedelta(days=7),
        max_deliver=5,
        ack_wait=timedelta(seconds=120),
    )

    assert deleted >= 1
    held = _held(connection, "test-cleanup")
    assert fresh in held
    assert old not in held


def test_cleanup_guard_math_scales_with_the_consumer_config() -> None:
    """The horizon is max_deliver x ack_wait x 2 — not a constant. A consumer
    with a longer ack_wait must be refused a retention the default would
    accept, or the guard is a hardcoded number wearing a formula's name."""
    with pytest.raises(ValueError, match="redelivery horizon"):
        consume.cleanup(
            None,  # type: ignore[arg-type]  # the guard must fire before any I/O
            retention=timedelta(hours=5),
            max_deliver=10,
            ack_wait=timedelta(minutes=30),  # horizon = 10 h
        )

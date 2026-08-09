"""The #48 hardening: poison decision, metrics, cleanup guard — and the #49
dead-lettering that turns a Term from a drop into a handoff.

The decision, metrics and dead-letter tests are pure. The cleanup tests run
against a real Postgres — the guard protects a DELETE, and a version with a
fake connection would test the mock — so they skip unless `OVERPASS_TEST_DSN`
is set, same as `test_idempotency.py`.

Mirrors `lib/go/consume` case for case, so the Go and Python halves of the
design are held to the same bar.
"""

from __future__ import annotations

import os
import uuid
from dataclasses import replace
from datetime import UTC, datetime, timedelta
from typing import TYPE_CHECKING, Any, cast

import psycopg
import pytest
from nats.js import JetStreamContext

from feasibility.messaging import consume
from feasibility.messaging.consume import (
    DeadLetter,
    Decision,
    Metrics,
    cleanup,
    on_failure,
    publish_dead_letter,
)
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


def test_metrics_count_dead_letters() -> None:
    """`terminated` minus `deadlettered` is how many messages this worker
    dropped without keeping a copy — which is why they are two counters."""
    m = Metrics()
    m.deadlettered += 2

    assert m.deadlettered == 2
    assert m.terminated == 0


# --- dead letters ------------------------------------------------------------


class Recorder:
    """The publisher the worker's JetStream context will be.

    Records rather than asserts, so each test below reads what actually went on
    the wire — and it is a fake of a two-line interface, not a mock of NATS.
    """

    def __init__(self, error: Exception | None = None) -> None:
        self.calls = 0
        self.subject = ""
        self.payload = b""
        self.headers: dict[str, str] = {}
        self.error = error

    async def publish(
        self,
        subject: str,
        payload: bytes = b"",
        *,
        headers: dict[str, str] | None = None,
    ) -> object:
        self.calls += 1
        self.subject, self.payload, self.headers = subject, payload, dict(headers or {})
        if self.error is not None:
            raise self.error
        return None


def sample() -> DeadLetter:
    return DeadLetter(
        subject="tasking.request.received.v1",
        payload=b'{"event_id":"9b2f0c0e-7f6f-4f6c-8f0c-2b3a4d5e6f70"}',
        reason=consume.REASON_DECODE,
        consumer="feasibility-worker",
        delivered=5,
        event_id="9b2f0c0e-7f6f-4f6c-8f0c-2b3a4d5e6f70",
        traceparent="00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
        failed_at=datetime(2026, 8, 9, 14, 30, tzinfo=UTC),
    )


@pytest.mark.asyncio
async def test_a_dead_letter_goes_to_the_prefixed_subject() -> None:
    """A prefix, not an infix: `tasking.dlq.>` is already inside the TASKING
    stream's wildcard and NATS refuses overlapping streams."""
    publisher = Recorder()

    await publish_dead_letter(publisher, sample())

    assert publisher.subject == "dlq.tasking.request.received.v1"


@pytest.mark.asyncio
async def test_a_dead_letter_carries_the_contract_header_set() -> None:
    """The header set is the contract. The inspect tooling reads exactly these
    names, so a typo here is a tool that silently shows nothing."""
    publisher = Recorder()

    await publish_dead_letter(publisher, sample())

    assert publisher.headers == {
        "Overpass-Dlq-Reason": "decode",
        "Overpass-Dlq-Original-Subject": "tasking.request.received.v1",
        "Overpass-Dlq-Delivery-Count": "5",
        "Overpass-Dlq-Failed-At": "2026-08-09T14:30:00Z",
        "Overpass-Dlq-Consumer": "feasibility-worker",
        "traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
        "Nats-Msg-Id": "9b2f0c0e-7f6f-4f6c-8f0c-2b3a4d5e6f70",
    }
    assert publisher.payload == sample().payload


@pytest.mark.asyncio
async def test_a_dead_letter_without_an_event_id_still_lands() -> None:
    """An unparseable envelope has no id, and that is precisely the message
    that dies here. Refusing it would leave the caller naking forever under
    ADR-0017's ordering — the loss this mechanism exists to prevent."""
    publisher = Recorder()

    await publish_dead_letter(publisher, replace(sample(), event_id=""))

    assert publisher.calls == 1
    assert "Nats-Msg-Id" not in publisher.headers


@pytest.mark.asyncio
async def test_an_absent_traceparent_is_omitted_rather_than_blank() -> None:
    """A blank traceparent parses downstream as a broken trace context, where
    an absent one parses as "no trace"."""
    publisher = Recorder()

    await publish_dead_letter(publisher, replace(sample(), traceparent=""))

    assert "traceparent" not in publisher.headers


@pytest.mark.asyncio
async def test_an_unset_failed_at_is_stamped_from_the_clock() -> None:
    """The stamp is the time of the terminal DECISION (ADR-0017's amendment),
    and a caller that supplies none gets the clock, not year zero."""
    publisher = Recorder()
    before = datetime.now(UTC) - timedelta(seconds=1)

    await publish_dead_letter(publisher, replace(sample(), failed_at=None))

    stamped = datetime.fromisoformat(publisher.headers["Overpass-Dlq-Failed-At"])
    assert before <= stamped <= datetime.now(UTC) + timedelta(seconds=1)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("letter", "message"),
    [
        (replace(sample(), subject=""), "subject"),
        (replace(sample(), reason=""), "reason"),
        (replace(sample(), consumer=""), "consumer"),
        # dlq.dlq.tasking.> is a stream nobody declared. A consumer bound to a
        # DLQ stream that dead-letters again is a wiring mistake, and it must be
        # loud rather than invent a subject space by accident.
        (replace(sample(), subject="dlq.tasking.request.received.v1"), "already"),
    ],
)
async def test_dead_lettering_refuses_an_incomplete_call_site(
    letter: DeadLetter, message: str
) -> None:
    """These are call-site facts — literals and a consumer name known at wiring
    time, never anything a bad message can influence. An empty one is a
    programming error, not traffic."""
    publisher = Recorder()

    with pytest.raises(ValueError, match=message):
        await publish_dead_letter(publisher, letter)

    assert publisher.calls == 0


@pytest.mark.asyncio
async def test_a_failed_publish_is_reported_so_the_caller_can_nak() -> None:
    """The caller chooses term or nak on this error. Swallowing it would term
    without a landed dead letter, which is the silent loss again."""
    publisher = Recorder(error=TimeoutError("no responders available for request"))

    with pytest.raises(TimeoutError):
        await publish_dead_letter(publisher, sample())


def test_the_real_jetstream_context_satisfies_the_publisher_protocol() -> None:
    """A protocol nothing real implements is a protocol that type-checks and
    then fails on the first live publish.

    The assertion here is the mypy one: passing a `JetStreamContext` where a
    `DlqPublisher` is expected fails the type check the day nats-py changes
    that signature — which is the day we would otherwise find out in an
    incident, dead-lettering a message we had already decided to lose.
    """
    accepts_publisher(cast("JetStreamContext", object()))


def accepts_publisher(publisher: consume.DlqPublisher) -> None:
    """Exists only to give the line above something to type-check against."""


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

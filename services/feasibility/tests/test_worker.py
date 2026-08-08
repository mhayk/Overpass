"""The consumer loop, against a real NATS and a real Postgres.

Skipped unless both `OVERPASS_TEST_DSN` and `OVERPASS_TEST_NATS` are set, which
they are in the `stack` workflow. There is no useful version of these tests with
a mocked broker: what is under test is that the durable consumer redelivers, the
dedup absorbs it, and the ack lands afterwards — all three of which are
properties of the real thing.

The worker runs with `max_batches` so the loop terminates on its own rather than
being cancelled from outside. A loop that can only be stopped by cancelling it is
a loop whose shutdown path is never exercised.
"""

from __future__ import annotations

import asyncio
import json
import os
import uuid
from typing import TYPE_CHECKING, Any

import nats
import psycopg
import pytest

from feasibility.messaging import CONSUMER, Delivery, WorkerConfig, run
from feasibility.messaging.idempotency import NonRetryableError

if TYPE_CHECKING:
    from collections.abc import Callable, Iterator

DSN = os.environ.get("OVERPASS_TEST_DSN")
NATS_URL = os.environ.get("OVERPASS_TEST_NATS")

pytestmark = [
    pytest.mark.skipif(
        not (DSN and NATS_URL),
        reason="set OVERPASS_TEST_DSN and OVERPASS_TEST_NATS to run the broker tests",
    ),
    pytest.mark.asyncio,
]

SUBJECT = "tasking.request.received.v1"


@pytest.fixture
def connection() -> Iterator[psycopg.Connection[Any]]:
    assert DSN is not None
    with psycopg.connect(DSN, autocommit=True) as conn:
        with conn.cursor() as cur:
            cur.execute(
                "CREATE TABLE IF NOT EXISTS feasibility._worker_results "
                "(id bigserial PRIMARY KEY, event_id uuid NOT NULL)"
            )
        yield conn
        with conn.cursor() as cur:
            cur.execute("DROP TABLE IF EXISTS feasibility._worker_results")
            cur.execute(
                "DELETE FROM feasibility.processed_events WHERE consumer LIKE %s",
                (f"{CONSUMER}%",),
            )
            cur.execute("DELETE FROM feasibility.outbox WHERE event_type = 'feasibility.failed.v1'")


def envelope(event_id: str, request_id: str) -> bytes:
    return json.dumps(
        {
            "event_id": event_id,
            "event_type": SUBJECT,
            "schema_version": "1.0.0",
            "occurred_at": "2026-08-07T00:00:00Z",
            "producer": "tasking-api",
            "data": {"request_id": request_id},
        }
    ).encode()


def _nats_url() -> str:
    assert NATS_URL is not None
    return NATS_URL


async def publish(payload: bytes, *, count: int = 1) -> None:
    """Publish the same bytes `count` times.

    Deliberately without a Nats-Msg-Id header. JetStream would deduplicate on
    that inside its dedup window, which would make the test pass without our
    dedup doing anything — the broker would be under test rather than us.
    """
    client = await nats.connect(servers=[_nats_url()])
    try:
        js = client.jetstream()
        for _ in range(count):
            await js.publish(SUBJECT, payload)
    finally:
        await client.drain()


def counting_handler(delivery: Delivery) -> Callable[[psycopg.Cursor[Any]], None]:
    def handler(cursor: psycopg.Cursor[Any]) -> None:
        cursor.execute(
            "INSERT INTO feasibility._worker_results (event_id) VALUES (%s)",
            (delivery.event_id,),
        )

    return handler


def config() -> WorkerConfig:
    assert DSN is not None and NATS_URL is not None
    return WorkerConfig(nats_url=NATS_URL, dsn=DSN, batch=16, fetch_timeout_s=2.0)


async def test_the_same_event_published_five_times_is_processed_once(
    connection: psycopg.Connection[Any],
) -> None:
    """End to end, through the real durable consumer.

    The database-level version of this is in test_idempotency.py. This one adds
    the broker: five real deliveries off a real pull consumer, settled by real
    acks.
    """
    event_id = str(uuid.uuid4())
    await publish(envelope(event_id, str(uuid.uuid4())), count=5)

    settled = await run(config(), counting_handler, max_batches=2)
    assert settled >= 5, f"only {settled} messages settled"

    with connection.cursor() as cur:
        cur.execute(
            "SELECT count(*) FROM feasibility._worker_results WHERE event_id = %s", (event_id,)
        )
        assert cur.fetchone()[0] == 1  # type: ignore[index]


async def test_everything_is_acked_so_nothing_redelivers(
    connection: psycopg.Connection[Any],
) -> None:
    """Acks actually landed, not merely were called.

    A duplicate that is processed correctly but never acked redelivers forever
    and eventually poisons the consumer. Asking the server how many messages are
    still pending is the only way to know the ack reached it.
    """
    event_id = str(uuid.uuid4())
    await publish(envelope(event_id, str(uuid.uuid4())), count=3)
    await run(config(), counting_handler, max_batches=2)

    client = await nats.connect(servers=[_nats_url()])
    try:
        info = await client.jetstream().consumer_info("TASKING", CONSUMER)
        assert info.num_pending == 0, f"{info.num_pending} messages never acked"
        assert info.num_ack_pending == 0, f"{info.num_ack_pending} acks outstanding"
    finally:
        await client.drain()


async def test_a_terminal_refusal_is_published_and_acked(
    connection: psycopg.Connection[Any],
) -> None:
    """A correct negative answer ends the message rather than retrying it.

    NO_ACCESS_IN_HORIZON will not become true on the fifth attempt. The refusal
    goes out through the outbox and the message is acked, so the redelivery
    budget stays available for failures that might actually succeed.
    """
    event_id = str(uuid.uuid4())
    request_id = str(uuid.uuid4())
    await publish(envelope(event_id, request_id), count=1)

    def refusing(delivery: Delivery) -> Callable[[psycopg.Cursor[Any]], None]:
        def handler(cursor: psycopg.Cursor[Any]) -> None:
            raise NonRetryableError("NO_ACCESS_IN_HORIZON", "nothing sees the target")

        return handler

    await run(config(), refusing, max_batches=2)

    # Read through `data`, because that is where the contract puts these.
    #
    # This assertion used to read them from the TOP LEVEL, and passed, because
    # the producer put them there — six bare fields with no envelope, invalid
    # against feasibility.failed.v1 and undecodable by every consumer. The test
    # and the code were wrong together, which is the failure mode #124 named
    # and #132 fixed. It is written this way now so that regressing the
    # producer breaks it.
    with connection.cursor() as cur:
        cur.execute(
            """
            SELECT payload->'data'->>'reason_code',
                   payload->'data'->>'retryable',
                   payload->'data'->>'request_id',
                   payload->>'event_type',
                   payload->>'causation_id'
            FROM feasibility.outbox
            WHERE event_type = 'feasibility.failed.v1'
            ORDER BY id DESC LIMIT 1
            """
        )
        row = cur.fetchone()
    assert row is not None, "no refusal was published"
    assert row[0] == "NO_ACCESS_IN_HORIZON"
    assert row[1] == "false"
    assert row[2] == request_id
    assert row[3] == "feasibility.failed.v1"
    # The refusal names the request that caused it. A null here would say the
    # failure came from nowhere.
    assert row[4] == event_id

    client = await nats.connect(servers=[_nats_url()])
    try:
        info = await client.jetstream().consumer_info("TASKING", CONSUMER)
        assert info.num_pending == 0, "a terminal refusal must not leave the message pending"
    finally:
        await client.drain()


async def test_a_transient_failure_is_redelivered_then_succeeds(
    connection: psycopg.Connection[Any],
) -> None:
    """The other half of the retryable distinction.

    A nak must bring the message back. If it did not, a database blip would
    silently drop a customer's request.
    """
    event_id = str(uuid.uuid4())
    await publish(envelope(event_id, str(uuid.uuid4())), count=1)

    attempts = {"n": 0}

    def flaky(delivery: Delivery) -> Callable[[psycopg.Cursor[Any]], None]:
        def handler(cursor: psycopg.Cursor[Any]) -> None:
            attempts["n"] += 1
            if attempts["n"] == 1:
                msg = "transient"
                raise RuntimeError(msg)
            cursor.execute(
                "INSERT INTO feasibility._worker_results (event_id) VALUES (%s)",
                (delivery.event_id,),
            )

        return handler

    await run(config(), flaky, max_batches=1)
    with connection.cursor() as cur:
        cur.execute(
            "SELECT count(*) FROM feasibility._worker_results WHERE event_id = %s", (event_id,)
        )
        assert cur.fetchone()[0] == 0, "the failed attempt must not have written anything"  # type: ignore[index]

    # The nak carries a delay, so give the broker time to make it available.
    await asyncio.sleep(6)
    await run(config(), flaky, max_batches=2)

    assert attempts["n"] >= 2, "the message was never redelivered"
    with connection.cursor() as cur:
        cur.execute(
            "SELECT count(*) FROM feasibility._worker_results WHERE event_id = %s", (event_id,)
        )
        assert cur.fetchone()[0] == 1  # type: ignore[index]


async def test_an_unparseable_envelope_is_terminated_not_retried(
    connection: psycopg.Connection[Any],
) -> None:
    # No event_id means nothing to deduplicate on. Redelivering it five times
    # cannot help, so it is terminated rather than left to burn the budget.
    await publish(b'{"not":"an envelope"}', count=1)
    settled = await run(config(), counting_handler, max_batches=2)
    assert settled >= 1

    client = await nats.connect(servers=[_nats_url()])
    try:
        info = await client.jetstream().consumer_info("TASKING", CONSUMER)
        assert info.num_pending == 0
    finally:
        await client.drain()

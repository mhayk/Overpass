"""The outbox relay, against a real NATS and a real Postgres.

#93 wrote events into the outbox and nothing ever took them out. These tests
exist to make sure that stays fixed, and to pin the two properties the relay is
actually for: an event reaches the stream, and it reaches it once.
"""

from __future__ import annotations

import os
import uuid
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any

import nats
import psycopg
import pytest

from feasibility.messaging import (
    OutboxMessage,
    RelayConfig,
    claim_unpublished,
    drain_once,
    enqueue,
    pending_count,
    run_relay,
)

if TYPE_CHECKING:
    from collections.abc import Iterator

DSN = os.environ.get("OVERPASS_TEST_DSN")
NATS_URL = os.environ.get("OVERPASS_TEST_NATS")

# skipif only at module level. The asyncio mark goes on the classes that need
# it, because marking a synchronous test as asyncio makes pytest warn — and a
# warning nobody reads is how a genuinely unawaited test slips through later.
pytestmark = pytest.mark.skipif(
    not (DSN and NATS_URL),
    reason="set OVERPASS_TEST_DSN and OVERPASS_TEST_NATS to run the relay tests",
)

# A real subject on a real stream, so the publish is genuinely accepted rather
# than swallowed by a broker with nowhere to put it.
SUBJECT = "feasibility.opportunities.computed.v1"


def nats_url() -> str:
    assert NATS_URL is not None
    return NATS_URL


def dsn() -> str:
    assert DSN is not None
    return DSN


@pytest.fixture
def connection() -> Iterator[psycopg.Connection[Any]]:
    with psycopg.connect(dsn()) as conn:
        yield conn
        conn.rollback()
        with conn.cursor() as cur:
            cur.execute("DELETE FROM feasibility.outbox WHERE event_type LIKE 'relaytest.%%'")
        conn.commit()


def a_message(subject: str = SUBJECT) -> OutboxMessage:
    return OutboxMessage(
        event_id=str(uuid.uuid4()),
        event_type="relaytest.event.v1",
        schema_version="1.0.0",
        subject=subject,
        payload={"hello": "world"},
        occurred_at=datetime.now(UTC),
        headers={"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
    )


def put(connection: psycopg.Connection[Any], message: OutboxMessage) -> None:
    with connection.cursor() as cur:
        enqueue(cur, message)
    connection.commit()


class TestTheClaimGuard:
    def test_claiming_on_an_autocommit_connection_is_refused(self) -> None:
        """The latent bug this guard exists for.

        Row locks live only as long as the transaction that took them. On an
        autocommit connection the FOR UPDATE lock is gone the instant the
        statement returns, so two relays claim the same rows and publish the
        same event twice. Every test passed before this guard, because they were
        reading rather than racing.
        """
        with psycopg.connect(dsn(), autocommit=True) as conn:  # noqa: SIM117
            with pytest.raises(RuntimeError, match="requires an open transaction"):
                claim_unpublished(conn)


@pytest.mark.asyncio
class TestDraining:
    async def test_an_enqueued_event_reaches_the_stream(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        message = a_message()
        put(connection, message)

        client = await nats.connect(servers=[nats_url()])
        try:
            before = (await client.jetstream().stream_info("FEASIBILITY")).state.messages
            stats = await drain_once(connection, client.jetstream())
            after = (await client.jetstream().stream_info("FEASIBILITY")).state.messages
        finally:
            await client.drain()

        assert stats.published >= 1
        assert stats.failed == 0
        assert after > before, "the stream did not grow — nothing was actually published"

    async def test_a_published_event_is_not_published_again(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # Without mark_published the relay would republish the same event on
        # every pass, forever, and the consumer's dedup would be the only thing
        # standing between that and a flood.
        put(connection, a_message())

        client = await nats.connect(servers=[nats_url()])
        try:
            first = await drain_once(connection, client.jetstream())
            second = await drain_once(connection, client.jetstream())
        finally:
            await client.drain()

        assert first.published >= 1
        assert second.published == 0

    async def test_the_traceparent_is_carried_onto_the_wire(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        """The point of storing headers at all.

        Losing the traceparent here is how one distributed trace silently
        becomes two, and this is the exact boundary where it would happen.
        """
        message = a_message()
        put(connection, message)

        client = await nats.connect(servers=[nats_url()])
        try:
            js = client.jetstream()
            await drain_once(connection, js)
            # Read it back off the stream by the id the relay published under.
            stream_msg = await js.get_msg("FEASIBILITY", seq=None, subject=SUBJECT, direct=True)
        finally:
            await client.drain()

        headers = stream_msg.headers or {}
        assert headers.get("traceparent") == message.headers["traceparent"]
        assert headers.get("Nats-Msg-Id") == message.event_id

    async def test_a_publish_failure_leaves_the_row_for_another_attempt(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        """A row dropped on a network blip is an event that never happened.

        The subject here belongs to no stream, so JetStream refuses the publish.
        That is the closest thing to a real failure that can be produced without
        breaking the broker.
        """
        put(connection, a_message(subject="nostream.nowhere.v1"))

        client = await nats.connect(servers=[nats_url()])
        try:
            stats = await drain_once(connection, client.jetstream())
        finally:
            await client.drain()

        assert stats.published == 0
        assert stats.failed == 1

        with connection.cursor() as cur:
            cur.execute(
                "SELECT attempts, last_error, published_at FROM feasibility.outbox "
                "WHERE subject = 'nostream.nowhere.v1' ORDER BY id DESC LIMIT 1"
            )
            attempts, last_error, published_at = cur.fetchone()  # type: ignore[misc]
        assert attempts == 1
        assert last_error
        assert published_at is None, "a failed publish must not be marked published"

    async def test_pending_count_falls_to_zero(self, connection: psycopg.Connection[Any]) -> None:
        for _ in range(3):
            put(connection, a_message())
        assert pending_count(connection) >= 3

        await run_relay(
            RelayConfig(nats_url=nats_url(), dsn=dsn(), idle_sleep_s=0.05),
            max_iterations=3,
        )

        # run_relay opens its own connection, so this one has to look again.
        connection.rollback()
        assert pending_count(connection) == 0


@pytest.mark.asyncio
class TestConcurrentRelays:
    async def test_two_relays_do_not_publish_the_same_row_twice(self) -> None:
        """What SKIP LOCKED buys, and what the autocommit bug destroyed.

        Two relays draining the same table must partition the work, not
        duplicate it. Run sequentially against two separate connections here —
        the second must find nothing left, because the first committed its
        marks.
        """
        message = a_message()
        with psycopg.connect(dsn()) as writer:
            put(writer, message)

        client = await nats.connect(servers=[nats_url()])
        try:
            with psycopg.connect(dsn()) as relay_a, psycopg.connect(dsn()) as relay_b:
                first = await drain_once(relay_a, client.jetstream())
                second = await drain_once(relay_b, client.jetstream())
        finally:
            await client.drain()

        assert first.published >= 1
        assert second.published == 0, "the second relay republished an already-published event"

        with psycopg.connect(dsn()) as cleanup, cleanup.cursor() as cur:
            cur.execute("DELETE FROM feasibility.outbox WHERE event_type LIKE 'relaytest.%%'")
            cleanup.commit()

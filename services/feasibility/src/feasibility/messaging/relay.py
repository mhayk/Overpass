"""The outbox relay: the process that actually gets events onto the wire.

`enqueue` writes an event inside the handler's transaction, which is what makes
the event exist if and only if its result does. Nothing in #93 took them out
again — the table filled up and no consumer downstream ever saw anything. This
is the other half.

THE TRANSACTION SPANS THE PUBLISH, deliberately. Claim, publish, mark, commit,
all inside one transaction:

  - Crash after publish, before commit: the row stays unpublished and is
    published again later. At-least-once, which every consumer already handles
    through its own dedup.
  - Crash after commit: nothing to redo.

The alternative — mark first, then publish — turns a crash into a LOST event,
and a lost event is invisible. The asymmetry is the same one that decides ack
ordering in the worker, and it points the same way.

It does mean a network publish happens with row locks held. The batch is small
for exactly that reason.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

import psycopg
from nats.aio.client import Client as NatsClient

from feasibility.messaging.outbox import claim_unpublished, mark_published, record_failure

if TYPE_CHECKING:
    from nats.js import JetStreamContext

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class RelayConfig:
    nats_url: str = "nats://localhost:4222"
    dsn: str = "postgres://overpass:overpass@localhost:5433/overpass"
    # Small on purpose: the publish happens with row locks held, so a large
    # batch means a long transaction blocking nothing useful.
    batch: int = 32
    idle_sleep_s: float = 1.0


@dataclass(frozen=True)
class RelayStats:
    published: int = 0
    failed: int = 0


async def drain_once(
    connection: psycopg.Connection[Any], js: JetStreamContext, batch: int = 32
) -> RelayStats:
    """Publish one batch. Returns what happened.

    Separated from the loop so it can be called directly in a test and asserted
    on, rather than started, slept against, and hoped about.
    """
    published = 0
    failed = 0

    with connection.transaction(), connection.cursor() as cursor:
        rows = claim_unpublished(connection, limit=batch)
        for outbox_id, event_id, subject, payload, headers in rows:
            try:
                # Nats-Msg-Id gives JetStream its own dedup window on top of
                # ours. It is a second line of defence, not the first: our
                # guarantee is the outbox row, and the broker's window expires.
                await js.publish(
                    subject,
                    payload,
                    headers={**headers, "Nats-Msg-Id": event_id},
                )
            except Exception as exc:
                # The row stays unpublished and the relay tries again. A row
                # dropped on the first network blip is an event that, as far as
                # everyone downstream is concerned, never happened.
                log.warning("publish failed for outbox row %s: %s", outbox_id, exc)
                record_failure(cursor, outbox_id, str(exc))
                failed += 1
                continue
            mark_published(cursor, outbox_id)
            published += 1

    return RelayStats(published=published, failed=failed)


async def run(
    config: RelayConfig,
    stop: asyncio.Event | None = None,
    max_iterations: int | None = None,
) -> RelayStats:
    """Drain the outbox until stopped.

    `max_iterations` exists for the same reason the worker's `max_batches`
    does: so a test can run the real loop to completion instead of cancelling
    it, and the shutdown path gets exercised.
    """
    stop = stop or asyncio.Event()
    client = NatsClient()
    await client.connect(servers=[config.nats_url])
    total = RelayStats()

    try:
        js = client.jetstream()
        # NOT autocommit. claim_unpublished refuses an autocommit connection,
        # because the row locks it takes would be released before the publish.
        with psycopg.connect(config.dsn) as connection:
            iterations = 0
            while not stop.is_set() and (max_iterations is None or iterations < max_iterations):
                iterations += 1
                stats = await drain_once(connection, js, config.batch)
                total = RelayStats(
                    published=total.published + stats.published,
                    failed=total.failed + stats.failed,
                )
                if stats.published == 0 and stats.failed == 0:
                    # Nothing to do. Sleeping beats spinning on an empty table.
                    with contextlib.suppress(TimeoutError):
                        await asyncio.wait_for(stop.wait(), timeout=config.idle_sleep_s)
    finally:
        await client.drain()

    return total


def pending_count(connection: psycopg.Connection[Any]) -> int:
    """How many events are waiting. The number to alert on in M3."""
    with connection.cursor() as cursor:
        cursor.execute("SELECT count(*) FROM feasibility.outbox WHERE published_at IS NULL")
        row = cursor.fetchone()
        return int(row[0]) if row else 0

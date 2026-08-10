"""The transactional outbox: publishing without the dual-write problem.

A handler that writes its result to Postgres and then publishes to NATS has two
writes and no transaction spanning them. Crash between and either the state
change is invisible downstream, or an event describes a state that was rolled
back. Both are silent, and both are discovered much later by someone reading a
plan that references an opportunity nobody can find.

The outbox removes the second write from the critical path. The event is
INSERTed into `feasibility.outbox` inside the same transaction as the result, so
it exists if and only if the result does. A relay drains the table afterwards.
Publishing becomes at-least-once — the relay can crash after publishing and
before marking the row — which is exactly what the consumers on the other side
already handle.

W3C `traceparent` travels in `headers`. Dropping it is how one distributed trace
silently becomes two unrelated ones, and the async hop is where that happens.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import datetime
from typing import TYPE_CHECKING, Any

from psycopg.pq import TransactionStatus

if TYPE_CHECKING:
    from collections.abc import Sequence

    import psycopg


@dataclass(frozen=True)
class OutboxMessage:
    """One event, ready to be written inside somebody else's transaction."""

    event_id: str
    event_type: str
    schema_version: str
    subject: str
    payload: dict[str, Any]
    occurred_at: datetime
    headers: dict[str, str]

    def __post_init__(self) -> None:
        if self.occurred_at.tzinfo is None:
            msg = "occurred_at must be timezone-aware"
            raise ValueError(msg)
        if not self.subject:
            msg = "an event with no subject can never be routed"
            raise ValueError(msg)


def enqueue(cursor: psycopg.Cursor[object], message: OutboxMessage) -> None:
    """Write one event to the outbox, using the CALLER'S cursor.

    Taking a cursor rather than a connection is the whole design. It makes it
    impossible to enqueue outside the caller's transaction by accident — there
    is no connection here to open one of our own with, so the event cannot
    commit separately from the result it describes.
    """
    cursor.execute(
        """
        INSERT INTO feasibility.outbox
            (event_id, event_type, schema_version, subject, payload, headers, occurred_at)
        VALUES (%s, %s, %s, %s, %s, %s, %s)
        """,
        (
            message.event_id,
            message.event_type,
            message.schema_version,
            message.subject,
            json.dumps(message.payload),
            json.dumps(message.headers),
            message.occurred_at,
        ),
    )


def enqueue_once(cursor: psycopg.Cursor[object], message: OutboxMessage) -> bool:
    """Write one event unless its id is already in the outbox. Returns whether it was.

    `enqueue` raises on a duplicate id, and that is right for a handler: an
    event id that already exists means the handler ran twice, which the
    idempotency ledger was supposed to have prevented, and swallowing it would
    hide a real fault.

    The ephemeris sweep is the one producer where a repeat is EXPECTED rather
    than exceptional. It runs on a timer over a rolling horizon, so every tick
    necessarily re-covers buckets it already published, and its event id is
    derived from `(satellite_id, bucket, tle_epoch)` precisely so that the
    repeat collides here instead of reaching the wire.

    A separate function rather than a flag on `enqueue`, so nothing acquires
    this behaviour by accident. A publisher that quietly tolerates duplicate
    ids is a publisher whose deduplication has stopped meaning anything.
    """
    cursor.execute(
        """
        INSERT INTO feasibility.outbox
            (event_id, event_type, schema_version, subject, payload, headers, occurred_at)
        VALUES (%s, %s, %s, %s, %s, %s, %s)
        ON CONFLICT (event_id) DO NOTHING
        """,
        (
            message.event_id,
            message.event_type,
            message.schema_version,
            message.subject,
            json.dumps(message.payload),
            json.dumps(message.headers),
            message.occurred_at,
        ),
    )
    return cursor.rowcount == 1


def already_enqueued(cursor: psycopg.Cursor[Any], event_ids: Sequence[str]) -> set[str]:
    """Which of these event ids the outbox already holds.

    A performance shortcut in front of `enqueue_once`, not a replacement for it.
    Deriving an ephemeris event's id costs nothing; producing its BODY costs an
    SGP4 propagation over a thousand instants. Asking first means a steady-state
    sweep does no physics at all, where enqueue-and-discard would do all of it
    every tick and throw the answer away.

    It is not a correctness mechanism and must not be treated as one: two
    sweepers can both see an id as absent. `enqueue_once`'s ON CONFLICT is what
    actually holds, which is why it stays even though this exists.
    """
    if not event_ids:
        return set()
    cursor.execute(
        "SELECT event_id::text FROM feasibility.outbox WHERE event_id = ANY(%s::uuid[])",
        (list(event_ids),),
    )
    return {row[0] for row in cursor.fetchall()}


def claim_unpublished(
    connection: psycopg.Connection[Any], limit: int = 100
) -> list[tuple[int, str, str, bytes, dict[str, str]]]:
    """Take the next batch of unpublished events for the relay.

    `FOR UPDATE SKIP LOCKED` so that two relay instances can drain the same
    table without either blocking on the other or handing out the same row
    twice. Ordered by id, which is the insertion order the events happened in.

    MUST be called inside an open transaction, and refuses otherwise. Row locks
    live for the length of the transaction that took them, so on an autocommit
    connection the lock is released the instant this statement returns — the
    rows come back looking claimed and are not, and a second relay hands out the
    same events. That is a duplicate publish, discovered under concurrency,
    which is the worst way to discover one.

    This guard exists because the first version of this function did exactly
    that, and every test passed: they were reading, not racing.

    The check is on the connection's TRANSACTION STATUS, not on its
    `autocommit` setting. Those are different things and the first version of
    the guard confused them: `connection.transaction()` opens a real
    transaction block even on an autocommit connection, so keying off the
    setting rejected correct callers. Measured — INTRANS inside
    `.transaction()`, IDLE outside, identically in both modes.
    """
    if connection.info.transaction_status != TransactionStatus.INTRANS:
        detail = (
            "claim_unpublished requires an open transaction. Row locks live only as "
            "long as the transaction that took them, so outside one the FOR UPDATE "
            "is released immediately and a second relay claims the same rows. Wrap "
            "the claim, the publish and the mark in one `with connection.transaction():`."
        )
        raise RuntimeError(detail)

    with connection.cursor() as cursor:
        cursor.execute(
            """
            -- event_id::text, not event_id. psycopg returns a uuid.UUID for a
            -- uuid column, and the signature here promises a str. An
            -- annotation that quietly disagrees with the value is worse than
            -- no annotation: every caller comparing against a string id gets
            -- False and no error.
            SELECT id, event_id::text, subject, payload::text, headers
            FROM feasibility.outbox
            WHERE published_at IS NULL
            ORDER BY id
            LIMIT %s
            FOR UPDATE SKIP LOCKED
            """,
            (limit,),
        )
        rows: list[tuple[int, str, str, str, dict[str, str]]] = cursor.fetchall()
        return [(i, eid, subj, body.encode(), hdrs) for i, eid, subj, body, hdrs in rows]


def mark_published(cursor: psycopg.Cursor[object], outbox_id: int) -> None:
    """Record that the relay got this one onto the wire."""
    cursor.execute(
        "UPDATE feasibility.outbox SET published_at = now() WHERE id = %s",
        (outbox_id,),
    )


def record_failure(cursor: psycopg.Cursor[object], outbox_id: int, error: str) -> None:
    """Count a failed publish attempt and keep the reason.

    The row stays unpublished, so the relay tries again. `attempts` is what a
    poison-message policy will key off in M3 — a row that has failed many times
    is not going to succeed, and the DLQ is where it belongs.
    """
    cursor.execute(
        """
        UPDATE feasibility.outbox
        SET attempts = attempts + 1, last_error = %s
        WHERE id = %s
        """,
        (error[:2000], outbox_id),
    )


def oldest_unpublished_age_s(connection: psycopg.Connection[Any]) -> float | None:
    """Age of the oldest unpublished outbox row, in seconds, or None if empty.

    None rather than 0.0 for an empty outbox is the caller's decision to make,
    not this function's: "nothing is waiting" and "nothing has ever been
    measured" are different facts and the relay reports them differently.

    Ordered on the same column and predicate as `claim_unpublished`, so what
    this measures is the age of the row that relay will take next.
    """
    with connection.cursor() as cursor:
        cursor.execute(
            """
            SELECT EXTRACT(EPOCH FROM (now() - min(occurred_at)))
            FROM feasibility.outbox
            WHERE published_at IS NULL
            """
        )
        row = cursor.fetchone()
    if row is None or row[0] is None:
        return None
    return max(0.0, float(row[0]))

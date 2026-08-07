"""Effectively-once processing on top of at-least-once delivery.

JetStream gives at-least-once. It does not give exactly-once, and nothing here
claims it does. What this module builds is *effectively-once processing*: the
work may be attempted more than once, but it lands exactly once.

The whole mechanism is one transaction, prescribed in `contracts/nats/topology.md`:

    BEGIN;
      INSERT INTO processed_events (consumer, event_id) VALUES (...);
      -- the actual state change
    COMMIT;
    -- then ACK

A duplicate delivery violates the unique constraint, the transaction rolls back
whole, and the message is acked anyway because the work was already done.

The two halves must commit or fail TOGETHER. As separate transactions, a crash
between them either loses the work or marks it permanently done without having
happened — and both are silent.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum, auto
from typing import TYPE_CHECKING

import psycopg
from psycopg import errors as pg_errors

if TYPE_CHECKING:
    from collections.abc import Callable


class Outcome(Enum):
    """What happened to one delivery."""

    PROCESSED = auto()
    """First time seen. The work committed."""

    DUPLICATE = auto()
    """Already processed. Nothing was done, and the message must still be acked."""

    FAILED_RETRYABLE = auto()
    """Transient. Do not ack; let the broker redeliver."""

    FAILED_TERMINAL = auto()
    """Will never succeed. Ack it, and publish the refusal — see below."""


class NonRetryableError(Exception):
    """A failure that retrying cannot fix.

    Named for what the caller must DO about it rather than for what it is —
    `retryable` is the flag the contract carries, so the exception that means
    "not that" should say so.

    Raised by a handler to say "this is a correct negative answer, not an
    error". `NO_ACCESS_IN_HORIZON` is the archetype: the geometry does not
    permit an acquisition, and it will not permit one on the fifth attempt
    either. Retrying a physical impossibility burns compute forever and
    consumes redelivery budget that a genuinely transient failure needs.

    The distinction is why `feasibility.failed.v1` carries a `retryable` flag
    rather than treating every failure the same.
    """

    def __init__(self, reason_code: str, detail: str = "") -> None:
        super().__init__(f"{reason_code}: {detail}" if detail else reason_code)
        self.reason_code = reason_code
        self.detail = detail


@dataclass(frozen=True)
class Delivery:
    """One message off the stream, with what the dedup needs to know about it."""

    event_id: str
    subject: str
    payload: bytes
    delivered_count: int
    headers: dict[str, str]


def process_once(
    connection: psycopg.Connection[object],
    consumer: str,
    delivery: Delivery,
    handler: Callable[[psycopg.Cursor[object]], None],
) -> Outcome:
    """Run `handler` exactly once for this event, in one transaction.

    `handler` receives the cursor and must do all of its writing through it.
    Anything it writes elsewhere — another connection, a file, an HTTP call —
    is outside the transaction and outside the guarantee, which is the usual
    way an idempotent consumer stops being idempotent.

    Returns without acking. Acking is the caller's job precisely BECAUSE it must
    happen after the commit: this function returning is the signal that the
    commit succeeded.
    """
    try:
        with connection.transaction(), connection.cursor() as cursor:
            # The dedup insert goes FIRST. Not for correctness — the two commit
            # together either way — but so that a duplicate is rejected before
            # the expensive work runs. On a redelivery of an already-processed
            # sweep, this saves the whole propagation.
            cursor.execute(
                "INSERT INTO feasibility.processed_events (consumer, event_id) VALUES (%s, %s)",
                (consumer, delivery.event_id),
            )
            handler(cursor)
    except pg_errors.UniqueViolation:
        # Already processed. The transaction rolled back whole, including the
        # duplicate dedup row. The caller acks: the work is done, and leaving it
        # unacked would redeliver forever.
        return Outcome.DUPLICATE
    except NonRetryableError:
        raise
    return Outcome.PROCESSED


def already_processed(connection: psycopg.Connection[object], consumer: str, event_id: str) -> bool:
    """Whether this consumer has already handled this event.

    Only for tests and operational inspection. The processing path does NOT
    check first and then insert — that is a race, and the unique constraint is
    the real guard.
    """
    with connection.cursor() as cursor:
        cursor.execute(
            "SELECT 1 FROM feasibility.processed_events WHERE consumer = %s AND event_id = %s",
            (consumer, event_id),
        )
        return cursor.fetchone() is not None

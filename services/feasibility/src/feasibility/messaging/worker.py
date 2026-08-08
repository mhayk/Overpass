"""The consumer loop: fetch, process, ack — in that order, always.

Binds to the durable pull consumer `feasibility-worker`, which
`deploy/nats/init.sh` already created with explicit ack, `ack_wait` 120s and
`max_deliver` 5. The worker does NOT create it: a consumer created lazily from
application code is a consumer whose settings live in whichever service started
first, and two services racing to create the same one with different settings is
a genuinely unpleasant afternoon.

THE HANDLING RUNS IN A THREAD, and that is not incidental. A constellation sweep
takes seconds of solid numpy, and psycopg is synchronous. Doing either on the
event loop stops the NATS client answering the server's heartbeats, and a client
that stops answering gets disconnected mid-sweep — after which the message
redelivers and the work happens again. `asyncio.to_thread` keeps the loop free
to do exactly the one thing it must keep doing.

Ack ordering is the whole point and is repeated here because it is the thing
most easily lost in a refactor: **ack strictly after commit, never before.**
"""

from __future__ import annotations

import asyncio
import json
import logging
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any

import psycopg
from nats.aio.client import Client as NatsClient
from nats.js.errors import NotFoundError
from opentelemetry.trace import Status, StatusCode

from feasibility import failures
from feasibility.messaging.idempotency import (
    Delivery,
    NonRetryableError,
    Outcome,
    process_once,
)
from feasibility.messaging.outbox import enqueue
from feasibility.telemetry import consumer_span

if TYPE_CHECKING:
    from collections.abc import Callable

    from nats.aio.msg import Msg

log = logging.getLogger(__name__)

CONSUMER = "feasibility-worker"
STREAM = "TASKING"

# Redelivery backoff for a transient failure. Short enough that a blip does not
# stall the request, long enough that a database that is down does not get five
# attempts inside a second and burn the whole max_deliver budget on one outage.
_NAK_DELAY_S = 5.0


@dataclass(frozen=True)
class WorkerConfig:
    nats_url: str = "nats://localhost:4222"
    dsn: str = "postgres://overpass:overpass@localhost:5433/overpass"
    batch: int = 8
    fetch_timeout_s: float = 5.0


def delivery_from(msg: Msg) -> Delivery:
    """Build a Delivery, taking the event id from the envelope.

    The contract puts `event_id` in the payload, and that is the identity the
    dedup keys on — not the NATS sequence number, which changes on republish and
    would let the same logical event through twice.

    A message whose payload has no `event_id` cannot be deduplicated at all, so
    it is a terminal failure rather than something to guess an id for.
    """
    headers = dict(msg.headers or {})
    try:
        envelope = json.loads(msg.data)
        event_id = str(envelope["event_id"])
    except (json.JSONDecodeError, KeyError, TypeError) as exc:
        detail = "message has no usable event_id in its envelope"
        raise NonRetryableError("INTERNAL_ERROR", detail) from exc

    metadata = msg.metadata
    return Delivery(
        event_id=event_id,
        subject=msg.subject,
        payload=msg.data,
        delivered_count=metadata.num_delivered or 1,
        headers=headers,
    )


def _publish_refusal(
    connection: psycopg.Connection[Any],
    delivery: Delivery,
    failure: NonRetryableError,
) -> None:
    """Record the refusal through the outbox, deduplicated like any other work.

    Uses `process_once` again rather than writing directly, so that the refusal
    is published exactly once even if the ack never lands and the message comes
    back. A refusal published twice is a second `feasibility.failed.v1` for a
    request that only failed once, and the state machine downstream would see a
    transition it cannot explain.

    The payload is built by `failures.build_refusal`, which is pure and which
    records what it emits for `contracts-validate`. It used to be built inline
    here, as six bare fields with no envelope — invalid against its own schema
    and undecodable by every consumer, for as long as nothing had shown the
    schema what this producer really sends. See #132.
    """

    def handler(cursor: psycopg.Cursor[Any]) -> None:
        enqueue(
            cursor,
            failures.build_refusal(
                delivery,
                failure.reason_code,
                # Terminal by construction: this path is reached only from a
                # NonRetryableError, which is the exception that means "correct
                # negative answer" rather than "something broke".
                retryable=False,
                detail=failure.detail,
                now=datetime.now(UTC),
            ),
        )

    try:
        process_once(connection, f"{CONSUMER}:refusal", delivery, handler)
    except ValueError:
        # A refusal that cannot be attributed to a request. The causing message
        # had no readable envelope, so there is no request_id to name and no
        # valid event to publish — and publishing an invalid one would be worse
        # than publishing none, because the consumer would reject it forever.
        #
        # Logged and dropped rather than raised: the caller is about to ACK a
        # message it has already decided is terminal, and turning this into a
        # retryable failure would redeliver a message nothing can ever handle.
        log.exception(
            "cannot publish a refusal for %s; the causing message is unattributable",
            delivery.event_id,
        )


def handle_one(
    connection: psycopg.Connection[Any],
    delivery: Delivery,
    handler: Callable[[psycopg.Cursor[Any]], None],
) -> Outcome:
    """Process one delivery and say what the caller should do with the message.

    Never acks. The caller acks, after this returns, because this returning is
    the evidence that the commit happened.
    """
    try:
        return process_once(connection, CONSUMER, delivery, handler)
    except NonRetryableError as failure:
        # A correct negative answer. Publish it and let the caller ack — this
        # will not succeed on the fifth attempt either, and retrying it would
        # consume redelivery budget a transient failure needs.
        log.info("terminal refusal for %s: %s", delivery.event_id, failure.reason_code)
        _publish_refusal(connection, delivery, failure)
        return Outcome.FAILED_TERMINAL
    except Exception:
        # Anything else is assumed transient. Assuming the other way round would
        # silently drop requests on a bug, and a stuck message is far easier to
        # notice than a missing one.
        log.exception("retryable failure for %s", delivery.event_id)
        return Outcome.FAILED_RETRYABLE


async def _settle(msg: Msg, outcome: Outcome) -> None:
    """Tell the broker what happened. Called only after the transaction ended."""
    if outcome is Outcome.FAILED_RETRYABLE:
        await msg.nak(delay=_NAK_DELAY_S)
    else:
        # PROCESSED, DUPLICATE and FAILED_TERMINAL all ack.
        #
        # DUPLICATE acks because the work is already done and leaving it unacked
        # would redeliver it forever. FAILED_TERMINAL acks because the refusal
        # has been published and the answer is final — a term() would say we
        # never handled it, which is not what happened.
        await msg.ack()


async def run(
    config: WorkerConfig,
    handler_factory: Callable[[Delivery], Callable[[psycopg.Cursor[Any]], None]],
    stop: asyncio.Event | None = None,
    max_batches: int | None = None,
) -> int:
    """Run the consume loop until `stop` is set or `max_batches` is reached.

    `max_batches` exists so tests can run the real loop to completion instead of
    racing a background task and hoping. A loop that can only be tested by
    cancelling it is a loop whose shutdown path is never exercised.

    Returns the number of messages settled.
    """
    stop = stop or asyncio.Event()
    client = NatsClient()
    await client.connect(servers=[config.nats_url])
    settled = 0

    try:
        js = client.jetstream()
        try:
            subscription = await js.pull_subscribe_bind(CONSUMER, stream=STREAM)
        except NotFoundError as exc:
            detail = (
                f"durable consumer {STREAM}/{CONSUMER} does not exist. It is created by "
                "deploy/nats/init.sh, deliberately not by this worker — see the module "
                "docstring."
            )
            raise RuntimeError(detail) from exc

        with psycopg.connect(config.dsn, autocommit=True) as connection:
            batches = 0
            while not stop.is_set() and (max_batches is None or batches < max_batches):
                batches += 1
                try:
                    messages = await subscription.fetch(
                        config.batch, timeout=config.fetch_timeout_s
                    )
                except TimeoutError:
                    continue

                for msg in messages:
                    try:
                        delivery = delivery_from(msg)
                    except NonRetryableError:
                        # Unparseable envelope. Nothing to dedup on and nothing
                        # to refuse about, so drop it rather than redeliver it
                        # five times.
                        log.exception("undeliverable message on %s", msg.subject)
                        await msg.term()
                        settled += 1
                        continue

                    # The consumer span wraps the work AND the settle, so the
                    # ack is inside the trace. An ack that fails after a
                    # successful commit is exactly the case that produces a
                    # duplicate delivery, and leaving it outside the span would
                    # hide the one hop that explains why the message came back.
                    with consumer_span(
                        f"{msg.subject} process",
                        delivery.headers,
                        {
                            "messaging.system": "nats",
                            "messaging.destination.name": msg.subject,
                            "messaging.message.id": delivery.event_id,
                            "messaging.consumer.group.name": CONSUMER,
                            "messaging.nats.delivered_count": delivery.delivered_count,
                        },
                    ) as span:
                        outcome = await asyncio.to_thread(
                            handle_one, connection, delivery, handler_factory(delivery)
                        )
                        span.set_attribute("overpass.outcome", outcome.name)
                        if outcome is Outcome.FAILED_RETRYABLE:
                            # An error status, but no exception recorded: the
                            # exception was logged where it happened, and a
                            # retryable failure is an expected state of this
                            # system rather than a fault of this span.
                            span.set_status(Status(StatusCode.ERROR, "retryable failure"))
                        await _settle(msg, outcome)
                    settled += 1
    finally:
        await client.drain()

    return settled

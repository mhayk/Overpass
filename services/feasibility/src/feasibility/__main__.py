"""Run the feasibility worker as a process.

``python -m feasibility``

The service had no entry point until now: ``worker.run`` existed and only tests
called it. That was fine while the only thing exercising it was pytest, and it
stopped being fine the moment #32 needed to assert that ONE trace spans this
service and tasking-api — which cannot be shown without this service actually
running as a process.

Deliberately thin. Everything it does is read configuration, install tracing and
logging, and hand off; the composition root is the one place a test cannot
reach, so there must be as little in it as possible.
"""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import sys
from typing import TYPE_CHECKING, Any

import psycopg

from feasibility import telemetry
from feasibility.messaging import Delivery, WorkerConfig, run

if TYPE_CHECKING:
    from collections.abc import Callable

log = logging.getLogger("feasibility")


def _configure_logging() -> None:
    """JSON-ish structured logging with the trace context attached.

    The filter goes on the HANDLER rather than the logger, so it applies to
    records that propagate up from library loggers too. A trace id present on
    this module's lines and absent from nats-py's would be worse than having
    none: it looks like the library ran outside the trace.
    """
    handler = logging.StreamHandler(sys.stdout)
    handler.addFilter(telemetry.TraceContextFilter())
    handler.setFormatter(
        logging.Formatter(
            '{"time":"%(asctime)s","level":"%(levelname)s","logger":"%(name)s",'
            '"trace_id":"%(trace_id)s","span_id":"%(span_id)s","msg":"%(message)s"}'
        )
    )
    logging.basicConfig(
        level=os.getenv("LOG_LEVEL", "INFO").upper(),
        handlers=[handler],
        force=True,
    )


def _record_only(_delivery: Delivery) -> Callable[[psycopg.Cursor[Any]], None]:
    """Do no domain work, and let the idempotency ledger be the evidence.

    NOT the SGP4 sweep. Wiring the real pipeline in needs the seeded
    constellation from #31, and this entry point exists so the async hop can be
    traced end to end before that lands.

    The handler is genuinely empty rather than writing a marker row of its own.
    `process_once` already inserts into feasibility.processed_events inside the
    same transaction, so that row IS the state change — a second table invented
    to prove the first one happened would be a fixture pretending to be schema.
    (The first draft of this file did exactly that, against a
    `feasibility.processed_requests` that does not exist and never has.)

    Replacing this with the real evaluate() is #31's job. The worker loop, the
    ledger and the tracing around them do not change when it does.
    """

    def handler(_cursor: psycopg.Cursor[Any]) -> None:
        return

    return handler


async def _run() -> int:
    config = WorkerConfig(
        nats_url=os.getenv("NATS_URL", "nats://localhost:4222"),
        dsn=os.getenv("DATABASE_URL", "postgres://overpass:overpass@localhost:5433/overpass"),
    )

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        # A worker that only stops on SIGKILL redelivers whatever it was holding
        # on every deploy. The loop checks `stop` between batches, so this drains
        # the current batch and exits rather than abandoning it mid-transaction.
        loop.add_signal_handler(sig, stop.set)

    log.info("feasibility worker starting")
    settled = await run(config, _record_only, stop=stop)
    log.info("feasibility worker stopped after settling %d messages", settled)
    return 0


def main() -> int:
    _configure_logging()

    # Tracing before the first message is fetched, and flushed on the way out.
    # Spans are batched, so exiting without a flush loses the last half second —
    # reliably the interesting half second, because it is the one around
    # whatever made the process stop.
    provider = telemetry.setup()
    try:
        return asyncio.run(_run())
    finally:
        if provider is not None:
            provider.shutdown()


if __name__ == "__main__":
    raise SystemExit(main())

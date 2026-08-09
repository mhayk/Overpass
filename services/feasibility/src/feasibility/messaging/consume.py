"""The #48 hardening, mirroring lib/go/consume so the pair reads as one design.

Three pieces the M1-13 idempotency module deliberately did not carry:

* the poison DECISION — retry until the last delivery, then terminate on
  purpose, because a lapsed ``max_deliver`` is a silent drop and a ``term()``
  has a log line and a metric;
* METRICS in the shape M3-06 will scrape;
* ledger CLEANUP whose guard refuses any retention inside the redelivery
  horizon, because deleting a row the broker can still redeliver against
  reprocesses the redelivery as new — a silent correctness bug that only
  appears under load.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import timedelta
from enum import Enum, auto

import psycopg

from feasibility.messaging.idempotency import Outcome


class Decision(Enum):
    """What to do with a failed delivery."""

    RETRY = auto()
    """Nak and let the broker redeliver."""

    TERMINATE = auto()
    """Term — poison, or the final attempt of a failure retrying has not
    fixed. Deliberate, logged, final."""


def on_failure(*, permanent: bool, delivered: int, max_deliver: int) -> Decision:
    """Decide RETRY or TERMINATE for one failed delivery.

    ``permanent`` failures terminate on the FIRST delivery: rerunning a
    deterministic failure ``max_deliver`` times adds nothing but latency for
    the messages behind it. Everything else retries until the LAST delivery,
    then terminates deliberately — the broker's counter reaching zero pages
    nobody. ``max_deliver <= 0`` never invents a bound the operator did not
    configure.
    """
    if permanent:
        return Decision.TERMINATE
    if max_deliver > 0 and delivered >= max_deliver:
        return Decision.TERMINATE
    return Decision.RETRY


@dataclass
class Metrics:
    """What an operator needs to know a consumer is healthy.

    ``duplicates`` is the counter that proves idempotency is WORKING rather
    than untested: duplicates arrive by design under at-least-once delivery,
    and a zero over weeks means the dedup path has never run in anger.
    ``redeliveries`` is the early warning — climbing with flat throughput is
    poison or a dying dependency, visible before ``max_deliver`` makes it a
    loss.
    """

    processed: int = 0
    duplicates: int = 0
    redeliveries: int = 0
    terminated: int = 0
    ack_latency_total_s: float = 0.0
    ack_count: int = 0
    ack_latency_max_s: float = 0.0

    def record(self, outcome: Outcome, delivered: int, seconds: float) -> None:
        """One call per settled delivery, from the worker's loop.

        Retries and terminations carry no ack latency; the worker advances
        ``terminated`` itself, because only it knows ``max_deliver``.
        """
        if delivered > 1:
            self.redeliveries += 1
        if outcome is Outcome.DUPLICATE:
            self.duplicates += 1
            self.ack_after(seconds)
        elif outcome in (Outcome.PROCESSED, Outcome.FAILED_TERMINAL):
            # A published refusal is work completed and acked, same as success.
            self.processed += 1
            self.ack_after(seconds)

    def ack_after(self, seconds: float) -> None:
        self.ack_latency_total_s += seconds
        self.ack_count += 1
        self.ack_latency_max_s = max(self.ack_latency_max_s, seconds)

    @property
    def ack_latency_mean_s(self) -> float:
        return self.ack_latency_total_s / self.ack_count if self.ack_count else 0.0


def cleanup(
    connection: psycopg.Connection[object],
    *,
    retention: timedelta,
    max_deliver: int,
    ack_wait: timedelta,
) -> int:
    """Delete ledger rows older than ``retention``.

    The guard is the point: retention inside the redelivery horizon
    (``max_deliver x ack_wait x 2``) is refused, because a ledger row younger
    than the broker's longest possible redelivery is still doing its job.
    """
    horizon = max_deliver * ack_wait * 2
    if retention < horizon:
        detail = (
            f"retention {retention} is inside the redelivery horizon {horizon} "
            f"(max_deliver {max_deliver} x ack_wait {ack_wait} x 2); "
            "a late redelivery would be reprocessed as new"
        )
        raise ValueError(detail)
    with connection.cursor() as cursor:
        cursor.execute(
            "DELETE FROM feasibility.processed_events WHERE processed_at < now() - %s",
            (retention,),
        )
        return cursor.rowcount

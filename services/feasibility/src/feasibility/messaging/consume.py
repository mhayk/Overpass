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

And, since #49, the step that turns a ``term()`` from a deliberate DROP into a
deliberate HANDOFF: :func:`publish_dead_letter`.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from enum import Enum, auto
from typing import Protocol

import psycopg

from feasibility import metrics
from feasibility.messaging.idempotency import Outcome

# The contract between this service's Outcome enum and the shared outcome
# vocabulary lib/go/consume publishes. Two languages reporting the same
# pipeline must agree on these strings, or every consumer panel needs one query
# per language.
#
# FAILED_TERMINAL maps to "processed", not to a failure: the refusal WAS
# published and the message WAS acked, so it is completed work. The failures
# this service reports as such are the ones that get naked or dead-lettered.
_OUTCOME_LABELS = {
    Outcome.PROCESSED: metrics.OUTCOME_PROCESSED,
    Outcome.DUPLICATE: metrics.OUTCOME_DUPLICATE,
    Outcome.FAILED_RETRYABLE: metrics.OUTCOME_FAILED,
    Outcome.FAILED_TERMINAL: metrics.OUTCOME_PROCESSED,
}


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
    deadlettered: int = 0
    """Terminal failures whose payload reached a DLQ stream. ``terminated``
    minus ``deadlettered`` is how many messages this worker dropped without
    keeping a copy — it should be zero, which is why they are two counters."""
    ack_latency_total_s: float = 0.0
    ack_count: int = 0
    ack_latency_max_s: float = 0.0

    def record(self, outcome: Outcome, delivered: int, seconds: float, subject: str = "") -> None:
        """One call per settled delivery, from the worker's loop.

        Retries and terminations carry no ack latency; the worker advances
        ``terminated`` itself, because only it knows ``max_deliver``.

        This is also where the OTel histogram is fed, because it is the one
        place every settled delivery already passes through. Feeding it from
        here rather than from the worker's branches is what stops the two
        counts drifting apart.
        """
        if delivered > 1:
            self.redeliveries += 1
            metrics.instruments().record_redelivery(subject)

        exported = _OUTCOME_LABELS[outcome]
        if outcome is Outcome.DUPLICATE:
            self.duplicates += 1
            self.ack_after(seconds)
        elif outcome in (Outcome.PROCESSED, Outcome.FAILED_TERMINAL):
            # A published refusal is work completed and acked, same as success.
            self.processed += 1
            self.ack_after(seconds)

        # Recorded for EVERY outcome, including FAILED_RETRYABLE — the one the
        # in-process counters above deliberately ignore because it carries no
        # ack latency. A consumer naking every delivery would otherwise report
        # no throughput and no errors at all: its panels would go flat, which
        # reads as idle rather than as on fire.
        metrics.instruments().record_consume(subject, exported, seconds * 1000.0)

    def ack_after(self, seconds: float) -> None:
        self.ack_latency_total_s += seconds
        self.ack_count += 1
        self.ack_latency_max_s = max(self.ack_latency_max_s, seconds)

    @property
    def ack_latency_mean_s(self) -> float:
        return self.ack_latency_total_s / self.ack_count if self.ack_count else 0.0


DLQ_SUBJECT_PREFIX = "dlq."
"""Maps an original subject onto its dead-letter subject.

A PREFIX, not an infix. The obvious-looking ``tasking.dlq.>`` is already inside
the TASKING stream's ``tasking.>`` wildcard, and NATS refuses to create two
streams whose subjects overlap (error 10065). Prefixing keeps the two subject
spaces disjoint and keeps the mapping reversible by trimming, which is what
makes the replay script a string operation rather than a lookup table.
"""

# The header set from contracts/nats/topology.md. Constants because the inspect
# and replay tooling reads exactly these strings: a typo is a tool that silently
# reports nothing rather than an error anyone sees.
HEADER_REASON = "Overpass-Dlq-Reason"
HEADER_ORIGINAL_SUBJECT = "Overpass-Dlq-Original-Subject"
HEADER_DELIVERY_COUNT = "Overpass-Dlq-Delivery-Count"
HEADER_FAILED_AT = "Overpass-Dlq-Failed-At"
HEADER_CONSUMER = "Overpass-Dlq-Consumer"
HEADER_TRACEPARENT = "traceparent"
HEADER_MSG_ID = "Nats-Msg-Id"

# Terminal error classes, shared with lib/go/consume. A small vocabulary rather
# than free text: the runbook triages on the reason header, and four consumers
# each inventing their own wording is a triage step that starts by reading
# source code.
REASON_DECODE = "decode"
REASON_CONTRACT = "contract"
REASON_METADATA = "metadata"
REASON_EXHAUSTED = "exhausted"


@dataclass(frozen=True)
class DeadLetter:
    """One terminal failure, as whoever inspects it will see it."""

    subject: str
    """Where it came from. Published to ``DLQ_SUBJECT_PREFIX + subject``."""

    payload: bytes
    """Republished byte for byte; replay depends on it being the original."""

    reason: str
    """Terminal error class — one of the ``REASON_*`` constants."""

    consumer: str
    """The durable consumer that gave up."""

    delivered: int = 1
    """Attempts the broker made."""

    event_id: str = ""
    """Becomes ``Nats-Msg-Id``. May be empty — see :func:`publish_dead_letter`."""

    traceparent: str = ""
    """As received. Empty omits the header."""

    failed_at: datetime | None = None
    """When the terminal decision was made; ``None`` means now.

    The decision time, NOT the first failure: consumers are stateless across
    deliveries, so nobody knows when the first failure happened, and a header
    cannot promise information nobody has (ADR-0017).
    """


class DlqPublisher(Protocol):
    """The one method :func:`publish_dead_letter` needs.

    A protocol rather than the JetStream context itself, so the header contract
    can be tested without a broker — and structural, so the real
    ``nats.js.JetStreamContext`` satisfies it with no adapter in between.

    ``headers`` is KEYWORD-ONLY, and that is not a style choice: nats-py's
    signature is ``publish(subject, payload, timeout, stream, headers, ...)``,
    so a protocol that takes headers third is one the real client does not
    satisfy. ``test_the_real_jetstream_context_satisfies_the_publisher_protocol``
    is what caught it, and is what will catch the next signature change.
    """

    async def publish(
        self,
        subject: str,
        payload: bytes = b"",
        *,
        headers: dict[str, str] | None = None,
    ) -> object: ...


async def publish_dead_letter(publisher: DlqPublisher, letter: DeadLetter) -> None:
    """Publish the dead letter. The caller ``term()``s on success, ``nak()``s on error.

    THE ORDERING INVARIANT, which this function cannot enforce because the ack
    handle belongs to the caller (ADR-0017):

        PUBLISH, THEN TERM. IF THE PUBLISH FAILS, NAK.

    A term without a landed dead letter re-creates the loss with extra steps. A
    nak retries the whole delivery — the handling and the dead-lettering — which
    is safe because both are idempotent. The residue is a crash between publish
    and term, which produces a duplicate dead letter; the original event id
    rides along as ``Nats-Msg-Id`` so the DLQ stream's dedup window absorbs it,
    and beyond that window duplicates are tolerated because everything
    downstream of the DLQ is duplicate-safe by construction.

    What it refuses and what it tolerates is a deliberate split.

    REFUSED — subject, reason, consumer. All three are call-site facts: literals
    and a consumer name known at wiring time, never anything a bad message can
    influence. An empty one is a programming error and cannot be provoked by
    traffic.

    TOLERATED — a missing event id or traceparent. Both are message data, and a
    message with no readable id is exactly the kind that dies here. Refusing
    would leave the caller naking forever under the ordering above, which is the
    loss this mechanism exists to prevent: a dead letter with no dedup key is
    worth far more than no dead letter at all.
    """
    if not letter.subject:
        detail = "dead letter has no original subject; there is nowhere to publish it"
        raise ValueError(detail)
    if letter.subject.startswith(DLQ_SUBJECT_PREFIX):
        detail = (
            f"subject {letter.subject!r} is already a dead letter; dead-lettering it "
            "again would invent a subject space nobody declared"
        )
        raise ValueError(detail)
    if not letter.reason:
        detail = (
            f"dead letter for {letter.subject!r} has no reason; an operator would find "
            "a payload and no account of why it is there"
        )
        raise ValueError(detail)
    if not letter.consumer:
        detail = (
            f"dead letter for {letter.subject!r} names no consumer; an operator would "
            "not know who gave up on it"
        )
        raise ValueError(detail)

    failed_at = letter.failed_at or datetime.now(UTC)
    headers = {
        HEADER_REASON: letter.reason,
        HEADER_ORIGINAL_SUBJECT: letter.subject,
        HEADER_DELIVERY_COUNT: str(letter.delivered),
        # Z rather than +00:00: the same RFC 3339 instant the Go half writes, so
        # one inspection tool reads both without a second format to handle.
        HEADER_FAILED_AT: failed_at.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ"),
        HEADER_CONSUMER: letter.consumer,
    }
    # Absent, not blank. A blank traceparent parses downstream as a broken trace
    # context where an absent one parses as "no trace", and a blank Msg-Id is a
    # dedup key every id-less message would share.
    if letter.traceparent:
        headers[HEADER_TRACEPARENT] = letter.traceparent
    if letter.event_id:
        headers[HEADER_MSG_ID] = letter.event_id

    await publisher.publish(
        DLQ_SUBJECT_PREFIX + letter.subject,
        letter.payload,
        headers=headers,
    )


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

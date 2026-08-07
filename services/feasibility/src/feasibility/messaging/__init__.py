"""Idempotent consumption and transactional publication.

JetStream is at-least-once. Everything here exists to turn that into
effectively-once processing — see idempotency.py for the mechanism and
contracts/nats/topology.md for the rule it implements.
"""

from feasibility.messaging.idempotency import (
    Delivery,
    NonRetryableError,
    Outcome,
    already_processed,
    process_once,
)
from feasibility.messaging.outbox import (
    OutboxMessage,
    claim_unpublished,
    enqueue,
    mark_published,
    record_failure,
)

__all__ = [
    "Delivery",
    "NonRetryableError",
    "OutboxMessage",
    "Outcome",
    "already_processed",
    "claim_unpublished",
    "enqueue",
    "mark_published",
    "process_once",
    "record_failure",
]

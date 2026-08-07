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
from feasibility.messaging.relay import RelayConfig, RelayStats, drain_once, pending_count
from feasibility.messaging.relay import run as run_relay
from feasibility.messaging.worker import (
    CONSUMER,
    STREAM,
    WorkerConfig,
    delivery_from,
    handle_one,
    run,
)

__all__ = [
    "CONSUMER",
    "STREAM",
    "Delivery",
    "NonRetryableError",
    "OutboxMessage",
    "Outcome",
    "RelayConfig",
    "RelayStats",
    "WorkerConfig",
    "already_processed",
    "claim_unpublished",
    "delivery_from",
    "drain_once",
    "enqueue",
    "handle_one",
    "mark_published",
    "pending_count",
    "process_once",
    "record_failure",
    "run",
    "run_relay",
]

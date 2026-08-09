"""A circuit breaker: three states, two counters, no dependency.

A slow dependency is worse than a dead one. It consumes the caller's timeout on
every call while still appearing available, so a fetch across a constellation of
tens turns a ten-second upstream stall into minutes of apparent hang — and the
caller has a perfectly good fallback it is not reaching.

The breaker makes the second failure cheap and the hundredth free.

WHY THIS IS HAND-ROLLED. `pybreaker` buys a state machine this needs about a
third of, and costs a dependency in the service with the longest dependency list
in the repository, plus its own failure semantics to learn — which is the kind
of thing configured wrongly once and believed forever. The policy below is three
states and two counters; written out, it is visible in review rather than in
somebody else's README.
"""

from __future__ import annotations

import threading
import time
from collections.abc import Callable
from enum import Enum
from typing import TypeVar

T = TypeVar("T")


class State(Enum):
    """Breaker state, numbered so it can be a gauge.

    #51 asks for state as a METRIC rather than a log line: "why did latency drop
    while errors rose" is only answerable if this is on a dashboard.
    """

    CLOSED = 0
    OPEN = 1
    HALF_OPEN = 2


class BreakerOpenError(Exception):
    """Raised instead of calling a dependency the breaker has given up on.

    A distinct type, not the dependency's own error: the caller's fallback wants
    to know it was refused rather than that the upstream failed again, and an
    operator reading logs wants the difference to be obvious.
    """


class Breaker:
    """Trips after `threshold` CONSECUTIVE failures; probes after `cooldown_s`.

    Consecutive, not a rolling error rate. The callers here are a startup fetch
    and a periodic sweep — a handful of calls an hour — and a rate computed over
    that is noise wearing a statistic's clothes.
    """

    def __init__(
        self,
        *,
        threshold: int = 3,
        cooldown_s: float = 30.0,
        now: Callable[[], float] = time.monotonic,
    ) -> None:
        if threshold < 1:
            detail = f"threshold must be at least 1, got {threshold}"
            raise ValueError(detail)
        self._threshold = threshold
        self._cooldown_s = cooldown_s
        self._now = now
        self._lock = threading.Lock()
        self._failures = 0
        self._opened_at = 0.0
        self._open = False

    @property
    def state(self) -> State:
        """The state right now — reading it never probes.

        A breaker that half-opened on inspection would send a live request every
        time Prometheus scraped it, which is a health check nobody asked for
        against a dependency already known to be failing.
        """
        with self._lock:
            return self._state_locked()

    def _state_locked(self) -> State:
        if not self._open:
            return State.CLOSED
        if self._now() - self._opened_at >= self._cooldown_s:
            return State.HALF_OPEN
        return State.OPEN

    def call(self, fn: Callable[[], T]) -> T:
        """Run `fn`, or refuse outright while the breaker is open."""
        with self._lock:
            if self._state_locked() is State.OPEN:
                raise BreakerOpenError(
                    f"refusing the call: {self._failures} consecutive failures, "
                    f"retrying in {self._cooldown_s - (self._now() - self._opened_at):.1f}s"
                )

        # Outside the lock. Holding it across the call would serialise every
        # caller behind the slow dependency this exists to protect them from.
        try:
            result = fn()
        except Exception:
            self._record_failure()
            raise
        self._record_success()
        return result

    def _record_failure(self) -> None:
        with self._lock:
            self._failures += 1
            # A failed half-open probe restarts the FULL cooldown from now, not
            # from the original trip. Otherwise the breaker lets a call through
            # on every subsequent check while the upstream is still down.
            if self._open or self._failures >= self._threshold:
                self._open = True
                self._opened_at = self._now()

    def _record_success(self) -> None:
        with self._lock:
            self._failures = 0
            self._open = False

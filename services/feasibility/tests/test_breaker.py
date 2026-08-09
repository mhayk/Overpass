"""The circuit breaker: three states, two counters, no library.

A slow dependency is worse than a dead one — it holds the caller for the full
timeout on every call while still looking available. The breaker turns the
second, third and hundredth of those into an immediate refusal, so the caller
reaches its fallback promptly instead of ten seconds late per satellite.
"""

from __future__ import annotations

import pytest

from feasibility.resilience import Breaker, BreakerOpenError, State


class BoomError(Exception):
    """A dependency failure, distinct from the breaker's own refusal."""


def failing() -> str:
    raise BoomError


def succeeding() -> str:
    return "ok"


def test_a_closed_breaker_passes_calls_through() -> None:
    breaker = Breaker(threshold=3, cooldown_s=60)

    assert breaker.call(succeeding) == "ok"
    assert breaker.state is State.CLOSED


def test_failures_below_the_threshold_leave_it_closed() -> None:
    """One blip is not an outage. Opening on the first failure would make a
    single lost packet look like a dead upstream."""
    breaker = Breaker(threshold=3, cooldown_s=60)

    for _ in range(2):
        with pytest.raises(BoomError):
            breaker.call(failing)

    assert breaker.state is State.CLOSED


def test_the_threshold_opens_it_and_the_next_call_fails_fast() -> None:
    breaker = Breaker(threshold=3, cooldown_s=60)

    for _ in range(3):
        with pytest.raises(BoomError):
            breaker.call(failing)
    assert breaker.state is State.OPEN

    # The point of the whole thing: the dependency is not called at all.
    calls = {"n": 0}

    def counted() -> str:
        calls["n"] += 1
        return "ok"

    with pytest.raises(BreakerOpenError):
        breaker.call(counted)
    assert calls["n"] == 0, "an open breaker called the dependency it is protecting"


def test_a_success_resets_the_count() -> None:
    """CONSECUTIVE failures, not cumulative. A breaker that never forgets opens
    eventually on any dependency, however healthy."""
    breaker = Breaker(threshold=3, cooldown_s=60)

    for _ in range(2):
        with pytest.raises(BoomError):
            breaker.call(failing)
    breaker.call(succeeding)
    with pytest.raises(BoomError):
        breaker.call(failing)

    assert breaker.state is State.CLOSED


def test_after_the_cooldown_it_half_opens_and_one_success_closes_it() -> None:
    clock = {"now": 1000.0}
    breaker = Breaker(threshold=1, cooldown_s=30, now=lambda: clock["now"])

    with pytest.raises(BoomError):
        breaker.call(failing)
    tripped = breaker.state
    assert tripped is State.OPEN

    clock["now"] += 31
    # Reading the state is not what probes it; the call is. A breaker that
    # half-opens on inspection would reopen every time a metric is scraped.
    #
    # Each reading goes into its own name: `breaker.state` is a property over
    # mutable state, and asserting on the expression twice lets a type checker
    # narrow the second check into something it thinks cannot happen.
    cooled = breaker.state
    assert cooled is State.HALF_OPEN
    assert breaker.call(succeeding) == "ok"
    closed = breaker.state
    assert closed is State.CLOSED


def test_a_failed_probe_opens_it_again_for_a_full_cooldown() -> None:
    """The half-open probe is one call, not a window. Letting traffic through
    while the upstream is still down is how a breaker becomes a thundering
    herd with extra steps."""
    clock = {"now": 1000.0}
    breaker = Breaker(threshold=1, cooldown_s=30, now=lambda: clock["now"])

    with pytest.raises(BoomError):
        breaker.call(failing)
    clock["now"] += 31
    with pytest.raises(BoomError):
        breaker.call(failing)

    reopened = breaker.state
    assert reopened is State.OPEN
    clock["now"] += 29
    still_open = breaker.state
    assert still_open is State.OPEN, (
        "the cooldown restarted from the first failure rather than from the failed probe"
    )


def test_the_state_is_readable_as_a_number_for_a_gauge() -> None:
    """#51 asks for breaker state as a metric, not a log line: "why did latency
    drop while errors rose" is only answerable if this is on a dashboard."""
    breaker = Breaker(threshold=1, cooldown_s=30)

    assert State.CLOSED.value == 0
    assert State.OPEN.value == 1
    assert State.HALF_OPEN.value == 2
    assert breaker.state.value == 0

"""The sweeper's retry interval when the constellation is not seeded yet."""

from __future__ import annotations

from feasibility.sweeper import SweepStats, retry_delay


def test_a_normal_sweep_waits_the_full_tick() -> None:
    assert retry_delay(SweepStats(satellites=9), tick_s=300.0) == 300.0


def test_an_unseeded_sweep_retries_promptly() -> None:
    """A worker that starts before `make seed` must not go quiet for five
    minutes.

    On a cold stack the first sweep finds nothing, and at the default tick the
    constellation then has no ephemeris — and overpass_tle_age_hours no series
    at all — for five minutes. That is the window in which someone is most
    likely to be watching a fresh stack, and the TLE staleness alert is blind
    for the whole of it.
    """
    assert retry_delay(SweepStats(satellites=0), tick_s=300.0) == 10.0


def test_a_failed_sweep_is_treated_as_unseeded() -> None:
    """The loop resets stats before the handler, so a sweep that raised — the
    database still reconnecting, most often — also retries promptly rather than
    inheriting the previous tick's satellite count."""
    assert retry_delay(SweepStats(), tick_s=300.0) == 10.0

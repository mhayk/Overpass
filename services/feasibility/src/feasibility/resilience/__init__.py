"""Resilience primitives: things that keep one slow dependency from becoming an
outage everywhere else."""

from feasibility.resilience.breaker import Breaker, BreakerOpenError, State

__all__ = ["Breaker", "BreakerOpenError", "State"]

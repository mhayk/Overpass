"""Make a skipped integration test loud where it is supposed to run.

Four test modules in this suite guard themselves on `OVERPASS_TEST_DSN` and
`OVERPASS_TEST_NATS` — the idempotency ledger, the outbox relay, the worker's
ack ordering, and the ephemeris sweep. Those are precisely the four things in
this service whose correctness is *about* a database and a broker, and for as
long as the python CI job had neither, all four skipped and the job was green.

A skip is not a pass, and pytest's exit code cannot tell them apart. So the
environment gets a gate of its own: set `OVERPASS_REQUIRE_INTEGRATION=1` — as CI
does — and a missing variable is a hard error at session start rather than
twelve quiet skips nobody reads.

Deliberately a check on the ENVIRONMENT rather than on the outcome. Counting
passed tests would drift every time somebody adds one; asserting that the
services are configured catches the thing that actually broke, which is a CI job
that stopped providing them.
"""

from __future__ import annotations

import os

import pytest

# Set by CI. Absent on a laptop, where skipping is the correct and convenient
# behaviour — `uv run pytest` with no stack running should still work.
_REQUIRE = "OVERPASS_REQUIRE_INTEGRATION"

_REQUIRED_WHEN_ENFORCED = {
    "OVERPASS_TEST_DSN": (
        "a migrated Postgres. Without it test_idempotency, test_relay, test_worker "
        "and test_ephemeris_sweep all skip."
    ),
    "OVERPASS_TEST_NATS": (
        "a NATS server with JetStream and the durable consumers from "
        "deploy/nats/init.sh. Without it test_relay and test_worker skip. Note the "
        "name: OVERPASS_TEST_NATS_URL is a different variable and does not work."
    ),
}


def pytest_configure(config: pytest.Config) -> None:
    """Refuse to run at all if enforcement is on and the environment is not.

    At configure time rather than in a fixture, so the failure arrives before
    any test does and reads as "this run was not going to prove anything"
    instead of as a broken test.
    """
    if not os.environ.get(_REQUIRE):
        return

    missing = [name for name in _REQUIRED_WHEN_ENFORCED if not os.environ.get(name)]
    if not missing:
        return

    detail = "\n".join(f"  {name}: needs {_REQUIRED_WHEN_ENFORCED[name]}" for name in missing)
    raise pytest.UsageError(
        f"{_REQUIRE} is set, so the integration-backed tests must run, but "
        f"{len(missing)} variable(s) are missing:\n{detail}\n"
        "Either provide the services or unset "
        f"{_REQUIRE} and accept that this run proves less."
    )

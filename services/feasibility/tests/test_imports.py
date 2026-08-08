"""Every module must import first.

A cheap test for a failure that is invisible in a normal test run and fatal in a
process.

Python resolves a circular import by handing out a PARTIALLY INITIALISED module,
so whether a cycle raises depends entirely on which module the interpreter
reached first. pytest imports test files alphabetically, so the suite happens to
pull `feasibility.messaging` in early and the cycle never fires — while
`python -c "import feasibility.failures"` fails outright.

That is exactly what #132 shipped: `failures` imported `messaging.outbox`, which
runs `messaging/__init__`, which imports `worker`, which imported a name from
`failures` — still executing, so the name did not exist yet. Every test passed.

The fix is the module-object idiom (`from feasibility import failures`, then
`failures.build_refusal(...)`): the attribute is looked up when it is CALLED
rather than when the module is imported, by which time the cycle has closed.

This test simply imports each module first, in turn, with the package evicted
from `sys.modules` in between so the import really re-executes.
"""

from __future__ import annotations

import importlib
import sys

import pytest

# Every module a process or a test might reach for directly. A cycle can only
# be introduced between two of these, so the list is the coverage.
MODULES = [
    "feasibility.ephemeris",
    "feasibility.failures",
    "feasibility.messaging",
    "feasibility.messaging.idempotency",
    "feasibility.messaging.outbox",
    "feasibility.messaging.relay",
    "feasibility.messaging.worker",
    "feasibility.orbit",
    "feasibility.orbit.ephemeris",
    "feasibility.pipeline",
    "feasibility.sar",
    "feasibility.sweeper",
    "feasibility.telemetry",
    "feasibility.tle.store",
]


@pytest.mark.parametrize("module", MODULES)
def test_the_package_imports_cleanly_when_this_module_is_reached_first(module: str) -> None:
    for name in [n for n in sys.modules if n == "feasibility" or n.startswith("feasibility.")]:
        del sys.modules[name]

    try:
        importlib.import_module(module)
    finally:
        # Leave the interpreter as it was found. A half-evicted package would
        # make whichever test runs next fail somewhere unrelated, which is the
        # same class of confusion this file exists to prevent.
        for name in [n for n in sys.modules if n == "feasibility" or n.startswith("feasibility.")]:
            del sys.modules[name]
        importlib.import_module("feasibility.messaging")

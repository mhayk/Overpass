"""Round-trip the contract fixtures through the generated Pydantic models.

The Python counterpart of ``gen/go/contracttest/roundtrip_test.go``. Together
they check the claim the whole contracts-first approach rests on: that a
payload the JSON Schema accepts is a payload **both** language bindings can
hold, and one it rejects is one they refuse.

That claim is not free. The generators were written by different people, in
different languages, with different opinions about how to render a constraint.
Nothing guarantees they agree — so it is checked rather than assumed. This is
the specific place where a polyglot split would rot silently, and it is why the
check exists as a build step rather than as a note in a README.

One asymmetry is worth knowing and is asserted below rather than glossed over:

    Pydantic enforces enum membership, numeric bounds, and lengths at parse
    time. Go's encoding/json enforces only structure and undeclared fields,
    because the generator renders constrained scalars as plain named string
    and int types.

So Python rejects more of the invalid fixtures than Go does. Neither is wrong.
It means the JSON Schema — not either binding — is the authority on validity,
and services validate against it at the boundary.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "gen" / "python"))

from pydantic import ValidationError  # noqa: E402

from overpass_contracts.events import (  # noqa: E402
    feasibility_ephemeris_computed_v1_schema,
    planning_plan_committed_v1_schema,
    planning_request_unfulfilled_v1_schema,
    tasking_request_received_v1_schema,
    tasking_request_rejected_v1_schema,
)

GREEN, RED, YELLOW, DIM, RESET = "\033[32m", "\033[31m", "\033[33m", "\033[90m", "\033[0m"

MODELS = {
    "tasking.request.received.v1": tasking_request_received_v1_schema.TaskingRequestReceived,
    "tasking.request.rejected.v1": tasking_request_rejected_v1_schema.TaskingRequestRejected,
    "planning.plan.committed.v1": planning_plan_committed_v1_schema.PlanCommitted,
    "planning.request.unfulfilled.v1": (
        planning_request_unfulfilled_v1_schema.PlanningRequestUnfulfilled
    ),
    # Wired because feasibility-service PRODUCES this one, in Python. For every
    # other event here the binding only has to read; for this one a model that
    # can read but not write would emit payloads its own schema rejects, which
    # is precisely what the round-trip below is for.
    "feasibility.ephemeris.computed.v1": (
        feasibility_ephemeris_computed_v1_schema.FeasibilityEphemerisComputed
    ),
}

failures: list[str] = []
checks = 0


def ok(msg: str) -> None:
    global checks
    checks += 1
    print(f"  {GREEN}ok{RESET}   {msg}")


def fail(msg: str, detail: str = "") -> None:
    global checks
    checks += 1
    failures.append(msg)
    print(f"  {RED}FAIL{RESET} {msg}")
    if detail:
        print(f"       {DIM}{detail}{RESET}")


def fixtures(kind: str):
    base = ROOT / "contracts" / "examples" / kind
    for path in sorted(base.rglob("*.json")):
        yield path.parent.name, path, json.loads(path.read_text())


def check_valid() -> None:
    print(f"\n{YELLOW}Valid fixtures parse into the generated models{RESET}")
    for event_type, path, payload in fixtures("valid"):
        model = MODELS.get(event_type)
        rel = path.relative_to(ROOT / "contracts" / "examples")
        if model is None:
            print(f"  {DIM}skip  {rel} (no model wired){RESET}")
            continue
        try:
            parsed = model.model_validate(payload)
        except ValidationError as exc:
            fail(f"{rel} failed to parse", str(exc).splitlines()[0])
            continue

        # Round-trip: dump and re-parse. Catches a model that can read a payload
        # but cannot reproduce it — an alias that does not round-trip, or a
        # field the serialiser drops. A binding that can read but not write is
        # only half a binding, and a producer built on it emits payloads its own
        # schema rejects.
        try:
            dumped = parsed.model_dump(mode="json", by_alias=True, exclude_none=True)
            model.model_validate(dumped)
            ok(str(rel))
        except ValidationError as exc:
            fail(f"{rel} does not round-trip", str(exc).splitlines()[0])


# Which invalid fixtures the PYDANTIC BINDING can reject on its own.
#
# This table is the honest map of where the generated bindings are weaker than
# the schema they came from. Keeping it explicit means the gap cannot grow
# silently: an unclassified fixture fails the run, so every new negative case
# forces someone to decide, on purpose, what the binding is expected to catch.
#
# The two False entries are both `prefixItems`. datamodel-code-generator 0.28.5
# renders the GeoJSON Position — a 2-tuple with per-element bounds, longitude
# in [-180, 180] and latitude in [-90, 90] — as a bare `RootModel[list]` with
# only a length constraint. The element bounds are dropped entirely, so a
# longitude of 200.5, or a swapped lat/lon pair, parses cleanly.
#
# The conclusion is the architectural one, and it is the same one the Go test
# reaches from a different direction: THE JSON SCHEMA IS THE AUTHORITY ON
# VALIDITY. The generated types are a convenience for handling data already
# known to be valid. Services validate against the schema at the boundary —
# inbound on consume, outbound before publish — and never treat "it parsed into
# the model" as "it is valid".
PYDANTIC_REJECTS = {
    "missing-required-data.json": True,          # required
    "wrong-event-type.json": True,               # Literal discriminator
    "unknown-tier.json": True,                   # Literal enum
    "empty-requested-modes.json": True,          # min_length
    "undeclared-extra-field.json": True,         # extra='forbid'
    "plan-version-zero.json": True,              # ge=1
    "longitude-out-of-range.json": False,        # prefixItems bounds dropped
    "latitude-longitude-swapped.json": False,    # prefixItems bounds dropped
    "sample-missing-altitude.json": True,        # min_length on the sample tuple
}


def check_invalid() -> None:
    print(f"\n{YELLOW}Invalid fixtures, against the declared binding strength{RESET}")
    for event_type, path, payload in fixtures("invalid"):
        model = MODELS.get(event_type)
        rel = path.relative_to(ROOT / "contracts" / "examples")
        if model is None:
            print(f"  {DIM}skip  {rel} (no model wired){RESET}")
            continue

        expected = PYDANTIC_REJECTS.get(path.name)
        if expected is None:
            fail(
                f"{rel} is unclassified",
                "add it to PYDANTIC_REJECTS and state whether the binding catches it",
            )
            continue

        try:
            model.model_validate(payload)
        except ValidationError as exc:
            first = exc.errors()[0]
            loc = "/".join(map(str, first["loc"]))
            if expected:
                ok(f"{rel} {DIM}({first['type']} at {loc}){RESET}")
            else:
                # Not a failure — the binding got stricter, which is good news.
                # It does mean the table is now stale, so say so loudly enough
                # to be fixed rather than quietly tolerated.
                ok(f"{rel} {DIM}(now rejected by the binding — tighten the table){RESET}")
        else:
            if expected:
                fail(
                    f"{rel} was accepted",
                    "the generated model is looser than the schema it came from",
                )
            else:
                ok(
                    f"{rel} {DIM}(accepted by the binding as documented — "
                    f"schema validation catches it){RESET}"
                )


def main() -> int:
    check_valid()
    check_invalid()
    print()
    if failures:
        print(f"{RED}{len(failures)} of {checks} checks failed{RESET}")
        return 1
    print(f"{GREEN}all {checks} checks passed{RESET}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

"""Validate the Overpass contracts.

Run via ``scripts/contracts-validate.sh`` (or ``make contracts-validate``),
which supplies the dependencies through ``uv run``.

Four checks, in order of how much they are worth:

1. Every schema is a structurally valid JSON Schema 2020-12 document. Catches
   typos like ``minimun`` that would otherwise be silently ignored — an unknown
   keyword is not an error in JSON Schema, it is simply not applied, which is
   the single easiest way to ship a constraint that does nothing.
2. Every ``examples`` entry validates against its own schema. An example that
   does not validate means nobody has ever actually tried to use the schema.
3. Positive fixtures validate; **negative fixtures must fail**. A schema that
   accepts everything passes every positive test ever written, so the negative
   fixtures are the ones carrying real information.
4. The OpenAPI document is valid OpenAPI 3.0.3.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import yaml
from jsonschema import Draft202012Validator, FormatChecker
from openapi_spec_validator import validate as validate_openapi
from referencing import Registry, Resource

ROOT = Path(__file__).resolve().parent.parent
CONTRACTS = ROOT / "contracts"

GREEN, RED, YELLOW, DIM, RESET = "\033[32m", "\033[31m", "\033[33m", "\033[90m", "\033[0m"

failures: list[str] = []
checks = 0


def ok(message: str) -> None:
    global checks
    checks += 1
    print(f"  {GREEN}ok{RESET}   {message}")


def fail(message: str, detail: str = "") -> None:
    global checks
    checks += 1
    failures.append(message)
    print(f"  {RED}FAIL{RESET} {message}")
    if detail:
        for line in detail.splitlines():
            print(f"       {DIM}{line}{RESET}")


def validator_for(schema: dict, registry: Registry) -> Draft202012Validator:
    """Build a validator anchored at the schema's own ``$id``.

    Subtle and worth spelling out. The schemas cross-reference each other with
    *relative* paths (``../common/envelope.v1.schema.json#/$defs/EventId``), and
    a relative ref is only meaningful relative to a base URI. Handing the raw
    dict to ``Draft202012Validator`` leaves the base empty, so the ref stays
    relative and fails to resolve.

    Validating against ``{"$ref": <the schema's absolute $id>}`` instead makes
    the registry resolve the document first. Everything inside it then resolves
    against that document's own ``$id``, which is exactly what the relative refs
    were written against.
    """
    return Draft202012Validator(
        {"$ref": schema["$id"]},
        registry=registry,
        # Turn `format` from an annotation into an ASSERTION.
        #
        # JSON Schema treats `format` as advisory by default: `"format": "uuid"`
        # documents intent and enforces nothing. That is why these schemas
        # originally carried a redundant `pattern` alongside it — which then
        # broke the Python generator, because a string pattern cannot be applied
        # to the UUID type it produces.
        #
        # Asserting `format` here is the correct fix: one rule, stated once,
        # actually enforced. `date-time` assertion additionally requires the
        # rfc3339-validator package, which the runner installs.
        format_checker=FormatChecker(),
    )


def build_registry() -> Registry:
    """Resolve $refs across files by registering every schema under its $id.

    Every schema declares an absolute ``$id`` and refers to its siblings by
    relative path. That combination is deliberate and is the only one both
    toolchains accept:

    - ``go-jsonschema`` resolves refs as filesystem paths. Given an absolute
      URI it tries to HTTP-fetch it, which fails.
    - Python's ``referencing`` resolves a relative ref against the document's
      ``$id``, producing the absolute URI it already has registered.

    Absolute ``$id`` + relative ``$ref`` satisfies both. Absolute refs satisfy
    only Python; omitting ``$id`` entirely would lose the stable identity that
    versioning depends on.
    """
    resources = []
    for path in sorted(CONTRACTS.rglob("*.schema.json")):
        schema = json.loads(path.read_text())
        if "$id" not in schema:
            fail(f"{path.relative_to(ROOT)} has no $id")
            continue
        resources.append((schema["$id"], Resource.from_contents(schema)))
    return Registry().with_resources(resources)


def check_schemas_are_valid(registry: Registry) -> None:
    print(f"\n{YELLOW}Schemas are valid JSON Schema 2020-12{RESET}")
    for path in sorted(CONTRACTS.rglob("*.schema.json")):
        schema = json.loads(path.read_text())
        try:
            Draft202012Validator.check_schema(schema)
            ok(str(path.relative_to(CONTRACTS)))
        except Exception as exc:  # noqa: BLE001 - report, do not abort the run
            fail(str(path.relative_to(CONTRACTS)), str(exc))


def check_examples(registry: Registry) -> None:
    print(f"\n{YELLOW}Embedded examples validate against their own schema{RESET}")
    for path in sorted((CONTRACTS / "events").glob("*.schema.json")):
        schema = json.loads(path.read_text())
        examples = schema.get("examples", [])
        name = path.name.removesuffix(".schema.json")
        if not examples:
            fail(f"{name} has no examples", "every event schema must carry at least one")
            continue
        validator = validator_for(schema, registry)
        for index, example in enumerate(examples):
            errors = sorted(validator.iter_errors(example), key=lambda e: list(e.path))
            if errors:
                detail = "\n".join(f"/{'/'.join(map(str, e.path))}: {e.message}" for e in errors)
                fail(f"{name} example[{index}]", detail)
            else:
                ok(f"{name} example[{index}]")


def check_published_events(registry: Registry) -> None:
    """Validate the bytes a SERVICE actually publishes.

    Layout: services/<svc>/testdata/published-events/<event-type>/<case>.json,
    written by that service's own tests from its real event builder.

    This check exists because of #124. tasking-api published a hand-written map
    with no envelope and no target — nine violations — and it compiled, vetted,
    linted and passed every Go test, because every test built the payload with
    the same helper it asserted against. Go's type system cannot enforce a
    contract; only the schema can, and it had never been shown what the producer
    emits.

    Fixtures under contracts/examples/ are hand-written illustrations of what a
    valid event looks like. These are the opposite: machine-written records of
    what a producer really does.
    """
    print(f"\n{YELLOW}Published events (what services actually emit){RESET}")

    validators: dict[str, Draft202012Validator] = {}
    for path in sorted((CONTRACTS / "events").glob("*.schema.json")):
        schema = json.loads(path.read_text())
        validators[path.name.removesuffix(".schema.json")] = validator_for(schema, registry)

    found = False
    for fixture in sorted(ROOT.glob("services/*/testdata/published-events/*/*.json")):
        found = True
        event = fixture.parent.name
        rel = fixture.relative_to(ROOT)
        validator = validators.get(event)
        if validator is None:
            fail(f"{rel}", f"no schema named {event}.schema.json")
            continue
        errors = sorted(
            validator.iter_errors(json.loads(fixture.read_text())),
            key=lambda e: (list(map(str, e.path)), e.message),
        )
        if errors:
            detail = "\n".join(f"/{'/'.join(map(str, e.path))}: {e.message}" for e in errors)
            fail(f"{rel} is not a valid {event}", detail)
        else:
            ok(str(rel))

    if not found:
        # Not a failure yet: only tasking-api emits events today. It becomes one
        # the moment a service ships a producer without this evidence.
        print(f"  {DIM}none found — no service records what it publishes yet{RESET}")


def check_fixtures(registry: Registry) -> None:
    """Positive fixtures must validate; negative fixtures must not.

    Layout: contracts/examples/<valid|invalid>/<event-name>/<case>.json
    """
    print(f"\n{YELLOW}Fixtures{RESET}")
    examples_dir = CONTRACTS / "examples"
    if not examples_dir.exists():
        print(f"  {DIM}skip  no contracts/examples/ yet{RESET}")
        return

    validators: dict[str, Draft202012Validator] = {}
    for path in sorted((CONTRACTS / "events").glob("*.schema.json")):
        schema = json.loads(path.read_text())
        validators[path.name.removesuffix(".schema.json")] = validator_for(schema, registry)

    found = False
    for expectation in ("valid", "invalid"):
        for fixture in sorted((examples_dir / expectation).rglob("*.json")):
            found = True
            event = fixture.parent.name
            rel = fixture.relative_to(examples_dir)
            validator = validators.get(event)
            if validator is None:
                fail(f"{rel}", f"no schema named {event}.schema.json")
                continue
            # Sorted by JSON path, not by the ValidationError itself.
            #
            # jsonschema's ValidationError defines no total ordering, so a bare
            # sorted() raises TypeError the moment a fixture produces TWO
            # errors. Every fixture happened to produce at most one until the
            # planner examples were added, so the crash sat here unnoticed —
            # and it crashes the whole run rather than failing one fixture,
            # which would have looked like a broken script rather than a broken
            # contract.
            errors = sorted(
                validator.iter_errors(json.loads(fixture.read_text())),
                key=lambda e: (list(map(str, e.path)), e.message),
            )
            if expectation == "valid":
                if errors:
                    detail = "\n".join(
                        f"/{'/'.join(map(str, e.path))}: {e.message}" for e in errors
                    )
                    fail(f"{rel} should validate", detail)
                else:
                    ok(str(rel))
            else:
                # A negative fixture that validates means the schema is not
                # actually constraining what the fixture was written to test.
                if errors:
                    ok(f"{rel} {DIM}(correctly rejected: {errors[0].message[:60]}){RESET}")
                else:
                    fail(f"{rel} should have been rejected", "the schema accepted it")

    if not found:
        print(f"  {DIM}skip  contracts/examples/ is empty{RESET}")


def check_openapi() -> None:
    print(f"\n{YELLOW}OpenAPI documents{RESET}")
    specs = sorted((CONTRACTS / "openapi").glob("*.yaml"))
    if not specs:
        print(f"  {DIM}skip  no OpenAPI documents yet{RESET}")
        return
    for path in specs:
        try:
            spec = yaml.safe_load(path.read_text())
            validate_openapi(spec)
            operations = sum(
                1
                for item in spec.get("paths", {}).values()
                for method in item
                if method in ("get", "post", "put", "patch", "delete")
            )
            ok(
                f"{path.name} {DIM}({len(spec.get('paths', {}))} paths, "
                f"{operations} operations, "
                f"{len(spec.get('components', {}).get('schemas', {}))} schemas){RESET}"
            )
        except Exception as exc:  # noqa: BLE001
            fail(path.name, str(exc).splitlines()[0])


def main() -> int:
    registry = build_registry()
    check_schemas_are_valid(registry)
    check_examples(registry)
    check_fixtures(registry)
    check_published_events(registry)
    check_openapi()

    print()
    if failures:
        print(f"{RED}{len(failures)} of {checks} checks failed{RESET}")
        return 1
    print(f"{GREEN}all {checks} checks passed{RESET}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

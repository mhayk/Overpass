#!/usr/bin/env bash
#
# Validate every contract artifact:
#
#   1. Each event schema is itself a valid JSON Schema 2020-12 document.
#   2. Each schema's `examples` entries validate against that schema.
#   3. Each fixture under contracts/examples/ validates (or, for the invalid/
#      fixtures, correctly FAILS to validate).
#   4. The OpenAPI document is a valid OpenAPI 3.0.3 document.
#
# Point 3 matters more than it looks. A schema that accepts everything passes
# every positive test ever written; the invalid/ fixtures are what prove the
# schema actually rejects things.
#
# Dependencies are declared inline (PEP 723) and resolved by `uv run`, so this
# needs no virtualenv setup and no requirements file to drift.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if ! command -v uv >/dev/null 2>&1; then
  echo "error: uv is required (https://docs.astral.sh/uv/). brew install uv" >&2
  exit 1
fi

exec uv run --quiet --no-project \
  --with jsonschema==4.23.0 \
  --with referencing==0.35.1 \
  --with openapi-spec-validator==0.7.1 \
  --with pyyaml==6.0.2 \
  --with rfc3339-validator==0.1.4 \
  python scripts/contracts_validate.py "$@"

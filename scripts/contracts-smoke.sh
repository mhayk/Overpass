#!/usr/bin/env bash
#
# Round-trip the contract fixtures through the generated Pydantic models.
# The Python counterpart of gen/go/contracttest/roundtrip_test.go.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
# Do not litter gen/ with bytecode; it is gitignored but the drift check
# compares the working tree.
export PYTHONDONTWRITEBYTECODE=1

exec uv run --quiet --no-project --python 3.12 \
  --with pydantic==2.9.2 \
  python scripts/contracts_smoke.py "$@"

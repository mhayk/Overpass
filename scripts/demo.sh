#!/usr/bin/env bash
#
# Submit a scripted, deliberately contested set of tasking requests.
#
# Standard library only — no `--with` at all. The demo is the last thing a
# reviewer runs and the worst possible moment to discover a dependency
# resolution problem, so it depends on nothing but python itself.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if ! command -v uv >/dev/null 2>&1; then
  echo "error: uv is required (https://docs.astral.sh/uv/). brew install uv" >&2
  exit 1
fi

# The demo drives the running system; it does not start one. `make up-all`
# brings the application services (#166), `make seed` gives the sweep a
# constellation. Checked here because "connection refused" twenty lines into
# a demo is a worse first impression than one line naming the fix.
if ! curl -sf "${TASKING_API_URL:-http://localhost:8080}/readyz" >/dev/null 2>&1; then
  echo "error: tasking-api is not answering on ${TASKING_API_URL:-http://localhost:8080}." >&2
  echo "       run 'make up-all' (and 'make seed' once) first." >&2
  exit 1
fi

exec uv run --quiet --no-project python scripts/demo.py "$@"

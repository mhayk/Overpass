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

exec uv run --quiet --no-project python scripts/demo.py "$@"

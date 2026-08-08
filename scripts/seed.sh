#!/usr/bin/env bash
#
# Seed the constellation, sensor modes and sample customers.
#
# Offline: the element sets come from the frozen snapshot in testdata/tle/, not
# from Celestrak. ADR-0011 splits this deliberately — live TLEs are a nice
# property for a deployment and a liability for a demo, because a reviewer with
# no signal gets a broken clone.
#
# Dependencies are declared inline and resolved by `uv run`, matching
# contracts-validate.sh, so this needs no virtualenv and no requirements file
# to drift.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if ! command -v uv >/dev/null 2>&1; then
  echo "error: uv is required (https://docs.astral.sh/uv/). brew install uv" >&2
  exit 1
fi

exec uv run --quiet --no-project \
  --with 'psycopg[binary]==3.2.3' \
  python scripts/seed.py "$@"

#!/usr/bin/env bash
#
# Assert every committed Grafana panel can actually render against the running
# Prometheus.
#
# Standard library only, like the demo — the last thing anyone wants when a
# gate fails is to discover the gate itself has a dependency problem.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PROMETHEUS_URL="${PROMETHEUS_URL:-http://localhost:9090}"

if ! curl -sf "${PROMETHEUS_URL}/-/healthy" >/dev/null 2>&1; then
  echo "error: Prometheus is not answering on ${PROMETHEUS_URL}." >&2
  echo "       run 'make up' first." >&2
  exit 1
fi

exec python3 scripts/dashboards_check.py "$@"

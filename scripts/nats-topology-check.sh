#!/usr/bin/env bash
#
# Verify that every declared consumer actually receives the subjects it claims.
#
# This exists because of a defect that nothing else could have caught. The
# topology script passed `--filter "planning.>,acquisition.>"` to `nats consumer
# add`, which accepts it silently and creates a consumer filtering on the
# literal subject "planning.>,acquisition.>". That matches nothing. The consumer
# existed, reported healthy, showed zero errors, and delivered zero messages —
# and `nats consumer info` printed the comma-joined filter back without
# complaint, so reading the output confirmed nothing.
#
# The only check that finds this is publishing a real message and requiring it
# to arrive. That is what this does.
#
# Usage: scripts/nats-topology-check.sh [nats-url]
set -euo pipefail

NATS_URL="${1:-${NATS_URL:-nats://127.0.0.1:4222}}"
NATS_BOX_IMAGE="${NATS_BOX_IMAGE:-natsio/nats-box:0.14.3}"

# Run the CLI in a container unless one is already on PATH, so this works the
# same on a laptop and in CI.
if command -v nats >/dev/null 2>&1; then
  nats_cli() { nats --server "$NATS_URL" "$@"; }
else
  nats_cli() { docker run --rm --network host "$NATS_BOX_IMAGE" nats --server "$NATS_URL" "$@"; }
fi

fail=0
note() { printf '  %s\n' "$*"; }

# consumer:subject pairs. One subject per consumer is enough to prove the filter
# resolves; the multi-filter consumers get an entry per subject because the whole
# point is that the SECOND one is the one that silently disappears.
CASES=(
  "TASKING:gateway-projector-tasking:tasking.request.received.v1"
  "FEASIBILITY:gateway-projector-feasibility:feasibility.opportunities.computed.v1"
  "FEASIBILITY:gateway-projector-feasibility:feasibility.ephemeris.computed.v1"
  "PLANNING:gateway-projector-planning:planning.plan.committed.v1"
  "PLANNING:gateway-projector-planning:acquisition.executed.v1"
  "TASKING:feasibility-worker:tasking.request.received.v1"
  "FEASIBILITY:planner-opportunities:feasibility.opportunities.computed.v1"
  "TASKING:planner-lifecycle:tasking.request.received.v1"
  "PLANNING:simulator-executor:planning.plan.committed.v1"
)

echo "== nats topology check =="
echo "   every consumer must actually receive the subjects it filters on"
echo "   broker: $NATS_URL"
echo

for entry in "${CASES[@]}"; do
  IFS=':' read -r stream consumer subject <<<"$entry"

  before=$(nats_cli consumer info "$stream" "$consumer" -j 2>/dev/null \
    | tr ',' '\n' | grep -o '"num_pending": *[0-9]*' | grep -o '[0-9]*' | head -1 || true)
  if [[ -z "$before" ]]; then
    printf '%s/%s\n' "$stream" "$consumer"
    note "FAIL consumer does not exist"
    fail=1
    continue
  fi

  nats_cli pub "$subject" '{"topology_check":true}' >/dev/null 2>&1

  after=$(nats_cli consumer info "$stream" "$consumer" -j 2>/dev/null \
    | tr ',' '\n' | grep -o '"num_pending": *[0-9]*' | grep -o '[0-9]*' | head -1 || true)

  printf '%s/%s <- %s\n' "$stream" "$consumer" "$subject"
  if [[ -z "$after" ]] || [[ "$after" -le "$before" ]]; then
    note "FAIL pending stayed at $before after publishing; this filter matches nothing"
    note "     a comma-joined --filter is accepted and silently matches no subject"
    fail=1
  else
    note "ok   pending $before -> $after"
  fi
done

echo
if [[ "$fail" -ne 0 ]]; then
  echo "topology check FAILED: at least one consumer receives nothing it claims to"
  exit 1
fi
echo "topology check passed: every consumer received a message on every subject it filters"

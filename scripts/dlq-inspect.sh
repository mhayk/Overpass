#!/usr/bin/env bash
#
# Show what is in the dead-letter queues, and why.
#
# The first half of the loop ADR-0017 defines: alert on depth, INSPECT, fix,
# replay, watch depth return to zero. With no arguments it answers the only
# question an alert asks — how much outstanding work is there — and with a
# stream it prints one row per dead letter, from the headers
# contracts/nats/topology.md specifies.
#
# A script over nats-box rather than a service. The DLQ is read during an
# incident, by a human, a handful of times a year; a service would be a
# deployment, a health check and a dashboard tile for something a shell loop
# does in ten lines.
#
# Usage:
#   scripts/dlq-inspect.sh                      # depth of all three
#   scripts/dlq-inspect.sh DLQ_TASKING          # one row per dead letter
#   DLQ_LIMIT=100 scripts/dlq-inspect.sh DLQ_TASKING
set -euo pipefail

STREAM="${1:-${STREAM:-}}"
NATS_URL="${2:-${NATS_URL:-nats://127.0.0.1:4222}}"
NATS_BOX_IMAGE="${NATS_BOX_IMAGE:-natsio/nats-box:0.14.3}"
# Newest first, bounded. Each message is one CLI call, which is one container
# when the CLI is not on PATH — unbounded here would mean an operator waiting
# minutes on a queue whose first ten rows already told them what broke.
LIMIT="${DLQ_LIMIT:-20}"

DLQ_STREAMS=(DLQ_TASKING DLQ_FEASIBILITY DLQ_PLANNING)

# Run the CLI in a container unless one is already on PATH, so this works the
# same on a laptop and in CI.
if command -v nats >/dev/null 2>&1; then
  nats_cli() { nats --server "$NATS_URL" "$@"; }
else
  nats_cli() { docker run --rm --network host "$NATS_BOX_IMAGE" nats --server "$NATS_URL" "$@"; }
fi

# jq the same way: whatever is on PATH, otherwise the copy inside nats-box. The
# alternative — parsing this JSON with sed — is fragile code in a tool that is
# only ever run while something is already going wrong.
if command -v jq >/dev/null 2>&1; then
  jq_run() { jq "$@"; }
else
  jq_run() { docker run --rm -i "$NATS_BOX_IMAGE" jq "$@"; }
fi

# depth prints the message count, or "-" for a stream that does not exist. A
# missing DLQ stream is a topology that was never applied, not an empty queue,
# and the two must not look the same.
depth() {
  local stream="$1" info
  if ! info="$(nats_cli stream info "$stream" --json 2>/dev/null)"; then
    printf '%s' "-"
    return
  fi
  printf '%s' "$info" | jq_run -r '.state.messages'
}

if [[ -z "$STREAM" ]]; then
  printf '%-18s %s\n' "STREAM" "DEPTH"
  total=0
  for s in "${DLQ_STREAMS[@]}"; do
    d="$(depth "$s")"
    printf '%-18s %s\n' "$s" "$d"
    [[ "$d" =~ ^[0-9]+$ ]] && total=$((total + d))
  done
  echo
  if [[ "$total" -eq 0 ]]; then
    echo "nothing outstanding."
  else
    echo "$total dead letter(s). Inspect one stream: scripts/dlq-inspect.sh <STREAM>"
  fi
  exit 0
fi

state="$(nats_cli stream info "$STREAM" --json)"
messages="$(printf '%s' "$state" | jq_run -r '.state.messages')"
first="$(printf '%s' "$state" | jq_run -r '.state.first_seq')"
last="$(printf '%s' "$state" | jq_run -r '.state.last_seq')"

echo "$STREAM: $messages dead letter(s), sequences $first..$last"
if [[ "$messages" -eq 0 ]]; then
  exit 0
fi

# The header block comes back as one base64 blob in the same wire format the
# broker stores: NATS/1.0 followed by CRLF-separated lines.
header_value() {
  printf '%s' "$1" | tr -d '\r' | sed -n "s/^$2: //p" | head -n 1
}

echo
printf '%-8s %-22s %-10s %-6s %-24s %s\n' "SEQ" "FAILED AT" "REASON" "TRIES" "CONSUMER" "EVENT ID"

shown=0
for ((seq = last; seq >= first; seq--)); do
  if [[ "$shown" -ge "$LIMIT" ]]; then
    break
  fi
  # A gap is a deleted message — replayed already, or aged out. Skipping it is
  # normal; failing would make a replayed queue unreadable.
  raw="$(nats_cli stream get "$STREAM" "$seq" --json 2>/dev/null)" || continue

  headers="$(printf '%s' "$raw" | jq_run -r '.hdrs // "" | @base64d')"
  printf '%-8s %-22s %-10s %-6s %-24s %s\n' \
    "$seq" \
    "$(header_value "$headers" "Overpass-Dlq-Failed-At")" \
    "$(header_value "$headers" "Overpass-Dlq-Reason")" \
    "$(header_value "$headers" "Overpass-Dlq-Delivery-Count")" \
    "$(header_value "$headers" "Overpass-Dlq-Consumer")" \
    "$(header_value "$headers" "Nats-Msg-Id")"
  shown=$((shown + 1))
done

echo
if [[ "$shown" -lt "$messages" ]]; then
  # Said out loud rather than truncated in silence: a listing that stops early
  # without saying so reads as "that is all of them".
  echo "showing $shown of $messages — raise DLQ_LIMIT to see more"
fi
cat <<'GUIDE'
Next: read the trace for one of these in Grafana (the traceparent is preserved),
fix the cause, then replay:

  make dlq-replay STREAM=<stream> EVENT_ID=<event id>

A dead letter with an empty EVENT ID had no readable id — replay it by sequence:

  make dlq-replay STREAM=<stream> SEQ=<seq>
GUIDE

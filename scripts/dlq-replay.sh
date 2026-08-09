#!/usr/bin/env bash
#
# Put one dead letter back on the subject it came from, then delete it.
#
# The second half of ADR-0017's loop. Replay is safe because every consumer is
# idempotent: the ledger arbitrates on event id, so a message that was
# half-processed before it died is absorbed as a duplicate, and one that never
# committed is processed as new. That is the payoff this repository paid the
# idempotency tax for — recovery is a routine operation, not an incident.
#
# The original Nats-Msg-Id is preserved for exactly that reason, and the
# traceparent with it: a replay that starts a fresh trace is a replay you cannot
# tie back to the failure that caused it.
#
# Deleting after a VERIFIED republish is what keeps depth meaning "outstanding
# operational work", which is what the alert fires on.
#
# Usage:
#   scripts/dlq-replay.sh DLQ_TASKING --event-id <uuid>
#   scripts/dlq-replay.sh DLQ_TASKING --seq 42        # for a dead letter with no id
set -euo pipefail

STREAM="${1:-${STREAM:-}}"
shift || true

EVENT_ID="${EVENT_ID:-}"
SEQ="${SEQ:-}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --event-id) EVENT_ID="${2:-}"; shift 2 ;;
    --seq) SEQ="${2:-}"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

NATS_URL="${NATS_URL:-nats://127.0.0.1:4222}"
NATS_BOX_IMAGE="${NATS_BOX_IMAGE:-natsio/nats-box:0.14.3}"

if [[ -z "$STREAM" || ( -z "$EVENT_ID" && -z "$SEQ" ) ]]; then
  echo "usage: $0 <DLQ_STREAM> (--event-id <uuid> | --seq <n>)" >&2
  exit 2
fi

if command -v nats >/dev/null 2>&1; then
  nats_cli() { nats --server "$NATS_URL" "$@"; }
  nats_stdin() { nats --server "$NATS_URL" "$@"; }
else
  nats_cli() { docker run --rm --network host "$NATS_BOX_IMAGE" nats --server "$NATS_URL" "$@"; }
  nats_stdin() { docker run --rm -i --network host "$NATS_BOX_IMAGE" nats --server "$NATS_URL" "$@"; }
fi

# jq the same way: whatever is on PATH, otherwise the copy inside nats-box. The
# alternative — parsing this JSON with sed — is fragile code in a tool that is
# only ever run while something is already going wrong.
if command -v jq >/dev/null 2>&1; then
  jq_run() { jq "$@"; }
else
  jq_run() { docker run --rm -i "$NATS_BOX_IMAGE" jq "$@"; }
fi

die() { echo "$*" >&2; exit 1; }

# The source stream is the DLQ stream without its prefix. That mapping is not a
# guess: deploy/nats/init.sh declares the two sets side by side, one DLQ per
# domain stream, and the subject spaces mirror each other the same way.
TARGET_STREAM="${STREAM#DLQ_}"
[[ "$TARGET_STREAM" != "$STREAM" ]] || die "$STREAM is not a DLQ stream (expected a DLQ_ prefix)"
nats_cli stream info "$TARGET_STREAM" --json >/dev/null 2>&1 ||
  die "no stream named $TARGET_STREAM — the DLQ_/source naming in deploy/nats/init.sh has changed"

header_value() {
  printf '%s' "$1" | tr -d '\r' | sed -n "s/^$2: //p" | head -n 1
}

# find_by_event_id walks newest to oldest. Unbounded on purpose: this one is
# asked for a specific message, and "I could not find it" must mean it is not
# there rather than that the search gave up early.
find_by_event_id() {
  local state first last seq raw headers
  state="$(nats_cli stream info "$STREAM" --json)"
  first="$(printf '%s' "$state" | jq_run -r '.state.first_seq')"
  last="$(printf '%s' "$state" | jq_run -r '.state.last_seq')"
  for ((seq = last; seq >= first; seq--)); do
    raw="$(nats_cli stream get "$STREAM" "$seq" --json 2>/dev/null)" || continue
    headers="$(printf '%s' "$raw" | jq_run -r '.hdrs // "" | @base64d')"
    if [[ "$(header_value "$headers" "Nats-Msg-Id")" == "$EVENT_ID" ]]; then
      printf '%s' "$seq"
      return 0
    fi
  done
  return 1
}

if [[ -z "$SEQ" ]]; then
  SEQ="$(find_by_event_id)" || die "no dead letter in $STREAM carries event id $EVENT_ID"
fi

raw="$(nats_cli stream get "$STREAM" "$SEQ" --json 2>/dev/null)" ||
  die "no message at $STREAM#$SEQ"
headers="$(printf '%s' "$raw" | jq_run -r '.hdrs // "" | @base64d')"

SUBJECT="$(header_value "$headers" "Overpass-Dlq-Original-Subject")"
MSG_ID="$(header_value "$headers" "Nats-Msg-Id")"
TRACEPARENT="$(header_value "$headers" "traceparent")"
[[ -n "$SUBJECT" ]] || die "$STREAM#$SEQ has no Overpass-Dlq-Original-Subject; there is nowhere to replay it to"

echo "replaying $STREAM#$SEQ -> $SUBJECT (event ${MSG_ID:-<none>})"

publish_args=("$SUBJECT" "--force-stdin")
[[ -n "$MSG_ID" ]] && publish_args+=("--header" "Nats-Msg-Id:$MSG_ID")
[[ -n "$TRACEPARENT" ]] && publish_args+=("--header" "traceparent:$TRACEPARENT")

# `nats pub` runs the body through Go templates — MEASURED, not assumed: a
# payload containing {{Count}} comes back as "1", via stdin as well as via the
# argument. Escaping `{{` as the template literal `{{"{{"}}` renders back to
# `{{` exactly, which was verified by round-tripping a payload containing both
# `{{Count}}` and a bare `}}` through a real broker. Without this, replaying a
# payload that happens to contain a brace pair silently corrupts it — and the
# whole promise here is byte-identical bytes.
printf '%s' "$(printf '%s' "$raw" | jq_run -r '.data // "" | @base64d')" |
  sed 's/{{/{{"{{"}}/g' |
  nats_stdin pub "${publish_args[@]}" >/dev/null

# Verify before deleting. `nats pub` is a core publish and reports success
# whether or not a stream stored the message; deleting on that alone would turn
# a topology mistake into the data loss this whole mechanism exists to prevent.
landed="$(nats_cli stream get "$TARGET_STREAM" --last-for "$SUBJECT" --json 2>/dev/null || true)"
landed_headers="$(printf '%s' "$landed" | jq_run -r '.hdrs // "" | @base64d')"
if [[ -n "$MSG_ID" ]]; then
  [[ "$(header_value "$landed_headers" "Nats-Msg-Id")" == "$MSG_ID" ]] ||
    die "the replayed message is not the newest one on $SUBJECT in $TARGET_STREAM; keeping the dead letter"
else
  [[ -n "$landed" ]] ||
    die "nothing was stored on $SUBJECT in $TARGET_STREAM; keeping the dead letter"
fi

nats_cli stream rmm "$STREAM" "$SEQ" -f >/dev/null
echo "replayed and removed. Depth is now:"
nats_cli stream info "$STREAM" --json | jq_run -r '"  " + .config.name + ": " + (.state.messages|tostring)'

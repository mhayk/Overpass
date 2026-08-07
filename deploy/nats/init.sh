#!/bin/sh
#
# Apply the NATS topology declared in contracts/nats/topology.md.
#
# Idempotent by design — it runs on every `docker compose up`. Existing streams
# are updated in place rather than recreated, because recreating a stream would
# discard messages and consumer state, which is a spectacularly bad thing to do
# on a routine restart.
set -eu

NATS_URL="${NATS_URL:-nats://nats:4222}"
export NATS_URL

say() { printf '\033[0;36m%s\033[0m\n' "$*"; }
ok()  { printf '  \033[0;32m%s\033[0m\n' "$*"; }

# --- Streams ---------------------------------------------------------------
#
# Three streams split by producing domain. One stream per subject would multiply
# operational surface for no gain; one giant stream would tie unrelated
# retention and replay decisions together.
#
# `--retention limits`, NOT workqueue: several consumer groups read the same
# subjects, and workqueue deletes a message once any one consumer acks it,
# which silently breaks fan-out.

add_stream() {
  name="$1"; subjects="$2"; max_age="$3"
  if nats stream info "$name" >/dev/null 2>&1; then
    nats stream edit "$name" \
      --subjects "$subjects" \
      --max-age "$max_age" \
      --force >/dev/null
    ok "stream $name (updated)"
  else
    nats stream add "$name" \
      --subjects "$subjects" \
      --storage file \
      --retention limits \
      --discard old \
      --max-age "$max_age" \
      --max-msgs=-1 \
      --max-bytes=-1 \
      --max-msg-size=-1 \
      --dupe-window 2m \
      --replicas 1 \
      --no-allow-rollup \
      --no-deny-delete \
      --no-deny-purge \
      --defaults >/dev/null
    ok "stream $name (created)"
  fi
}

say "streams"
add_stream TASKING     "tasking.>"                  168h
add_stream FEASIBILITY "feasibility.>"              72h
add_stream PLANNING    "planning.>,acquisition.>"   168h

# Dead-letter streams. A message that exhausts max-deliver is republished here
# with its original payload and trace context, then acked on the source stream —
# so a poison message stops consuming a consumer slot forever and becomes a
# documented, replayable operational task instead.
# NOTE ON SUBJECT LAYOUT: dead letters live under a `dlq.` PREFIX, not a
# `.dlq.` infix. The obvious-looking `tasking.dlq.>` is already captured by the
# TASKING stream's `tasking.>` wildcard, and NATS refuses to create two streams
# whose subjects overlap:
#
#   nats: error: could not create Stream: subjects overlap with an existing
#   stream (10065)
#
# So a dead-lettered `tasking.request.received.v1` is republished to
# `dlq.tasking.request.received.v1` — the original subject, prefixed. Round-trip
# is a pure string operation in both directions, which keeps the replay tooling
# trivial.
say "dead-letter streams"
add_stream DLQ_TASKING     "dlq.tasking.>"                     720h
add_stream DLQ_FEASIBILITY "dlq.feasibility.>"                 720h
add_stream DLQ_PLANNING    "dlq.planning.>,dlq.acquisition.>"  720h

# --- Consumers -------------------------------------------------------------
#
# All durable PULL consumers with explicit ack. Push consumers would hand flow
# control to the broker; pull lets each service choose its own concurrency,
# which matters because an SGP4 sweep and a planner round have completely
# different cost profiles per message.

add_consumer() {
  stream="$1"; name="$2"; filter="$3"; wait="$4"; max_deliver="$5"; max_pending="$6"
  if nats consumer info "$stream" "$name" >/dev/null 2>&1; then
    ok "consumer $stream/$name (exists)"
    return
  fi
  # shellcheck disable=SC2086
  nats consumer add "$stream" "$name" \
    --pull \
    --deliver all \
    --ack explicit \
    --filter "$filter" \
    --wait "$wait" \
    --max-deliver "$max_deliver" \
    --max-pending "$max_pending" \
    --replay instant \
    --defaults >/dev/null
  ok "consumer $stream/$name (created)"
}

say "consumers"
# ack-wait must exceed worst-case processing time or the broker redelivers a
# message still being worked on and we do real work twice. 120s for the
# feasibility sweep is a PLACEHOLDER until #24 measures the real p99 — noted
# here and in contracts/nats/topology.md rather than presented as measured.
add_consumer TASKING     feasibility-worker    "tasking.request.received.v1"          120s 5  64
add_consumer FEASIBILITY planner-opportunities "feasibility.opportunities.computed.v1" 60s 5  32
add_consumer TASKING     planner-lifecycle     "tasking.request.>"                      30s 5  64
add_consumer PLANNING    simulator-executor    "planning.plan.committed.v1"             60s 3  16

# gateway-projector gets a higher max-deliver: it is a pure projector with no
# side effects beyond its own read model, so retrying it hard is cheap and safe.
# Consumers that publish further events get 5, because each retry there risks
# amplifying downstream work.
add_consumer TASKING     gateway-projector-tasking     "tasking.>"                 30s 10 256
add_consumer FEASIBILITY gateway-projector-feasibility "feasibility.>"             30s 10 256
add_consumer PLANNING    gateway-projector-planning    "planning.>,acquisition.>"  30s 10 256

say "topology applied"
nats stream ls

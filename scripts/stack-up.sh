#!/usr/bin/env bash
#
# Bring the stack up, wait for it to be genuinely ready, and report how long it
# took against the five-minute budget from the definition of done.
#
# Why this exists rather than plain `docker compose up -d --wait`:
#
#   `--wait` treats ANY exited container as a failure, including a one-shot
#   init container that completed successfully. nats-init applies the JetStream
#   topology and exits 0, and `--wait` reports that as a failed startup. The
#   choice is between making the init container linger pointlessly (`sleep
#   infinity`) so it looks long-running, or waiting properly. Lingering to
#   satisfy a health check is the kind of thing that later gets mistaken for a
#   real service.
#
# The timing is not decoration. "Under five minutes on a clean machine" is a
# stated requirement, and a requirement nobody measures is an aspiration.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Load .env so the endpoint summary reports the ports actually in use rather
# than the compose defaults. Compose reads .env itself for variable
# substitution; this shell does not, so without it the summary would confidently
# print the wrong URLs.
if [[ -f "$ROOT/.env" ]]; then
  set -a; . "$ROOT/.env"; set +a
fi

BUDGET_SECONDS="${BUDGET_SECONDS:-300}"

# Long-running services that must report healthy.
SERVICES=(postgres nats nats-exporter otel-collector prometheus grafana)
# One-shot containers that must complete with exit 0.
ONESHOT=(nats-init migrate)

# With the `app` profile (make up-all), the application services join the
# readiness wait. feasibility has no healthcheck — it is a consumer with no
# listener — so the "running counts as ready" branch below is what covers it.
if [[ ",${COMPOSE_PROFILES:-}," == *",app,"* ]]; then
  SERVICES+=(tasking-api feasibility planner plan-gateway web)
fi

cyan() { printf '\033[0;36m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[0;32m%-22s %s\033[0m\n' "$1" "$2"; }
bad()  { printf '  \033[0;31m%-22s %s\033[0m\n' "$1" "$2"; }

start=$(date +%s)
cyan "==> docker compose up -d"
docker compose up -d

cyan "==> waiting for health (budget ${BUDGET_SECONDS}s)"
deadline=$(( start + BUDGET_SECONDS ))
while :; do
  pending=()
  for svc in "${SERVICES[@]}"; do
    cid="$(docker compose ps -q "$svc" 2>/dev/null || true)"
    if [[ -z "$cid" ]]; then pending+=("$svc"); continue; fi
    # A service with no healthcheck reports "<no value>"; treat running as ready.
    state="$(docker inspect "$cid" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' 2>/dev/null || echo unknown)"
    [[ "$state" == "healthy" || "$state" == "running" ]] || pending+=("$svc")
  done
  [[ ${#pending[@]} -eq 0 ]] && break
  if (( $(date +%s) > deadline )); then
    bad "TIMEOUT" "still not healthy: ${pending[*]}"
    docker compose ps
    exit 1
  fi
  sleep 2
done

# One-shot containers: completion with a zero exit code is the success
# condition, not "still running".
for svc in "${ONESHOT[@]}"; do
  cid="$(docker compose ps -aq "$svc" 2>/dev/null || true)"
  if [[ -z "$cid" ]]; then bad "$svc" "never started"; exit 1; fi
  for _ in $(seq 1 60); do
    status="$(docker inspect "$cid" --format '{{.State.Status}}')"
    [[ "$status" == "exited" ]] && break
    sleep 2
  done
  code="$(docker inspect "$cid" --format '{{.State.ExitCode}}')"
  if [[ "$code" != "0" ]]; then
    bad "$svc" "exited $code"
    docker compose logs "$svc" | tail -30
    exit 1
  fi
done

elapsed=$(( $(date +%s) - start ))

cyan "==> ready in ${elapsed}s (budget ${BUDGET_SECONDS}s)"
for svc in "${SERVICES[@]}"; do
  cid="$(docker compose ps -q "$svc")"
  ok "$svc" "$(docker inspect "$cid" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}')"
done
for svc in "${ONESHOT[@]}"; do ok "$svc" "completed"; done

printf '\n'
cyan "==> endpoints"
printf '  Postgres    postgres://%s@localhost:%s/%s\n' \
  "${POSTGRES_USER:-overpass}" "${POSTGRES_PORT:-5433}" "${POSTGRES_DB:-overpass}"
printf '  NATS        nats://localhost:%s   monitor http://localhost:%s\n' \
  "${NATS_PORT:-4222}" "${NATS_MONITOR_PORT:-8222}"
printf '  Prometheus  http://localhost:%s\n' "${PROMETHEUS_PORT:-9090}"
printf '  Grafana     http://localhost:%s   (anonymous, no login)\n' "${GRAFANA_PORT:-3001}"
printf '  OTLP        grpc localhost:%s   http localhost:%s\n' \
  "${OTLP_GRPC_PORT:-4317}" "${OTLP_HTTP_PORT:-4318}"
if [[ ",${COMPOSE_PROFILES:-}," == *",app,"* ]]; then
  printf '  Tasking API http://localhost:%s\n' "${TASKING_API_PORT:-8080}"
  printf '  Gateway     http://localhost:%s\n' "${PLAN_GATEWAY_PORT:-8083}"
  printf '  Planner     http://localhost:%s\n' "${PLANNER_PORT:-8084}"
  printf '  Web         http://localhost:%s\n' "${WEB_PORT:-3000}"
fi
printf '\n'

if (( elapsed > BUDGET_SECONDS )); then
  bad "BUDGET" "exceeded by $(( elapsed - BUDGET_SECONDS ))s"
  exit 1
fi

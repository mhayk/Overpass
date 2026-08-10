#!/usr/bin/env bash
#
# Measure the frontend: frame time, time to interactive, memory over a session.
#
# Separate from scripts/e2e.sh because this takes ten minutes and answers a
# different question. The E2E suite asks whether the thing works; this asks what
# it costs. Merging them would put a ten-minute measurement in the path of every
# CI run, and the first person in a hurry would add a skip.
#
# Needs a stack WITH DATA in it — an empty read model measures an empty page.
# The run refuses rather than reporting a flattering number about nothing.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WEB_URL="${WEB_URL:-http://localhost:3000}"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8083}"
TASKING_API_URL="${TASKING_API_URL:-http://localhost:8080}"
PERF_MINUTES="${PERF_MINUTES:-10}"

PLAYWRIGHT_VERSION="$(
  node -e 'process.stdout.write(require("./web/node_modules/@playwright/test/package.json").version)' \
    2>/dev/null ||
  docker run --rm -v "$ROOT/web:/w" -w /w node:22-alpine \
    node -e 'process.stdout.write(require("@playwright/test/package.json").version)'
)"

for url in "$WEB_URL" "$GATEWAY_URL/readyz" "$TASKING_API_URL/readyz"; do
  curl -sf "$url" >/dev/null 2>&1 || {
    echo "error: nothing answering at $url. run 'make up' first." >&2
    exit 1
  }
done

echo "==> measuring for ${PERF_MINUTES} minutes against ${WEB_URL}"
echo "    (frame time and TTI are quick; the session length is the memory criterion)"

docker run --rm \
  --network host \
  --user "$(id -u):$(id -g)" \
  -e HOME=/tmp \
  -e CI="${CI:-}" \
  -e WEB_URL="$WEB_URL" \
  -e GATEWAY_URL="$GATEWAY_URL" \
  -e TASKING_API_URL="$TASKING_API_URL" \
  -e PERF_MINUTES="$PERF_MINUTES" \
  -v "$ROOT/web:/w" \
  -w /w \
  "mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-noble" \
  npx playwright test --config=playwright.perf.config.ts "$@"

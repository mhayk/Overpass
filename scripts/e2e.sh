#!/usr/bin/env bash
#
# Run the Playwright suite against the running stack, in a real browser.
#
# THROUGH THE OFFICIAL IMAGE, NOT A LOCAL INSTALL. Playwright refuses to run
# when the installed browsers do not match the library version, and the first
# attempt at this failed exactly that way — a v1.56 image against a 1.62.1
# package. Pinning the image to the version in web/package.json makes the two
# impossible to drift, and means this needs nothing on the host but Docker.
#
# --network host, because the suite talks to :3000, :8080 and :8083 on the host
# and the page under test resolves the same names the browser would.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WEB_URL="${WEB_URL:-http://localhost:3000}"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8083}"
TASKING_API_URL="${TASKING_API_URL:-http://localhost:8080}"

# The version comes out of the lockfile rather than being restated here. A
# constant in this script is a fourth place to forget on upgrade.
PLAYWRIGHT_VERSION="$(
  node -e 'process.stdout.write(require("./web/node_modules/@playwright/test/package.json").version)' \
    2>/dev/null ||
  docker run --rm -v "$ROOT/web:/w" -w /w node:22-alpine \
    node -e 'process.stdout.write(require("@playwright/test/package.json").version)'
)"

if [ -z "$PLAYWRIGHT_VERSION" ]; then
  echo "error: could not read @playwright/test's version." >&2
  echo "       run 'npm install' in web/ first." >&2
  exit 1
fi

for url in "$WEB_URL" "$GATEWAY_URL/readyz" "$TASKING_API_URL/readyz"; do
  if ! curl -sf "$url" >/dev/null 2>&1; then
    echo "error: nothing answering at $url." >&2
    echo "       run 'make up' and 'make seed' first." >&2
    exit 1
  fi
done

echo "==> playwright v${PLAYWRIGHT_VERSION} against ${WEB_URL}"

# --user, so the report and any traces belong to the invoking user rather than
# to root. A gate that leaves root-owned files in the working tree is a gate
# people stop running.
exec docker run --rm \
  --network host \
  --user "$(id -u):$(id -g)" \
  -e HOME=/tmp \
  -e CI="${CI:-}" \
  -e WEB_URL="$WEB_URL" \
  -e GATEWAY_URL="$GATEWAY_URL" \
  -e TASKING_API_URL="$TASKING_API_URL" \
  -v "$ROOT/web:/w" \
  -w /w \
  "mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-noble" \
  npx playwright test "$@"

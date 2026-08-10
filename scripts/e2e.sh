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

# CONTENTION HAS TO BE ARRANGED, because a healthy constellation does not
# produce it.
#
# The contested spec is the one that matters: it is the only test that
# exercises de-confliction and the explanation a customer acts on. But four
# requests over one point do not contend on a freshly seeded stack, and it took
# two CI failures to establish why. Feasibility returns ONE opportunity across
# all nine satellites for a three-hour window over a single point, so every
# request shares one pass — and that pass is ~9 minutes long, a SCAN dwell is
# 15s, slewing between identical targets is free, and the duty budget is 600s
# per orbit. Roughly thirteen acquisitions fit. Nobody loses.
#
# So the budget is narrowed for the duration of the run: 30s admits two
# acquisitions and refuses the rest, which makes DUTY_CYCLE_EXHAUSTED a
# property of configuration rather than of luck. The planner re-reads this per
# round from reference.satellites — rounds.go says so explicitly, "a
# satellite's configuration can change between rounds" — so no restart is
# needed and no committed plan is rewritten.
#
# The previous values are captured and restored on EXIT rather than assuming
# the seeded 600. A gate that leaves the constellation altered after it fails
# is a gate that breaks the next thing to run.
CONTENTION_BUDGET_S="${CONTENTION_BUDGET_S:-30}"

psql() {
  docker compose exec -T postgres psql -U "${POSTGRES_USER:-overpass}" -d "${POSTGRES_DB:-overpass}" "$@"
}

if [ "$CONTENTION_BUDGET_S" != "off" ]; then
  previous="$(psql -tAc "
    SELECT string_agg(format('(%L,%s)', satellite_id, duty_cycle_budget_s), ',')
    FROM reference.satellites")"

  if [ -z "$previous" ]; then
    echo "error: reference.satellites is empty — run 'make seed' first." >&2
    exit 1
  fi

  restore() {
    psql -q -c "
      UPDATE reference.satellites AS s
         SET duty_cycle_budget_s = v.budget
        FROM (VALUES ${previous}) AS v(satellite_id, budget)
       WHERE s.satellite_id = v.satellite_id" >/dev/null
    echo "==> duty-cycle budgets restored"
  }
  trap restore EXIT

  psql -q -c "UPDATE reference.satellites SET duty_cycle_budget_s = ${CONTENTION_BUDGET_S}" >/dev/null
  echo "==> duty-cycle budget narrowed to ${CONTENTION_BUDGET_S}s so the contested path contends"
fi

echo "==> playwright v${PLAYWRIGHT_VERSION} against ${WEB_URL}"

# --user, so the report and any traces belong to the invoking user rather than
# to root. A gate that leaves root-owned files in the working tree is a gate
# people stop running.
#
# NOT `exec`. Replacing this shell would discard the EXIT trap above, leaving
# the constellation on a 30-second duty budget for whatever runs next — and the
# demo would then produce a schedule full of refusals for no visible reason.
docker run --rm \
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

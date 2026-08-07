#!/usr/bin/env bash
#
# Table-driven tests for the path-ownership gate.
#
# The gate's job is to catch a problem, so it has to be demonstrated catching
# it. The M0 codegen drift check silently compared a directory against itself
# and passed unconditionally, and nobody knew because nobody had watched it
# fail. This file is the standing answer to that: every rule has at least one
# case that must FAIL and one that must PASS, and a gate that stopped rejecting
# would turn this suite red rather than going quietly green.
#
# The rule is a pure function of the changed-file list, so these need no
# repository state, no fixture branches, and no real pull request.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$HERE/check-path-ownership.sh"

pass=0; fail=0
green() { printf '  \033[0;32mok    %s\033[0m\n' "$*"; }
red()   { printf '  \033[0;31mFAIL  %s\033[0m\n' "$*"; }

# check <accept|reject> <description> <path...>
check() {
  local want="$1" desc="$2"; shift 2
  local got
  if "$GATE" "$@" >/dev/null 2>&1; then got=accept; else got=reject; fi
  if [[ "$got" == "$want" ]]; then
    green "$desc"; pass=$((pass + 1))
  else
    red "$desc — expected $want, got $got"; fail=$((fail + 1))
  fi
}

printf '\n\033[0;36mLegal changes — must be accepted\033[0m\n'

check accept "one track, several files" \
  services/tasking-api/main.go services/tasking-api/internal/http/handler.go

check accept "a track plus common paths it may touch" \
  services/feasibility/sgp4.py Makefile scripts/stack-up.sh

check accept "contracts and gen together — make contracts moves both" \
  contracts/events/x.v1.schema.json gen/go/x.go gen/python/x.py

check accept "a migration on its own" \
  db/migrations/00006_thing.sql

check accept "an ADR on its own" \
  docs/decisions/0014-planner-replanning.md

check accept "docs outside docs/decisions are common, not shared-fate" \
  docs/backlog.md README.md

check accept "the web track alone" \
  web/app/page.tsx web/package.json

check accept "no changes at all" ""

printf '\n\033[0;36mRule 1 — at most one track directory\033[0m\n'

check reject "two Go service tracks" \
  services/tasking-api/main.go services/plan-gateway/main.go

check reject "a Go track and the web track" \
  services/planner/allocate.go web/app/page.tsx

check reject "Go and Python tracks" \
  services/feasibility/sgp4.py services/tasking-api/main.go

printf '\n\033[0;36mRule 2 — shared-fate paths land alone\033[0m\n'

check reject "a contract change alongside a service" \
  contracts/events/x.v1.schema.json services/tasking-api/main.go

check reject "generated code alongside a service" \
  gen/go/x.go services/planner/allocate.go

check reject "a migration alongside a service" \
  db/migrations/00006_thing.sql services/plan-gateway/read.go

check reject "CLAUDE.md alongside anything" \
  CLAUDE.md Makefile

check reject "an ADR alongside a service" \
  docs/decisions/0014-x.md services/feasibility/geometry.py

check reject "a contract change alongside a common path" \
  contracts/openapi/plan-gateway.v1.yaml Makefile

printf '\n\033[0;36mBoundaries — near misses that must NOT be mistaken for violations\033[0m\n'

# docs/decisions is shared-fate; docs/architecture is not. A prefix match that
# was one character sloppy would get this wrong.
check accept "docs/architecture is not docs/decisions" \
  docs/architecture/c4-context.md Makefile

# CLAUDE.md is an exact file, not a prefix.
check accept "a file merely starting with the same letters" \
  CLAUDE_NOTES.md Makefile

# db/migrations is shared; db/ generally is not.
check accept "db/README.md is not db/migrations" \
  db/README.md scripts/x.sh

printf '\n\033[0;36mResult\033[0m\n'
printf '  %d passed, %d failed\n\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]] || exit 1

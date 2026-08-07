#!/usr/bin/env bash
#
# The ADR-0013 path-ownership rule, enforced instead of merely stated.
#
# ADR-0013 makes parallel agent tracks safe with two rules: each track owns a
# disjoint set of paths, and nobody writes to the shared-fate paths. It also
# admits, in its own Consequences section, that the rule is "stated, not
# enforced ... until something does, this is a convention and conventions
# decay". This is that something.
#
# Two rules, both decided from the diff alone:
#
#   1. A change touches at most ONE track directory. Two tracks in one change
#      means the branches were not disjoint after all.
#
#   2. A change that touches a shared-fate path touches NOTHING else. Contract
#      changes serialise — that is the whole point of M0 and it does not stop
#      being true because four agents are running. contracts/ and gen/ together
#      are fine: both are shared-fate, and `make contracts` moves them as one
#      unit.
#
# Deliberately stateless. CI has no idea what a "track" is and should not learn:
# every violation ADR-0013 cares about is visible in the list of changed files,
# and a check with no state cannot be confused by a rename. Deriving the owning
# track from the area/* labels was rejected — labels are mutable by anyone and
# applied after the fact, and a gate whose input the author edits is not a gate.
#
# The escape hatch is a label, not a hardcoded allowlist. M1-18, M1-19 and M1-20
# legitimately span every service; ADR-0013 calls the integration phase serial
# for exactly that reason. Those changes carry `crosses-tracks`, which documents
# the crossing in review. This weakens the gate ON PURPOSE: the ADR's stated
# goal is that violations be loud, not impossible.
#
# Usage:
#   scripts/check-path-ownership.sh path/one path/two ...
#   git diff --name-only origin/main... | scripts/check-path-ownership.sh
set -euo pipefail

# Track directories, from the ADR-0013 ownership table. Longest-prefix order is
# not needed — these are disjoint by construction, which is the point.
TRACKS=(
  "services/tasking-api/"
  "services/feasibility/"
  "services/plan-gateway/"
  "services/planner/"
  "web/"
)

# Shared-fate paths. The first two ARE the interface; db/migrations is a shared
# ordering, not a shared file; the last two are the standing agreement.
SHARED=(
  "contracts/"
  "gen/"
  "db/migrations/"
  "CLAUDE.md"
  "docs/decisions/"
)

paths=()
if [[ $# -gt 0 ]]; then
  paths=("$@")
else
  while IFS= read -r line; do
    [[ -n "$line" ]] && paths+=("$line")
  done
fi

if [[ ${#paths[@]} -eq 0 ]]; then
  echo "no changed paths — nothing to check"
  exit 0
fi

matched_track() {
  local p="$1" t
  for t in "${TRACKS[@]}"; do
    [[ "$p" == "$t"* ]] && { echo "$t"; return 0; }
  done
  return 1
}

is_shared() {
  local p="$1" s
  for s in "${SHARED[@]}"; do
    # A trailing slash means a directory prefix; anything else is an exact file.
    if [[ "$s" == */ ]]; then
      [[ "$p" == "$s"* ]] && return 0
    else
      [[ "$p" == "$s" ]] && return 0
    fi
  done
  return 1
}

tracks_touched=()
shared_touched=()
other_touched=()

for p in "${paths[@]}"; do
  if t="$(matched_track "$p")"; then
    # shellcheck disable=SC2076
    [[ " ${tracks_touched[*]-} " == *" $t "* ]] || tracks_touched+=("$t")
  elif is_shared "$p"; then
    shared_touched+=("$p")
  else
    other_touched+=("$p")
  fi
done

violations=0
report() { printf '  \033[0;31m%s\033[0m\n' "$*"; violations=$((violations + 1)); }

if [[ ${#tracks_touched[@]} -gt 1 ]]; then
  report "more than one track directory in a single change: ${tracks_touched[*]}"
  echo "      ADR-0013 rule 1 — each track owns a disjoint set of paths."
fi

if [[ ${#shared_touched[@]} -gt 0 ]]; then
  # A track directory is "something else"; so is any common path.
  if [[ ${#tracks_touched[@]} -gt 0 || ${#other_touched[@]} -gt 0 ]]; then
    report "a shared-fate path is changed alongside other work"
    printf '      shared: %s\n' "${shared_touched[*]}"
    [[ ${#tracks_touched[@]} -gt 0 ]] && printf '      track:  %s\n' "${tracks_touched[*]}"
    [[ ${#other_touched[@]} -gt 0 ]]  && printf '      other:  %s\n' "${other_touched[*]}"
    echo "      ADR-0013 rule 2 — contract changes serialise and land on their own."
  fi
fi

if [[ "$violations" -eq 0 ]]; then
  printf '\033[0;32mpath ownership ok\033[0m'
  [[ ${#tracks_touched[@]} -gt 0 ]] && printf ' — track %s' "${tracks_touched[*]}"
  [[ ${#shared_touched[@]} -gt 0 ]] && printf ' — shared-fate only'
  printf '\n'
  exit 0
fi

cat <<'MSG'

If this crossing is deliberate — an integration change that genuinely spans
services, such as M1-18, M1-19 or M1-20 — label the pull request
`crosses-tracks`. That documents the crossing in review rather than hiding it,
which is what ADR-0013 asks for.
MSG
exit 1

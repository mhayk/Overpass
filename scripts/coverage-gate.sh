#!/usr/bin/env bash
#
# Enforce coverage thresholds.
#
#   overall                   80%
#   planner and geometry      95%
#
# Why not 100%: coverage on adapter and wiring code is theatre — it measures
# that a constructor was called, not that anything works. 95% on the two
# packages where correctness is genuinely hard, and where a subtle error is
# both most likely and least visible, is a real signal. That argument is made
# properly in ADR-0010.
#
# Packages that do not exist yet are reported and skipped rather than silently
# passing at 0%, so the gate is visible from the day it is written instead of
# arriving with the code it guards.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

OVERALL_MIN="${OVERALL_MIN:-80}"
CRITICAL_MIN="${CRITICAL_MIN:-95}"

# Packages held to the higher bar. Path fragments, matched against the coverage
# profile. These are the ones whose failure modes are silent: a wrong incidence
# angle produces a plausible opportunity, and a scheduler that violates slew
# time produces a plan that simply cannot be flown.
CRITICAL_PATHS=(
  "services/planner/internal/domain"
  "services/planner/internal/allocation"
  "services/feasibility/geometry"
)

fail=0
note() { printf '  %s\n' "$*"; }

profiles=$(find . -name coverage.out -not -path './.tools/*' 2>/dev/null || true)
if [[ -z "$profiles" ]]; then
  echo "no coverage profiles found — no Go packages with tests yet"
  echo "gate is declared and will apply from M1 (issue #14 onward)"
  exit 0
fi

echo "== coverage gate =="
echo "   overall >= ${OVERALL_MIN}%   critical packages >= ${CRITICAL_MIN}%"
echo

while read -r profile; do
  [[ -z "$profile" ]] && continue
  dir=$(dirname "$profile")
  total=$(cd "$dir" && go tool cover -func=coverage.out | awk '/^total:/ {gsub("%","",$3); print $3}')
  [[ -z "$total" ]] && continue

  printf '%s: %s%%\n' "$dir" "$total"
  if awk "BEGIN{exit !($total < $OVERALL_MIN)}"; then
    note "FAIL below overall minimum of ${OVERALL_MIN}%"
    fail=1
  fi

  for critical in "${CRITICAL_PATHS[@]}"; do
    pkg_cov=$(cd "$dir" && go tool cover -func=coverage.out 2>/dev/null \
      | grep -F "$critical" | awk '{gsub("%","",$NF); s+=$NF; n++} END {if (n>0) printf "%.1f", s/n}')
    [[ -z "$pkg_cov" ]] && continue
    printf '  %s: %s%%\n' "$critical" "$pkg_cov"
    if awk "BEGIN{exit !($pkg_cov < $CRITICAL_MIN)}"; then
      note "FAIL below critical minimum of ${CRITICAL_MIN}%"
      fail=1
    fi
  done
done <<< "$profiles"

if [[ "$fail" -ne 0 ]]; then
  echo
  echo "coverage gate failed"
  exit 1
fi
echo
echo "coverage gate passed"

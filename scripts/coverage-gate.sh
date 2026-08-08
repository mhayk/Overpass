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
checked=0
note() { printf '  %s\n' "$*"; }

# gen/ is excluded. Coverage of generated types measures nothing: the code was
# not written by anyone, cannot be changed without editing a generator, and its
# correctness is established by the round-trip tests in gen/go/contracttest and
# by the drift gate — both stronger evidence than a percentage. Including it
# would also drag the overall figure down with thousands of generated lines and
# make the number meaningless for the code that matters.
# tests/ is excluded for the same reason gen/ is, from the other direction.
# gen/ is code nobody wrote; tests/integration is code that is ALL test — the
# module contains no production statements at all, so its coverage is 0.0% by
# construction and reporting it as a failure says nothing about anything.
profiles=$(find . -name coverage.out -not -path './.tools/*' -not -path './gen/*' -not -path './tests/*' 2>/dev/null || true)
if [[ -z "$profiles" ]]; then
  # This used to exit 0, with a note that the gate applied "from M1 (issue #14
  # onward)". #14 is the issue that landed the first hand-written Go package, so
  # that grace period is over and an empty run is now a failure.
  #
  # It has to be. A gate that reports success because it found nothing to check
  # is worse than no gate: it is a green tick that means the opposite of what it
  # appears to, and deleting every test would produce one.
  echo "coverage gate found no coverage profiles to check"
  echo "  looked for **/coverage.out outside gen/ and .tools/"
  echo "  either the test step did not run, or it did not write a profile"
  exit 1
fi

echo "== coverage gate =="
echo "   overall >= ${OVERALL_MIN}%   critical packages >= ${CRITICAL_MIN}%"
echo

while read -r profile; do
  [[ -z "$profile" ]] && continue
  dir=$(dirname "$profile")
  total=$(cd "$dir" && go tool cover -func=coverage.out | awk '/^total:/ {gsub("%","",$3); print $3}')
  [[ -z "$total" ]] && continue

  checked=$((checked + 1))
  printf '%s: %s%%\n' "$dir" "$total"
  if awk "BEGIN{exit !($total < $OVERALL_MIN)}"; then
    note "FAIL below overall minimum of ${OVERALL_MIN}%"
    fail=1
  fi

  for critical in "${CRITICAL_PATHS[@]}"; do
    # The `|| true` is load-bearing. grep exits 1 when it matches nothing,
    # pipefail propagates that, and set -e then kills the whole script —
    # silently, mid-loop, before any verdict is printed.
    #
    # That is exactly what happened the first time this gate ever had a profile
    # to look at. Until services/tasking-api existed the loop body never ran —
    # deliberately, the script said so and named this very issue as the point it
    # would start applying. The grace period was intentional; this bug inside it
    # was not, and it survived because nobody had watched the gate execute.
    pkg_cov=$( { cd "$dir" && go tool cover -func=coverage.out 2>/dev/null \
      | grep -F "$critical" | awk '{gsub("%","",$NF); s+=$NF; n++} END {if (n>0) printf "%.1f", s/n}'; } || true)
    [[ -z "$pkg_cov" ]] && continue
    printf '  %s: %s%%\n' "$critical" "$pkg_cov"
    if awk "BEGIN{exit !($pkg_cov < $CRITICAL_MIN)}"; then
      note "FAIL below critical minimum of ${CRITICAL_MIN}%"
      fail=1
    fi
  done
done <<< "$profiles"

# Belt to the braces above: profiles were found, but every one of them was
# unreadable or produced no total. Same reasoning — a run that checked nothing
# must not report success.
if [[ "$checked" -eq 0 ]]; then
  echo
  echo "coverage gate found profiles but could not read a total from any of them"
  exit 1
fi

if [[ "$fail" -ne 0 ]]; then
  echo
  echo "coverage gate failed"
  exit 1
fi
echo
echo "coverage gate passed (${checked} module(s) checked)"

#!/usr/bin/env bash
#
# The codegen drift gate: regenerate into a temp directory and fail if the
# result differs from what is committed under gen/.
#
# This is the mechanism that makes contracts-first real rather than aspirational.
# Without it, "generated code is committed" degrades into "some code that was
# generated once is committed", and the moment a schema changes without a
# regeneration, the Go and Python halves of the system disagree about what an
# Opportunity is — discovered in production, at the worst possible time.
#
# It catches exactly two mistakes, both common:
#   1. Editing a schema and forgetting to regenerate.
#   2. Hand-editing generated code, which then evaporates on the next run.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

printf '\033[0;36m==> regenerating into a scratch tree\033[0m\n'
"$ROOT/scripts/contracts-generate.sh" "$TMP" >/dev/null

# Compare only the trees the generators own. gen/ also holds hand-written files
# that are legitimately not generated — go.mod, go.sum, README.md — and diffing
# the whole directory would report those as drift forever. Listing the generated
# trees explicitly is more precise than a growing --exclude list, and it fails
# loudly if a generator stops producing one of them.
GENERATED_TREES=(
  go/events
  go/taskingapi
  python/overpass_contracts
)

printf '\033[0;36m==> comparing against gen/\033[0m\n'
: > "$TMP.diff"
drift=0
for tree in "${GENERATED_TREES[@]}"; do
  if [[ ! -d "$TMP/$tree" ]]; then
    echo "generator produced no $tree — the generate script is broken" >> "$TMP.diff"
    drift=1
    continue
  fi
  # __pycache__ is gitignored but still present in the working tree after any
  # Python run, so it must be excluded here or the gate fails for a reason that
  # has nothing to do with the contracts.
  diff -ru --exclude='__pycache__' "$ROOT/gen/$tree" "$TMP/$tree" >> "$TMP.diff" 2>&1 || drift=1
done

if [[ "$drift" -eq 0 ]]; then
  printf '  \033[0;32mno drift — gen/ matches the contracts\033[0m\n'
  exit 0
fi

printf '  \033[0;31mDRIFT DETECTED\033[0m\n\n'
head -100 "$TMP.diff"
lines=$(wc -l < "$TMP.diff" | tr -d ' ')
if [[ "$lines" -gt 100 ]]; then
  printf '\n  ... %s more diff lines suppressed\n' "$((lines - 100))"
fi
cat >&2 <<'MSG'

gen/ does not match a fresh generation from contracts/.

  If you changed a schema:   run `make contracts-generate` and commit gen/.
  If you edited gen/ by hand: don't. Change the contract instead — hand edits
                              are erased by the next generation, silently.
MSG
exit 1

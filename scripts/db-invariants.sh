#!/usr/bin/env bash
#
# The non-overlap invariant, asserted against a real database.
#
# This is the load-bearing test of the whole repository. ADR-0003 and ADR-0004
# both rest on the claim that no code path, migration, or manual INSERT can
# produce two overlapping acquisitions on one satellite. Everything here goes in
# through raw SQL, deliberately bypassing every line of application code,
# because a guarantee that only holds when the application is well-behaved is
# not a guarantee.
#
# TWO PHASES, and the first one is the point.
#
#   Phase 1 — mutants. Build deliberately WRONG versions of the table and
#   require the assertions to catch each one. An M0 defect came from a drift
#   gate that compared a directory against itself and passed unconditionally; a
#   check never seen failing is not known to work. So this script breaks the
#   schema on purpose, three ways, and fails if any mutant survives.
#
#   Phase 2 — the real planning.acquisitions table, which must pass everything.
#
# A green run therefore means both "the invariant holds" and "these assertions
# would have noticed if it did not".
#
# Usage:
#   scripts/db-invariants.sh              # against the compose stack
#   PSQL_CMD="psql -U overpass -d overpass" scripts/db-invariants.sh
set -euo pipefail

PSQL_CMD="${PSQL_CMD:-docker compose exec -T postgres psql -U overpass -d overpass}"

pass=0; fail=0
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
cyan()  { printf '\n\033[0;36m%s\033[0m\n' "$*"; }

# Run SQL, discard output, return the exit status. ON_ERROR_STOP makes a
# constraint violation a non-zero exit rather than a warning on stdout.
sql() { $PSQL_CMD -v ON_ERROR_STOP=1 -q -c "$1" >/dev/null 2>&1; }
sql_out() { $PSQL_CMD -tAc "$1" 2>/dev/null; }

# expect accepts|rejects <description> <sql>
expect() {
  local want="$1" desc="$2" stmt="$3"
  if sql "$stmt"; then got=accepts; else got=rejects; fi
  if [[ "$got" == "$want" ]]; then
    green "  ok    $desc"; pass=$((pass + 1))
  else
    red   "  FAIL  $desc (expected to $want, it ${got})"; fail=$((fail + 1))
  fi
}

# expect_mutant_caught <description> <sql> — a mutant is caught when the
# statement that SHOULD have been rejected is accepted by the broken schema.
expect_mutant_caught() {
  local desc="$1" stmt="$2"
  if sql "$stmt"; then
    green "  ok    mutant caught — $desc"; pass=$((pass + 1))
  else
    red   "  FAIL  mutant SURVIVED — $desc was still rejected, so the assertion proves nothing"
    fail=$((fail + 1))
  fi
}

W1="tstzrange('2031-03-01 10:00:00+00','2031-03-01 10:05:00+00')"
W2="tstzrange('2031-03-01 10:03:00+00','2031-03-01 10:08:00+00')"   # overlaps W1
POLY="ST_GeomFromText('POLYGON((0 0,0 1,1 1,1 0,0 0))',4326)"

cleanup() {
  sql "DROP TABLE IF EXISTS planning._mutant" || true
  sql "DELETE FROM planning.acquisitions   WHERE request_id = '00000000-0000-0000-0000-0000000000ff'" || true
  sql "DELETE FROM planning.collection_plans WHERE round_id = '00000000-0000-0000-0000-0000000000ff'" || true
  sql "DELETE FROM reference.satellites    WHERE satellite_id IN ('DBTEST-A','DBTEST-B')" || true
  sql "DELETE FROM reference.customers     WHERE customer_id = 'dbtest-customer'" || true
}
trap cleanup EXIT
cleanup

# ---------------------------------------------------------------------------
# Phase 1 — mutants
# ---------------------------------------------------------------------------
# Each mutant is planning.acquisitions with exactly one property removed. The
# assertions below are the same ones phase 2 runs; here they must FAIL to hold,
# which is what proves they are load-bearing rather than decorative.

mutant() {
  sql "DROP TABLE IF EXISTS planning._mutant"
  sql "CREATE TABLE planning._mutant (
         id bigserial PRIMARY KEY,
         satellite_id text NOT NULL,
         status $1,
         acq_window tstzrange NOT NULL
         $2
       )"
}

cyan "Phase 1 — mutants: break the schema on purpose, require the assertions to notice"

echo "  mutant A: no exclusion constraint at all"
mutant "text NOT NULL" ""
sql "INSERT INTO planning._mutant (satellite_id,status,acq_window) VALUES ('DBTEST-A','ACTIVE',$W1)"
expect_mutant_caught "overlapping live rows" \
  "INSERT INTO planning._mutant (satellite_id,status,acq_window) VALUES ('DBTEST-A','ACTIVE',$W2)"

echo "  mutant B: predicate is status = 'ACTIVE', so EXECUTED stops blocking"
mutant "text NOT NULL" ", CONSTRAINT m EXCLUDE USING gist (satellite_id WITH =, acq_window WITH &&) WHERE (status = 'ACTIVE')"
sql "INSERT INTO planning._mutant (satellite_id,status,acq_window) VALUES ('DBTEST-A','EXECUTED',$W1)"
expect_mutant_caught "a new ACTIVE laid over an EXECUTED acquisition" \
  "INSERT INTO planning._mutant (satellite_id,status,acq_window) VALUES ('DBTEST-A','ACTIVE',$W2)"

echo "  mutant C: status is nullable, so NULL escapes the partial predicate"
mutant "text" ", CONSTRAINT m EXCLUDE USING gist (satellite_id WITH =, acq_window WITH &&) WHERE (status <> 'SUPERSEDED')"
sql "INSERT INTO planning._mutant (satellite_id,status,acq_window) VALUES ('DBTEST-A','ACTIVE',$W1)"
expect_mutant_caught "a NULL-status row overlapping a live one" \
  "INSERT INTO planning._mutant (satellite_id,status,acq_window) VALUES ('DBTEST-A',NULL,$W2)"

sql "DROP TABLE IF EXISTS planning._mutant"

# ---------------------------------------------------------------------------
# Phase 2 — the real table
# ---------------------------------------------------------------------------
cyan "Phase 2 — planning.acquisitions, the real thing"

sql "INSERT INTO reference.customers (customer_id, display_name)
     VALUES ('dbtest-customer','db invariant test')"
sql "INSERT INTO reference.satellites (satellite_id, norad_id, display_name, sensor_modes, duty_cycle_budget_s)
     VALUES ('DBTEST-A', 990001, 'probe A', '{}'::jsonb, 600),
            ('DBTEST-B', 990002, 'probe B', '{}'::jsonb, 600)"
sql "INSERT INTO planning.collection_plans (plan_id, round_id, satellite_id, bucket, plan_version, policy, committed_at)
     VALUES ('aaaaaaaa-0000-0000-0000-0000000000ff','00000000-0000-0000-0000-0000000000ff','DBTEST-A',
             tstzrange('2031-03-01 09:00:00+00','2031-03-01 12:00:00+00'), 1, 'DBTEST', now()),
            ('bbbbbbbb-0000-0000-0000-0000000000ff','00000000-0000-0000-0000-0000000000ff','DBTEST-B',
             tstzrange('2031-03-01 09:00:00+00','2031-03-01 12:00:00+00'), 1, 'DBTEST', now())"

# acq <id> <satellite> <plan> <status> <window> [superseded_at]
acq() {
  local sup="NULL"; [[ "$4" == "SUPERSEDED" ]] && sup="now()"
  echo "INSERT INTO planning.acquisitions
          (acquisition_id, plan_id, request_id, opportunity_id, customer_id, satellite_id,
           mode, acq_window, geometry, footprint, duty_cycle_cost_s, awarded_value_credits,
           status, superseded_at)
        VALUES ('$1','$3','00000000-0000-0000-0000-0000000000ff',gen_random_uuid(),
                'dbtest-customer','$2','SPOTLIGHT',$5,'{}'::jsonb,$POLY,30,100,'$4',$sup)"
}

PA=aaaaaaaa-0000-0000-0000-0000000000ff
PB=bbbbbbbb-0000-0000-0000-0000000000ff

expect accepts "baseline live acquisition" \
  "$(acq 10000000-0000-0000-0000-0000000000ff DBTEST-A $PA ACTIVE "$W1")"

expect rejects "two overlapping live acquisitions, raw SQL, no application code" \
  "$(acq 10000001-0000-0000-0000-0000000000ff DBTEST-A $PA ACTIVE "$W2")"

expect accepts "the same window on a DIFFERENT satellite" \
  "$(acq 10000002-0000-0000-0000-0000000000ff DBTEST-B $PB ACTIVE "$W2")"

expect accepts "an overlapping SUPERSEDED row — history survives re-planning" \
  "$(acq 10000003-0000-0000-0000-0000000000ff DBTEST-A $PA SUPERSEDED "$W2")"

expect accepts "the ACTIVE -> EXECUTED transition does not self-conflict" \
  "UPDATE planning.acquisitions SET status='EXECUTED'
     WHERE acquisition_id='10000000-0000-0000-0000-0000000000ff'"

expect rejects "a new ACTIVE overlapping an EXECUTED acquisition" \
  "$(acq 10000004-0000-0000-0000-0000000000ff DBTEST-A $PA ACTIVE "$W2")"

expect rejects "two overlapping EXECUTED acquisitions" \
  "$(acq 10000005-0000-0000-0000-0000000000ff DBTEST-A $PA EXECUTED "$W2")"

expect rejects "status cannot be NULL — the partial predicate depends on it" \
  "INSERT INTO planning.acquisitions
     (acquisition_id, plan_id, request_id, opportunity_id, customer_id, satellite_id,
      mode, acq_window, geometry, footprint, duty_cycle_cost_s, awarded_value_credits, status)
   VALUES ('10000006-0000-0000-0000-0000000000ff','$PA','00000000-0000-0000-0000-0000000000ff',
           gen_random_uuid(),'dbtest-customer','DBTEST-A','SPOTLIGHT',$W2,'{}'::jsonb,$POLY,30,100,NULL)"

expect rejects "a recorded gap smaller than the recorded slew" \
  "UPDATE planning.acquisitions SET slew_time_from_previous_s = 40, gap_from_previous_s = 10
     WHERE acquisition_id='10000002-0000-0000-0000-0000000000ff'"

# Supersession in the natural write order: insert the replacement first, demote
# the incumbent second. This is the case an immediate constraint rejects.
expect accepts "supersession in the natural write order (insert v2, then demote v1)" \
  "BEGIN;
     $(acq 10000007-0000-0000-0000-0000000000ff DBTEST-A $PA ACTIVE "$W2");
     UPDATE planning.acquisitions SET status='SUPERSEDED', superseded_at=now()
       WHERE acquisition_id='10000000-0000-0000-0000-0000000000ff';
   COMMIT;"

# Deferring moves WHEN the check runs, not WHETHER.
expect rejects "a genuinely conflicting plan is still rejected at COMMIT" \
  "BEGIN;
     $(acq 10000008-0000-0000-0000-0000000000ff DBTEST-A $PA ACTIVE "$W1");
   COMMIT;"

cyan "Result"
printf '  %d passed, %d failed\n\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]] || { red "the invariant is not holding, or the assertions are not testing it"; exit 1; }
green "invariant holds, and the assertions were shown to catch three ways of breaking it"

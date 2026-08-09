package integration_test

import (
	"context"
	"hash/fnv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The planner and its advisory lock, killed on purpose.
//
// The previous file proves what Postgres does with a lock when a connection
// dies. This one proves what the SYSTEM does with it: that a planner killed
// mid-round leaves no half-planned bucket behind, and that the next planner
// picks the bucket up and allocates it exactly once.
//
// Killing is the only honest way to test this. A planner that shuts down
// cleanly rolls its transaction back through the path every error already
// takes; the interesting case is the one where nothing in the process gets to
// run at all.

// advisoryKeyFor mirrors domain.AdvisoryLockKey, which lives in the planner's
// internal package and cannot be imported across modules.
//
// A duplicate, and therefore a thing that can drift — deliberately made to fail
// loudly rather than silently: every test here waits for a lock at exactly
// these numbers, so a changed derivation makes them time out with a message
// that says so, instead of quietly watching the wrong lock.
func advisoryKeyFor(satelliteID string, bucketStart time.Time) (int32, int32) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(satelliteID))
	//nolint:gosec // the truncation is the point: a 32-bit lock half
	return int32(h.Sum32()), int32(bucketStart.UTC().Unix() / 60)
}

// plannableBucket seeds one satellite, one request and `candidates` candidate
// opportunities inside a bucket the planner's sweep will find dirty.
//
// Seeded directly rather than driven through feasibility. The claim under test
// is about the round's transaction, and routing the setup through two more
// services would make a failure here ambiguous between three of them.
func plannableBucket(t *testing.T, candidates int) (satelliteID string, bucketStart time.Time, requestID string) {
	t.Helper()
	ctx := context.Background()

	satelliteID = seedChaosSatellite(t)
	requestID = uuid.NewString()

	// The NEXT-but-one six-hour boundary: always in the future, always inside
	// the 24h horizon.
	//
	// The first version truncated now+2h to the boundary, which lands in the
	// PAST for most of the day — at 21:30 it yields 18:00. A bucket that has
	// already elapsed cannot be flown, so the planner correctly ignored it and
	// the test waited 150 seconds for a round that was never going to happen.
	// It passed every afternoon and failed at night, which is the worst kind of
	// green.
	const bucket = 6 * time.Hour
	start := time.Now().UTC().Truncate(bucket).Add(2 * bucket)
	bucketStart = start

	if _, err := env.pool.Exec(ctx, `
		INSERT INTO reference.customers (customer_id, display_name)
		VALUES ($1, 'Chaos Test Customer') ON CONFLICT (customer_id) DO NOTHING`,
		testCustomerID); err != nil {
		t.Fatalf("seeding the customer: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO planning.request_snapshots
			(request_id, customer_id, priority_tier, bid_credits, request_window,
			 submitted_at, source_event_id, occurred_at)
		VALUES ($1, $2, 'COMMERCIAL', 100, tstzrange($3, $4, '[)'), now(), $5, now())`,
		requestID, testCustomerID, start.Add(-time.Hour), start.Add(bucket), uuid.NewString()); err != nil {
		t.Fatalf("seeding the request snapshot: %v", err)
	}

	// Many candidates, because the lock is held for the length of one round:
	// reads, allocation, insert. A handful would make the window too narrow to
	// kill inside reliably, and a test that usually misses its own window is a
	// test that usually proves nothing.
	for i := range candidates {
		accessStart := start.Add(time.Duration(i) * 3 * time.Second)
		if _, err := env.pool.Exec(ctx, `
			INSERT INTO planning.candidate_opportunities
				(opportunity_id, request_id, satellite_id, mode, access_window,
				 acquisition_duration_s, orbit_number, geometry, footprint,
				 duty_cycle_cost_s, quality_score, source_event_id, computed_at)
			VALUES ($1, $2, $3, 'STRIPMAP', tstzrange($4, $5, '[)'),
			        12, $6, '{"look_side":"LEFT","squint_deg":0}'::jsonb,
			        ST_GeomFromText('POLYGON((0 0,0 1,1 1,1 0,0 0))', 4326),
			        12, 0.9, $7, now())`,
			uuid.NewString(), requestID, satelliteID,
			accessStart, accessStart.Add(90*time.Second), i, uuid.NewString()); err != nil {
			t.Fatalf("seeding candidate %d: %v", i, err)
		}
	}

	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM planning.acquisitions WHERE request_id = $1`,
			`DELETE FROM planning.candidate_opportunities WHERE request_id = $1`,
			`DELETE FROM planning.request_snapshots WHERE request_id = $1`,
		} {
			if _, err := env.pool.Exec(context.Background(), statement, requestID); err != nil {
				t.Logf("cleanup %q: %v", statement, err)
			}
		}
	})
	return satelliteID, bucketStart, requestID
}

func plannerEnv() map[string]string {
	return map[string]string{
		// Sweep hard, and shorten the quiet period to its minimum rather than
		// removing it: the config refuses a zero, correctly — a quiet period of
		// zero would open a round on every single candidate. One second is
		// enough for a seeded bucket that stops changing the moment the test
		// finishes writing it.
		"SWEEP_INTERVAL":     "1",
		"ROUND_QUIET_PERIOD": "1",
		// Must EXCEED the quiet period, or the debounce can never fire —
		// the planner refuses to start otherwise, which is how this number
		// was chosen rather than guessed.
		"ROUND_STALENESS_CEILING": "3",
	}
}

func roundsFor(t *testing.T, satelliteID string) int {
	t.Helper()
	return rowCount(t, `SELECT count(*) FROM planning.rounds WHERE satellite_id = $1`, satelliteID)
}

// waitForLock returns true once the planner is holding the bucket's lock.
//
// Polled tightly rather than slept at: the whole point is to kill INSIDE the
// transaction, and a sleep either lands before the round starts or after it
// commits.
func waitForLock(t *testing.T, class, obj int32, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if holdersOf(t, class, obj) > 0 {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// A planner killed while holding the lock leaves nothing half-planned, and the
// next one plans the bucket exactly once.
func TestAPlannerKilledHoldingTheLockLeavesNoHalfPlannedBucket(t *testing.T) {
	satelliteID, bucketStart, requestID := plannableBucket(t, 400)
	class, obj := advisoryKeyFor(satelliteID, bucketStart)

	first, err := start(env.plannerBin, "planner", plannerEnv())
	if err != nil {
		t.Fatalf("starting the planner: %v", err)
	}

	killedInsideTheRound := waitForLock(t, class, obj, 60*time.Second)
	if killErr := first.Kill(); killErr != nil {
		t.Fatalf("killing the planner: %v", killErr)
	}
	if !killedInsideTheRound {
		// Reported, not asserted. If the lock never appeared the planner either
		// never got to this bucket or committed faster than a 1ms poll — this
		// run then tested the restart rather than the kill, and saying so is
		// better than a green tick for a scenario that did not happen.
		t.Logf("the bucket's lock was never observed: this run exercised a restart, not a kill")
	}

	// The lock dies with the process. Nothing releases it; nothing can.
	//
	// Waited for rather than asserted instantly, and the difference is a real
	// property rather than test hygiene: Postgres releases the lock when it
	// NOTICES the client is gone, which happens on the backend's next socket
	// operation and not at the moment of the kill. Measured — asserting it
	// synchronously passed locally and failed in CI with the lock still held
	// 1.13s in. The bound stays tight, because "eventually, some minutes later"
	// would not be a guarantee an operator could rely on.
	if !waitFor(30*time.Second, func() bool { return holdersOf(t, class, obj) == 0 }) {
		t.Fatalf("the bucket's lock is still held 30s after the planner died — " +
			"the satellite is now unplannable until someone notices")
	}

	second, err := start(env.plannerBin, "planner", plannerEnv())
	if err != nil {
		t.Fatalf("restarting the planner: %v", err)
	}
	t.Cleanup(func() {
		if killErr := second.Kill(); killErr != nil {
			t.Errorf("killing the second planner: %v", killErr)
		}
	})

	eventually(t, "the second planner to plan the bucket", 90*time.Second, func() bool {
		return roundsFor(t, satelliteID) >= 1
	})

	// One ACTIVE acquisition per request, globally — the #163 invariant. A
	// half-committed round followed by a full one is exactly how that would
	// break, which is why this test asserts it rather than trusting the
	// constraint to have been exercised elsewhere.
	active := rowCount(t, `SELECT count(*) FROM planning.acquisitions
	                        WHERE request_id = $1 AND status = 'ACTIVE'`, requestID)
	if active > 1 {
		t.Fatalf("%d ACTIVE acquisitions for one request after a killed round — the bucket was allocated twice", active)
	}

	// Settle, then require the count to be stable: a second round racing the
	// first would show up as a later increase rather than an immediate one.
	time.Sleep(3 * time.Second)
	if got := rowCount(t, `SELECT count(*) FROM planning.acquisitions
	                        WHERE request_id = $1 AND status = 'ACTIVE'`, requestID); got != active {
		t.Errorf("ACTIVE acquisitions moved from %d to %d after settling", active, got)
	}
}

// A planner waiting on a lock whose holder dies takes it over and completes.
//
// The deterministic half of the same claim: no race with a kill, because the
// test itself is the holder. This is the failover story stated plainly — one
// planner dies mid-bucket, another was already waiting, and the bucket gets
// planned without anyone releasing anything by hand.
func TestAPlannerWaitingOnADeadHoldersLockTakesOver(t *testing.T) {
	satelliteID, bucketStart, _ := plannableBucket(t, 5)
	class, obj := advisoryKeyFor(satelliteID, bucketStart)

	holder := holdAdvisoryLock(t, class, obj)

	planner, err := start(env.plannerBin, "planner", plannerEnv())
	if err != nil {
		t.Fatalf("starting the planner: %v", err)
	}
	t.Cleanup(func() {
		if killErr := planner.Kill(); killErr != nil {
			t.Errorf("killing the planner: %v", killErr)
		}
	})

	// Give the planner time to reach the lock and block on it. It cannot plan
	// while the test holds it, and that standstill is the precondition.
	time.Sleep(5 * time.Second)
	if got := roundsFor(t, satelliteID); got != 0 {
		t.Fatalf("%d rounds while the lock was held by someone else — the planner did not wait, "+
			"so nothing serialises two planners on one bucket", got)
	}

	// The holder dies the way a killed process dies: no rollback, no release.
	if _, err := env.pool.Exec(t.Context(), `SELECT pg_terminate_backend($1)`, holder); err != nil {
		t.Fatalf("terminating the holder: %v", err)
	}

	if !waitFor(90*time.Second, func() bool { return roundsFor(t, satelliteID) >= 1 }) {
		t.Fatalf("the waiting planner never planned the bucket after the holder died\n%s", planner.logs.String())
	}
}

// holdAdvisoryLock takes the lock on its own connection and returns that
// connection's backend pid, so the test can kill it later. The connection is
// deliberately leaked into the test's lifetime — it has to stay open, because
// closing it would release the lock politely, which is the opposite of the
// scenario.
func holdAdvisoryLock(t *testing.T, class, obj int32) int {
	t.Helper()
	ctx := context.Background()

	conn, err := env.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a connection: %v", err)
	}
	var pid int
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("reading the backend pid: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the holder's transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, class, obj); err != nil {
		t.Fatalf("taking the bucket's lock: %v", err)
	}
	t.Cleanup(func() {
		// The connection is already dead by now in the happy path; releasing it
		// back to the pool would hand out a corpse.
		conn.Hijack().Close(context.Background()) //nolint:errcheck // teardown of a killed connection
	})

	if got := holdersOf(t, class, obj); got != 1 {
		t.Fatalf("%d holders after taking the bucket's lock, want 1 — "+
			"the key derivation in this test no longer matches the planner's", got)
	}
	return pid
}

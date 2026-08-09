package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The advisory lock is the planner's single-writer guarantee, and ADR-0003
// chose pg_advisory_xact_lock over an application-level lease for one reason:
// Postgres releases it when the connection dies, so there is no expiry logic to
// get wrong and no lease to renew.
//
// That is a claim about Postgres. This file executes it.
//
// Without it, the design's whole argument rests on a sentence somebody read
// once — and the failure mode if it were wrong is the worst one available: a
// satellite that can never be planned again, silently, until someone restarts a
// database connection nobody has thought about in months.

// lockKeys mirrors domain.AdvisoryLockKey for a bucket this file owns.
//
// Hard-coded rather than imported: services/planner is a separate module whose
// internal/ this package cannot reach, and the numbers only have to be
// consistent WITHIN this test — the claim is about the lock, not about the
// derivation, which has its own unit tests.
const (
	chaosLockClass int32 = 424242
	chaosLockObj   int32 = 242424
)

// backendPID identifies a connection so another one can kill it.
func backendPID(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var pid int
	if err := conn.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("reading the backend pid: %v", err)
	}
	return pid
}

// holdersOf counts sessions holding the advisory lock, straight out of pg_locks
// — the same view an operator would read during an incident.
//
// The signature takes int32 and the query sends uint32, and that conversion is
// not cosmetic. pg_advisory_xact_lock takes SIGNED 32-bit keys, while pg_locks
// exposes the same two halves as classid and objid, which are OIDs and
// therefore UNSIGNED. A planner key whose hash happens to land above 2^31 is
// negative on the way in and huge on the way out; passing the negative straight
// through fails to encode ("is less than minimum value for uint32"), which is
// how this was found rather than reasoned about.
func holdersOf(t *testing.T, class, obj int32) int {
	t.Helper()
	var n int
	err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks
		  WHERE locktype = 'advisory' AND classid = $1 AND objid = $2 AND granted`,
		uint32(class), uint32(obj)).Scan(&n)
	if err != nil {
		t.Fatalf("reading pg_locks: %v", err)
	}
	return n
}

func TestAnAdvisoryLockDiesWithTheConnectionThatHeldIt(t *testing.T) {
	ctx := t.Context()

	// A dedicated connection, not one from the pool: this one is about to be
	// killed, and handing a corpse back to the pool would leak the failure into
	// whichever test ran next.
	holder, err := pgx.Connect(ctx, env.dsn)
	if err != nil {
		t.Fatalf("connecting the holder: %v", err)
	}
	defer holder.Close(context.Background()) //nolint:errcheck // it is about to be killed

	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the holder's transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, chaosLockClass, chaosLockObj); err != nil {
		t.Fatalf("taking the lock: %v", err)
	}
	if got := holdersOf(t, chaosLockClass, chaosLockObj); got != 1 {
		t.Fatalf("%d holders after taking the lock, want 1 — the test is not testing what it thinks", got)
	}

	// SIGKILL is what a dying planner looks like to Postgres: the socket goes
	// away without a ROLLBACK. pg_terminate_backend produces the same thing
	// from the server side and is the only version a test can trigger
	// deterministically.
	if _, err := env.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, backendPID(t, holder)); err != nil {
		t.Fatalf("terminating the holder: %v", err)
	}

	// The claim: another round takes the same lock, promptly, with nobody
	// having released anything. lock_timeout turns the failure into an error in
	// two seconds rather than a suite that hangs until CI gives up — and a hang
	// is exactly what a leaked lock would produce.
	waiter, err := pgx.Connect(ctx, env.dsn)
	if err != nil {
		t.Fatalf("connecting the waiter: %v", err)
	}
	defer waiter.Close(context.Background()) //nolint:errcheck // test teardown

	if _, err := waiter.Exec(ctx, `SET lock_timeout = '2s'`); err != nil {
		t.Fatalf("setting lock_timeout: %v", err)
	}
	next, err := waiter.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the waiter's transaction: %v", err)
	}
	if _, err := next.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, chaosLockClass, chaosLockObj); err != nil {
		t.Fatalf("the lock outlived the connection that held it: %v\n"+
			"ADR-0003 chose an advisory lock precisely because this cannot happen; "+
			"if it can, the planner needs a lease with an expiry after all", err)
	}
	if err := next.Rollback(ctx); err != nil {
		t.Fatalf("rolling back the waiter: %v", err)
	}

	// And the rollback released it too — the ordinary path, asserted here so
	// the test cannot pass by the lock never having been exclusive at all.
	if got := holdersOf(t, chaosLockClass, chaosLockObj); got != 0 {
		t.Errorf("%d holders after the waiter rolled back, want 0", got)
	}
}

// The lock is exclusive in the first place.
//
// Without this, the test above would pass just as happily against a lock that
// never blocked anyone — which is the failure mode that would make the planner
// double-plan a bucket rather than leave it unplannable, and is worse.
func TestTheAdvisoryLockActuallyExcludes(t *testing.T) {
	ctx := t.Context()

	holder, err := pgx.Connect(ctx, env.dsn)
	if err != nil {
		t.Fatalf("connecting the holder: %v", err)
	}
	defer holder.Close(context.Background()) //nolint:errcheck // test teardown

	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning: %v", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // test teardown

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, chaosLockClass, chaosLockObj+1); err != nil {
		t.Fatalf("taking the lock: %v", err)
	}

	blocked, err := pgx.Connect(ctx, env.dsn)
	if err != nil {
		t.Fatalf("connecting the contender: %v", err)
	}
	defer blocked.Close(context.Background()) //nolint:errcheck // test teardown

	if _, err := blocked.Exec(ctx, `SET lock_timeout = '1s'`); err != nil {
		t.Fatalf("setting lock_timeout: %v", err)
	}
	contend, err := blocked.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the contender: %v", err)
	}
	defer contend.Rollback(context.Background()) //nolint:errcheck // test teardown

	start := time.Now()
	_, err = contend.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, chaosLockClass, chaosLockObj+1)
	if err == nil {
		t.Fatal("a second session took a lock the first one was holding; the planner's single-writer guarantee is not one")
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("the contender failed after %s, before lock_timeout could have fired — it failed for some other reason: %v",
			elapsed, err)
	}
}

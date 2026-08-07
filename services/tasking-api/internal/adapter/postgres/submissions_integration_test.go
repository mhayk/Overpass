package postgres_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

// The concurrency test is the reason this file exists, and what it actually
// proves is narrower than it first appears.
//
// The issue frames the choice as "unique constraint over check-then-insert".
// Both were tried here. Rewriting the adapter to SELECT-then-INSERT and running
// this test again: it PASSED. The constraint was still on the table, so the
// losing transactions hit it anyway and the outcome was identical.
//
// Dropping the primary key on idempotency_keys and running it again:
//
//     16 concurrent identical submissions created 16 requests
//
// So the mechanism is the CONSTRAINT, not the code path. This test detects the
// absence of the constraint, which is the thing that matters; it cannot
// distinguish two application-level strategies that both sit behind one. Saying
// otherwise would claim more than the evidence supports.
//
// Skipped without OVERPASS_TEST_DSN, which the stack workflow sets.

func dsn(t *testing.T) string {
	t.Helper()
	value := os.Getenv("OVERPASS_TEST_DSN")
	if value == "" {
		t.Skip("set OVERPASS_TEST_DSN to run the database integration tests")
	}
	return value
}

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedCustomer inserts the customer the foreign key requires.
func seedCustomer(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO reference.customers (customer_id, display_name) VALUES ($1, $1)
		 ON CONFLICT (customer_id) DO NOTHING`, id); err != nil {
		t.Fatalf("seeding customer: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx,
			`DELETE FROM tasking.outbox WHERE event_type = 'test.received.v1'`); err != nil {
			t.Errorf("cleanup outbox: %v", err)
		}
		// Order matters: the foreign keys point idempotency_keys at requests and
		// requests at customers, so they come off in the reverse of that.
		for _, stmt := range []string{
			`DELETE FROM tasking.idempotency_keys WHERE customer_id = $1`,
			`DELETE FROM tasking.tasking_requests WHERE customer_id = $1`,
			`DELETE FROM reference.customers WHERE customer_id = $1`,
		} {
			if _, err := pool.Exec(ctx, stmt, id); err != nil {
				t.Errorf("cleanup %q: %v", stmt, err)
			}
		}
	})
}

func request(customer string) port.StoredRequest {
	return port.StoredRequest{
		RequestID:       uuid.NewString(),
		CustomerID:      customer,
		TargetName:      "concurrency probe",
		TargetWKT:       "POINT(4.4 51.9)",
		WindowStart:     time.Now().UTC().Add(time.Hour),
		WindowEnd:       time.Now().UTC().Add(25 * time.Hour),
		PriorityTier:    "COMMERCIAL",
		BidCredits:      100,
		RequestedModes:  []string{"STRIPMAP"},
		ConstraintsJSON: []byte(`{}`),
		SubmittedAt:     time.Now().UTC(),
	}
}

func event() port.OutboxEvent {
	payload := []byte(`{"probe":"yes"}`)
	return port.OutboxEvent{
		EventID:       uuid.NewString(),
		EventType:     "test.received.v1",
		SchemaVersion: "1.0.0",
		Subject:       "tasking.request.received.v1",
		PayloadJSON:   payload,
		HeadersJSON:   []byte(`{}`),
		OccurredAt:    time.Now().UTC(),
	}
}

func claim(customer, key, fingerprint string) port.IdempotencyClaim {
	return port.IdempotencyClaim{
		CustomerID:  customer,
		Key:         key,
		Fingerprint: fingerprint,
		ExpiresAt:   time.Now().UTC().Add(24 * time.Hour),
	}
}

const digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestConcurrentIdenticalSubmissionsCreateExactlyOneRequest(t *testing.T) {
	// The acceptance test. Sixteen clients arrive together with the same key
	// and the same body; exactly one request must exist afterwards.
	const concurrency = 16

	pool := newPool(t)
	customer := "concurrency-" + uuid.NewString()[:8]
	seedCustomer(t, pool, customer)
	store := postgres.NewSubmissions(pool)
	key := "concurrent-" + uuid.NewString()[:8]

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		created  int
		replayed int
		failures []error
	)

	start := make(chan struct{})
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, to make the race as likely as possible
			replay, err := store.Save(context.Background(), claim(customer, key, digestA),
				request(customer), event())

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failures = append(failures, err)
			case replay.Replayed:
				replayed++
			default:
				created++
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range failures {
		t.Errorf("a concurrent submission failed: %v", err)
	}

	var stored int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM tasking.tasking_requests WHERE customer_id = $1`, customer,
	).Scan(&stored); err != nil {
		t.Fatalf("counting: %v", err)
	}

	if stored != 1 {
		t.Fatalf("%d concurrent identical submissions created %d requests", concurrency, stored)
	}
	if created != 1 {
		t.Fatalf("%d callers were told they created a request", created)
	}
	if replayed != concurrency-1 {
		t.Fatalf("%d replays reported, want %d", replayed, concurrency-1)
	}
}

func TestConcurrentSubmissionsProduceExactlyOneOutboxEvent(t *testing.T) {
	// A duplicate event is worse than a duplicate row: the planner would
	// allocate against a request that exists once and was announced twice.
	pool := newPool(t)
	customer := "outbox-" + uuid.NewString()[:8]
	seedCustomer(t, pool, customer)
	store := postgres.NewSubmissions(pool)
	key := "outbox-key-" + uuid.NewString()[:8]

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Errors are the point of the OTHER test; here only the count of
			// events matters, and a failure shows up as the wrong count.
			if _, err := store.Save(context.Background(), claim(customer, key, digestA),
				request(customer), event()); err != nil {
				t.Logf("submission returned %v", err)
			}
		}()
	}
	wg.Wait()

	var events int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM tasking.outbox WHERE event_type = 'test.received.v1'`,
	).Scan(&events); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if events != 1 {
		t.Fatalf("eight concurrent submissions produced %d outbox events", events)
	}
}

func TestTheSameKeyWithADifferentBodyConflicts(t *testing.T) {
	pool := newPool(t)
	customer := "conflict-" + uuid.NewString()[:8]
	seedCustomer(t, pool, customer)
	store := postgres.NewSubmissions(pool)
	key := "conflict-key-" + uuid.NewString()[:8]

	if _, err := store.Save(t.Context(), claim(customer, key, digestA), request(customer), event()); err != nil {
		t.Fatalf("first submission: %v", err)
	}
	_, err := store.Save(t.Context(), claim(customer, key, digestB), request(customer), event())
	if !errors.Is(err, port.ErrIdempotencyConflict) {
		t.Fatalf("got %v, want ErrIdempotencyConflict", err)
	}
}

func TestAFailedRequestInsertLeavesNoIdempotencyClaim(t *testing.T) {
	// The window this transaction closes. A claim without its request would
	// swallow that submission forever: every retry would look like a replay of
	// something that never happened.
	pool := newPool(t)
	customer := "atomic-" + uuid.NewString()[:8]
	seedCustomer(t, pool, customer)
	store := postgres.NewSubmissions(pool)
	key := "atomic-key-" + uuid.NewString()[:8]

	bad := request(customer)
	bad.TargetWKT = "NOT WKT AT ALL"

	if _, err := store.Save(t.Context(), claim(customer, key, digestA), bad, event()); err == nil {
		t.Fatal("an invalid geometry was accepted")
	}

	var claims int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM tasking.idempotency_keys WHERE customer_id = $1`, customer,
	).Scan(&claims); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if claims != 0 {
		t.Fatal("a failed submission left an idempotency claim behind")
	}
}

func TestExpiredKeysArePurged(t *testing.T) {
	pool := newPool(t)
	customer := "purge-" + uuid.NewString()[:8]
	seedCustomer(t, pool, customer)
	store := postgres.NewSubmissions(pool)

	expired := claim(customer, "purge-key-"+uuid.NewString()[:8], digestA)
	expired.ExpiresAt = time.Now().UTC().Add(-time.Hour)
	if _, err := store.Save(t.Context(), expired, request(customer), event()); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	removed, err := store.PurgeExpiredKeys(t.Context(), time.Now().UTC())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed < 1 {
		t.Fatal("the expired claim was not purged; the table grows without bound")
	}
}

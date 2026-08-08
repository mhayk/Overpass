package postgres_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

// The state guard lives in the database, under a row lock, and that is the only
// place it can live: three services drive this machine and two of them can be
// applying different events to the same request at the same instant. These
// tests run against a real Postgres because a read-then-write race is not a
// property of Go code.

func seedRequest(t *testing.T, pool *pgxpool.Pool, customer string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO tasking.tasking_requests
			(request_id, customer_id, target_name, target, request_window,
			 priority_tier, bid_credits, requested_modes, constraints, state, submitted_at, updated_at)
		VALUES ($1, $2, 'state probe', ST_GeomFromText('POINT(4.4 51.9)', 4326),
		        tstzrange(now(), now() + interval '1 day', '[)'),
		        'COMMERCIAL', 100, ARRAY['STRIPMAP'], '{}'::jsonb, 'RECEIVED', now(), $3)
	`, id, customer, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("seeding request: %v", err)
	}
	return id
}

func currentState(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(),
		`SELECT state FROM tasking.tasking_requests WHERE request_id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("reading state: %v", err)
	}
	return state
}

func TestALegalTransitionIsPersisted(t *testing.T) {
	pool := newPool(t)
	customer := "state-" + uuid.NewString()[:8]
	seedCustomer(t, pool, customer)
	id := seedRequest(t, pool, customer)
	states := postgres.NewStates(pool)

	got, err := states.Apply(t.Context(), id, string(domain.TriggerOpportunitiesFound), time.Now().UTC())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !got.Changed || got.To != "AWAITING_PLANNING" {
		t.Fatalf("got %+v", got)
	}
	if s := currentState(t, pool, id); s != "AWAITING_PLANNING" {
		t.Fatalf("the database says %s", s)
	}
}

func TestAnIllegalTransitionIsRefusedAndChangesNothing(t *testing.T) {
	// Refused and reported, never silently ignored. An ignored illegal
	// transition is a bug that has already happened and left no evidence.
	pool := newPool(t)
	customer := "illegal-" + uuid.NewString()[:8]
	seedCustomer(t, pool, customer)
	id := seedRequest(t, pool, customer)
	states := postgres.NewStates(pool)

	_, err := states.Apply(t.Context(), id, string(domain.TriggerAcquired), time.Now().UTC())
	if err == nil {
		t.Fatal("ACQUIRED was accepted straight from RECEIVED")
	}
	var te *domain.TransitionError
	if !errors.As(err, &te) {
		t.Fatalf("got %T, want a TransitionError the caller can log", err)
	}
	if s := currentState(t, pool, id); s != "RECEIVED" {
		t.Fatalf("a refused transition moved the state to %s", s)
	}
}

func TestAStaleEventDoesNotRewindTheState(t *testing.T) {
	// Redelivery is out of band and consumers run concurrently, so an older
	// event arriving after a newer one is normal. Applying it would rewind a
	// request that has already moved on.
	pool := newPool(t)
	customer := "stale-" + uuid.NewString()[:8]
	seedCustomer(t, pool, customer)
	id := seedRequest(t, pool, customer)
	states := postgres.NewStates(pool)

	recent := time.Now().UTC()
	if _, err := states.Apply(t.Context(), id, string(domain.TriggerOpportunitiesFound), recent); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	got, err := states.Apply(t.Context(), id,
		string(domain.TriggerOpportunitiesFound), recent.Add(-time.Hour))
	if err != nil {
		t.Fatalf("a stale event errored: %v", err)
	}
	if got.Changed {
		t.Fatal("a stale event was applied")
	}
	if s := currentState(t, pool, id); s != "AWAITING_PLANNING" {
		t.Fatalf("state is %s", s)
	}
}

func TestConcurrentTransitionsDoNotLoseOne(t *testing.T) {
	// The reason the guard is under a row lock. Two consumers apply two
	// different legal transitions at the same instant; exactly one must win and
	// the loser must be refused rather than silently overwriting.
	pool := newPool(t)
	customer := "race-" + uuid.NewString()[:8]
	seedCustomer(t, pool, customer)
	id := seedRequest(t, pool, customer)
	states := postgres.NewStates(pool)

	at := time.Now().UTC()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted []string
	)
	for _, trigger := range []domain.Trigger{
		domain.TriggerOpportunitiesFound, domain.TriggerFeasibilityFailed,
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := states.Apply(t.Context(), id, string(trigger), at)
			if err == nil && got.Changed {
				mu.Lock()
				accepted = append(accepted, got.To)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(accepted) != 1 {
		t.Fatalf("%d transitions were accepted: %v", len(accepted), accepted)
	}
	if s := currentState(t, pool, id); s != accepted[0] {
		t.Fatalf("the database says %s but %s was accepted", s, accepted[0])
	}
}

func TestAnUnknownRequestIsReportedNotRetriedForever(t *testing.T) {
	// A real case rather than a bug: events can arrive for a request that was
	// purged. The caller acks instead of burning its redelivery budget.
	pool := newPool(t)
	states := postgres.NewStates(pool)

	_, err := states.Apply(t.Context(), uuid.NewString(),
		string(domain.TriggerOpportunitiesFound), time.Now().UTC())
	if !errors.Is(err, port.ErrRequestNotFound) {
		t.Fatalf("got %v, want ErrRequestNotFound", err)
	}
}

func TestATerminalRequestIsNotReopened(t *testing.T) {
	pool := newPool(t)
	customer := "terminal-" + uuid.NewString()[:8]
	seedCustomer(t, pool, customer)
	id := seedRequest(t, pool, customer)
	states := postgres.NewStates(pool)

	at := time.Now().UTC()
	if _, err := states.Apply(t.Context(), id, string(domain.TriggerCancelled), at); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := states.Apply(t.Context(), id,
		string(domain.TriggerOpportunitiesFound), at.Add(time.Minute)); err == nil {
		t.Fatal("a cancelled request was reopened")
	}
}

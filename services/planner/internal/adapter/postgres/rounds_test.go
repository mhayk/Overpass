package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/planner/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// The advisory lock is the whole of M2-01, and the only honest way to test it is
// against a real Postgres with real concurrency. A fake lock would agree with
// whatever this file asserts.

const bucketDuration = 3 * time.Hour

// span records when one attempt held the lock.
type span struct {
	key   string
	start time.Time
	end   time.Time
}

func overlaps(a, b span) bool {
	return a.start.Before(b.end) && b.start.Before(a.end)
}

func seedBucket(t *testing.T, p *pgxpool.Pool, satellite string, bucketStart time.Time, candidates int) {
	t.Helper()
	seedSatellite(t, p, satellite)
	customer := fmt.Sprintf("cust-%d", time.Now().UnixNano())
	seedCustomer(t, p, customer)

	requestID := uuid.NewString()
	projections := postgres.NewProjections(p)
	if _, err := projections.ProjectSnapshot(context.Background(), port.ConsumerLifecycle,
		snapshotEvent(uuid.NewString(), requestID, customer)); err != nil {
		t.Fatalf("seeding the snapshot: %v", err)
	}

	ids := make([]string, 0, candidates)
	for range candidates {
		ids = append(ids, uuid.NewString())
	}
	event := candidateEvent(uuid.NewString(), requestID, satellite, ids...)
	// Pin every candidate's access window inside the target bucket, since the
	// bucket is derived from lower(access_window).
	for i := range event.Candidates {
		event.Candidates[i].AccessStart = bucketStart.Add(time.Duration(i) * time.Minute)
		event.Candidates[i].AccessEnd = bucketStart.Add(time.Duration(i)*time.Minute + 5*time.Minute)
	}
	if _, err := projections.ProjectCandidates(context.Background(), port.ConsumerOpportunities, event); err != nil {
		t.Fatalf("seeding candidates: %v", err)
	}
}

// TestSameKeySerialisesAndDifferentKeysOverlap is #33's load-bearing test.
//
// Serialisation alone is not the claim. A global mutex would serialise too, and
// would also destroy the property that makes serialising the planner acceptable:
// different satellites plan in parallel. Both halves are asserted, because
// either one on its own is satisfied by a design the other rejects.
func TestSameKeySerialisesAndDifferentKeysOverlap(t *testing.T) {
	p := pool(t)
	rounds := postgres.NewRounds(p)
	ctx := context.Background()

	// A bucket far in the future, so no other test's data lands in it.
	bucketStart := domain.BucketStart(time.Now().UTC().Add(400*time.Hour), bucketDuration)
	bucketEnd := bucketStart.Add(bucketDuration)

	satA := fmt.Sprintf("SAT-LA%d", time.Now().UnixNano()%100000)
	satB := fmt.Sprintf("SAT-LB%d", time.Now().UnixNano()%100000+1)
	seedBucket(t, p, satA, bucketStart, 2)
	seedBucket(t, p, satB, bucketStart, 2)

	// Long enough that a serialisation failure is unambiguous, short enough that
	// the test stays quick. Held INSIDE the callback, which runs inside the
	// locked transaction.
	const hold = 300 * time.Millisecond

	var (
		mu       sync.Mutex
		observed []span
		wg       sync.WaitGroup
	)

	attempt := func(satellite string) {
		defer wg.Done()
		key := domain.RoundKey{SatelliteID: satellite, BucketStart: bucketStart}
		_, err := rounds.OpenRound(ctx, key, bucketEnd,
			func(port.RoundInputs) (port.Round, []byte, error) {
				start := time.Now()
				time.Sleep(hold)
				mu.Lock()
				observed = append(observed, span{key: satellite, start: start, end: time.Now()})
				mu.Unlock()
				// Skip: this test is about the lock, not about what gets
				// written. Rolling back also releases the lock through the same
				// path every error takes.
				return port.Round{}, nil, port.ErrSkipRound
			})
		if err != nil {
			t.Errorf("OpenRound(%s): %v", satellite, err)
		}
	}

	wg.Add(4)
	go attempt(satA)
	go attempt(satA)
	go attempt(satB)
	go attempt(satB)
	wg.Wait()

	if len(observed) != 4 {
		t.Fatalf("recorded %d attempts, want 4", len(observed))
	}

	var sameKeyPairs, crossKeyOverlaps int
	for i := range observed {
		for j := i + 1; j < len(observed); j++ {
			a, b := observed[i], observed[j]
			if a.key == b.key {
				sameKeyPairs++
				if overlaps(a, b) {
					t.Errorf("two rounds held %s at once: [%s..%s] and [%s..%s] — the advisory lock did not serialise, and two customers could win the same window",
						a.key, a.start.Format(time.StampMilli), a.end.Format(time.StampMilli),
						b.start.Format(time.StampMilli), b.end.Format(time.StampMilli))
				}
				continue
			}
			if overlaps(a, b) {
				crossKeyOverlaps++
			}
		}
	}

	if sameKeyPairs != 2 {
		t.Fatalf("compared %d same-key pairs, want 2 — the test is not exercising what it claims", sameKeyPairs)
	}
	if crossKeyOverlaps == 0 {
		t.Error("no two satellites ever held their locks at the same time; the lock is global rather than per-satellite, which is a throughput ceiling across the whole constellation")
	}
}

// The lock must release on the error path too. A leaked lock is a satellite that
// can never be planned again until the connection is recycled, and it would look
// exactly like a planner that had quietly stopped.
func TestTheLockIsReleasedAfterAFailedRound(t *testing.T) {
	p := pool(t)
	rounds := postgres.NewRounds(p)
	ctx := context.Background()

	bucketStart := domain.BucketStart(time.Now().UTC().Add(500*time.Hour), bucketDuration)
	bucketEnd := bucketStart.Add(bucketDuration)
	satellite := fmt.Sprintf("SAT-LC%d", time.Now().UnixNano()%100000)
	seedBucket(t, p, satellite, bucketStart, 1)

	key := domain.RoundKey{SatelliteID: satellite, BucketStart: bucketStart}

	if _, err := rounds.OpenRound(ctx, key, bucketEnd,
		func(port.RoundInputs) (port.Round, []byte, error) {
			return port.Round{}, nil, fmt.Errorf("the round blew up")
		}); err == nil {
		t.Fatal("a failing round reported success")
	}

	// If the lock leaked, this blocks forever. The timeout is what turns that
	// into a failure rather than a hung suite.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := rounds.OpenRound(ctx, key, bucketEnd,
			func(port.RoundInputs) (port.Round, []byte, error) {
				return port.Round{}, nil, port.ErrSkipRound
			}); err != nil {
			t.Errorf("re-acquiring the lock: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the lock was still held after a failed round; that satellite can never be planned again")
	}

	var held int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'`).Scan(&held); err != nil {
		t.Fatalf("reading pg_locks: %v", err)
	}
	if held != 0 {
		t.Errorf("%d advisory locks are still held after every round finished", held)
	}
}

// A round and its outbox row are written in ONE transaction, per ADR-0006.
func TestOpeningARoundRecordsItAndEnqueuesTheEvent(t *testing.T) {
	p := pool(t)
	rounds := postgres.NewRounds(p)
	ctx := context.Background()

	bucketStart := domain.BucketStart(time.Now().UTC().Add(600*time.Hour), bucketDuration)
	bucketEnd := bucketStart.Add(bucketDuration)
	satellite := fmt.Sprintf("SAT-LD%d", time.Now().UnixNano()%100000)
	seedBucket(t, p, satellite, bucketStart, 3)

	key := domain.RoundKey{SatelliteID: satellite, BucketStart: bucketStart}
	roundID, eventID := uuid.NewString(), uuid.NewString()

	var seen port.RoundInputs
	opened, err := rounds.OpenRound(ctx, key, bucketEnd,
		func(inputs port.RoundInputs) (port.Round, []byte, error) {
			seen = inputs
			return port.Round{
				RoundID:                   roundID,
				EventID:                   eventID,
				CorrelationID:             uuid.NewString(),
				Key:                       key,
				BucketEnd:                 bucketEnd,
				Trigger:                   domain.TriggerCadence,
				Policy:                    "GREEDY_BY_BID",
				CandidateOpportunityCount: inputs.CandidateOpportunityCount,
				CandidateRequestIDs:       inputs.CandidateRequestIDs,
				DutyCycleBudgetS:          inputs.DutyCycleBudgetS,
				TriggeredAt:               time.Now().UTC(),
			}, []byte(`{"event_id":"` + eventID + `"}`), nil
		})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if !opened {
		t.Fatal("the round reported as not opened")
	}

	// The inputs were read UNDER the lock, not carried in from the sweep.
	if seen.CandidateOpportunityCount != 3 {
		t.Errorf("read %d candidates under the lock, want 3", seen.CandidateOpportunityCount)
	}
	if len(seen.CandidateRequestIDs) != 1 {
		t.Errorf("read %d competing requests, want 1", len(seen.CandidateRequestIDs))
	}
	if seen.DutyCycleBudgetS <= 0 {
		t.Errorf("duty-cycle budget = %v; the round must record the number it actually used", seen.DutyCycleBudgetS)
	}

	var (
		trigger string
		count   int
	)
	if err := p.QueryRow(ctx,
		`SELECT trigger, candidate_opportunity_count FROM planning.rounds WHERE round_id = $1`,
		roundID).Scan(&trigger, &count); err != nil {
		t.Fatalf("reading the round back: %v", err)
	}
	if trigger != domain.TriggerCadence || count != 3 {
		t.Errorf("stored trigger=%q count=%d", trigger, count)
	}

	var queued int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.outbox WHERE event_id = $1 AND published_at IS NULL`,
		eventID).Scan(&queued); err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	if queued != 1 {
		t.Errorf("%d outbox rows for the round event, want 1 — the event and the round must be written together or the round is announced to nobody", queued)
	}
}

// A round that fails to record must leave NO outbox row, or the system announces
// a decision boundary that never happened.
func TestAFailedRoundAnnouncesNothing(t *testing.T) {
	p := pool(t)
	rounds := postgres.NewRounds(p)
	ctx := context.Background()

	bucketStart := domain.BucketStart(time.Now().UTC().Add(700*time.Hour), bucketDuration)
	bucketEnd := bucketStart.Add(bucketDuration)
	satellite := fmt.Sprintf("SAT-LE%d", time.Now().UnixNano()%100000)
	seedBucket(t, p, satellite, bucketStart, 1)

	key := domain.RoundKey{SatelliteID: satellite, BucketStart: bucketStart}
	eventID := uuid.NewString()

	// REPLAN with nothing to supersede: rounds_supersedes_iff_replan rejects it.
	_, err := rounds.OpenRound(ctx, key, bucketEnd,
		func(inputs port.RoundInputs) (port.Round, []byte, error) {
			return port.Round{
				RoundID:                   uuid.NewString(),
				EventID:                   eventID,
				CorrelationID:             uuid.NewString(),
				Key:                       key,
				BucketEnd:                 bucketEnd,
				Trigger:                   domain.TriggerReplan,
				Policy:                    "GREEDY_BY_BID",
				CandidateOpportunityCount: inputs.CandidateOpportunityCount,
				CandidateRequestIDs:       inputs.CandidateRequestIDs,
				DutyCycleBudgetS:          inputs.DutyCycleBudgetS,
				SupersedesPlanID:          nil,
				TriggeredAt:               time.Now().UTC(),
			}, []byte(`{}`), nil
		})
	if err == nil {
		t.Fatal("a REPLAN with nothing to supersede was accepted; the CHECK is not doing its job")
	}

	var queued int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.outbox WHERE event_id = $1`, eventID).Scan(&queued); err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	if queued != 0 {
		t.Error("the failed round left an outbox row; a decision boundary that never happened would be announced to every consumer")
	}
}

// DirtyBuckets must find a bucket with new candidates, and stop finding it once
// a round has covered them. That transition IS ADR-0014's rule.
func TestDirtyBucketsClearsAfterARound(t *testing.T) {
	p := pool(t)
	rounds := postgres.NewRounds(p)
	ctx := context.Background()

	bucketStart := domain.BucketStart(time.Now().UTC().Add(800*time.Hour), bucketDuration)
	bucketEnd := bucketStart.Add(bucketDuration)
	satellite := fmt.Sprintf("SAT-LF%d", time.Now().UnixNano()%100000)
	seedBucket(t, p, satellite, bucketStart, 2)

	query := port.BucketQuery{
		BucketDuration: bucketDuration,
		HorizonStart:   bucketStart,
		HorizonEnd:     bucketEnd,
		Limit:          50,
	}

	before, err := rounds.DirtyBuckets(ctx, query)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	found := findBucket(before, satellite)
	if found == nil {
		t.Fatal("a bucket with brand-new candidates was not reported dirty")
	}
	if found.PendingCandidates != 2 {
		t.Errorf("pending = %d, want 2", found.PendingCandidates)
	}
	if found.LastRoundAt != nil {
		t.Error("a never-planned bucket reported a last round")
	}
	if found.TippingEventID == "" {
		t.Error("no tipping event; causation_id would be null on a debounce-fired round")
	}

	key := domain.RoundKey{SatelliteID: satellite, BucketStart: bucketStart}
	if _, openErr := rounds.OpenRound(ctx, key, bucketEnd,
		func(inputs port.RoundInputs) (port.Round, []byte, error) {
			eventID := uuid.NewString()
			return port.Round{
				RoundID: uuid.NewString(), EventID: eventID, CorrelationID: uuid.NewString(),
				Key: key, BucketEnd: bucketEnd,
				Trigger: domain.TriggerCadence, Policy: "GREEDY_BY_BID",
				CandidateOpportunityCount: inputs.CandidateOpportunityCount,
				CandidateRequestIDs:       inputs.CandidateRequestIDs,
				DutyCycleBudgetS:          inputs.DutyCycleBudgetS,
				TriggeredAt:               time.Now().UTC(),
			}, []byte(`{"event_id":"` + eventID + `"}`), nil
		}); openErr != nil {
		t.Fatalf("opening: %v", openErr)
	}

	after, err := rounds.DirtyBuckets(ctx, query)
	if err != nil {
		t.Fatalf("re-sweeping: %v", err)
	}
	if still := findBucket(after, satellite); still != nil {
		t.Errorf("the bucket is still dirty after a round covered it (pending=%d); it would refire forever and hot-loop the advisory lock",
			still.PendingCandidates)
	}
}

func findBucket(states []domain.BucketState, satellite string) *domain.BucketState {
	for i := range states {
		if states[i].Key.SatelliteID == satellite {
			return &states[i]
		}
	}
	return nil
}

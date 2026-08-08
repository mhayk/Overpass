package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/planner/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// These run against a real Postgres because every claim they make is about the
// database's behaviour, not the code's. ADR-0015 chose to HOLD a candidate whose
// request snapshot has not arrived, and that choice lives in the absence of a
// foreign key — something no in-memory fake can be wrong about in the same way.

func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv("OVERPASS_TEST_DSN")
	if v == "" {
		t.Skip("set OVERPASS_TEST_DSN to run the projection tests")
	}
	return v
}

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), dsn(t))
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(p.Close)
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("pinging: %v", err)
	}
	return p
}

// unique keeps parallel runs and repeated runs from colliding without needing a
// truncate between tests. Truncating planning.* would race any other test in
// the same job.
func unique(t *testing.T, n int) string {
	t.Helper()
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", uint32(time.Now().UnixNano()), n)
}

func seedCustomer(t *testing.T, p *pgxpool.Pool, id string) {
	t.Helper()
	// No tier column here, deliberately checked rather than assumed: the tier
	// lives on the REQUEST, not the customer, because a customer can submit at
	// different tiers and the planner allocates per request.
	_, err := p.Exec(context.Background(), `
		INSERT INTO reference.customers (customer_id, display_name)
		VALUES ($1, $1) ON CONFLICT DO NOTHING`, id)
	if err != nil {
		// The column set is asserted by the migration, not by this test. If it
		// has moved, say so plainly rather than failing later with a confusing
		// foreign-key error.
		t.Fatalf("seeding customer %s: %v", id, err)
	}
}

func seedSatellite(t *testing.T, p *pgxpool.Pool, id string) {
	t.Helper()
	_, err := p.Exec(context.Background(), `
		INSERT INTO reference.satellites (satellite_id, norad_id, display_name, sensor_modes, duty_cycle_budget_s)
		VALUES ($1, $2, $1, '{}'::jsonb, 600) ON CONFLICT DO NOTHING`,
		id, time.Now().UnixNano()%900000+1000)
	if err != nil {
		t.Fatalf("seeding satellite %s: %v", id, err)
	}
}

func snapshotEvent(eventID, requestID, customerID string) port.RequestReceived {
	return port.RequestReceived{
		EventID: eventID,
		EventAt: time.Now().UTC(),
		Snapshot: domain.Snapshot{
			RequestID:     requestID,
			CustomerID:    customerID,
			PriorityTier:  "COMMERCIAL",
			BidCredits:    500,
			WindowStart:   time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC),
			WindowEnd:     time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC),
			SubmittedAt:   time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
			SourceEventID: eventID,
			OccurredAt:    time.Now().UTC(),
		},
	}
}

func candidateEvent(eventID, requestID, satelliteID string, opportunityIDs ...string) port.OpportunitiesComputed {
	e := port.OpportunitiesComputed{
		EventID:   eventID,
		EventAt:   time.Now().UTC(),
		RequestID: requestID,
	}
	for i, id := range opportunityIDs {
		orbit := 47110 + i
		e.Candidates = append(e.Candidates, domain.Candidate{
			OpportunityID:        id,
			RequestID:            requestID,
			SatelliteID:          satelliteID,
			Mode:                 "STRIPMAP",
			AccessStart:          time.Date(2026, 8, 7, 10, 14, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour),
			AccessEnd:            time.Date(2026, 8, 7, 10, 16, 30, 0, time.UTC).Add(time.Duration(i) * time.Hour),
			AcquisitionDurationS: 18.5,
			OrbitNumber:          &orbit,
			DutyCycleCostS:       18.5,
			QualityScore:         0.87,
			GeometryJSON:         []byte(`{"incidence_angle_deg":32.5,"look_side":"RIGHT","squint_angle_deg":0.1}`),
			FootprintGeoJSON:     []byte(`{"type":"Polygon","coordinates":[[[0,0],[0,1],[1,1],[1,0],[0,0]]]}`),
			ComputedAt:           time.Now().UTC(),
			SourceEventID:        eventID,
		})
	}
	return e
}

func TestSnapshotIsProjected(t *testing.T) {
	p := pool(t)
	projections := postgres.NewProjections(p)
	ctx := context.Background()

	customer := fmt.Sprintf("cust-%d", time.Now().UnixNano())
	seedCustomer(t, p, customer)
	requestID, eventID := unique(t, 1), unique(t, 2)

	applied, err := projections.ProjectSnapshot(ctx, port.ConsumerLifecycle,
		snapshotEvent(eventID, requestID, customer))
	if err != nil {
		t.Fatalf("projecting: %v", err)
	}
	if !applied {
		t.Fatal("a first-time event reported as already processed")
	}

	var (
		tier string
		bid  int64
	)
	err = p.QueryRow(ctx, `SELECT priority_tier, bid_credits FROM planning.request_snapshots WHERE request_id = $1`,
		requestID).Scan(&tier, &bid)
	if err != nil {
		t.Fatalf("reading back the snapshot: %v", err)
	}
	if tier != "COMMERCIAL" || bid != 500 {
		t.Errorf("stored tier=%q bid=%d, want COMMERCIAL/500", tier, bid)
	}
}

// Redelivery is the normal case, not the exceptional one. The outbox publishes
// at-least-once by design, so this path runs in production constantly.
func TestRedeliveryIsAbsorbed(t *testing.T) {
	p := pool(t)
	projections := postgres.NewProjections(p)
	ctx := context.Background()

	customer := fmt.Sprintf("cust-%d", time.Now().UnixNano())
	seedCustomer(t, p, customer)
	requestID, eventID := unique(t, 3), unique(t, 4)
	event := snapshotEvent(eventID, requestID, customer)

	first, err := projections.ProjectSnapshot(ctx, port.ConsumerLifecycle, event)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	second, err := projections.ProjectSnapshot(ctx, port.ConsumerLifecycle, event)
	if err != nil {
		t.Fatalf("redelivery errored instead of being absorbed: %v", err)
	}

	if !first || second {
		t.Errorf("applied first=%v second=%v, want true then false", first, second)
	}

	var rows int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM planning.request_snapshots WHERE request_id = $1`,
		requestID).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d snapshot rows after a redelivery, want 1", rows)
	}
}

// The dedup ledger is partitioned by consumer, per ADR-0008. The same event id
// arriving on a different consumer is a SEPARATE fact and must be applied.
func TestTheLedgerIsPartitionedByConsumer(t *testing.T) {
	p := pool(t)
	projections := postgres.NewProjections(p)
	ctx := context.Background()

	customer := fmt.Sprintf("cust-%d", time.Now().UnixNano())
	seedCustomer(t, p, customer)
	requestID, eventID := unique(t, 5), unique(t, 6)
	event := snapshotEvent(eventID, requestID, customer)

	if _, err := projections.ProjectSnapshot(ctx, port.ConsumerLifecycle, event); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	applied, err := projections.ProjectSnapshot(ctx, port.ConsumerOpportunities, event)
	if err != nil {
		t.Fatalf("opportunities: %v", err)
	}
	if !applied {
		t.Error("the same event id on a second consumer was treated as a duplicate; " +
			"the ledger is keyed on event_id alone and the two streams mask each other")
	}
}

// THE ADR-0015 TEST. A candidate whose request snapshot has not arrived must be
// STORED, not rejected — the events race, and a foreign key here would turn a
// benign ordering fact into a retry storm.
func TestCandidateArrivingBeforeItsSnapshotIsHeldNotRejected(t *testing.T) {
	p := pool(t)
	projections := postgres.NewProjections(p)
	ctx := context.Background()

	satellite := fmt.Sprintf("SAT-T%d", time.Now().UnixNano()%100000)
	seedSatellite(t, p, satellite)
	requestID, eventID, opportunityID := unique(t, 7), unique(t, 8), unique(t, 9)

	// No snapshot projected. This is the out-of-order arrival.
	applied, err := projections.ProjectCandidates(ctx, port.ConsumerOpportunities,
		candidateEvent(eventID, requestID, satellite, opportunityID))
	if err != nil {
		t.Fatalf("a candidate arriving before its request was REJECTED; ADR-0015 requires it held: %v", err)
	}
	if !applied {
		t.Fatal("the candidate was not applied")
	}

	// Stored...
	var stored int
	if countErr := p.QueryRow(ctx, `SELECT count(*) FROM planning.candidate_opportunities WHERE opportunity_id = $1`,
		opportunityID).Scan(&stored); countErr != nil {
		t.Fatalf("counting: %v", countErr)
	}
	if stored != 1 {
		t.Fatalf("%d candidate rows, want 1", stored)
	}

	// ...and invisible to the round's join, which is what "held" means.
	var joinable int
	err = p.QueryRow(ctx, `
		SELECT count(*)
		FROM planning.candidate_opportunities c
		JOIN planning.request_snapshots s ON s.request_id = c.request_id
		WHERE c.opportunity_id = $1`, opportunityID).Scan(&joinable)
	if err != nil {
		t.Fatalf("joining: %v", err)
	}
	if joinable != 0 {
		t.Error("a candidate with no snapshot is visible to the round's join; it has no bid and no deadline, so allocating it would be guessing")
	}

	// Then the snapshot lands, and the same candidate becomes visible without
	// anything re-delivering it. That is the whole point of holding rather than
	// dropping.
	customer := fmt.Sprintf("cust-%d", time.Now().UnixNano())
	seedCustomer(t, p, customer)
	if _, projectErr := projections.ProjectSnapshot(ctx, port.ConsumerLifecycle,
		snapshotEvent(unique(t, 10), requestID, customer)); projectErr != nil {
		t.Fatalf("projecting the late snapshot: %v", projectErr)
	}

	err = p.QueryRow(ctx, `
		SELECT count(*)
		FROM planning.candidate_opportunities c
		JOIN planning.request_snapshots s ON s.request_id = c.request_id
		WHERE c.opportunity_id = $1`, opportunityID).Scan(&joinable)
	if err != nil {
		t.Fatalf("re-joining: %v", err)
	}
	if joinable != 1 {
		t.Error("the candidate stayed invisible after its snapshot arrived; a held candidate that never becomes visible is a dropped one")
	}
}

func TestAllCandidatesInOneEventAreStored(t *testing.T) {
	p := pool(t)
	projections := postgres.NewProjections(p)
	ctx := context.Background()

	satellite := fmt.Sprintf("SAT-M%d", time.Now().UnixNano()%100000)
	seedSatellite(t, p, satellite)
	requestID, eventID := unique(t, 11), unique(t, 12)
	ids := []string{unique(t, 13), unique(t, 14), unique(t, 15)}

	if _, err := projections.ProjectCandidates(ctx, port.ConsumerOpportunities,
		candidateEvent(eventID, requestID, satellite, ids...)); err != nil {
		t.Fatalf("projecting: %v", err)
	}

	var stored int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM planning.candidate_opportunities WHERE request_id = $1`,
		requestID).Scan(&stored); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if stored != len(ids) {
		t.Errorf("%d of %d candidates stored — a partially projected event lets a round allocate over some of a request's options and call the rest unfulfilled", stored, len(ids))
	}
}

// The batch and the ledger entry commit together or not at all. If a candidate
// in the batch is unstorable, NOTHING may remain — including the ledger row,
// or the redelivery would be absorbed as a duplicate and the event lost for
// good.
func TestAFailedBatchLeavesNothingBehind(t *testing.T) {
	p := pool(t)
	projections := postgres.NewProjections(p)
	ctx := context.Background()

	requestID, eventID := unique(t, 16), unique(t, 17)
	// A satellite that does not exist. The foreign key on
	// candidate_opportunities.satellite_id is real, unlike the deliberately
	// absent one on request_id.
	event := candidateEvent(eventID, requestID, "SAT-DOES-NOT-EXIST", unique(t, 18))

	if _, err := projections.ProjectCandidates(ctx, port.ConsumerOpportunities, event); err == nil {
		t.Fatal("an unknown satellite was accepted; the foreign key is not doing its job")
	}

	var ledger int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.processed_events WHERE consumer = $1 AND event_id = $2`,
		port.ConsumerOpportunities, eventID).Scan(&ledger); err != nil {
		t.Fatalf("counting ledger rows: %v", err)
	}
	if ledger != 0 {
		t.Error("the failed event was marked processed; its redelivery would be absorbed as a duplicate and the event lost for good")
	}

	var candidates int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM planning.candidate_opportunities WHERE request_id = $1`,
		requestID).Scan(&candidates); err != nil {
		t.Fatalf("counting candidates: %v", err)
	}
	if candidates != 0 {
		t.Errorf("%d candidate rows survived a failed transaction", candidates)
	}
}

// An empty event_id would become an empty dedup key, so every such message
// would look like a redelivery of the same one and all but the first would be
// silently dropped. Silent data loss earns a second line of defence.
func TestEmptyEventIDIsRefused(t *testing.T) {
	p := pool(t)
	projections := postgres.NewProjections(p)
	ctx := context.Background()

	customer := fmt.Sprintf("cust-%d", time.Now().UnixNano())
	seedCustomer(t, p, customer)

	_, err := projections.ProjectSnapshot(ctx, port.ConsumerLifecycle,
		snapshotEvent("", unique(t, 19), customer))
	if err == nil {
		t.Fatal("an event with no id entered the ledger under an empty key")
	}
}

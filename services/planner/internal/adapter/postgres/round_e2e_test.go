package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mhayk/overpass/services/planner/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/planner/internal/allocation"
	"github.com/mhayk/overpass/services/planner/internal/app"
	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// The whole of M2 in one pass: candidates arrive, the sweep notices, the round
// opens under the lock, the policy allocates, and ONE transaction commits the
// plan, its acquisitions, and every event — round.triggered, plan.committed,
// and one request.unfulfilled per loser.
func TestAFullRoundAllocatesAndAnnounces(t *testing.T) {
	p := pool(t)
	ctx := context.Background()

	satellite := fmt.Sprintf("SAT-E2E%d", time.Now().UnixNano()%100000)
	seedSatellite(t, p, satellite)
	// A budget that fits one 30 s acquisition, so the round is CONTESTED.
	if _, err := p.Exec(ctx,
		`UPDATE reference.satellites SET duty_cycle_budget_s = 40 WHERE satellite_id = $1`,
		satellite); err != nil {
		t.Fatalf("configuring the budget: %v", err)
	}

	customer := fmt.Sprintf("cust-%d", time.Now().UnixNano())
	seedCustomer(t, p, customer)
	projections := postgres.NewProjections(p)

	// Two requests, each with one candidate wanting the same stretch of time.
	// The rich one wins; the poor one gets the structured explanation.
	accessStart := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	seed := func(bid int64) string {
		requestID := uuid.NewString()
		snapshot := snapshotEvent(uuid.NewString(), requestID, customer)
		snapshot.Snapshot.BidCredits = bid
		// The helper's fixed 2026-08-07 window is in the past by the time this
		// test runs against the real clock, and a past deadline fails every
		// candidate with DEADLINE_PASSED — correctly. The request must cover
		// the access window it is bidding for.
		snapshot.Snapshot.WindowStart = accessStart.Add(-time.Hour)
		snapshot.Snapshot.WindowEnd = accessStart.Add(6 * time.Hour)
		snapshot.Snapshot.SubmittedAt = time.Now().UTC().Add(-2 * time.Hour)
		if _, err := projections.ProjectSnapshot(ctx, port.ConsumerLifecycle, snapshot); err != nil {
			t.Fatalf("seeding snapshot: %v", err)
		}
		event := candidateEvent(uuid.NewString(), requestID, satellite, uuid.NewString())
		event.Candidates[0].AccessStart = accessStart
		event.Candidates[0].AccessEnd = accessStart.Add(10 * time.Minute)
		event.Candidates[0].AcquisitionDurationS = 30
		event.Candidates[0].DutyCycleCostS = 30
		event.Candidates[0].GeometryJSON = []byte(
			`{"incidence_angle_deg":30,"look_side":"RIGHT","squint_angle_deg":0.1,"slant_range_km":570.51,"elevation_angle_deg":55.2}`)
		if _, err := projections.ProjectCandidates(ctx, port.ConsumerOpportunities, event); err != nil {
			t.Fatalf("seeding candidate: %v", err)
		}
		return requestID
	}
	richRequest := seed(900)
	poorRequest := seed(100)

	trigger, err := app.NewTrigger(postgres.NewRounds(p), app.TriggerConfig{
		Policy: domain.TriggerPolicy{
			// The candidates were inserted moments ago; a nanosecond of quiet
			// has always elapsed by the time the sweep runs, so the round fires
			// on the first pass without the test sleeping.
			QuietPeriod:      time.Nanosecond,
			StalenessCeiling: time.Hour,
		},
		BucketDuration: 3 * time.Hour,
		HorizonAhead:   24 * time.Hour,
		SweepLimit:     16,
		Allocator:      allocation.GreedyByBid{},
		Fairness:       domain.DefaultFairness(),
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("wiring the trigger: %v", err)
	}

	stats, err := trigger.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if stats.Opened != 1 {
		t.Fatalf("stats = %+v, want one round opened", stats)
	}

	// The plan and its acquisition are in the database, atomically with the
	// round.
	var planID string
	var version int
	if planErr := p.QueryRow(ctx, `
		SELECT plan_id::text, plan_version FROM planning.collection_plans
		WHERE satellite_id = $1 ORDER BY committed_at DESC LIMIT 1
	`, satellite).Scan(&planID, &version); planErr != nil {
		t.Fatalf("no plan was committed: %v", planErr)
	}
	if version != 1 {
		t.Errorf("plan version = %d, want 1", version)
	}

	var winner string
	if winnerErr := p.QueryRow(ctx, `
		SELECT request_id::text FROM planning.acquisitions
		WHERE plan_id = $1 AND status = 'ACTIVE'
	`, planID).Scan(&winner); winnerErr != nil {
		t.Fatalf("no acquisition: %v", winnerErr)
	}
	if winner != richRequest {
		t.Errorf("the winner is %s, want the 900-credit request %s", winner, richRequest)
	}

	// All three event kinds queued in the same transaction.
	rows, queryErr := p.Query(ctx, `
		SELECT event_type, payload::text FROM planning.outbox
		WHERE occurred_at > now() - interval '2 minutes'
		ORDER BY id
	`)
	if queryErr != nil {
		t.Fatalf("reading the outbox: %v", queryErr)
	}
	defer rows.Close()

	kinds := map[string]int{}
	var unfulfilledPayload string
	for rows.Next() {
		var kind, payload string
		if scanErr := rows.Scan(&kind, &payload); scanErr != nil {
			t.Fatalf("scanning: %v", scanErr)
		}
		kinds[kind]++
		if kind == "planning.request.unfulfilled.v1" && unfulfilledPayload == "" {
			// Ours specifically — the outbox is shared across tests.
			var envelope map[string]any
			if decodeErr := json.Unmarshal([]byte(payload), &envelope); decodeErr != nil {
				t.Fatalf("an outbox payload is not JSON: %v", decodeErr)
			}
			if data, ok := envelope["data"].(map[string]any); ok && data["request_id"] == poorRequest {
				unfulfilledPayload = payload
			}
		}
	}
	if kinds["planning.round.triggered.v1"] == 0 || kinds["planning.plan.committed.v1"] == 0 {
		t.Fatalf("outbox kinds = %v; the round and the plan must both announce", kinds)
	}
	if unfulfilledPayload == "" {
		t.Fatal("the losing request has no unfulfilment event; it silently vanished")
	}

	// The loser's event is structured: reason, numbers, the ghost to render.
	var envelope map[string]any
	if unmarshalErr := json.Unmarshal([]byte(unfulfilledPayload), &envelope); unmarshalErr != nil {
		t.Fatalf("unfulfilment payload: %v", unmarshalErr)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatal("the unfulfilment data is not an object")
	}
	if data["reason_code"] != domain.ReasonDutyCycle {
		t.Errorf("reason = %v, want %s", data["reason_code"], domain.ReasonDutyCycle)
	}
	if data["eligible_for_retry"] != true {
		t.Error("a competitive loss must stay in contention")
	}
	age, _ := data["age_rounds"].(float64) //nolint:errcheck // a missing field reads as 0 and fails the assertion below
	if age < 1 {
		t.Errorf("age_rounds = %v, want at least 1", data["age_rounds"])
	}
	explanation, ok := data["explanation"].(map[string]any)
	if !ok {
		t.Fatal("no structured explanation")
	}
	for _, field := range []string{"duty_cycle_required_s", "duty_cycle_remaining_s", "best_rejected_opportunity_id", "own_effective_value_credits"} {
		if _, present := explanation[field]; !present {
			t.Errorf("explanation lacks %q", field)
		}
	}

	// A second sweep finds the bucket CLEAN: the round covered its candidates,
	// which is ADR-0014's rule holding end to end.
	again, err := trigger.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("re-sweeping: %v", err)
	}
	_ = again // other tests may dirty other buckets; assert on OUR satellite
	var plans int
	if countErr := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.collection_plans WHERE satellite_id = $1`,
		satellite).Scan(&plans); countErr != nil {
		t.Fatalf("counting plans: %v", countErr)
	}
	if plans != 1 {
		t.Errorf("%d plans after a clean re-sweep, want 1 — the bucket refired with nothing new", plans)
	}
}

// THE #42 ACCEPTANCE TEST: rapid re-planning converges and never leaves
// orphaned acquisitions. Three rounds over one bucket as candidates keep
// arriving; after each, exactly one plan's acquisitions are ACTIVE and every
// other row is SUPERSEDED with a timestamp.
func TestRapidReplanningConverges(t *testing.T) {
	p := pool(t)
	ctx := context.Background()

	satellite := fmt.Sprintf("SAT-RR%d", time.Now().UnixNano()%100000)
	seedSatellite(t, p, satellite)
	if _, err := p.Exec(ctx,
		`UPDATE reference.satellites SET duty_cycle_budget_s = 70 WHERE satellite_id = $1`,
		satellite); err != nil {
		t.Fatalf("configuring: %v", err)
	}
	customer := fmt.Sprintf("cust-%d", time.Now().UnixNano())
	seedCustomer(t, p, customer)
	projections := postgres.NewProjections(p)

	accessStart := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	arrive := func(bid int64) string {
		requestID := uuid.NewString()
		snapshot := snapshotEvent(uuid.NewString(), requestID, customer)
		snapshot.Snapshot.BidCredits = bid
		snapshot.Snapshot.WindowStart = accessStart.Add(-time.Hour)
		snapshot.Snapshot.WindowEnd = accessStart.Add(6 * time.Hour)
		snapshot.Snapshot.SubmittedAt = time.Now().UTC().Add(-time.Hour)
		if _, err := projections.ProjectSnapshot(ctx, port.ConsumerLifecycle, snapshot); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		event := candidateEvent(uuid.NewString(), requestID, satellite, uuid.NewString())
		event.Candidates[0].AccessStart = accessStart
		event.Candidates[0].AccessEnd = accessStart.Add(20 * time.Minute)
		event.Candidates[0].AcquisitionDurationS = 30
		event.Candidates[0].DutyCycleCostS = 30
		event.Candidates[0].GeometryJSON = []byte(
			`{"incidence_angle_deg":30,"look_side":"RIGHT","squint_angle_deg":0.1,"slant_range_km":570.51,"elevation_angle_deg":55.2}`)
		if _, err := projections.ProjectCandidates(ctx, port.ConsumerOpportunities, event); err != nil {
			t.Fatalf("candidate: %v", err)
		}
		return requestID
	}

	trigger, err := app.NewTrigger(postgres.NewRounds(p), app.TriggerConfig{
		Policy:         domain.TriggerPolicy{QuietPeriod: time.Nanosecond, StalenessCeiling: time.Hour},
		BucketDuration: 3 * time.Hour,
		HorizonAhead:   24 * time.Hour,
		SweepLimit:     16,
		Allocator:      allocation.GreedyByBid{},
		Fairness:       domain.DefaultFairness(),
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("wiring: %v", err)
	}

	// Round 1: one modest request wins. Round 2: a richer arrival displaces it
	// (budget fits two 30s acquisitions, but the windows force contention —
	// budget 70 fits two, so make three arrivals total so someone always
	// loses). Round 3: richer still.
	first := arrive(100)
	if _, sweepErr := trigger.SweepOnce(ctx); sweepErr != nil {
		t.Fatalf("round 1: %v", sweepErr)
	}
	second := arrive(500)
	if _, sweepErr := trigger.SweepOnce(ctx); sweepErr != nil {
		t.Fatalf("round 2: %v", sweepErr)
	}
	third := arrive(900)
	if _, sweepErr := trigger.SweepOnce(ctx); sweepErr != nil {
		t.Fatalf("round 3: %v", sweepErr)
	}
	_, _, _ = first, second, third

	// Versions are dense and monotone.
	var versions []int
	rows, err := p.Query(ctx, `
		SELECT plan_version FROM planning.collection_plans
		WHERE satellite_id = $1 ORDER BY plan_version
	`, satellite)
	if err != nil {
		t.Fatalf("reading plans: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		versions = append(versions, v)
	}
	if len(versions) != 3 {
		t.Fatalf("%d plans, want 3 — a round did not fire or did not commit", len(versions))
	}
	for i, v := range versions {
		if v != i+1 {
			t.Fatalf("versions = %v, want dense 1,2,3", versions)
		}
	}

	// Exactly one plan's acquisitions are ACTIVE, and it is the NEWEST.
	var activePlans int
	if err := p.QueryRow(ctx, `
		SELECT count(DISTINCT a.plan_id) FROM planning.acquisitions a
		JOIN planning.collection_plans c ON c.plan_id = a.plan_id
		WHERE c.satellite_id = $1 AND a.status = 'ACTIVE'
	`, satellite).Scan(&activePlans); err != nil {
		t.Fatalf("counting active plans: %v", err)
	}
	if activePlans != 1 {
		t.Fatalf("%d plans hold ACTIVE acquisitions, want exactly 1 — the previous plans were not released", activePlans)
	}
	var newestIsActive bool
	if err := p.QueryRow(ctx, `
		SELECT bool_and(c.plan_version = (
			SELECT max(plan_version) FROM planning.collection_plans WHERE satellite_id = $1
		))
		FROM planning.acquisitions a
		JOIN planning.collection_plans c ON c.plan_id = a.plan_id
		WHERE c.satellite_id = $1 AND a.status = 'ACTIVE'
	`, satellite).Scan(&newestIsActive); err != nil {
		t.Fatalf("checking the active plan: %v", err)
	}
	if !newestIsActive {
		t.Fatal("an OLDER plan still holds ACTIVE acquisitions")
	}

	// No orphans: every superseded row carries its timestamp, none dangles.
	var orphans int
	if err := p.QueryRow(ctx, `
		SELECT count(*) FROM planning.acquisitions a
		JOIN planning.collection_plans c ON c.plan_id = a.plan_id
		WHERE c.satellite_id = $1 AND a.status = 'SUPERSEDED' AND a.superseded_at IS NULL
	`, satellite).Scan(&orphans); err != nil {
		t.Fatalf("counting orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d superseded acquisitions have no superseded_at", orphans)
	}

	// And displaced holders were TOLD: at least one SUPERSEDED event exists for
	// this satellite's requests.
	var supersededEvents int
	if err := p.QueryRow(ctx, `
		SELECT count(*) FROM planning.outbox
		WHERE event_type = 'planning.request.unfulfilled.v1'
		  AND payload->'data'->>'reason_code' = 'SUPERSEDED'
		  AND payload->'data'->>'request_id' IN ($1, $2, $3)
	`, first, second, third).Scan(&supersededEvents); err != nil {
		t.Fatalf("counting superseded events: %v", err)
	}
	if supersededEvents == 0 {
		t.Error("no SUPERSEDED event was ever emitted; a displaced holder lost a won slot silently")
	}
}

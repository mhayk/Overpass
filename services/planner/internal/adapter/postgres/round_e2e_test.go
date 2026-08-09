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

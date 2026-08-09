package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/allocation"
	"github.com/mhayk/overpass/services/planner/internal/app"
	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

var sweepNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

type fakeRounds struct {
	states []domain.BucketState
	inputs port.RoundInputs
	err    error

	openedKeys []domain.RoundKey
	rounds     []port.Round
	payloads   [][]byte
	plans      []*port.PlanCommit
	openErr    error
	// failSatellite makes OpenRound fail for exactly one satellite, so a sweep
	// can be shown to survive one bad key.
	failSatellite string
}

func (f *fakeRounds) DirtyBuckets(context.Context, port.BucketQuery) ([]domain.BucketState, error) {
	return f.states, f.err
}

func (f *fakeRounds) OpenRound(_ context.Context, key domain.RoundKey, bucketEnd time.Time,
	open func(port.RoundInputs) (port.RoundOutcome, error),
) (bool, error) {
	if f.openErr != nil {
		return false, f.openErr
	}
	if f.failSatellite != "" && key.SatelliteID == f.failSatellite {
		return false, errors.New("this satellite's round blew up")
	}
	inputs := f.inputs
	inputs.Key = key
	inputs.BucketEnd = bucketEnd

	outcome, err := open(inputs)
	if errors.Is(err, port.ErrSkipRound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	f.openedKeys = append(f.openedKeys, key)
	f.rounds = append(f.rounds, outcome.Round)
	f.payloads = append(f.payloads, outcome.RoundPayload)
	f.plans = append(f.plans, outcome.Plan)
	return true, nil
}

func trigger(t *testing.T, rounds port.Rounds) *app.Trigger {
	t.Helper()
	tr, err := app.NewTrigger(rounds, app.TriggerConfig{
		Policy:         domain.TriggerPolicy{QuietPeriod: 5 * time.Second, StalenessCeiling: time.Minute},
		BucketDuration: 3 * time.Hour,
		HorizonAhead:   24 * time.Hour,
		SweepLimit:     64,
		Allocator:      allocation.GreedyByBid{},
		Fairness:       domain.DefaultFairness(),
		Now:            func() time.Time { return sweepNow },
	}, discard())
	if err != nil {
		t.Fatalf("wiring the trigger: %v", err)
	}
	return tr
}

func dueBucket(satellite string) domain.BucketState {
	return domain.BucketState{
		Key: domain.RoundKey{
			SatelliteID: satellite,
			BucketStart: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
		},
		PendingCandidates: 3,
		NewestCandidateAt: sweepNow.Add(-30 * time.Second), // quiet
		OldestPendingAt:   sweepNow.Add(-40 * time.Second),
		TippingEventID:    "eeeeeeee-0000-4000-8000-000000000001",
	}
}

func someInputs() port.RoundInputs {
	orbit := 47110
	joinable := port.JoinableCandidate{
		Candidate: domain.Candidate{
			OpportunityID:        "aaaaaaaa-0000-4000-8000-0000000000aa",
			RequestID:            "aaaaaaaa-0000-4000-8000-000000000001",
			SatelliteID:          "CAPELLA-14",
			Mode:                 "STRIPMAP",
			AccessStart:          sweepNow.Add(30 * time.Minute),
			AccessEnd:            sweepNow.Add(40 * time.Minute),
			AcquisitionDurationS: 30,
			OrbitNumber:          &orbit,
			DutyCycleCostS:       30,
			QualityScore:         0.9,
			// Real contract geometry, so the roll derives and the plan event's
			// AccessGeometry decodes.
			GeometryJSON:     []byte(`{"incidence_angle_deg":30,"look_side":"RIGHT","squint_angle_deg":0.1,"slant_range_km":570.51,"elevation_angle_deg":55.2}`),
			FootprintGeoJSON: []byte(`{"type":"Polygon","coordinates":[[[0,0],[0,1],[1,1],[1,0],[0,0]]]}`),
		},
		CustomerID:   "acme",
		PriorityTier: "COMMERCIAL",
		BidCredits:   500,
		SubmittedAt:  sweepNow.Add(-2 * time.Hour),
		Deadline:     sweepNow.Add(3 * time.Hour),
	}
	return port.RoundInputs{
		CandidateOpportunityCount: 7,
		CandidateRequestIDs:       []string{"aaaaaaaa-0000-4000-8000-000000000001"},
		DutyCycleBudgetS:          600,
		Joinable:                  []port.JoinableCandidate{joinable},
		Profile: domain.SatelliteProfile{
			Agility: domain.Agility{
				SlewRateDegS: 1, SettleTimeS: 5, ModeTransitionS: 0, MaxRollDeg: 45,
			},
			DutyCycleBudgetS: 600,
		},
		AgeRounds: map[string]int{"aaaaaaaa-0000-4000-8000-000000000001": 2},
	}
}

// A misconfigured firing rule must be a STARTUP failure. A ceiling below the
// quiet period still plans; it has just silently lost the debounce.
func TestTriggerRefusesABrokenConfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  app.TriggerConfig
	}{
		{"ceiling below the quiet period", app.TriggerConfig{
			Policy:         domain.TriggerPolicy{QuietPeriod: time.Minute, StalenessCeiling: time.Second},
			BucketDuration: time.Hour, HorizonAhead: time.Hour, SweepLimit: 1,
			Allocator: allocation.GreedyByBid{}, Fairness: domain.DefaultFairness(),
		}},
		{"bucket duration that does not divide a day", app.TriggerConfig{
			Policy:         domain.TriggerPolicy{QuietPeriod: time.Second, StalenessCeiling: time.Minute},
			BucketDuration: 7 * time.Hour, HorizonAhead: time.Hour, SweepLimit: 1,
			Allocator: allocation.GreedyByBid{}, Fairness: domain.DefaultFairness(),
		}},
		{"no sweep limit", app.TriggerConfig{
			Policy:         domain.TriggerPolicy{QuietPeriod: time.Second, StalenessCeiling: time.Minute},
			BucketDuration: time.Hour, HorizonAhead: time.Hour, SweepLimit: 0,
			Allocator: allocation.GreedyByBid{}, Fairness: domain.DefaultFairness(),
		}},
		{"no horizon", app.TriggerConfig{
			Policy:         domain.TriggerPolicy{QuietPeriod: time.Second, StalenessCeiling: time.Minute},
			BucketDuration: time.Hour, SweepLimit: 1,
			Allocator: allocation.GreedyByBid{}, Fairness: domain.DefaultFairness(),
		}},
		{"no allocator", app.TriggerConfig{
			Policy:         domain.TriggerPolicy{QuietPeriod: time.Second, StalenessCeiling: time.Minute},
			BucketDuration: time.Hour, HorizonAhead: time.Hour, SweepLimit: 1,
			Fairness: domain.DefaultFairness(),
		}},
		{"fairness that can invert the tiers", app.TriggerConfig{
			Policy:         domain.TriggerPolicy{QuietPeriod: time.Second, StalenessCeiling: time.Minute},
			BucketDuration: time.Hour, HorizonAhead: time.Hour, SweepLimit: 1,
			Allocator: allocation.GreedyByBid{},
			Fairness: domain.Fairness{
				TierMultipliers:    map[string]float64{"GOVERNMENT": 4, "CIVIL_PROTECTION": 3, "COMMERCIAL": 1, "BEST_EFFORT": 0.5},
				AgeingTimeConstant: time.Hour, MaxAgeingFactor: 20,
			},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := app.NewTrigger(&fakeRounds{}, tt.cfg, discard()); err == nil {
				t.Fatalf("accepted %s", tt.name)
			}
		})
	}
}

func TestADueBucketOpensARound(t *testing.T) {
	rounds := &fakeRounds{states: []domain.BucketState{dueBucket("CAPELLA-14")}, inputs: someInputs()}

	stats, err := trigger(t, rounds).SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if stats.Opened != 1 || stats.Waiting != 0 {
		t.Fatalf("stats = %+v, want one opened", stats)
	}

	round := rounds.rounds[0]
	if round.Trigger != domain.TriggerDebounce {
		t.Errorf("trigger = %q, want %q", round.Trigger, domain.TriggerDebounce)
	}
	if round.Policy != "GREEDY_BY_BID" {
		t.Errorf("policy = %q; a committed plan must be attributable to a strategy", round.Policy)
	}
	// The count and the ids are DIFFERENT questions: the count is what was on
	// the table, the ids are who could actually compete.
	if round.CandidateOpportunityCount != 7 {
		t.Errorf("count = %d, want the 7 read under the lock", round.CandidateOpportunityCount)
	}
	if len(round.CandidateRequestIDs) != 1 {
		t.Errorf("request ids = %v, want the one that had a snapshot", round.CandidateRequestIDs)
	}
	if round.CausationID == nil || *round.CausationID != "eeeeeeee-0000-4000-8000-000000000001" {
		t.Errorf("causation = %v, want the tipping event", round.CausationID)
	}
	if round.BucketEnd.Sub(round.Key.BucketStart) != 3*time.Hour {
		t.Errorf("bucket span = %s, want the configured 3h", round.BucketEnd.Sub(round.Key.BucketStart))
	}
}

// The payload must be a real contract event, not a bare data object. #124 is
// the precedent: nine schema violations that every test agreed with because
// every test built the payload with the same helper it asserted against.
func TestThePublishedPayloadIsAnEnvelopedContractEvent(t *testing.T) {
	rounds := &fakeRounds{states: []domain.BucketState{dueBucket("CAPELLA-14")}, inputs: someInputs()}

	if _, err := trigger(t, rounds).SweepOnce(context.Background()); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(rounds.payloads[0], &envelope); err != nil {
		t.Fatalf("the payload is not JSON: %v", err)
	}

	for _, required := range []string{
		"event_id", "event_type", "schema_version", "occurred_at",
		"correlation_id", "causation_id", "producer", "data",
	} {
		if _, ok := envelope[required]; !ok {
			t.Errorf("the envelope has no %q; the schema requires it", required)
		}
	}
	if envelope["event_type"] != "planning.round.triggered.v1" {
		t.Errorf("event_type = %v", envelope["event_type"])
	}
	if envelope["producer"] != "planner-service" {
		t.Errorf("producer = %v", envelope["producer"])
	}

	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatal("data is not an object")
	}
	for _, required := range []string{
		"round_id", "satellite_id", "bucket", "trigger", "policy",
		"candidate_opportunity_count", "triggered_at",
	} {
		if _, ok := data[required]; !ok {
			t.Errorf("data has no %q; the schema requires it", required)
		}
	}
	// A round aggregates many requests, so it cannot inherit any one request's
	// correlation_id — the contract gives it a fresh one.
	if envelope["correlation_id"] == "" || envelope["correlation_id"] == nil {
		t.Error("correlation_id is absent; a round still has to be greppable as one transaction")
	}
}

// A ceiling-fired round names no causing event, because nothing tipped it.
func TestACeilingFiredRoundCarriesNoCausation(t *testing.T) {
	state := dueBucket("CAPELLA-14")
	state.NewestCandidateAt = sweepNow.Add(-time.Second)   // still arriving
	state.OldestPendingAt = sweepNow.Add(-2 * time.Minute) // past the 1m ceiling

	rounds := &fakeRounds{states: []domain.BucketState{state}, inputs: someInputs()}
	if _, err := trigger(t, rounds).SweepOnce(context.Background()); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	round := rounds.rounds[0]
	if round.Trigger != domain.TriggerCadence {
		t.Errorf("trigger = %q, want %q", round.Trigger, domain.TriggerCadence)
	}
	if round.CausationID != nil {
		t.Errorf("causation = %v, want null — the ceiling fired, so nothing tipped it", *round.CausationID)
	}
}

func TestABucketNotYetDueIsLeftAlone(t *testing.T) {
	state := dueBucket("CAPELLA-14")
	state.NewestCandidateAt = sweepNow.Add(-time.Second) // still arriving
	state.OldestPendingAt = sweepNow.Add(-2 * time.Second)

	rounds := &fakeRounds{states: []domain.BucketState{state}, inputs: someInputs()}
	stats, err := trigger(t, rounds).SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if stats.Opened != 0 || stats.Waiting != 1 {
		t.Errorf("stats = %+v, want one waiting", stats)
	}
}

// An empty candidate set UNDER THE LOCK means another planner got there first.
// Opening anyway would announce the same requests in a second conservation
// ledger.
func TestAnEmptySetUnderTheLockSkipsTheRound(t *testing.T) {
	rounds := &fakeRounds{
		states: []domain.BucketState{dueBucket("CAPELLA-14")},
		inputs: port.RoundInputs{CandidateOpportunityCount: 0},
	}

	stats, err := trigger(t, rounds).SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if stats.Opened != 0 {
		t.Errorf("opened %d rounds over an empty candidate set", stats.Opened)
	}
	if len(rounds.rounds) != 0 {
		t.Error("a round was recorded for a bucket another planner had already covered")
	}
}

// One satellite failing must not abandon the sweep. Satellites are independent,
// and letting one bad key starve the constellation is exactly what per-satellite
// locking exists to avoid.
func TestOneFailingSatelliteDoesNotStopTheSweep(t *testing.T) {
	rounds := &fakeRounds{
		states:        []domain.BucketState{dueBucket("SAT-BAD"), dueBucket("SAT-GOOD")},
		inputs:        someInputs(),
		failSatellite: "SAT-BAD",
	}

	stats, err := trigger(t, rounds).SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("the sweep aborted on one satellite's failure: %v", err)
	}
	if stats.Considered != 2 {
		t.Fatalf("considered %d buckets, want 2", stats.Considered)
	}
	if stats.Failed != 1 || stats.Opened != 1 {
		t.Errorf("stats = %+v, want one failure and one success", stats)
	}
	if len(rounds.openedKeys) != 1 || rounds.openedKeys[0].SatelliteID != "SAT-GOOD" {
		t.Errorf("opened %v; the healthy satellite must still be planned", rounds.openedKeys)
	}
}

func TestSweepPropagatesADiscoveryFailure(t *testing.T) {
	rounds := &fakeRounds{err: errors.New("connection refused")}

	if _, err := trigger(t, rounds).SweepOnce(context.Background()); err == nil {
		t.Fatal("a failed sweep reported success; the planner would look idle rather than broken")
	}
}

func TestRunStopsAfterItsIterationBudget(t *testing.T) {
	rounds := &fakeRounds{states: []domain.BucketState{dueBucket("CAPELLA-14")}, inputs: someInputs()}

	done := make(chan error, 1)
	go func() { done <- trigger(t, rounds).Run(context.Background(), 2, time.Millisecond) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its iteration budget")
	}
}

func TestRunReturnsOnCancellationDuringSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tr := trigger(t, &fakeRounds{})

	done := make(chan error, 1)
	go func() { done <- tr.Run(ctx, 0, time.Hour) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation is a clean stop, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run ignored cancellation")
	}
}

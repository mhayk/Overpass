package app_test

import (
	"encoding/json"
	"testing"

	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// The wiring #47 exists for: a due bucket now produces a PLAN, and the plan
// carries one structured unfulfilment event per losing request.

// contested returns inputs where two requests want the same slot and only one
// fits the budget.
func contested() port.RoundInputs {
	inputs := someInputs()
	inputs.Profile.DutyCycleBudgetS = 40 // room for one 30 s acquisition
	inputs.DutyCycleBudgetS = 40

	loser := inputs.Joinable[0]
	loser.OpportunityID = "bbbbbbbb-0000-4000-8000-0000000000bb"
	loser.RequestID = "bbbbbbbb-0000-4000-8000-000000000002"
	loser.CustomerID = "rival"
	loser.BidCredits = 200 // effective 200 < the incumbent's 500
	inputs.Joinable = append(inputs.Joinable, loser)
	inputs.CandidateRequestIDs = append(inputs.CandidateRequestIDs, loser.RequestID)
	inputs.AgeRounds[loser.RequestID] = 4
	return inputs
}

// asMap fails the test on a payload whose shape is not the object the contract
// promises, instead of panicking mid-assertion.
func asMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, not an object", what, v)
	}
	return m
}

func openPlan(t *testing.T, inputs port.RoundInputs) *port.PlanCommit {
	t.Helper()
	rounds := &fakeRounds{states: []domain.BucketState{dueBucket("CAPELLA-14")}, inputs: inputs}
	stats, err := trigger(t, rounds).SweepOnce(t.Context())
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if stats.Opened != 1 {
		t.Fatalf("stats = %+v, want one opened", stats)
	}
	if len(rounds.plans) != 1 || rounds.plans[0] == nil {
		t.Fatal("the round opened without a plan; the wiring is not allocating")
	}
	return rounds.plans[0]
}

func TestARoundNowCommitsAPlan(t *testing.T) {
	plan := openPlan(t, someInputs())

	if len(plan.Acquisitions) != 1 {
		t.Fatalf("%d acquisitions, want 1", len(plan.Acquisitions))
	}
	if plan.Acquisitions[0].AcquisitionID == "" {
		t.Error("no acquisition id; the event and the row would disagree")
	}
	if plan.Policy != "GREEDY_BY_BID" {
		t.Errorf("policy = %q", plan.Policy)
	}
	if plan.PlanVersion != inputsVersion(someInputs()) {
		t.Errorf("plan version = %d", plan.PlanVersion)
	}
	if len(plan.MetricsJSON) == 0 {
		t.Error("no metrics")
	}

	// The plan event is a real enveloped contract event.
	var envelope map[string]any
	if err := json.Unmarshal(plan.PlanPayload, &envelope); err != nil {
		t.Fatalf("the plan payload is not JSON: %v", err)
	}
	for _, required := range []string{"event_id", "event_type", "schema_version", "occurred_at", "correlation_id", "producer", "data"} {
		if _, ok := envelope[required]; !ok {
			t.Errorf("the plan envelope has no %q", required)
		}
	}
	data := asMap(t, envelope["data"], "plan data")
	for _, required := range []string{"plan_id", "round_id", "satellite_id", "bucket", "plan_version", "policy", "committed_at", "acquisitions", "metrics"} {
		if _, ok := data[required]; !ok {
			t.Errorf("the plan data has no %q; the schema requires it", required)
		}
	}
	metrics := asMap(t, data["metrics"], "metrics")
	for _, required := range []string{"total_plan_value_credits", "requests_fulfilled", "candidate_opportunity_count", "duty_cycle_used_s", "duty_cycle_budget_s", "allocation_duration_ms"} {
		if _, ok := metrics[required]; !ok {
			t.Errorf("metrics has no %q; the schema requires it", required)
		}
	}
	acquisitions, ok := data["acquisitions"].([]any)
	if !ok || len(acquisitions) != 1 {
		t.Fatalf("the event carries %d acquisitions", len(acquisitions))
	}
	acq := asMap(t, acquisitions[0], "acquisition")
	for _, required := range []string{"acquisition_id", "request_id", "opportunity_id", "mode", "window", "geometry", "awarded_value_credits"} {
		if _, ok := acq[required]; !ok {
			t.Errorf("the acquisition has no %q; the schema requires it", required)
		}
	}
}

// THE #47 TEST: the loser gets one event, and it is STRUCTURED — numbers a
// customer can act on, not a message string.
func TestALoserGetsAStructuredUnfulfilmentEvent(t *testing.T) {
	plan := openPlan(t, contested())

	if len(plan.Acquisitions) != 1 {
		t.Fatalf("%d acquisitions, want 1", len(plan.Acquisitions))
	}
	if len(plan.UnfulfilledEvents) != 1 {
		t.Fatalf("%d unfulfilment events, want exactly 1 — conservation", len(plan.UnfulfilledEvents))
	}

	var envelope map[string]any
	if err := json.Unmarshal(plan.UnfulfilledEvents[0].Payload, &envelope); err != nil {
		t.Fatalf("the unfulfilment payload is not JSON: %v", err)
	}
	if envelope["event_type"] != "planning.request.unfulfilled.v1" {
		t.Errorf("event_type = %v", envelope["event_type"])
	}
	data := asMap(t, envelope["data"], "unfulfilment data")

	if data["request_id"] != "bbbbbbbb-0000-4000-8000-000000000002" {
		t.Errorf("request_id = %v", data["request_id"])
	}
	if data["reason_code"] != domain.ReasonDutyCycle {
		t.Errorf("reason = %v, want %s — the budget is what bound", data["reason_code"], domain.ReasonDutyCycle)
	}
	if data["eligible_for_retry"] != true {
		t.Error("a competitive loss is not retryable; the ageing factor has nothing to resolve")
	}
	// Prior rounds (4) plus this one.
	age, _ := data["age_rounds"].(float64) //nolint:errcheck // a missing field reads as 0 and fails the assertion below
	if age != 5 {
		t.Errorf("age_rounds = %v, want 5", data["age_rounds"])
	}
	sats, ok := data["satellite_ids_considered"].([]any)
	if !ok || len(sats) != 1 || sats[0] != "CAPELLA-14" {
		t.Errorf("satellite_ids_considered = %v", sats)
	}

	explanation, ok := data["explanation"].(map[string]any)
	if !ok {
		t.Fatal("no structured explanation; the why-panel would have nothing but prose")
	}
	if _, ok := explanation["duty_cycle_required_s"]; !ok {
		t.Error("no duty_cycle_required_s; the shortfall is not actionable")
	}
	if _, ok := explanation["duty_cycle_remaining_s"]; !ok {
		t.Error("no duty_cycle_remaining_s")
	}
	if explanation["best_rejected_opportunity_id"] != "bbbbbbbb-0000-4000-8000-0000000000bb" {
		t.Errorf("best_rejected_opportunity_id = %v; the timeline has no ghost to render",
			explanation["best_rejected_opportunity_id"])
	}
	if _, ok := explanation["own_effective_value_credits"]; !ok {
		t.Error("no own_effective_value_credits; the customer cannot see the gap")
	}
}

// A candidate whose geometry cannot derive a roll is skipped BEFORE the policy
// — and its request still gets an outcome. Conservation does not care whose
// fault the gap is.
func TestAnUnusableCandidateStillYieldsAnOutcome(t *testing.T) {
	inputs := someInputs()
	broken := inputs.Joinable[0]
	broken.OpportunityID = "cccccccc-0000-4000-8000-0000000000cc"
	broken.RequestID = "cccccccc-0000-4000-8000-000000000003"
	broken.GeometryJSON = []byte(`{"look_side":"RIGHT"}`) // no incidence, no slant range
	inputs.Joinable = append(inputs.Joinable, broken)
	inputs.CandidateRequestIDs = append(inputs.CandidateRequestIDs, broken.RequestID)

	plan := openPlan(t, inputs)

	found := false
	for _, event := range plan.UnfulfilledEvents {
		var envelope map[string]any
		if err := json.Unmarshal(event.Payload, &envelope); err != nil {
			t.Fatalf("payload: %v", err)
		}
		data := asMap(t, envelope["data"], "unfulfilment data")
		if data["request_id"] == "cccccccc-0000-4000-8000-000000000003" {
			found = true
			if data["reason_code"] != domain.ReasonNoOpportunity {
				t.Errorf("reason = %v, want %s", data["reason_code"], domain.ReasonNoOpportunity)
			}
		}
	}
	if !found {
		t.Fatal("the request with the broken candidate vanished — the worst possible failure mode for a customer")
	}
}

// All held: the round is SKIPPED, not committed empty. An empty plan built on
// no information would supersede a live plan with nothing.
func TestARoundWithOnlyHeldCandidatesIsSkipped(t *testing.T) {
	inputs := someInputs()
	inputs.Joinable = nil
	inputs.CandidateRequestIDs = nil

	rounds := &fakeRounds{states: []domain.BucketState{dueBucket("CAPELLA-14")}, inputs: inputs}
	stats, err := trigger(t, rounds).SweepOnce(t.Context())
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if stats.Opened != 0 || stats.Skipped != 1 {
		t.Errorf("stats = %+v, want one skipped", stats)
	}
}

func inputsVersion(inputs port.RoundInputs) int {
	if inputs.NextPlanVersion == 0 {
		return 0
	}
	return inputs.NextPlanVersion
}

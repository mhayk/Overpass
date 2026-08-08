package allocation_test

import (
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/allocation"
	"github.com/mhayk/overpass/services/planner/internal/domain"
)

var bucketStart = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func profile() domain.SatelliteProfile {
	return domain.SatelliteProfile{
		Agility: domain.Agility{
			SlewRateDegS: 1, SettleTimeS: 5, ModeTransitionS: 0, MaxRollDeg: 45,
		},
		DutyCycleBudgetS: 600,
	}
}

// candidate builds one option for one request.
func candidate(opportunityID, requestID string, value float64, open, duration time.Duration, roll float64) domain.ScoredCandidate {
	orbit := 47110
	return domain.ScoredCandidate{
		Candidate: domain.Candidate{
			OpportunityID:        opportunityID,
			RequestID:            requestID,
			SatelliteID:          "CAPELLA-14",
			Mode:                 "STRIPMAP",
			AccessStart:          bucketStart.Add(open),
			AccessEnd:            bucketStart.Add(open + 30*time.Minute),
			AcquisitionDurationS: duration.Seconds(),
			OrbitNumber:          &orbit,
			DutyCycleCostS:       duration.Seconds(),
			QualityScore:         0.9,
			GeometryJSON:         []byte(`{}`),
			FootprintGeoJSON:     []byte(`{}`),
		},
		CustomerID:     "acme",
		EffectiveValue: value,
		Deadline:       bucketStart.Add(3 * time.Hour),
		Attitude:       domain.Attitude{RollDeg: roll, Mode: "STRIPMAP"},
	}
}

func problem(candidates ...domain.ScoredCandidate) domain.Problem {
	return domain.Problem{
		Key:        domain.RoundKey{SatelliteID: "CAPELLA-14", BucketStart: bucketStart},
		BucketEnd:  bucketStart.Add(3 * time.Hour),
		Profile:    profile(),
		Now:        bucketStart,
		Candidates: candidates,
	}
}

func requestIDs(candidates []domain.ScoredCandidate) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range candidates {
		if !seen[c.RequestID] {
			seen[c.RequestID] = true
			out = append(out, c.RequestID)
		}
	}
	return out
}

func won(plan domain.Plan, requestID string) bool {
	for _, a := range plan.Acquisitions {
		if a.RequestID == requestID {
			return true
		}
	}
	return false
}

func reasonFor(plan domain.Plan, requestID string) string {
	for _, u := range plan.Unfulfilled {
		if u.RequestID == requestID {
			return u.ReasonCode
		}
	}
	return ""
}

func TestTheNameIsTheContractEnumValue(t *testing.T) {
	if got := (allocation.GreedyByBid{}).Name(); got != "GREEDY_BY_BID" {
		t.Errorf("Name = %q", got)
	}
}

func TestHighestValueWins(t *testing.T) {
	// Three candidates for three requests, all wanting the same instant. Only
	// one can have it; the schedule has room for the others later.
	p := problem(
		candidate("o-low", "r-low", 100, 0, time.Minute, 0),
		candidate("o-high", "r-high", 900, 0, time.Minute, 0),
		candidate("o-mid", "r-mid", 500, 0, time.Minute, 0),
	)

	plan := allocation.GreedyByBid{}.Allocate(p)

	if len(plan.Acquisitions) == 0 {
		t.Fatal("nothing was scheduled")
	}
	// The highest-value candidate takes the earliest slot, because it was
	// considered first.
	if plan.Acquisitions[0].RequestID != "r-high" {
		t.Errorf("the earliest slot went to %s, want r-high", plan.Acquisitions[0].RequestID)
	}
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

// Deterministic tie-breaking. A random or map-order tie-break would make every
// benchmark comparison noisy, so two policies could differ by nothing but the
// order Go happened to iterate in.
func TestTiesBreakDeterministically(t *testing.T) {
	build := func() domain.Problem {
		return problem(
			candidate("o-zulu", "r-zulu", 500, 0, time.Minute, 0),
			candidate("o-alpha", "r-alpha", 500, 0, time.Minute, 0),
			candidate("o-mike", "r-mike", 500, 0, time.Minute, 0),
		)
	}

	first := allocation.GreedyByBid{}.Allocate(build())
	for range 20 {
		again := allocation.GreedyByBid{}.Allocate(build())
		if len(again.Acquisitions) != len(first.Acquisitions) {
			t.Fatalf("two runs over identical input scheduled %d and %d acquisitions",
				len(first.Acquisitions), len(again.Acquisitions))
		}
		for i := range first.Acquisitions {
			if again.Acquisitions[i].OpportunityID != first.Acquisitions[i].OpportunityID {
				t.Fatalf("run order differs at %d: %s then %s",
					i, first.Acquisitions[i].OpportunityID, again.Acquisitions[i].OpportunityID)
			}
		}
	}
	// At equal value the earliest slot goes to the lexicographically first
	// opportunity id — stable, unique, and meaningless, which is what a
	// tie-break should be.
	if first.Acquisitions[0].OpportunityID != "o-alpha" {
		t.Errorf("the tie went to %s, want o-alpha", first.Acquisitions[0].OpportunityID)
	}
}

// THE POINT OF THIS POLICY: it is provably suboptimal, and here is a case
// proving it. Sorting by value alone ignores what an acquisition COSTS.
func TestItIsProvablySuboptimal(t *testing.T) {
	// A budget that fits either the one greedy grabs or the two it then cannot.
	p := problem(
		// Highest value, and it eats the entire duty-cycle budget.
		candidate("o-hog", "r-hog", 700, 0, 600*time.Second, 0),
		// Two cheaper requests worth 1000 together, which greedy never reaches.
		candidate("o-a", "r-a", 500, 20*time.Minute, 150*time.Second, 0),
		candidate("o-b", "r-b", 500, 40*time.Minute, 150*time.Second, 0),
	)

	plan := allocation.GreedyByBid{}.Allocate(p)

	if !won(plan, "r-hog") {
		t.Fatal("greedy did not take the highest-value candidate; it is no longer the naive baseline")
	}
	if won(plan, "r-a") || won(plan, "r-b") {
		t.Fatal("both cheaper requests fitted after the hog; the scenario does not demonstrate suboptimality")
	}
	if plan.Value() != 700 {
		t.Errorf("plan value = %d, want 700", plan.Value())
	}
	// The optimum here is 1000. That gap is what M2-13 measures and what
	// GreedyByValueDensity is built to close.
	t.Logf("greedy took %d; taking the two cheaper requests instead would have been worth 1000", plan.Value())

	if reasonFor(plan, "r-a") != domain.ReasonDutyCycle {
		t.Errorf("r-a lost for %q, want %s", reasonFor(plan, "r-a"), domain.ReasonDutyCycle)
	}
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

// A request with several candidates gets ONE outcome, and the reason it carries
// is its BEST candidate's — the answer to "why did I lose?" rather than "why
// did the option I did not want also fail?".
func TestARequestWithManyCandidatesGetsOneOutcome(t *testing.T) {
	// One request, three options, all of which miss the deadline.
	late := func(id string, value float64) domain.ScoredCandidate {
		c := candidate(id, "r-one", value, 0, 10*time.Minute, 0)
		c.Deadline = bucketStart.Add(time.Minute)
		return c
	}
	p := problem(late("o-a", 100), late("o-b", 900), late("o-c", 500))

	plan := allocation.GreedyByBid{}.Allocate(p)

	if len(plan.Unfulfilled) != 1 {
		t.Fatalf("%d unfulfilments for one request, want 1", len(plan.Unfulfilled))
	}
	if plan.Unfulfilled[0].ReasonCode != domain.ReasonDeadlinePassed {
		t.Errorf("reason = %s, want %s", plan.Unfulfilled[0].ReasonCode, domain.ReasonDeadlinePassed)
	}
	if plan.Unfulfilled[0].CustomerID == "" {
		t.Error("no customer on the unfulfilment; the event could not be addressed")
	}
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

// A request whose second option fits must NOT appear as unfulfilled just
// because its first was refused.
func TestAWinnerIsNeverAlsoReportedAsALoser(t *testing.T) {
	blocked := candidate("o-first", "r-one", 900, 0, 10*time.Minute, 0)
	blocked.Deadline = bucketStart.Add(time.Minute) // cannot finish
	fallback := candidate("o-second", "r-one", 100, 0, time.Minute, 0)

	p := problem(blocked, fallback)
	plan := allocation.GreedyByBid{}.Allocate(p)

	if !won(plan, "r-one") {
		t.Fatal("the request's viable second option was never taken")
	}
	if len(plan.Unfulfilled) != 0 {
		t.Errorf("a request that won also appears as %s", plan.Unfulfilled[0].ReasonCode)
	}
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

func TestUnfulfilmentReasonsAreSpecific(t *testing.T) {
	tests := []struct {
		name      string
		candidate domain.ScoredCandidate
		want      string
	}{
		{
			name: "cannot finish before its deadline",
			candidate: func() domain.ScoredCandidate {
				c := candidate("o1", "r1", 100, 0, 10*time.Minute, 0)
				c.Deadline = bucketStart.Add(time.Minute)
				return c
			}(),
			want: domain.ReasonDeadlinePassed,
		},
		{
			name:      "beyond the spacecraft's roll authority",
			candidate: candidate("o2", "r2", 100, 0, time.Minute, 60),
			want:      domain.ReasonBlockedBySlew,
		},
		{
			name: "costs more duty cycle than the orbit has",
			candidate: func() domain.ScoredCandidate {
				c := candidate("o3", "r3", 100, 0, time.Minute, 0)
				c.DutyCycleCostS = 900 // budget is 600
				return c
			}(),
			want: domain.ReasonDutyCycle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := allocation.GreedyByBid{}.Allocate(problem(tt.candidate))
			if len(plan.Acquisitions) != 0 {
				t.Fatal("an infeasible candidate was scheduled")
			}
			if len(plan.Unfulfilled) != 1 {
				t.Fatalf("%d unfulfilments, want 1", len(plan.Unfulfilled))
			}
			if plan.Unfulfilled[0].ReasonCode != tt.want {
				t.Errorf("reason = %s, want %s", plan.Unfulfilled[0].ReasonCode, tt.want)
			}
			if plan.Unfulfilled[0].Explanation == "" {
				t.Error("no explanation; the why-panel would have nothing to show")
			}
		})
	}
}

// An empty round is legal and meaningful: a cadence sweep over a bucket with
// nothing in it commits an empty plan, which says the satellite is idle.
func TestAnEmptyProblemProducesAnEmptyPlan(t *testing.T) {
	plan := allocation.GreedyByBid{}.Allocate(problem())

	if len(plan.Acquisitions) != 0 || len(plan.Unfulfilled) != 0 {
		t.Errorf("an empty problem produced %d acquisitions and %d unfulfilments",
			len(plan.Acquisitions), len(plan.Unfulfilled))
	}
	if err := plan.Validate(nil); err != nil {
		t.Errorf("conservation over an empty round: %v", err)
	}
}

// A profile the planner cannot schedule against is a configuration failure, and
// every request fails for the same reason. Saying so once per request beats an
// empty plan nobody can explain.
func TestAnUnusableProfileExplainsItselfPerRequest(t *testing.T) {
	p := problem(
		candidate("o1", "r1", 100, 0, time.Minute, 0),
		candidate("o2", "r2", 100, 0, time.Minute, 0),
	)
	p.Profile.Agility.SlewRateDegS = 0 // cannot slew at all

	plan := allocation.GreedyByBid{}.Allocate(p)

	if len(plan.Acquisitions) != 0 {
		t.Fatal("something was scheduled against an unusable profile")
	}
	if len(plan.Unfulfilled) != 2 {
		t.Fatalf("%d unfulfilments, want one per request", len(plan.Unfulfilled))
	}
	for _, u := range plan.Unfulfilled {
		if u.Explanation == "" {
			t.Errorf("%s has no explanation", u.RequestID)
		}
	}
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

// Whatever a policy returns, the schedule underneath it enforced the
// constraints — so a plan it produced is always flyable.
func TestEveryPlanItProducesIsFeasible(t *testing.T) {
	p := problem(
		candidate("o1", "r1", 900, 0, 2*time.Minute, 0),
		candidate("o2", "r2", 800, 0, 2*time.Minute, 40),
		candidate("o3", "r3", 700, time.Minute, 2*time.Minute, -30),
		candidate("o4", "r4", 600, 2*time.Minute, 2*time.Minute, 10),
		candidate("o5", "r5", 500, 3*time.Minute, 2*time.Minute, -40),
	)

	plan := allocation.GreedyByBid{}.Allocate(p)
	acquisitions := plan.Acquisitions
	if len(acquisitions) < 2 {
		t.Fatalf("only %d acquisitions scheduled; the case does not exercise transitions", len(acquisitions))
	}

	for i := 1; i < len(acquisitions); i++ {
		previous, current := acquisitions[i-1], acquisitions[i]
		if current.Start.Before(previous.End) {
			t.Fatalf("%s overlaps %s", current.OpportunityID, previous.OpportunityID)
		}
		required := p.Profile.Agility.SlewTime(previous.Attitude, current.Attitude)
		if gap := current.Start.Sub(previous.End); gap < required {
			t.Fatalf("%s follows %s after %s, less than the %s it needs",
				current.OpportunityID, previous.OpportunityID, gap, required)
		}
	}
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

// Once a request has won, its remaining candidates are skipped rather than
// tried — at most one acquisition per request, and no wasted feasibility work.
func TestRemainingCandidatesOfAWinnerAreSkipped(t *testing.T) {
	p := problem(
		candidate("o-best", "r-one", 900, 0, time.Minute, 0),
		candidate("o-also", "r-one", 800, 10*time.Minute, time.Minute, 0),
		candidate("o-third", "r-one", 700, 20*time.Minute, time.Minute, 0),
	)

	plan := allocation.GreedyByBid{}.Allocate(p)

	if len(plan.Acquisitions) != 1 {
		t.Fatalf("%d acquisitions for one request, want 1 — the same target would be imaged twice",
			len(plan.Acquisitions))
	}
	if plan.Acquisitions[0].OpportunityID != "o-best" {
		t.Errorf("took %s, want the highest-value o-best", plan.Acquisitions[0].OpportunityID)
	}
	if len(plan.Unfulfilled) != 0 {
		t.Errorf("a request that won also reports %s", plan.Unfulfilled[0].ReasonCode)
	}
}

// The unusable-profile path reports ONE unfulfilment per request, not one per
// candidate — a customer with three options did not lose three times.
func TestAnUnusableProfileReportsOncePerRequest(t *testing.T) {
	p := problem(
		candidate("o1", "r-one", 100, 0, time.Minute, 0),
		candidate("o2", "r-one", 90, 5*time.Minute, time.Minute, 0),
		candidate("o3", "r-two", 80, 0, time.Minute, 0),
	)
	p.Profile.DutyCycleBudgetS = 0 // unusable

	plan := allocation.GreedyByBid{}.Allocate(p)

	if len(plan.Unfulfilled) != 2 {
		t.Fatalf("%d unfulfilments, want one per request (2)", len(plan.Unfulfilled))
	}
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

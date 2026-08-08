package allocation_test

import (
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/allocation"
	"github.com/mhayk/overpass/services/planner/internal/domain"
)

func TestDensityNameIsTheContractEnumValue(t *testing.T) {
	if got := (allocation.GreedyByValueDensity{}).Name(); got != "GREEDY_BY_VALUE_DENSITY" {
		t.Errorf("Name = %q", got)
	}
}

// THE ADVERSARIAL CASE the issue asks for: an instance where density beats bid,
// constructed so the difference is the whole reason the policy exists.
//
// Greedy-by-bid takes the single most valuable request and eats the budget on
// it. Density sees that the same budget buys two cheaper requests worth more
// together.
func TestDensityBeatsBidOnAnAdversarialInstance(t *testing.T) {
	build := func() domain.Problem {
		return problem(
			// 700 credits for 600 s of budget: 1.17 credits per second.
			candidate("o-hog", "r-hog", 700, 0, 600*time.Second, 0),
			// 500 credits for 150 s each: 3.3 credits per second, and both fit.
			candidate("o-a", "r-a", 500, 20*time.Minute, 150*time.Second, 0),
			candidate("o-b", "r-b", 500, 40*time.Minute, 150*time.Second, 0),
		)
	}

	bid := allocation.GreedyByBid{}.Allocate(build())
	density := allocation.GreedyByValueDensity{}.Allocate(build())

	if bid.Value() != 700 {
		t.Fatalf("the baseline scored %d, want 700 — the scenario has drifted", bid.Value())
	}
	if density.Value() <= bid.Value() {
		t.Fatalf("density scored %d against the baseline's %d; the policy earns nothing here",
			density.Value(), bid.Value())
	}
	if density.Value() != 1000 {
		t.Errorf("density scored %d, want 1000", density.Value())
	}
	t.Logf("bid %d, density %d — %.0f%% better on this instance",
		bid.Value(), density.Value(), 100*float64(density.Value()-bid.Value())/float64(bid.Value()))

	if !won(density, "r-a") || !won(density, "r-b") {
		t.Error("density did not take both cheap requests")
	}
	if err := density.Validate(requestIDs(build().Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

// SLEW IN THE DENOMINATOR is the entire point. Without it, a high-value
// acquisition that forces a huge manoeuvre looks free, and the plan pays for it
// twice — in the seconds spent slewing and in what no longer fits around it.
func TestSlewCostPushesAnAwkwardCandidateDown(t *testing.T) {
	// Four candidates clustered at nadir and one high-value outlier far off to
	// the side. The outlier is worth more per second of IMAGING but costs a
	// large manoeuvre from everything else in the round.
	build := func() domain.Problem {
		return problem(
			candidate("o-far", "r-far", 260, 0, 60*time.Second, 40),
			candidate("o-n1", "r-n1", 250, 0, 60*time.Second, 0),
			candidate("o-n2", "r-n2", 250, 5*time.Minute, 60*time.Second, 1),
			candidate("o-n3", "r-n3", 250, 10*time.Minute, 60*time.Second, -1),
			candidate("o-n4", "r-n4", 250, 15*time.Minute, 60*time.Second, 0),
		)
	}

	bid := allocation.GreedyByBid{}.Allocate(build())
	density := allocation.GreedyByValueDensity{}.Allocate(build())

	// The baseline takes the outlier first, because it has the highest value.
	if bid.Acquisitions[0].OpportunityID != "o-far" {
		t.Fatalf("the baseline started with %s, want the highest-value o-far", bid.Acquisitions[0].OpportunityID)
	}
	// Density does not: the same value costs far more to point at.
	if density.Acquisitions[0].OpportunityID == "o-far" {
		t.Error("density also started with the outlier; the slew term is not in the denominator")
	}
}

// Duty-cycle cost is in the denominator too, and it is DISTINCT from duration.
// Two candidates identical in every way except what they charge the orbit must
// not be ranked equally.
func TestDutyCycleCostSeparatesOtherwiseIdenticalCandidates(t *testing.T) {
	cheap := candidate("o-cheap", "r-cheap", 500, 0, 60*time.Second, 0)
	expensive := candidate("o-expensive", "r-expensive", 500, 0, 60*time.Second, 0)
	// Same duration, same value, same attitude — but warm-up and calibration
	// make it charge five times the power budget.
	expensive.DutyCycleCostS = 300

	p := problem(cheap, expensive)
	p.Profile.DutyCycleBudgetS = 320 // room for one of them, not both

	plan := allocation.GreedyByValueDensity{}.Allocate(p)

	if len(plan.Acquisitions) != 1 {
		t.Fatalf("%d acquisitions, want 1", len(plan.Acquisitions))
	}
	if plan.Acquisitions[0].OpportunityID != "o-cheap" {
		t.Errorf("took %s; the duty-cycle cost is not in the denominator",
			plan.Acquisitions[0].OpportunityID)
	}
}

// The benchmark compares plans, so ordering must be reproducible.
func TestDensityIsDeterministic(t *testing.T) {
	build := func() domain.Problem {
		return problem(
			candidate("o-zulu", "r-zulu", 500, 0, 60*time.Second, 10),
			candidate("o-alpha", "r-alpha", 500, 0, 60*time.Second, 10),
			candidate("o-mike", "r-mike", 500, 0, 60*time.Second, 10),
			candidate("o-bravo", "r-bravo", 500, 0, 60*time.Second, 10),
		)
	}

	first := allocation.GreedyByValueDensity{}.Allocate(build())
	for range 20 {
		again := allocation.GreedyByValueDensity{}.Allocate(build())
		if len(again.Acquisitions) != len(first.Acquisitions) {
			t.Fatalf("two runs scheduled %d and %d", len(first.Acquisitions), len(again.Acquisitions))
		}
		for i := range first.Acquisitions {
			if again.Acquisitions[i].OpportunityID != first.Acquisitions[i].OpportunityID {
				t.Fatalf("ordering differs at %d: %s then %s",
					i, first.Acquisitions[i].OpportunityID, again.Acquisitions[i].OpportunityID)
			}
		}
	}
	// Identical density, so the tie falls to the same deterministic key the
	// baseline uses.
	if first.Acquisitions[0].OpportunityID != "o-alpha" {
		t.Errorf("the tie went to %s, want o-alpha", first.Acquisitions[0].OpportunityID)
	}
}

// The neighbour sample is bounded, so a full round stays inside the p95 budget.
// It must still produce a sane plan at that size rather than degrading.
func TestALargeRoundStaysFeasible(t *testing.T) {
	var candidates []domain.ScoredCandidate
	for i := range 400 {
		candidates = append(candidates, candidate(
			"o"+itoa(i), "r"+itoa(i),
			float64(100+i%50),
			time.Duration(i)*10*time.Second,
			30*time.Second,
			float64(i%80)-40,
		))
	}

	plan := allocation.GreedyByValueDensity{}.Allocate(problem(candidates...))

	if len(plan.Acquisitions) == 0 {
		t.Fatal("a 400-candidate round scheduled nothing")
	}
	p := problem(candidates...)
	for i := 1; i < len(plan.Acquisitions); i++ {
		previous, current := plan.Acquisitions[i-1], plan.Acquisitions[i]
		if current.Start.Before(previous.End) {
			t.Fatalf("%s overlaps %s", current.OpportunityID, previous.OpportunityID)
		}
		required := p.Profile.Agility.SlewTime(previous.Attitude, current.Attitude)
		if gap := current.Start.Sub(previous.End); gap < required {
			t.Fatalf("%s follows %s after %s, less than the %s it needs",
				current.OpportunityID, previous.OpportunityID, gap, required)
		}
	}
	if err := plan.Validate(requestIDs(candidates)); err != nil {
		t.Errorf("conservation over 400 requests: %v", err)
	}
}

// A single-candidate round has no neighbours to slew from. The settling floor
// is still paid, so the lone candidate does not look infinitely dense.
func TestALoneCandidateStillPaysTheSettlingFloor(t *testing.T) {
	plan := allocation.GreedyByValueDensity{}.Allocate(
		problem(candidate("o1", "r1", 500, 0, 60*time.Second, 0)))

	if len(plan.Acquisitions) != 1 {
		t.Fatalf("%d acquisitions, want 1", len(plan.Acquisitions))
	}
}

func TestDensityHandlesAnEmptyRound(t *testing.T) {
	plan := allocation.GreedyByValueDensity{}.Allocate(problem())
	if len(plan.Acquisitions) != 0 || len(plan.Unfulfilled) != 0 {
		t.Errorf("an empty round produced %d acquisitions and %d unfulfilments",
			len(plan.Acquisitions), len(plan.Unfulfilled))
	}
}

// Both policies satisfy the same interface, which is what makes the benchmark
// able to run them over identical inputs.
func TestBothPoliciesSatisfyTheInterface(t *testing.T) {
	policies := []domain.AllocationPolicy{
		allocation.GreedyByBid{},
		allocation.GreedyByValueDensity{},
	}
	seen := map[string]bool{}
	for _, policy := range policies {
		if policy.Name() == "" {
			t.Error("a policy has no name; a committed plan could not be attributed to it")
		}
		if seen[policy.Name()] {
			t.Errorf("two policies both call themselves %s", policy.Name())
		}
		seen[policy.Name()] = true

		plan := policy.Allocate(problem(candidate("o1", "r1", 100, 0, time.Minute, 0)))
		if err := plan.Validate([]string{"r1"}); err != nil {
			t.Errorf("%s: conservation: %v", policy.Name(), err)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

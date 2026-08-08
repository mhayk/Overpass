package allocation_test

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/allocation"
	"github.com/mhayk/overpass/services/planner/internal/domain"
)

func TestExactNameIsTheContractEnumValue(t *testing.T) {
	if got := allocation.NewExactDP().Name(); got != "EXACT_DP" {
		t.Errorf("Name = %q", got)
	}
}

// The instance whose optimum is computable by hand: 700 for the hog, or 1000
// for the two it crowds out.
func TestExactFindsTheHandComputedOptimum(t *testing.T) {
	p := problem(
		candidate("o-hog", "r-hog", 700, 0, 600*time.Second, 0),
		candidate("o-a", "r-a", 500, 20*time.Minute, 150*time.Second, 0),
		candidate("o-b", "r-b", 500, 40*time.Minute, 150*time.Second, 0),
	)

	plan, report := allocation.NewExactDP().Solve(p)

	if !report.Optimal {
		t.Fatalf("the search did not complete: %s", report.Reason)
	}
	if plan.Value() != 1000 {
		t.Errorf("optimum = %d, want 1000", plan.Value())
	}
	if report.Bound != 1000 {
		t.Errorf("bound = %v, want 1000 when the search is complete", report.Bound)
	}
	if won(plan, "r-hog") {
		t.Error("the optimum included the hog; it crowds out 1000 credits of cheaper work")
	}
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

// THE STRONGEST CORRECTNESS CHECK IN THE PLANNER.
//
// On instances the exact solver actually solves, no heuristic may report a
// higher plan value. A heuristic that wins is not clever — it is violating a
// constraint the exact solver respected, and this is the test that finds it.
// Nothing else would: each policy's own tests assert its plans are feasible,
// but a policy could be feasible AND be scoring something it should not.
func TestNoHeuristicEverExceedsTheOptimum(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 32)) //nolint:gosec // deterministic by design
	exact := allocation.NewExactDP()

	heuristics := []domain.AllocationPolicy{
		allocation.GreedyByBid{},
		allocation.GreedyByValueDensity{},
	}

	var compared, tight int
	for instance := range 120 {
		p := randomSmallProblem(rng, instance)

		optimal, report := exact.Solve(p)
		if !report.Optimal {
			continue // an unsolved instance proves nothing either way
		}
		compared++

		for _, policy := range heuristics {
			plan := policy.Allocate(p)

			if plan.Value() > optimal.Value() {
				t.Fatalf("instance %d: %s scored %d against an optimum of %d — it is violating a constraint the exact solver respected",
					instance, policy.Name(), plan.Value(), optimal.Value())
			}
			if plan.Value() == optimal.Value() {
				tight++
			}
			// A heuristic's plan must also be feasible in its own right, so a
			// failure above is attributable to scoring rather than to placement.
			assertPlanIsFeasible(t, p, plan, policy.Name())
		}
	}

	if compared < 50 {
		t.Fatalf("only %d instances were solved to optimality; the test is not exercising what it claims", compared)
	}
	t.Logf("%d instances solved exactly, %d heuristic runs matched the optimum", compared, tight)
}

// The optimum itself must be flyable. An "optimal" plan that violates slew is
// not a bound on anything.
func TestTheOptimalPlanIsFeasible(t *testing.T) {
	rng := rand.New(rand.NewPCG(33, 34)) //nolint:gosec // deterministic by design
	exact := allocation.NewExactDP()

	for instance := range 60 {
		p := randomSmallProblem(rng, instance)
		plan, report := exact.Solve(p)
		if !report.Optimal {
			continue
		}
		assertPlanIsFeasible(t, p, plan, "EXACT_DP")
		if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
			t.Fatalf("instance %d: conservation: %v", instance, err)
		}
	}
}

// The size limit is LOUD. A silently degrading exact solver would corrupt the
// reference the whole benchmark divides by, and the corruption would look like
// a heuristic performing suspiciously well.
func TestAnOversizedInstanceIsRefusedLoudly(t *testing.T) {
	var candidates []domain.ScoredCandidate
	for i := range 30 {
		candidates = append(candidates, candidate("o"+itoa(i), "r"+itoa(i), 100, time.Duration(i)*time.Minute, time.Minute, 0))
	}
	p := problem(candidates...)

	plan, report := allocation.NewExactDP().Solve(p)

	if report.Optimal {
		t.Fatal("a 30-candidate instance reported an optimum; the limit is not being enforced")
	}
	if report.Reason == "" {
		t.Error("no reason given; a caller cannot tell a refusal from an empty optimum")
	}
	if len(plan.Acquisitions) != 0 {
		t.Errorf("%d acquisitions scheduled beyond the limit; the solver degraded instead of refusing",
			len(plan.Acquisitions))
	}
	// Every request still gets an outcome, so the conservation property holds
	// even when the solver refuses.
	if err := plan.Validate(requestIDs(candidates)); err != nil {
		t.Errorf("conservation on a refused instance: %v", err)
	}
}

// A timeout returns the best plan FOUND, and says it is not proven optimal.
// Those are different claims and the benchmark must not average them.
func TestATimeoutReturnsTheBestKnownAndSaysSo(t *testing.T) {
	// A clock that has already run out on the first check.
	frozen := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	elapsed := 0
	solver := allocation.ExactDP{
		MaxCandidates: 14,
		Timeout:       time.Nanosecond,
		Now: func() time.Time {
			elapsed++
			return frozen.Add(time.Duration(elapsed) * time.Second)
		},
	}

	var candidates []domain.ScoredCandidate
	for i := range 12 {
		candidates = append(candidates, candidate(
			"o"+itoa(i), "r"+itoa(i), float64(100+i), time.Duration(i)*time.Minute, time.Minute, float64(i%30)))
	}

	plan, report := solver.Solve(problem(candidates...))

	if report.Optimal {
		t.Fatal("a search stopped by the clock reported itself optimal")
	}
	if report.Reason == "" {
		t.Error("no reason; a caller would treat a partial answer as a proven optimum")
	}
	// Whatever it found must still be a legal plan.
	assertPlanIsFeasible(t, problem(candidates...), plan, "EXACT_DP")
}

// An empty round has an optimum of zero, and that is a complete answer rather
// than a failure.
func TestAnEmptyInstanceIsOptimallyEmpty(t *testing.T) {
	plan, report := allocation.NewExactDP().Solve(problem())

	if !report.Optimal {
		t.Errorf("an empty instance was not solved: %s", report.Reason)
	}
	if plan.Value() != 0 || len(plan.Acquisitions) != 0 {
		t.Errorf("an empty instance produced %d acquisitions worth %d", len(plan.Acquisitions), plan.Value())
	}
}

// randomSmallProblem builds instances small enough to solve exactly and
// contended enough that the policies disagree.
func randomSmallProblem(rng *rand.Rand, seed int) domain.Problem {
	count := 4 + rng.IntN(6) // 4..9 candidates
	var candidates []domain.ScoredCandidate
	for i := range count {
		c := candidate(
			"i"+itoa(seed)+"-o"+itoa(i),
			"i"+itoa(seed)+"-r"+itoa(i),
			float64(100+rng.IntN(900)),
			time.Duration(rng.IntN(40))*time.Minute,
			time.Duration(30+rng.IntN(240))*time.Second,
			rng.Float64()*80-40,
		)
		// Duty-cycle cost distinct from duration, which is what makes the
		// budget a knapsack rather than a restatement of the interval.
		c.DutyCycleCostS = c.AcquisitionDurationS * (1 + rng.Float64())
		candidates = append(candidates, c)
	}

	p := problem(candidates...)
	// Tight enough that not everything fits, which is the only regime where a
	// comparison between policies means anything.
	p.Profile.DutyCycleBudgetS = 200 + rng.Float64()*500
	p.Profile.Agility.SlewRateDegS = 0.5 + rng.Float64()*2
	return p
}

func assertPlanIsFeasible(t *testing.T, p domain.Problem, plan domain.Plan, who string) {
	t.Helper()

	byOpportunity := map[string]domain.ScoredCandidate{}
	for _, c := range p.Candidates {
		byOpportunity[c.OpportunityID] = c
	}

	spent := map[int]float64{}
	seenRequests := map[string]bool{}
	for i, a := range plan.Acquisitions {
		if seenRequests[a.RequestID] {
			t.Fatalf("%s: request %s holds two acquisitions", who, a.RequestID)
		}
		seenRequests[a.RequestID] = true

		c := byOpportunity[a.OpportunityID]
		if a.Start.Before(c.AccessStart) || a.Start.After(c.AccessEnd) {
			t.Fatalf("%s: %s starts outside its access window", who, a.OpportunityID)
		}
		if a.End.After(c.Deadline) {
			t.Fatalf("%s: %s finishes after its deadline", who, a.OpportunityID)
		}
		spent[a.OrbitNumber] += a.DutyCycleCostS

		if i == 0 {
			continue
		}
		previous := plan.Acquisitions[i-1]
		if a.Start.Before(previous.End) {
			t.Fatalf("%s: %s overlaps %s", who, a.OpportunityID, previous.OpportunityID)
		}
		required := p.Profile.Agility.SlewTime(previous.Attitude, a.Attitude)
		if gap := a.Start.Sub(previous.End); gap < required {
			t.Fatalf("%s: %s follows %s after %s, less than the %s slew it needs",
				who, a.OpportunityID, previous.OpportunityID, gap, required)
		}
	}
	for orbit, used := range spent {
		if used > p.Profile.DutyCycleBudgetS {
			t.Fatalf("%s: orbit %d spent %.2f s against a %.2f s budget",
				who, orbit, used, p.Profile.DutyCycleBudgetS)
		}
	}
}

// Allocate is the AllocationPolicy face of the solver: same answer, report
// discarded. A benchmark uses Solve; a round would use this.
func TestExactAllocateMatchesSolve(t *testing.T) {
	p := problem(
		candidate("o-hog", "r-hog", 700, 0, 600*time.Second, 0),
		candidate("o-a", "r-a", 500, 20*time.Minute, 150*time.Second, 0),
		candidate("o-b", "r-b", 500, 40*time.Minute, 150*time.Second, 0),
	)

	viaSolve, _ := allocation.NewExactDP().Solve(p)
	viaAllocate := allocation.NewExactDP().Allocate(p)

	if viaAllocate.Value() != viaSolve.Value() {
		t.Errorf("Allocate scored %d and Solve %d", viaAllocate.Value(), viaSolve.Value())
	}
	if len(viaAllocate.Acquisitions) != len(viaSolve.Acquisitions) {
		t.Errorf("Allocate scheduled %d and Solve %d",
			len(viaAllocate.Acquisitions), len(viaSolve.Acquisitions))
	}
}

// A zero-value solver falls back to the defaults rather than treating "no limit
// configured" as "no limit" — which on an exponential search is an outage.
func TestAZeroValueSolverUsesTheDefaults(t *testing.T) {
	var candidates []domain.ScoredCandidate
	for i := range 30 {
		candidates = append(candidates, candidate("o"+itoa(i), "r"+itoa(i), 100, time.Duration(i)*time.Minute, time.Minute, 0))
	}

	_, report := allocation.ExactDP{}.Solve(problem(candidates...))

	if report.Optimal {
		t.Fatal("a zero-value solver accepted a 30-candidate instance; an unconfigured limit is not an absent one")
	}
}

// An unusable profile is refused before the search starts, and every request
// still gets an outcome.
func TestExactRefusesAnUnusableProfile(t *testing.T) {
	p := problem(
		candidate("o1", "r1", 100, 0, time.Minute, 0),
		candidate("o2", "r2", 100, 5*time.Minute, time.Minute, 0),
	)
	p.Profile.DutyCycleBudgetS = 0

	plan, report := allocation.NewExactDP().Solve(p)

	if report.Optimal {
		t.Fatal("an unusable profile reported an optimum")
	}
	if len(plan.Unfulfilled) != 2 {
		t.Errorf("%d unfulfilments, want one per request", len(plan.Unfulfilled))
	}
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

// Several candidates for ONE request: the optimum takes at most one of them,
// and the request appears exactly once in the plan.
func TestExactTakesAtMostOneCandidatePerRequest(t *testing.T) {
	p := problem(
		candidate("o-a", "r-same", 500, 0, time.Minute, 0),
		candidate("o-b", "r-same", 400, 20*time.Minute, time.Minute, 0),
		candidate("o-c", "r-same", 300, 40*time.Minute, time.Minute, 0),
		candidate("o-other", "r-other", 200, 60*time.Minute, time.Minute, 0),
	)

	plan, report := allocation.NewExactDP().Solve(p)
	if !report.Optimal {
		t.Fatalf("not solved: %s", report.Reason)
	}

	held := 0
	for _, a := range plan.Acquisitions {
		if a.RequestID == "r-same" {
			held++
		}
	}
	if held != 1 {
		t.Errorf("r-same holds %d acquisitions, want 1 — the same target would be imaged twice", held)
	}
	if plan.Value() != 700 {
		t.Errorf("optimum = %d, want 700 (the best option for r-same plus r-other)", plan.Value())
	}
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

// An instance where NOTHING can be scheduled still solves, optimally, to an
// empty plan — and everyone is told why.
func TestAnInfeasibleInstanceIsOptimallyEmpty(t *testing.T) {
	p := problem(
		candidate("o1", "r1", 500, 0, time.Minute, 60), // beyond roll authority
		candidate("o2", "r2", 400, 0, time.Minute, 55),
	)

	plan, report := allocation.NewExactDP().Solve(p)

	if !report.Optimal {
		t.Fatalf("an infeasible instance was not solved: %s", report.Reason)
	}
	if len(plan.Acquisitions) != 0 {
		t.Errorf("%d acquisitions scheduled from candidates none of which can be flown", len(plan.Acquisitions))
	}
	if len(plan.Unfulfilled) != 2 {
		t.Errorf("%d unfulfilments, want one per request", len(plan.Unfulfilled))
	}
}

package domain_test

import (
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

func ledger(t *testing.T, budgetS float64) *domain.DutyCycleLedger {
	t.Helper()
	l, err := domain.NewDutyCycleLedger(budgetS)
	if err != nil {
		t.Fatalf("opening a ledger at %v s: %v", budgetS, err)
	}
	return l
}

func TestABudgetMustBePositive(t *testing.T) {
	for _, budget := range []float64{0, -1, -600} {
		if _, err := domain.NewDutyCycleLedger(budget); err == nil {
			t.Errorf("opened a ledger at %v s", budget)
		} else if !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%v: does not wrap ErrInvalid: %v", budget, err)
		}
	}
}

func TestChargingSpendsTheOrbitsBudget(t *testing.T) {
	l := ledger(t, 600)

	if got := l.RemainingS(47110); got != 600 {
		t.Fatalf("a fresh orbit has %v s remaining, want the full 600", got)
	}
	if err := l.Charge(47110, 250); err != nil {
		t.Fatalf("charging 250 s: %v", err)
	}
	if got := l.SpentS(47110); got != 250 {
		t.Errorf("spent = %v, want 250", got)
	}
	if got := l.RemainingS(47110); got != 350 {
		t.Errorf("remaining = %v, want 350", got)
	}
}

// PER ORBIT, not per bucket. Orbits refill independently, because power
// recovery is tied to the orbital cycle — averaging over a bucket would permit
// a burst the spacecraft cannot perform.
func TestOrbitsAreIndependent(t *testing.T) {
	l := ledger(t, 600)

	if err := l.Charge(47110, 600); err != nil {
		t.Fatalf("filling orbit 47110: %v", err)
	}
	if got := l.RemainingS(47110); got != 0 {
		t.Errorf("the filled orbit has %v s left, want 0", got)
	}
	// The next revolution starts fresh.
	if got := l.RemainingS(47111); got != 600 {
		t.Errorf("the next orbit has %v s, want the full 600 — the budget is being averaged over the bucket, which permits a burst the spacecraft cannot perform", got)
	}
	if err := l.Charge(47111, 600); err != nil {
		t.Errorf("the next orbit refused a full charge: %v", err)
	}
}

// The refusal is the invariant. A ledger that allowed an overdraft and expected
// callers to correct it would produce a plan that is wrong in a way no test of
// the policy would catch.
func TestAnOverrunIsRefused(t *testing.T) {
	l := ledger(t, 600)

	if err := l.Charge(47110, 590); err != nil {
		t.Fatalf("charging 590 s: %v", err)
	}
	err := l.Charge(47110, 20)
	if err == nil {
		t.Fatal("a charge that overruns the budget was accepted")
	}
	if !errors.Is(err, domain.ErrDutyCycleExhausted) {
		t.Errorf("does not wrap ErrDutyCycleExhausted: %v", err)
	}
	// A refused charge must not have partially spent.
	if got := l.SpentS(47110); got != 590 {
		t.Errorf("spent = %v after a refusal, want the original 590", got)
	}

	// Exhaustion is a scheduling OUTCOME, not a fault. Conflating it with
	// ErrInvalid would make a normal allocation result look like a defect.
	if errors.Is(err, domain.ErrInvalid) {
		t.Error("exhaustion also wraps ErrInvalid; a lost candidate would be reported as malformed")
	}
}

// The boundary is inclusive: a candidate that exactly fills the remaining
// budget fits. Off-by-one here silently wastes a slot on every satellite.
func TestExactlyFillingTheBudgetIsAllowed(t *testing.T) {
	l := ledger(t, 600)

	if err := l.Charge(47110, 600); err != nil {
		t.Fatalf("a charge exactly equal to the budget was refused: %v", err)
	}
	if got := l.RemainingS(47110); got != 0 {
		t.Errorf("remaining = %v, want 0", got)
	}
	if err := l.Charge(47110, 0.0001); err == nil {
		t.Error("a charge against an exhausted orbit was accepted")
	}
}

// DUTY_CYCLE_EXHAUSTED is supposed to be actionable: "you needed 43 s and 12
// were left in orbit 47110" tells a customer whether to rebid or move the
// request. "Budget exhausted" tells them nothing.
func TestTheShortfallCarriesBothNumbers(t *testing.T) {
	l := ledger(t, 600)
	if err := l.Charge(47110, 588); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ok, short := l.CanAfford(47110, 43)
	if ok {
		t.Fatal("43 s fitted in 12 s remaining")
	}
	if short.Orbit != 47110 {
		t.Errorf("orbit = %d, want 47110", short.Orbit)
	}
	if short.RequiredS != 43 {
		t.Errorf("required = %v, want 43", short.RequiredS)
	}
	if short.RemainingS != 12 {
		t.Errorf("remaining = %v, want 12", short.RemainingS)
	}
	// It has to read as an explanation, not as a struct dump.
	if got := short.String(); got == "" {
		t.Error("the shortfall has no readable form")
	}
}

func TestNegativeCostsAreRefused(t *testing.T) {
	l := ledger(t, 600)
	err := l.Charge(47110, -10)
	if err == nil {
		t.Fatal("a negative charge was accepted; it would refund budget nobody spent")
	}
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a negative cost is malformed input, not an exhausted budget: %v", err)
	}
}

// PROPERTY: no sequence of accepted charges ever exceeds the budget, for any
// orbit. This is the domain half of the invariant M2-12 asserts over committed
// plans.
func TestNoAcceptedSequenceEverExceedsTheBudget(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12)) //nolint:gosec // deterministic by design

	for range 200 {
		budget := 60 + rng.Float64()*900
		l := ledger(t, budget)
		orbits := []int{47110, 47111, 47112}

		for range 200 {
			orbit := orbits[rng.IntN(len(orbits))]
			// Costs that frequently exceed what is left, so refusals are
			// exercised rather than incidental.
			cost := rng.Float64() * budget * 0.4
			_ = l.Charge(orbit, cost) //nolint:errcheck // a refusal is a valid outcome here

			for _, o := range orbits {
				if spent := l.SpentS(o); spent > budget {
					t.Fatalf("orbit %d spent %.3f s against a %.3f s budget", o, spent, budget)
				}
				if remaining := l.RemainingS(o); remaining < 0 {
					t.Fatalf("orbit %d has %.3f s remaining", o, remaining)
				}
			}
		}
	}
}

// Usage goes onto plan metrics that a human reads and a golden test compares.
// Map iteration order would make the same plan serialise differently on every
// commit.
func TestUsageIsOrderedAndProportional(t *testing.T) {
	l := ledger(t, 600)
	for orbit, cost := range map[int]float64{47112: 60, 47110: 300, 47111: 150} {
		if err := l.Charge(orbit, cost); err != nil {
			t.Fatalf("charging orbit %d: %v", orbit, err)
		}
	}

	usage := l.Usage()
	if len(usage) != 3 {
		t.Fatalf("reported %d orbits, want 3", len(usage))
	}
	for i := 1; i < len(usage); i++ {
		if usage[i-1].Orbit >= usage[i].Orbit {
			t.Fatalf("usage is not in orbit order: %v", usage)
		}
	}
	if usage[0].Orbit != 47110 || usage[0].SpentS != 300 {
		t.Errorf("first entry = %+v", usage[0])
	}
	if usage[0].Utilisation != 0.5 {
		t.Errorf("utilisation = %v, want 0.5", usage[0].Utilisation)
	}
	// An orbit nobody touched must not appear. Reporting every orbit in the
	// horizon at 0% would bury the ones that matter.
	for _, u := range usage {
		if u.SpentS == 0 {
			t.Errorf("orbit %d appears in usage having spent nothing", u.Orbit)
		}
	}
}

// A candidate with no orbit number cannot be charged against any budget, so it
// is HELD rather than skipped — see OrbitOf's comment for the full argument.
func TestACandidateWithoutAnOrbitIsNotSchedulable(t *testing.T) {
	c := validCandidate()
	c.OrbitNumber = nil

	if _, ok := domain.OrbitOf(c); ok {
		t.Error("a candidate with no orbit reported as schedulable; it would be charged against a budget that does not exist")
	}
}

func TestOrbitZeroIsSchedulable(t *testing.T) {
	c := validCandidate()
	zero := 0
	c.OrbitNumber = &zero

	orbit, ok := domain.OrbitOf(c)
	if !ok {
		t.Fatal("orbit 0 was treated as absent")
	}
	if orbit != 0 {
		t.Errorf("orbit = %d, want 0", orbit)
	}
}

// duty_cycle_cost_s is DISTINCT from acquisition duration, and that difference
// is what makes this a knapsack constraint rather than a restatement of the
// interval constraint. Modes with warm-up or calibration overhead charge more
// than they occupy.
func TestTheCostMayExceedTheAcquisitionDuration(t *testing.T) {
	c := validCandidate()
	c.AcquisitionDurationS = 18.5
	c.DutyCycleCostS = 42.0 // warm-up plus calibration

	if err := c.Validate(); err != nil {
		t.Fatalf("a cost exceeding the duration was refused: %v", err)
	}

	l := ledger(t, 100)
	orbit, ok := domain.OrbitOf(c)
	if !ok {
		t.Fatal("the baseline candidate has no orbit")
	}
	if err := l.Charge(orbit, c.DutyCycleCostS); err != nil {
		t.Fatalf("charging: %v", err)
	}
	// If the ledger had charged the duration instead, 81.5 s would remain.
	if got := l.RemainingS(orbit); got != 58 {
		t.Errorf("remaining = %v, want 58 — the ledger charged the duration rather than the duty-cycle cost", got)
	}
}

func TestTheLedgerReportsItsBudget(t *testing.T) {
	l := ledger(t, 742.5)
	if got := l.BudgetS(); got != 742.5 {
		t.Errorf("BudgetS = %v, want 742.5", got)
	}
	// And it does not drift as the ledger is spent — the budget is the
	// allowance, not the remainder.
	if err := l.Charge(47110, 100); err != nil {
		t.Fatalf("charging: %v", err)
	}
	if got := l.BudgetS(); got != 742.5 {
		t.Errorf("BudgetS = %v after spending, want the unchanged 742.5", got)
	}
}

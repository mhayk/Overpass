package domain

import (
	"fmt"
	"sort"
)

// The per-orbit duty-cycle budget: the knapsack dimension of the allocation
// problem.
//
// A SAR satellite cannot image continuously — the transmitter draws more power
// than the panels replace and dumps heat the radiators shed slowly. The budget
// is the imaging seconds available before either limit bites.
//
// PER ORBIT, not per bucket, and the distinction is physical rather than
// bookkeeping. Power recovery is tied to the orbital cycle: the satellite
// charges through the sunlit arc and radiates continuously, so the ledger
// refills once per revolution. Averaging over a three-hour bucket would permit
// spending two orbits' worth of budget inside one — a burst the spacecraft
// cannot actually perform, and a plan that looks fine and cannot be flown.
//
// This is what makes the problem a knapsack on top of the interval constraints,
// and it is the reduction ADR-0007 uses for its WEAK NP-hardness argument. The
// strong one runs through slew.

// DutyCycleLedger tracks imaging seconds spent per orbit against one
// satellite's budget.
//
// A mutable ledger rather than a pure function, because allocation is
// sequential: a policy adds acquisitions one at a time and each has to be
// priced against what the earlier ones already spent. The zero value is not
// usable — NewDutyCycleLedger is what establishes the budget.
type DutyCycleLedger struct {
	budgetS float64
	spent   map[int]float64
}

// NewDutyCycleLedger opens a ledger against a per-orbit budget.
func NewDutyCycleLedger(budgetS float64) (*DutyCycleLedger, error) {
	if budgetS <= 0 {
		// reference.satellites CHECKs this, so a violating row cannot exist —
		// but a zero budget reaching here would make every candidate
		// unaffordable and the plan would come out empty with no explanation
		// anybody could act on.
		return nil, fmt.Errorf("%w: duty-cycle budget must be positive, got %v s", ErrInvalid, budgetS)
	}
	return &DutyCycleLedger{budgetS: budgetS, spent: map[int]float64{}}, nil
}

// BudgetS is the per-orbit allowance.
func (l *DutyCycleLedger) BudgetS() float64 { return l.budgetS }

// SpentS is what one orbit has already been charged.
func (l *DutyCycleLedger) SpentS(orbit int) float64 { return l.spent[orbit] }

// RemainingS is what is left in one orbit.
func (l *DutyCycleLedger) RemainingS(orbit int) float64 { return l.budgetS - l.spent[orbit] }

// Shortfall describes a candidate that does not fit.
//
// Carries both numbers because the contract's DUTY_CYCLE_EXHAUSTED reason is
// supposed to be actionable: "you needed 43 seconds and 12 were left in orbit
// 47110" tells a customer whether to rebid or to move the request, where
// "budget exhausted" tells them nothing.
type Shortfall struct {
	Orbit      int
	RequiredS  float64
	RemainingS float64
}

func (s Shortfall) String() string {
	return fmt.Sprintf("orbit %d needed %.1fs with %.1fs remaining", s.Orbit, s.RequiredS, s.RemainingS)
}

// CanAfford reports whether a cost fits in an orbit's remaining budget, and
// what is missing when it does not.
//
// The shortfall is returned alongside the answer rather than recomputed by the
// caller, because the caller is a policy in a hot loop and the numbers it would
// need are already in hand here.
func (l *DutyCycleLedger) CanAfford(orbit int, costS float64) (bool, Shortfall) {
	remaining := l.RemainingS(orbit)
	if costS <= remaining {
		return true, Shortfall{}
	}
	return false, Shortfall{Orbit: orbit, RequiredS: costS, RemainingS: remaining}
}

// Charge spends against an orbit, refusing anything that would overrun.
//
// Refusing rather than allowing an overdraft is the invariant. A policy that
// could overspend and be corrected afterwards would need every caller to
// remember the correction, and the one that forgets produces a plan that is
// wrong in a way no test of that policy would catch — which is exactly what the
// property test in M2-12 exists to prevent at the plan level.
func (l *DutyCycleLedger) Charge(orbit int, costS float64) error {
	if costS < 0 {
		return fmt.Errorf("%w: duty-cycle cost cannot be negative, got %v s", ErrInvalid, costS)
	}
	if ok, short := l.CanAfford(orbit, costS); !ok {
		return fmt.Errorf("%w: %s", ErrDutyCycleExhausted, short)
	}
	l.spent[orbit] += costS
	return nil
}

// ErrDutyCycleExhausted marks a refusal that is a scheduling outcome rather
// than a fault.
//
// Its own sentinel, distinct from ErrInvalid. A candidate that does not fit is
// not malformed — it lost, and it earns a DUTY_CYCLE_EXHAUSTED unfulfilment
// rather than an error in a log. Conflating the two would make a normal
// allocation outcome look like a defect.
var ErrDutyCycleExhausted = fmt.Errorf("duty cycle exhausted")

// Usage summarises what a plan spent, for the committed plan's metrics.
type Usage struct {
	Orbit       int
	SpentS      float64
	BudgetS     float64
	Utilisation float64
}

// Usage returns per-orbit consumption in orbit order.
//
// Sorted, because it goes onto plan metrics that a human reads and a test
// compares. Map iteration order would make the same plan serialise differently
// on every commit, which turns a golden test into a coin toss.
func (l *DutyCycleLedger) Usage() []Usage {
	orbits := make([]int, 0, len(l.spent))
	for orbit := range l.spent {
		orbits = append(orbits, orbit)
	}
	sort.Ints(orbits)

	out := make([]Usage, 0, len(orbits))
	for _, orbit := range orbits {
		spent := l.spent[orbit]
		out = append(out, Usage{
			Orbit:       orbit,
			SpentS:      spent,
			BudgetS:     l.budgetS,
			Utilisation: spent / l.budgetS,
		})
	}
	return out
}

// OrbitOf resolves which orbit a candidate is charged against.
//
// A candidate with no orbit number is NOT schedulable, and returns false.
//
// The contract permits it: orbit_number is absent from the required list of an
// opportunity item, and 00006_planning_inputs.sql made the column nullable
// rather than reject a contract-valid event. It then said M2-03 must decide, in
// the open, whether such a candidate is skipped or charged to a synthetic
// bucket. This is that decision.
//
// A synthetic bucket was rejected because it is not conservative when mixed. If
// unknown-orbit candidates share their own pool, two candidates that are really
// in the same orbit can each fit under separate budgets and together overrun the
// real one — a plan that overspends the spacecraft while every ledger balances.
//
// Deriving the orbit from the access window was rejected on ADR-0001: it needs
// the orbital period, which means orbital arithmetic in the Go service, and the
// polyglot split exists precisely so the physics lives in one place with golden
// tests on it.
//
// So the candidate is HELD, exactly as ADR-0015 holds one whose request
// snapshot has not landed: stored, invisible to the round, and schedulable the
// moment the field arrives. It is not reported as unfulfilled, because none of
// the contract's seven reason codes describes "the producer omitted an optional
// field" and inventing a meaning for NO_OPPORTUNITY_IN_BUCKET would tell a
// customer something untrue.
//
// In practice this path is defensive: feasibility-service types orbit_number as
// a plain int and always populates it (pipeline.py:66). If that ever changes,
// the effect is candidates silently not flying — so the planner counts them, and
// M2-04 surfaces the count.
func OrbitOf(c Candidate) (int, bool) {
	if c.OrbitNumber == nil {
		return 0, false
	}
	return *c.OrbitNumber, true
}

package domain

import (
	"fmt"
	"time"
)

// Effective value: the single quantity every policy allocates by.
//
// ADR-0007 keeps fairness OUT of the policies deliberately. All four allocate by
// this number and none of them sees a priority tier, which is what keeps "should
// civil protection outrank a commercial bid?" a product question with one
// implementation rather than a scheduling question with four.
//
// Computed in one place, here, and it is the only place. A second formula
// anywhere would be a second fairness policy nobody agreed to.

// Fairness turns a request into the number policies compete on.
//
// Planner configuration, deliberately kept out of the published contract, so it
// can be re-tuned without versioning a schema — and so clients cannot optimise
// against a published formula.
type Fairness struct {
	// TierMultipliers scales the bid by who is asking. A civil-protection
	// request with a low bid can outrank a commercial one with a high bid, but
	// not automatically: money still means something, which a strict
	// tier-dominates-bid ordering would destroy along with any reason to run an
	// auction at all.
	TierMultipliers map[string]float64

	// AgeingTimeConstant is how long a request takes to gain one full
	// multiple of weight.
	AgeingTimeConstant time.Duration

	// MaxAgeingFactor caps the growth. Unbounded ageing eventually lets an
	// ancient trivial request outrank an urgent civil-protection one, which is
	// a worse failure than the starvation it fixes.
	MaxAgeingFactor float64
}

// DefaultFairness is the starting configuration.
//
// Not measured. The starvation-versus-value tradeoff these numbers encode is
// what M2-13 is supposed to put numbers on, and recording them as a default
// rather than a constant is what lets the benchmark vary them.
func DefaultFairness() Fairness {
	return Fairness{
		TierMultipliers: map[string]float64{
			"GOVERNMENT":       4.0,
			"CIVIL_PROTECTION": 3.0,
			"COMMERCIAL":       1.0,
			"BEST_EFFORT":      0.5,
		},
		AgeingTimeConstant: 6 * time.Hour,
		MaxAgeingFactor:    3.0,
	}
}

// Validate refuses a configuration that cannot behave as the fairness model
// describes.
func (f Fairness) Validate() error {
	if len(f.TierMultipliers) == 0 {
		return fmt.Errorf("%w: no tier multipliers configured", ErrInvalid)
	}
	// Every tier the schema accepts must have a multiplier, or a
	// contract-valid request would be valued at zero and lose every round in
	// silence.
	for tier := range PriorityTiers {
		multiplier, ok := f.TierMultipliers[tier]
		if !ok {
			return fmt.Errorf("%w: tier %s has no multiplier, so such requests would be valued at zero", ErrInvalid, tier)
		}
		if multiplier <= 0 {
			return fmt.Errorf("%w: tier %s has multiplier %v; a non-positive multiplier means that tier can never win", ErrInvalid, tier, multiplier)
		}
	}
	if f.AgeingTimeConstant <= 0 {
		return fmt.Errorf("%w: ageing time constant must be positive, got %s", ErrInvalid, f.AgeingTimeConstant)
	}
	if f.MaxAgeingFactor < 1 {
		return fmt.Errorf("%w: max ageing factor must be at least 1 (no ageing), got %v", ErrInvalid, f.MaxAgeingFactor)
	}

	// THE BOUND THAT MATTERS. Ageing must not be able to invert the tier
	// ordering entirely: at equal bids, the lowest tier aged to the cap must
	// still lose to the highest tier at zero age. Otherwise a stale best-effort
	// request outranks an urgent government one, which is exactly the failure
	// the cap exists to prevent — and it would appear only after the system had
	// been running long enough for anything to be stale.
	highest, lowest := 0.0, 0.0
	for _, m := range f.TierMultipliers {
		if highest == 0 || m > highest {
			highest = m
		}
		if lowest == 0 || m < lowest {
			lowest = m
		}
	}
	if ratio := highest / lowest; f.MaxAgeingFactor >= ratio {
		return fmt.Errorf(
			"%w: max ageing factor %v is not below the tier spread %v (%v/%v); an aged bottom-tier request could outrank a fresh top-tier one",
			ErrInvalid, f.MaxAgeingFactor, ratio, highest, lowest)
	}
	return nil
}

// AgeingFactor is how much weight a request has gained by waiting.
//
// Linear in age, saturating at MaxAgeingFactor. Measured from submitted_at —
// from ACCEPTANCE, not from when the planner first heard about the request —
// because a slow consumer would otherwise silently reset a customer's accrued
// fairness. ADR-0015 projected submitted_at for exactly this.
//
// TIME, NOT ROUND COUNT, and the choice is deliberate. Counting lost rounds
// would make effective value a function of allocation history, so the same
// candidate set would score differently depending on how many rounds happened
// to have fired — which is the dependence ADR-0014 rejected when it rejected the
// incumbency bonus, and which would break M2-13's identical-inputs premise. The
// clock is already an input to a round (deadlines are checked against it), so
// ageing by time adds no new kind of dependence at all.
//
// The contract's age_rounds field describes itself as "input to the fairness
// ageing factor". It is emitted as an observability counter instead — see
// the M2-09 pull request. Its DESCRIPTION is the thing out of step, and
// descriptions are not constraints.
func (f Fairness) AgeingFactor(submittedAt, now time.Time) float64 {
	age := now.Sub(submittedAt)
	if age <= 0 {
		// A request submitted in the future has not aged. Clock skew between
		// services is real and a negative age would REDUCE weight below the
		// floor, quietly penalising the newest requests.
		return 1
	}
	factor := 1 + age.Seconds()/f.AgeingTimeConstant.Seconds()
	return min(factor, f.MaxAgeingFactor)
}

// EffectiveValue is the number policies allocate by.
//
//	effective = bid × tier multiplier × ageing factor
//
// Multiplicative rather than additive, and the difference is who it favours: a
// flat bonus helps low bids disproportionately, while a multiplier amplifies
// whatever is already there. Multiplicative also keeps the formula
// scale-invariant — an additive bonus is denominated in credits and has to be
// recalibrated every time the bid scale moves.
func (f Fairness) EffectiveValue(s Snapshot, now time.Time) float64 {
	return float64(s.BidCredits) *
		f.TierMultipliers[s.PriorityTier] *
		f.AgeingFactor(s.SubmittedAt, now)
}

// TimeToOutrank reports how long a request must wait before its effective value
// exceeds a rival's, or false if it never will.
//
// The fairness model's central promise, made checkable. "We have ageing" is an
// untested claim; "a best-effort request at 100 credits overtakes a commercial
// one at 250 after 9 hours, and a government one at 400 never" is a measurement,
// and it is what the starvation-versus-value tradeoff is actually made of.
func (f Fairness) TimeToOutrank(challenger, incumbent Snapshot, now time.Time) (time.Duration, bool) {
	incumbentValue := f.EffectiveValue(incumbent, now)
	base := float64(challenger.BidCredits) * f.TierMultipliers[challenger.PriorityTier]
	if base <= 0 {
		return 0, false
	}

	// The best the challenger can ever reach, at the ageing cap.
	if base*f.MaxAgeingFactor <= incumbentValue {
		return 0, false
	}

	needed := incumbentValue / base // the ageing factor required to draw level
	if needed <= 1 {
		return 0, true // already ahead
	}
	// factor = 1 + age/T  →  age = (factor − 1) · T
	age := time.Duration((needed - 1) * f.AgeingTimeConstant.Seconds() * float64(time.Second))
	waited := now.Sub(challenger.SubmittedAt)
	if waited >= age {
		return 0, true
	}
	return age - waited, true
}

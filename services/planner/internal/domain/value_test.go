package domain_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

func request(tier string, bid int64, submittedAt time.Time) domain.Snapshot {
	s := validSnapshot()
	s.PriorityTier = tier
	s.BidCredits = bid
	s.SubmittedAt = submittedAt
	return s
}

func TestDefaultFairnessIsValid(t *testing.T) {
	if err := domain.DefaultFairness().Validate(); err != nil {
		t.Fatalf("the shipped default does not satisfy its own rules: %v", err)
	}
}

func TestFairnessValidation(t *testing.T) {
	full := func() map[string]float64 {
		return map[string]float64{
			"GOVERNMENT": 4, "CIVIL_PROTECTION": 3, "COMMERCIAL": 1, "BEST_EFFORT": 0.5,
		}
	}

	tests := []struct {
		name     string
		fairness domain.Fairness
		want     string
	}{
		{"no multipliers at all", domain.Fairness{
			AgeingTimeConstant: time.Hour, MaxAgeingFactor: 2,
		}, "tier multipliers"},
		{"a tier the schema accepts has none", domain.Fairness{
			TierMultipliers:    map[string]float64{"GOVERNMENT": 4, "COMMERCIAL": 1, "BEST_EFFORT": 0.5},
			AgeingTimeConstant: time.Hour, MaxAgeingFactor: 2,
		}, "CIVIL_PROTECTION"},
		{"a zero multiplier", domain.Fairness{
			TierMultipliers: map[string]float64{
				"GOVERNMENT": 4, "CIVIL_PROTECTION": 0, "COMMERCIAL": 1, "BEST_EFFORT": 0.5,
			},
			AgeingTimeConstant: time.Hour, MaxAgeingFactor: 2,
		}, "can never win"},
		{"no ageing time constant", domain.Fairness{
			TierMultipliers: full(), MaxAgeingFactor: 2,
		}, "time constant"},
		{"ageing that reduces weight", domain.Fairness{
			TierMultipliers: full(), AgeingTimeConstant: time.Hour, MaxAgeingFactor: 0.5,
		}, "at least 1"},
		// THE BOUND. 4.0/0.5 is a spread of 8, so a cap of 8 lets a fully aged
		// best-effort request draw level with a fresh government one at the
		// same bid — the inversion the cap exists to prevent.
		{"ageing that can invert the tier ordering", domain.Fairness{
			TierMultipliers: full(), AgeingTimeConstant: time.Hour, MaxAgeingFactor: 8,
		}, "tier spread"},
		{"ageing that inverts it outright", domain.Fairness{
			TierMultipliers: full(), AgeingTimeConstant: time.Hour, MaxAgeingFactor: 20,
		}, "tier spread"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fairness.Validate()
			if err == nil {
				t.Fatalf("accepted %s", tt.name)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("does not wrap ErrInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not explain %q", err, tt.want)
			}
		})
	}
}

func TestTierMultipliersApply(t *testing.T) {
	f := domain.DefaultFairness()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	// Same bid, four tiers, no ageing.
	for tier, want := range map[string]float64{
		"GOVERNMENT": 400, "CIVIL_PROTECTION": 300, "COMMERCIAL": 100, "BEST_EFFORT": 50,
	} {
		got := f.EffectiveValue(request(tier, 100, now), now)
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s at 100 credits = %v, want %v", tier, got, want)
		}
	}
}

// The point of a multiplier rather than a strict ordering: a low bid from a
// high tier CAN beat a high bid from a low one, but money still means
// something.
func TestALowCivilProtectionBidBeatsAHigherCommercialOne(t *testing.T) {
	f := domain.DefaultFairness()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	civil := f.EffectiveValue(request("CIVIL_PROTECTION", 100, now), now)
	commercial := f.EffectiveValue(request("COMMERCIAL", 250, now), now)

	if civil <= commercial {
		t.Errorf("civil protection at 100 (%v) did not beat commercial at 250 (%v)", civil, commercial)
	}
	// And money still means something: a big enough commercial bid wins.
	rich := f.EffectiveValue(request("COMMERCIAL", 400, now), now)
	if rich <= civil {
		t.Errorf("commercial at 400 (%v) lost to civil protection at 100 (%v); the tier is dominating the bid entirely", rich, civil)
	}
}

func TestAgeingGrowsLinearlyAndSaturates(t *testing.T) {
	f := domain.DefaultFairness() // T = 6h, cap 3.0
	submitted := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		age  time.Duration
		want float64
	}{
		{0, 1.0},
		{3 * time.Hour, 1.5},
		{6 * time.Hour, 2.0},
		{12 * time.Hour, 3.0},
		{24 * time.Hour, 3.0},       // saturated
		{365 * 24 * time.Hour, 3.0}, // still saturated
	}

	for _, tt := range tests {
		got := f.AgeingFactor(submitted, submitted.Add(tt.age))
		if math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("after %s the factor is %v, want %v", tt.age, got, tt.want)
		}
	}
}

// Clock skew between services is real, and a negative age must not REDUCE
// weight below the floor — that would quietly penalise the newest requests.
func TestAFutureSubmissionDoesNotLoseWeight(t *testing.T) {
	f := domain.DefaultFairness()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	if got := f.AgeingFactor(now.Add(time.Hour), now); got != 1 {
		t.Errorf("a request submitted an hour in the future has factor %v, want 1", got)
	}
}

// THE ACCEPTANCE CRITERION for the whole fairness model. Without this,
// "we have ageing" is an untested claim.
func TestAPersistentlyLowBidRequestEventuallyWins(t *testing.T) {
	f := domain.DefaultFairness()
	start := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)

	// A best-effort request at 100 credits — effective value 50 at zero age.
	poor := request("BEST_EFFORT", 100, start)
	// A commercial rival at 120, resubmitted fresh every round so it never
	// ages. This is the starvation scenario: without ageing the poor request
	// loses forever.
	rivalBid := int64(120)

	if f.EffectiveValue(poor, start) >= f.EffectiveValue(request("COMMERCIAL", rivalBid, start), start) {
		t.Fatal("the low-bid request already wins at zero age; the scenario proves nothing")
	}

	var wonAt time.Duration
	won := false
	for age := time.Duration(0); age <= 48*time.Hour; age += 15 * time.Minute {
		now := start.Add(age)
		rival := request("COMMERCIAL", rivalBid, now) // always fresh
		if f.EffectiveValue(poor, now) > f.EffectiveValue(rival, now) {
			won, wonAt = true, age
			break
		}
	}

	if !won {
		t.Fatal("a persistently losing low-bid request never overtook a fresh rival; the ageing model does not fix starvation")
	}
	t.Logf("a BEST_EFFORT request at 100 credits overtakes a fresh COMMERCIAL rival at 120 after %s", wonAt)

	// And the promise is computable in closed form, not only by simulation.
	wait, reachable := f.TimeToOutrank(poor, request("COMMERCIAL", rivalBid, start), start)
	if !reachable {
		t.Fatal("TimeToOutrank says it never happens, but the simulation above found it does")
	}
	if wait > wonAt || wonAt-wait > 15*time.Minute {
		t.Errorf("TimeToOutrank said %s, the simulation found %s; the two disagree", wait, wonAt)
	}
}

// The other half of the tradeoff: ageing is BOUNDED, so an urgent
// civil-protection request is not eventually outranked by a stale trivial one.
func TestAgeingCannotInvertTheTierOrdering(t *testing.T) {
	f := domain.DefaultFairness()
	start := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)

	ancient := request("BEST_EFFORT", 100, start)
	// A year later, fully saturated.
	now := start.Add(365 * 24 * time.Hour)
	fresh := request("GOVERNMENT", 100, now)

	if f.EffectiveValue(ancient, now) >= f.EffectiveValue(fresh, now) {
		t.Errorf("a year-old best-effort request (%v) outranks a fresh government one at the same bid (%v)",
			f.EffectiveValue(ancient, now), f.EffectiveValue(fresh, now))
	}

	// Stated as the reachability answer too, which is what a policy would ask.
	if _, reachable := f.TimeToOutrank(ancient, fresh, now); reachable {
		t.Error("TimeToOutrank claims a bottom-tier request can eventually outrank a fresh top-tier one at the same bid")
	}
}

// A rival too far ahead is never overtaken, and saying so is more useful than
// returning a duration nobody should wait for.
func TestAnUnreachableRivalIsReportedUnreachable(t *testing.T) {
	f := domain.DefaultFairness()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	poor := request("BEST_EFFORT", 10, now)    // effective 5, cap 15
	rich := request("GOVERNMENT", 10_000, now) // effective 40 000

	if _, reachable := f.TimeToOutrank(poor, rich, now); reachable {
		t.Error("a request whose ceiling is far below the rival was reported as eventually winning")
	}
}

func TestAZeroBidNeverOutranksAnybody(t *testing.T) {
	f := domain.DefaultFairness()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	// The contract permits bid_credits of 0 — minimal.json uses it.
	free := request("GOVERNMENT", 0, now)
	if got := f.EffectiveValue(free, now); got != 0 {
		t.Errorf("a zero bid is worth %v", got)
	}
	if _, reachable := f.TimeToOutrank(free, request("BEST_EFFORT", 1, now), now); reachable {
		t.Error("a zero bid was reported as eventually outranking a paying request; anything times zero is zero")
	}
}

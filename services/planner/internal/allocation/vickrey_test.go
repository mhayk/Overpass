package allocation_test

import (
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/allocation"
	"github.com/mhayk/overpass/services/planner/internal/domain"
)

func TestVickreyNameIsTheContractEnumValue(t *testing.T) {
	if got := (allocation.VickreySealedBid{}).Name(); got != "VICKREY_SEALED_BID" {
		t.Errorf("Name = %q", got)
	}
}

// Winner determination is UNCHANGED from the baseline, deliberately. A sealed
// second-price auction does not change who wins — that is the whole point, and
// the property the truthfulness argument rests on. It changes what they pay.
func TestVickreyPicksTheSameWinnersAsTheBaseline(t *testing.T) {
	build := func() domain.Problem {
		return problem(
			candidate("o-a", "r-a", 900, 0, 5*time.Minute, 0),
			candidate("o-b", "r-b", 700, 0, 5*time.Minute, 0),
			candidate("o-c", "r-c", 500, 0, 5*time.Minute, 0),
		)
	}

	bid := allocation.GreedyByBid{}.Allocate(build())
	vickrey := allocation.VickreySealedBid{}.Allocate(build())

	if len(bid.Acquisitions) != len(vickrey.Acquisitions) {
		t.Fatalf("baseline scheduled %d, vickrey %d", len(bid.Acquisitions), len(vickrey.Acquisitions))
	}
	for i := range bid.Acquisitions {
		if bid.Acquisitions[i].OpportunityID != vickrey.Acquisitions[i].OpportunityID {
			t.Errorf("winner %d differs: %s against %s",
				i, bid.Acquisitions[i].OpportunityID, vickrey.Acquisitions[i].OpportunityID)
		}
	}
}

// The winner pays what it DISPLACED, not what it bid.
func TestTheWinnerPaysWhatItDisplaced(t *testing.T) {
	// One slot's worth of budget. The 900 wins; the 700 is what it kept out.
	p := problem(
		candidate("o-win", "r-win", 900, 0, 300*time.Second, 0),
		candidate("o-loser", "r-loser", 700, 0, 300*time.Second, 0),
	)
	p.Profile.DutyCycleBudgetS = 320 // room for exactly one

	plan := allocation.VickreySealedBid{}.Allocate(p)

	if len(plan.Acquisitions) != 1 {
		t.Fatalf("%d acquisitions, want 1", len(plan.Acquisitions))
	}
	winner := plan.Acquisitions[0]
	if winner.RequestID != "r-win" {
		t.Fatalf("%s won, want r-win", winner.RequestID)
	}
	if winner.ClearingPriceCredits == nil {
		t.Fatal("no clearing price; the auction computed nothing")
	}
	if *winner.ClearingPriceCredits != 700 {
		t.Errorf("clearing price = %d, want the displaced 700 — not its own 900",
			*winner.ClearingPriceCredits)
	}
	if *winner.ClearingPriceCredits >= winner.AwardedValueCredits {
		t.Error("the winner paid its own bid; this is first-price, not second-price")
	}
}

// A winner that displaced NOBODY pays nothing. Zero rather than null: "this
// winner displaced nothing" is a fact the auction established, where null would
// mean "not computed".
func TestAWinnerThatDisplacedNobodyPaysNothing(t *testing.T) {
	p := problem(
		candidate("o-a", "r-a", 900, 0, time.Minute, 0),
		candidate("o-b", "r-b", 700, 30*time.Minute, time.Minute, 0),
	)

	plan := allocation.VickreySealedBid{}.Allocate(p)

	if len(plan.Acquisitions) != 2 {
		t.Fatalf("%d acquisitions, want 2 — nothing should be contested here", len(plan.Acquisitions))
	}
	for _, a := range plan.Acquisitions {
		if a.ClearingPriceCredits == nil {
			t.Fatalf("%s has no clearing price", a.OpportunityID)
		}
		if *a.ClearingPriceCredits != 0 {
			t.Errorf("%s paid %d having displaced nobody", a.OpportunityID, *a.ClearingPriceCredits)
		}
	}
}

// THE MECHANISM-DESIGN TEST: overbidding does not improve the outcome.
//
// A bidder who inflates its valuation wins the same slot it would have won
// anyway and pays the same price, because the price is set by somebody else.
func TestOverbiddingDoesNotImproveTheOutcome(t *testing.T) {
	build := func(bid float64) domain.Problem {
		p := problem(
			candidate("o-win", "r-win", bid, 0, 300*time.Second, 0),
			candidate("o-loser", "r-loser", 700, 0, 300*time.Second, 0),
		)
		p.Profile.DutyCycleBudgetS = 320
		return p
	}

	truthful := allocation.VickreySealedBid{}.Allocate(build(900))
	inflated := allocation.VickreySealedBid{}.Allocate(build(5000))

	if len(truthful.Acquisitions) != 1 || len(inflated.Acquisitions) != 1 {
		t.Fatal("the scenario no longer contests a single slot")
	}
	if truthful.Acquisitions[0].RequestID != inflated.Acquisitions[0].RequestID {
		t.Error("overbidding changed who won")
	}
	truthfulPrice := *truthful.Acquisitions[0].ClearingPriceCredits
	inflatedPrice := *inflated.Acquisitions[0].ClearingPriceCredits
	if truthfulPrice != inflatedPrice {
		t.Errorf("bidding 5000 instead of 900 changed the price from %d to %d; the price is not set by the runner-up",
			truthfulPrice, inflatedPrice)
	}
}

// And the other half: truthful bidding is NOT DOMINATED. Shading a bid
// downwards can only lose a slot that was worth having, and never lowers the
// price of one it still wins.
func TestUnderbiddingCanOnlyLose(t *testing.T) {
	build := func(bid float64) domain.Problem {
		p := problem(
			candidate("o-mine", "r-mine", bid, 0, 300*time.Second, 0),
			candidate("o-rival", "r-rival", 700, 0, 300*time.Second, 0),
		)
		p.Profile.DutyCycleBudgetS = 320
		return p
	}

	truthful := allocation.VickreySealedBid{}.Allocate(build(900))
	shaded := allocation.VickreySealedBid{}.Allocate(build(650)) // below the rival

	if truthful.Acquisitions[0].RequestID != "r-mine" {
		t.Fatal("the truthful bid did not win; the scenario is wrong")
	}
	if shaded.Acquisitions[0].RequestID == "r-mine" {
		t.Fatal("shading below the rival still won; the scenario does not test what it claims")
	}
	// Shading turned a win at 700 — a surplus of 200 — into no slot at all.
	if won(shaded, "r-mine") {
		t.Error("shading kept the slot")
	}
}

// The price never exceeds the bid. Without that clamp a low-value winner that
// happened to block a high-value loser would be charged above its own
// valuation, which no second-price mechanism does — and which would make
// truthful bidding actively harmful.
func TestAWinnerNeverPaysMoreThanItBid(t *testing.T) {
	// The cheap candidate wins the earliest slot on ordering, and a much more
	// valuable request is blocked elsewhere in the round.
	p := problem(
		candidate("o-cheap", "r-cheap", 100, 0, 300*time.Second, 0),
		candidate("o-rich", "r-rich", 9000, 0, 300*time.Second, 0),
		candidate("o-third", "r-third", 50, 0, 300*time.Second, 0),
	)
	p.Profile.DutyCycleBudgetS = 620 // room for two of the three

	plan := allocation.VickreySealedBid{}.Allocate(p)

	for _, a := range plan.Acquisitions {
		if a.ClearingPriceCredits == nil {
			t.Fatalf("%s has no clearing price", a.OpportunityID)
		}
		if *a.ClearingPriceCredits > a.AwardedValueCredits {
			t.Errorf("%s bid %d and was charged %d",
				a.OpportunityID, a.AwardedValueCredits, *a.ClearingPriceCredits)
		}
		if *a.ClearingPriceCredits < 0 {
			t.Errorf("%s was charged a negative price", a.OpportunityID)
		}
	}
}

// The schema bounds clearing_price_credits to [0, 100000000], so a price the
// database would refuse is a plan that cannot commit.
func TestClearingPricesAreStorable(t *testing.T) {
	p := problem(
		candidate("o-a", "r-a", 900, 0, 300*time.Second, 0),
		candidate("o-b", "r-b", 700, 0, 300*time.Second, 0),
		candidate("o-c", "r-c", 500, 0, 300*time.Second, 0),
	)
	p.Profile.DutyCycleBudgetS = 620

	plan := allocation.VickreySealedBid{}.Allocate(p)

	for _, a := range plan.Acquisitions {
		if a.ClearingPriceCredits == nil {
			continue
		}
		if *a.ClearingPriceCredits < 0 || *a.ClearingPriceCredits > 100_000_000 {
			t.Errorf("%s priced at %d, outside the schema's [0, 100000000]",
				a.OpportunityID, *a.ClearingPriceCredits)
		}
	}
}

func TestVickreyPlansAreFeasibleAndConserving(t *testing.T) {
	p := problem(
		candidate("o1", "r1", 900, 0, 2*time.Minute, 0),
		candidate("o2", "r2", 800, 0, 2*time.Minute, 40),
		candidate("o3", "r3", 700, time.Minute, 2*time.Minute, -30),
		candidate("o4", "r4", 600, 2*time.Minute, 2*time.Minute, 10),
	)

	plan := allocation.VickreySealedBid{}.Allocate(p)

	assertPlanIsFeasible(t, p, plan, "VICKREY_SEALED_BID")
	if err := plan.Validate(requestIDs(p.Candidates)); err != nil {
		t.Errorf("conservation: %v", err)
	}
}

// It never beats the optimum either — the pricing pass must not change what was
// scheduled.
func TestVickreyNeverExceedsTheOptimum(t *testing.T) {
	p := problem(
		candidate("o-hog", "r-hog", 700, 0, 600*time.Second, 0),
		candidate("o-a", "r-a", 500, 20*time.Minute, 150*time.Second, 0),
		candidate("o-b", "r-b", 500, 40*time.Minute, 150*time.Second, 0),
	)

	optimal, report := allocation.NewExactDP().Solve(p)
	if !report.Optimal {
		t.Fatalf("not solved: %s", report.Reason)
	}
	plan := allocation.VickreySealedBid{}.Allocate(p)

	if plan.Value() > optimal.Value() {
		t.Errorf("vickrey scored %d against an optimum of %d", plan.Value(), optimal.Value())
	}
}

func TestVickreyHandlesAnEmptyRound(t *testing.T) {
	plan := allocation.VickreySealedBid{}.Allocate(problem())
	if len(plan.Acquisitions) != 0 || len(plan.Unfulfilled) != 0 {
		t.Errorf("an empty round produced %d acquisitions and %d unfulfilments",
			len(plan.Acquisitions), len(plan.Unfulfilled))
	}
}

// All four policies satisfy the interface with distinct contract names, which
// is what lets the benchmark run them over identical inputs.
func TestAllFourPoliciesAreDistinct(t *testing.T) {
	policies := []domain.AllocationPolicy{
		allocation.GreedyByBid{},
		allocation.GreedyByValueDensity{},
		allocation.VickreySealedBid{},
		allocation.NewExactDP(),
	}
	names := map[string]bool{}
	for _, policy := range policies {
		if names[policy.Name()] {
			t.Errorf("two policies both call themselves %s", policy.Name())
		}
		names[policy.Name()] = true
	}
	for _, want := range []string{"GREEDY_BY_BID", "GREEDY_BY_VALUE_DENSITY", "VICKREY_SEALED_BID", "EXACT_DP"} {
		if !names[want] {
			t.Errorf("no policy is named %s; the contract enum has a value nothing implements", want)
		}
	}
}

// An unusable profile short-circuits before pricing: nothing won, so there is
// nothing to charge, and every request is still told why.
func TestVickreyRefusesAnUnusableProfile(t *testing.T) {
	p := problem(
		candidate("o1", "r1", 900, 0, time.Minute, 0),
		candidate("o2", "r2", 700, 5*time.Minute, time.Minute, 0),
	)
	p.Profile.Agility.SlewRateDegS = 0 // cannot slew at all

	plan := allocation.VickreySealedBid{}.Allocate(p)

	if len(plan.Acquisitions) != 0 {
		t.Fatal("something was scheduled against an unusable profile")
	}
	if len(plan.Unfulfilled) != 2 {
		t.Errorf("%d unfulfilments, want one per request", len(plan.Unfulfilled))
	}
}

// A round where EVERY request wins has no losers, so the pricing pass has
// nothing to probe and every winner pays zero.
func TestARoundWithNoLosersPricesEverythingAtZero(t *testing.T) {
	p := problem(
		candidate("o-a", "r-a", 900, 0, time.Minute, 0),
		candidate("o-b", "r-b", 700, 30*time.Minute, time.Minute, 0),
		candidate("o-c", "r-c", 500, 60*time.Minute, time.Minute, 0),
	)

	plan := allocation.VickreySealedBid{}.Allocate(p)

	if len(plan.Unfulfilled) != 0 {
		t.Fatalf("%d requests lost; the scenario is meant to have none", len(plan.Unfulfilled))
	}
	for _, a := range plan.Acquisitions {
		if a.ClearingPriceCredits == nil || *a.ClearingPriceCredits != 0 {
			t.Errorf("%s paid %v with nobody to displace", a.OpportunityID, a.ClearingPriceCredits)
		}
	}
}

// A request with SEVERAL losing candidates contributes its best one to the
// pricing pass, not all of them — a customer's three options are one rival, not
// three.
func TestALosingRequestContributesItsBestCandidateOnly(t *testing.T) {
	p := problem(
		candidate("o-win", "r-win", 900, 0, 300*time.Second, 0),
		candidate("o-alt1", "r-loser", 300, 0, 300*time.Second, 0),
		candidate("o-alt2", "r-loser", 700, 0, 300*time.Second, 0),
		candidate("o-alt3", "r-loser", 500, 0, 300*time.Second, 0),
	)
	p.Profile.DutyCycleBudgetS = 320

	plan := allocation.VickreySealedBid{}.Allocate(p)

	if len(plan.Acquisitions) != 1 {
		t.Fatalf("%d acquisitions, want 1", len(plan.Acquisitions))
	}
	price := *plan.Acquisitions[0].ClearingPriceCredits
	if price != 700 {
		t.Errorf("clearing price = %d, want the loser's BEST option at 700", price)
	}
}

// Pricing must not disturb what was scheduled. A pass that reordered or dropped
// acquisitions would make the auction change its own allocation.
func TestPricingDoesNotDisturbTheAllocation(t *testing.T) {
	build := func() domain.Problem {
		p := problem(
			candidate("o1", "r1", 900, 0, 2*time.Minute, 0),
			candidate("o2", "r2", 800, 0, 2*time.Minute, 40),
			candidate("o3", "r3", 700, time.Minute, 2*time.Minute, -30),
			candidate("o4", "r4", 600, 2*time.Minute, 2*time.Minute, 10),
		)
		p.Profile.DutyCycleBudgetS = 400
		return p
	}

	baseline := allocation.GreedyByBid{}.Allocate(build())
	vickrey := allocation.VickreySealedBid{}.Allocate(build())

	if len(baseline.Acquisitions) != len(vickrey.Acquisitions) {
		t.Fatalf("pricing changed the plan size: %d against %d",
			len(baseline.Acquisitions), len(vickrey.Acquisitions))
	}
	for i := range baseline.Acquisitions {
		if baseline.Acquisitions[i].OpportunityID != vickrey.Acquisitions[i].OpportunityID {
			t.Errorf("pricing reordered position %d", i)
		}
		if !baseline.Acquisitions[i].Start.Equal(vickrey.Acquisitions[i].Start) {
			t.Errorf("pricing moved %s", baseline.Acquisitions[i].OpportunityID)
		}
	}
	if baseline.Value() != vickrey.Value() {
		t.Errorf("pricing changed plan value from %d to %d", baseline.Value(), vickrey.Value())
	}
}

// The pricing pass is bounded. Beyond the cap it considers the HIGHEST-valued
// losers, so a truncated price under-estimates the harm done rather than
// over-estimating it — erring downward is the defensible direction for a price.
func TestPricingIsBoundedAndErrsDownward(t *testing.T) {
	var candidates []domain.ScoredCandidate
	// One winner and 200 losing requests, well past the probe cap.
	candidates = append(candidates, candidate("o-win", "r-win", 100_000, 0, 300*time.Second, 0))
	for i := range 200 {
		candidates = append(candidates,
			candidate("o-l"+itoa(i), "r-l"+itoa(i), float64(100+i), 0, 300*time.Second, 0))
	}
	p := problem(candidates...)
	p.Profile.DutyCycleBudgetS = 320 // room for exactly one

	plan := allocation.VickreySealedBid{}.Allocate(p)

	if len(plan.Acquisitions) != 1 {
		t.Fatalf("%d acquisitions, want 1", len(plan.Acquisitions))
	}
	price := *plan.Acquisitions[0].ClearingPriceCredits
	// The best loser is worth 299. A cap that dropped the high-valued losers
	// would price this at something much lower.
	if price != 299 {
		t.Errorf("clearing price = %d, want 299 — the cap is discarding the best losers", price)
	}
	if err := plan.Validate(requestIDs(candidates)); err != nil {
		t.Errorf("conservation over 201 requests: %v", err)
	}
}

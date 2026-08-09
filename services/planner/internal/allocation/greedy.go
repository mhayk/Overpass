// Package allocation holds the AllocationPolicy implementations.
//
// Its own package, beside domain rather than inside it, because a policy is a
// STRATEGY over the domain rather than part of it — and because
// scripts/coverage-gate.sh has held services/planner/internal/allocation to 95%
// since M0, which is the repository saying the same thing.
//
// Every policy here is a pure function of its Problem. No context, no error, no
// database. That is what makes M2-12's property tests possible and what lets
// M2-13 replay identical inputs through four policies.
package allocation

import (
	"fmt"
	"sort"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

// GreedyByBid is the naive baseline: sort by effective value, take what fits.
//
// DELIBERATELY NAIVE. Its purpose is to be beaten, visibly and with numbers.
// Improving it — by looking ahead, by preferring cheap slews, by any of the
// obvious refinements — would flatter the heuristics that follow and destroy
// the comparison the benchmark exists to make. If this policy ever gets
// cleverer, M2-13 stops measuring anything.
//
// It is also the policy whose failures are easiest to explain on a whiteboard,
// which is why it is the configured default until ADR-0007 names one on
// evidence.
type GreedyByBid struct{}

// Name is the contract enum value recorded on every round and plan.
func (GreedyByBid) Name() string { return "GREEDY_BY_BID" }

// Allocate sorts by effective value and takes what fits.
func (p GreedyByBid) Allocate(problem domain.Problem) domain.Plan {
	return allocateGreedily(problem, byEffectiveValue)
}

// byEffectiveValue is the naive ordering: highest value first, and nothing else.
//
// It ignores what an acquisition COSTS — duration, slew, duty cycle — which is
// precisely the weakness GreedyByValueDensity exists to fix and the benchmark
// exists to quantify.
func byEffectiveValue(a, b domain.ScoredCandidate) bool {
	if a.EffectiveValue != b.EffectiveValue {
		return a.EffectiveValue > b.EffectiveValue
	}
	// DETERMINISTIC tie-breaking. A random or map-order tie-break makes every
	// benchmark comparison noisy, so two policies could differ by nothing but
	// the order Go happened to iterate in. Opportunity id is stable, unique and
	// carries no meaning — which is exactly what a tie-break should be.
	return a.OpportunityID < b.OpportunityID
}

// allocateGreedily is the shared skeleton: order the candidates, then take each
// in turn if the schedule accepts it.
//
// Shared because the ONLY difference between the two greedy policies is the
// ordering function. Writing the loop twice would let them drift apart in ways
// that have nothing to do with the comparison being made — which is the same
// argument the feasibility engine rests on.
func allocateGreedily(problem domain.Problem, less func(a, b domain.ScoredCandidate) bool) domain.Plan {
	schedule, err := domain.NewSchedule(problem.Profile)
	if err != nil {
		// A profile the planner cannot schedule against is a configuration
		// failure, and every request in the round is unfulfillable for the same
		// reason. Saying so once per request is better than an empty plan with
		// no explanation.
		return unfulfilAll(problem, domain.ReasonNoOpportunity,
			fmt.Sprintf("the satellite's profile cannot be scheduled against: %v", err))
	}

	ordered := make([]domain.ScoredCandidate, len(problem.Candidates))
	copy(ordered, problem.Candidates)
	sort.SliceStable(ordered, func(i, j int) bool { return less(ordered[i], ordered[j]) })

	// A request usually has several candidates. It gets at most one
	// acquisition, and exactly one outcome — so the refusal remembered is the
	// one from its BEST candidate, which is the answer to "why did I lose?"
	// rather than "why did the last option I did not want also fail?". The
	// best candidate is also what the frontend ghosts on the timeline.
	firstRefusal := map[string]domain.Refusal{}
	bestCandidate := map[string]domain.ScoredCandidate{}
	customerOf := map[string]string{}

	for _, candidate := range ordered {
		customerOf[candidate.RequestID] = candidate.CustomerID
		if current, seen := bestCandidate[candidate.RequestID]; !seen || candidate.EffectiveValue > current.EffectiveValue {
			bestCandidate[candidate.RequestID] = candidate
		}

		if schedule.HasRequest(candidate.RequestID) {
			continue // already won a slot; do not image the same target twice
		}

		placement, refusal, ok := schedule.TryPlace(candidate)
		if !ok {
			if _, seen := firstRefusal[candidate.RequestID]; !seen {
				firstRefusal[candidate.RequestID] = refusal
			}
			continue
		}
		if err := schedule.Commit(placement); err != nil {
			// TryPlace approved it and Commit refused, which means the two
			// disagree. Recorded rather than swallowed: it is a bug in the
			// engine, and a silent skip would show up only as a plan that is
			// mysteriously worse than it should be.
			if _, seen := firstRefusal[candidate.RequestID]; !seen {
				firstRefusal[candidate.RequestID] = domain.Refusal{
					ReasonCode:  domain.ReasonLostToHigherValue,
					Explanation: fmt.Sprintf("placement was approved and then refused: %v", err),
				}
			}
			continue
		}
		delete(firstRefusal, candidate.RequestID)
	}

	plan := domain.Plan{Acquisitions: schedule.Acquisitions()}
	for requestID, refusal := range firstRefusal {
		best := bestCandidate[requestID]
		plan.Unfulfilled = append(plan.Unfulfilled, domain.Unfulfilment{
			RequestID:   requestID,
			CustomerID:  customerOf[requestID],
			ReasonCode:  refusal.ReasonCode,
			Explanation: refusal.Explanation,
			Detail:      refusal.Detail,
			// The loser's own value, so the event can show the gap and not just
			// its existence.
			OwnValueCredits:           int64(best.EffectiveValue),
			BestRejectedOpportunityID: best.OpportunityID,
		})
	}
	// Sorted, because map iteration order would make the same input produce a
	// differently-ordered plan on every run — and M2-13 compares plans.
	sort.Slice(plan.Unfulfilled, func(i, j int) bool {
		return plan.Unfulfilled[i].RequestID < plan.Unfulfilled[j].RequestID
	})
	return plan
}

func unfulfilAll(problem domain.Problem, reasonCode, explanation string) domain.Plan {
	seen := map[string]bool{}
	var plan domain.Plan
	for _, c := range problem.Candidates {
		if seen[c.RequestID] {
			continue
		}
		seen[c.RequestID] = true
		plan.Unfulfilled = append(plan.Unfulfilled, domain.Unfulfilment{
			RequestID:   c.RequestID,
			CustomerID:  c.CustomerID,
			ReasonCode:  reasonCode,
			Explanation: explanation,
		})
	}
	sort.Slice(plan.Unfulfilled, func(i, j int) bool {
		return plan.Unfulfilled[i].RequestID < plan.Unfulfilled[j].RequestID
	})
	return plan
}

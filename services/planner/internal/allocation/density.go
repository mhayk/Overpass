package allocation

import (
	"math"
	"sort"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

// GreedyByValueDensity sorts by value per unit of resource consumed.
//
// The resource is not just time. An acquisition costs the seconds it occupies,
// the manoeuvre that gets the spacecraft pointing at it, and the slice of the
// orbit's power budget it burns — and a policy that counts only the first is
// GreedyByBid with extra steps.
//
//	density = effective value / (duration + expected slew + duty-cycle cost)
//
// SLEW BELONGS IN THE DENOMINATOR, and that is the entire point. Without it a
// high-value acquisition that forces a huge attitude manoeuvre looks free, and
// the plan pays for it twice: once in the seconds spent slewing, and again in
// the acquisitions that no longer fit around it.
type GreedyByValueDensity struct{}

// Name is the contract enum value recorded on every round and plan.
func (GreedyByValueDensity) Name() string { return "GREEDY_BY_VALUE_DENSITY" }

// Allocate sorts by density and takes what fits.
func (GreedyByValueDensity) Allocate(problem domain.Problem) domain.Plan {
	density := densities(problem)
	return allocateGreedily(problem, func(a, b domain.ScoredCandidate) bool {
		da, db := density[a.OpportunityID], density[b.OpportunityID]
		if da != db {
			return da > db
		}
		// Same deterministic tie-break as the baseline, so the two policies
		// differ by their ordering rule and by nothing else.
		return a.OpportunityID < b.OpportunityID
	})
}

// slewSampleSize bounds the neighbour estimate.
//
// The expected slew cost is a mean over other candidates, and computing it
// against ALL of them is quadratic. A round is contract-capped at 5 000
// opportunities, so the exact version is 25 million slew evaluations inside the
// advisory lock — measurable against the p95 < 800 ms budget for something that
// is an estimate either way.
//
// Sampling is deterministic (evenly spaced over the roll-sorted candidates), so
// the benchmark still compares like with like. Spread over the sorted order
// rather than taken from the front, because the front is all one attitude and
// would make every candidate look cheap.
const slewSampleSize = 32

// densities scores every candidate once.
//
// Precomputed rather than evaluated inside the sort comparator. A comparator
// that recomputed would call this O(n log n) times instead of n, and — worse —
// sort.Slice may compare the same pair more than once, so an expensive
// comparator makes the cost unpredictable rather than merely high.
func densities(problem domain.Problem) map[string]float64 {
	out := make(map[string]float64, len(problem.Candidates))
	sample := attitudeSample(problem.Candidates)

	for _, c := range problem.Candidates {
		expectedSlew := meanSlewSeconds(problem.Profile.Agility, c.Attitude, sample)

		// Every term is seconds of a resource: seconds of sensor time, seconds
		// of manoeuvre, seconds of the orbit's power budget. Adding them is a
		// simplification — they are not interchangeable, and the duty-cycle
		// second is the scarcest of the three — but weighting them would be
		// three more constants nobody has measured, and M2-13 is where the
		// simplification shows up as lost plan value.
		cost := c.AcquisitionDurationS + expectedSlew + c.DutyCycleCostS
		if cost <= 0 {
			// Unreachable: the domain refuses non-positive durations and costs,
			// and the settling floor makes expected slew positive. Guarded
			// because the alternative is +Inf, which sorts first and would put
			// a malformed candidate at the head of every plan.
			out[c.OpportunityID] = 0
			continue
		}
		out[c.OpportunityID] = c.EffectiveValue / cost
	}
	return out
}

// attitudeSample picks a deterministic, spread-out subset of the attitudes in
// the round.
func attitudeSample(candidates []domain.ScoredCandidate) []domain.Attitude {
	if len(candidates) == 0 {
		return nil
	}

	attitudes := make([]domain.Attitude, len(candidates))
	for i, c := range candidates {
		attitudes[i] = c.Attitude
	}
	sort.Slice(attitudes, func(i, j int) bool {
		if attitudes[i].RollDeg != attitudes[j].RollDeg {
			return attitudes[i].RollDeg < attitudes[j].RollDeg
		}
		return attitudes[i].Mode < attitudes[j].Mode
	})

	if len(attitudes) <= slewSampleSize {
		return attitudes
	}
	out := make([]domain.Attitude, 0, slewSampleSize)
	step := float64(len(attitudes)-1) / float64(slewSampleSize-1)
	for i := range slewSampleSize {
		out = append(out, attitudes[int(math.Round(float64(i)*step))])
	}
	return out
}

// meanSlewSeconds is what it costs, on average, to point here from somewhere
// else in this round.
//
// A candidate far from the crowd is expensive even if its own acquisition is
// short, and that is exactly the cost GreedyByBid cannot see.
func meanSlewSeconds(agility domain.Agility, at domain.Attitude, sample []domain.Attitude) float64 {
	if len(sample) == 0 {
		// A single-candidate round has no neighbours to slew from. The settling
		// floor is still paid, and using it keeps the denominator honest rather
		// than making the lone candidate look infinitely dense.
		return agility.SettlingFloor().Seconds()
	}
	var total float64
	for _, other := range sample {
		total += agility.SlewTime(other, at).Seconds()
	}
	return total / float64(len(sample))
}

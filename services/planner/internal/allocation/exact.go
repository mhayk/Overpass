package allocation

import (
	"fmt"
	"sort"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

// ExactDP is the ground truth. It exists to BOUND the heuristics, not to run in
// production.
//
// The problem is weighted job scheduling with sequence-dependent setup times on
// parallel machines — NP-hard, and ADR-0007 argues the strong case through the
// slew term. So this is exponential and says so, rather than being a heuristic
// wearing an exact name.
//
// WHY IT MATTERS MORE THAN ITS RUNTIME SUGGESTS: without it,
// GreedyByValueDensity has no optimality ratio, and "43% better than the
// baseline" is a comparison between two heuristics rather than a distance from
// optimal. It also carries the strongest correctness test in the planner — no
// heuristic may ever exceed the exact plan value on an instance both can solve —
// and a heuristic that wins that comparison is not clever, it is violating a
// constraint.
//
// THE SEARCH ENUMERATES SEQUENCES, NOT SUBSETS, and that distinction is the
// whole reason this file was rewritten before it was ever tested. Any feasible
// plan is its acquisitions in time order, so choosing "which acquisition comes
// next" enumerates every feasible plan exactly once. Deciding include/exclude
// over a fixed candidate order and checking feasibility afterwards does NOT:
// the check would have to fix an ordering, and a subset feasible in some other
// order would be discarded. The exact solver would then under-report, a
// heuristic could beat it without violating anything, and the
// no-heuristic-beats-optimal test would fire on a solver bug while looking like
// a policy bug.
//
// COMPLEXITY, statable from memory: O(n!) sequences in the worst case, bounded
// by an admissible optimistic-suffix bound and by feasibility pruning. That is
// why MaxCandidates exists and why it is small.
//
// The contract enum value is EXACT_DP and stays — the enum is frozen. The
// algorithm is branch-and-bound rather than a dynamic program, and calling it
// what it is here beats bending it to fit a label: start times are continuous
// within an access window, so a DP would have to quantise time and would stop
// being exact at whatever resolution it chose.

// ExactDP solves small instances optimally.
type ExactDP struct {
	// MaxCandidates is the hard instance-size limit. Above it the policy
	// REFUSES rather than degrading.
	//
	// A silently degrading exact solver would be worse than no exact solver at
	// all: it would corrupt the reference the whole benchmark divides by, and
	// the corruption would look like a heuristic performing suspiciously well.
	MaxCandidates int

	// Timeout bounds the search. On expiry the best plan found so far is
	// returned WITH a report saying it is not proven optimal — a different
	// claim from "this is the optimum", and one the benchmark must not average
	// together with the other.
	Timeout time.Duration

	// Now is injected so the timeout is testable without sleeping.
	Now func() time.Time
}

// NewExactDP builds a solver with defensible defaults.
//
// Fourteen candidates because the search is over sequences and 14! is where
// pruning stops rescuing it. The benchmark generates small instances
// deliberately, and this limit is what keeps "optimality ratio" honest about
// which instances it actually covers.
func NewExactDP() ExactDP {
	return ExactDP{MaxCandidates: 14, Timeout: 5 * time.Second}
}

// Name is the contract enum value recorded on every round and plan.
func (ExactDP) Name() string { return "EXACT_DP" }

// Report says what the search actually established.
//
// Returned separately from the Plan because AllocationPolicy deliberately has
// no error: a policy returns what it decided. But "this is the optimum" and
// "this is the best I found before the clock ran out" are different claims.
type Report struct {
	// Optimal is true only when the search completed. False means the timeout
	// or the size limit stopped it.
	Optimal bool
	// Bound is an upper bound on achievable plan value. Equal to the plan's
	// value when Optimal.
	Bound float64
	// Explored counts search nodes, so a benchmark can report how hard an
	// instance was rather than only how long it took.
	Explored int
	// Reason is empty when Optimal.
	Reason string
}

// Allocate satisfies AllocationPolicy, discarding the report.
//
// Any caller that needs to know whether the answer is proven optimal — which is
// every caller that matters — uses Solve.
func (e ExactDP) Allocate(problem domain.Problem) domain.Plan {
	plan, _ := e.Solve(problem)
	return plan
}

// Solve returns the optimal plan and what the search established about it.
func (e ExactDP) Solve(problem domain.Problem) (domain.Plan, Report) {
	if e.MaxCandidates <= 0 {
		e = NewExactDP()
	}
	now := e.Now
	if now == nil {
		now = time.Now
	}

	if len(problem.Candidates) > e.MaxCandidates {
		// LOUD, not graceful. See MaxCandidates.
		reason := fmt.Sprintf(
			"instance has %d candidates, above the exact solver's limit of %d; it is a reference, not a production policy",
			len(problem.Candidates), e.MaxCandidates)
		return unfulfilAll(problem, domain.ReasonNoOpportunity, reason), Report{Reason: reason}
	}
	if _, err := domain.NewSchedule(problem.Profile); err != nil {
		reason := fmt.Sprintf("the satellite's profile cannot be scheduled against: %v", err)
		return unfulfilAll(problem, domain.ReasonNoOpportunity, reason), Report{Reason: reason}
	}

	// Value-sorted so the first dive finds a good incumbent quickly, which is
	// what makes the bound prune anything at all. Depth-first with a bad
	// incumbent explores almost everything.
	ordered := make([]domain.ScoredCandidate, len(problem.Candidates))
	copy(ordered, problem.Candidates)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].EffectiveValue != ordered[j].EffectiveValue {
			return ordered[i].EffectiveValue > ordered[j].EffectiveValue
		}
		return ordered[i].OpportunityID < ordered[j].OpportunityID
	})

	search := &exactSearch{
		problem:  problem,
		ordered:  ordered,
		deadline: now().Add(e.Timeout),
		now:      now,
		used:     make([]bool, len(ordered)),
	}
	search.dive(nil, 0)

	plan := search.bestPlan(problem)
	report := Report{
		Optimal:  !search.timedOut,
		Bound:    search.bestValue,
		Explored: search.explored,
	}
	if search.timedOut {
		report.Reason = fmt.Sprintf(
			"search stopped after %s having explored %d nodes; the plan is the best found, not a proven optimum",
			e.Timeout, search.explored)
	}
	return plan, report
}

type exactSearch struct {
	problem domain.Problem
	ordered []domain.ScoredCandidate

	deadline time.Time
	now      func() time.Time
	timedOut bool
	explored int

	used      []bool
	bestValue float64
	bestSet   []domain.ScoredCandidate
}

// dive chooses which acquisition comes NEXT, extending the sequence built so
// far.
//
// Feasibility is decided by replaying through the SAME domain.Schedule every
// policy uses, rather than by a private test. That is the point: if the exact
// solver had its own notion of what fits, it would be an optimum for a
// different problem, and the ratio the benchmark reports would be meaningless.
func (s *exactSearch) dive(chosen []domain.ScoredCandidate, value float64) {
	s.explored++

	// Checked every 512 nodes rather than every one: time.Now is a syscall on
	// some platforms and the search should be dominated by branching.
	if s.explored%512 == 0 && s.now().After(s.deadline) {
		s.timedOut = true
	}
	if s.timedOut {
		return
	}

	if value > s.bestValue {
		s.bestValue = value
		s.bestSet = append(s.bestSet[:0], chosen...)
	}

	// The bound. If every remaining candidate were free and compatible, could
	// this branch still beat the incumbent? If not, nothing below it can.
	//
	// Counting each REQUEST at most once is what makes it tight without making
	// it inadmissible: at most one acquisition per request is a hard
	// constraint, not an observation. A bound that under-counts would prune the
	// optimum.
	if value+s.optimisticRemaining() <= s.bestValue {
		return
	}

	for i, candidate := range s.ordered {
		if s.used[i] || s.holds(chosen, candidate.RequestID) {
			continue
		}
		// Copied, not appended in place. append may reuse the backing array,
		// so two sibling branches would share and overwrite each other's
		// sequence — producing a plan assembled from candidates that were never
		// explored together.
		next := make([]domain.ScoredCandidate, len(chosen), len(chosen)+1)
		copy(next, chosen)
		next = append(next, candidate)
		if !s.feasible(next) {
			continue
		}
		s.used[i] = true
		s.dive(next, value+candidate.EffectiveValue)
		s.used[i] = false
	}
}

func (s *exactSearch) optimisticRemaining() float64 {
	best := map[string]float64{}
	for i, c := range s.ordered {
		if s.used[i] {
			continue
		}
		if c.EffectiveValue > best[c.RequestID] {
			best[c.RequestID] = c.EffectiveValue
		}
	}
	total := 0.0
	for _, v := range best {
		total += v
	}
	return total
}

func (s *exactSearch) holds(chosen []domain.ScoredCandidate, requestID string) bool {
	for _, c := range chosen {
		if c.RequestID == requestID {
			return true
		}
	}
	return false
}

// feasible replays a sequence through the shared engine, in the order given.
//
// TryPlace returns the earliest feasible start, so committing in sequence order
// is exactly left-packing — and for a FIXED order, the left-packed schedule
// exists whenever any schedule for that order does. Enumerating orders is what
// the caller does.
func (s *exactSearch) feasible(sequence []domain.ScoredCandidate) bool {
	schedule, err := domain.NewSchedule(s.problem.Profile)
	if err != nil {
		return false
	}
	for _, c := range sequence {
		placement, _, ok := schedule.TryPlace(c)
		if !ok {
			return false
		}
		if err := schedule.Commit(placement); err != nil {
			return false
		}
	}
	return true
}

// bestPlan replays the winning sequence to produce the acquisitions, and
// reports everyone else.
func (s *exactSearch) bestPlan(problem domain.Problem) domain.Plan {
	schedule, err := domain.NewSchedule(problem.Profile)
	if err != nil {
		return unfulfilAll(problem, domain.ReasonNoOpportunity, err.Error())
	}

	won := map[string]bool{}
	for _, c := range s.bestSet {
		placement, _, ok := schedule.TryPlace(c)
		if !ok {
			continue
		}
		if err := schedule.Commit(placement); err != nil {
			continue
		}
		won[c.RequestID] = true
	}

	plan := domain.Plan{Acquisitions: schedule.Acquisitions()}

	// Everyone who competed and did not win lost to a better OVERALL plan.
	// LOST_TO_HIGHER_VALUE is the truthful code: the exact solver did not
	// refuse them for a constraint, it found a combination worth more.
	seen := map[string]bool{}
	for _, c := range problem.Candidates {
		if won[c.RequestID] || seen[c.RequestID] {
			continue
		}
		seen[c.RequestID] = true
		plan.Unfulfilled = append(plan.Unfulfilled, domain.Unfulfilment{
			RequestID:   c.RequestID,
			CustomerID:  c.CustomerID,
			ReasonCode:  domain.ReasonLostToHigherValue,
			Explanation: "excluded from the optimal plan; including it would have lowered total plan value",
		})
	}
	sort.Slice(plan.Unfulfilled, func(i, j int) bool {
		return plan.Unfulfilled[i].RequestID < plan.Unfulfilled[j].RequestID
	})
	return plan
}

package allocation_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"

	"github.com/mhayk/overpass/services/planner/internal/allocation"
	"github.com/mhayk/overpass/services/planner/internal/domain"
)

// M2-12: the invariants, over generated inputs, for ALL FOUR policies from one
// suite — so a fifth policy inherits the entire correctness bar for free.
//
// gopter rather than hand-rolled rand, for one reason the hand-rolled loops in
// this package cannot provide: SHRINKING. A failure over forty random
// candidates is a debugging session; gopter removes candidates and shrinks
// fields until the counterexample is minimal, which is the difference between
// "something is wrong" and "THIS is wrong". The dependency is test-only,
// pure Go, and the SPEC names it for exactly this job.

// candidateSpec is the shrinkable description of one candidate. Everything is
// small integers, because gopter shrinks integers towards zero and slices by
// removing elements — so a minimal counterexample is few candidates with small
// values, which is what a human wants to read.
type candidateSpec struct {
	Value        int // credits
	OpenMin      int // window opens, minutes after bucket start
	SlackMin     int // window width, minutes
	DurationSec  int // acquisition length
	RollDeg      int // signed attitude
	DutyFactor   int // duty cost = duration * (1 + factor/4): may exceed duration
	DeadlineSlop int // minutes past earliest finish; 0 = knife-edge deadline
}

type profileSpec struct {
	BudgetS   int
	SlewRate4 int // quarter-degrees per second, so shrinking stays integral
	SettleS   int
	MaxRoll   int
}

func candidateSpecType() reflect.Type { return reflect.TypeOf(candidateSpec{}) }
func profileSpecType() reflect.Type   { return reflect.TypeOf(profileSpec{}) }

func genCandidate() gopter.Gen {
	return gen.Struct(candidateSpecType(), map[string]gopter.Gen{
		"Value":        gen.IntRange(1, 900),
		"OpenMin":      gen.IntRange(0, 120),
		"SlackMin":     gen.IntRange(0, 30),
		"DurationSec":  gen.IntRange(10, 300),
		"RollDeg":      gen.IntRange(-60, 60), // beyond authority on purpose: refusals must be exercised
		"DutyFactor":   gen.IntRange(0, 8),
		"DeadlineSlop": gen.IntRange(0, 240),
	})
}

func genProfile() gopter.Gen {
	return gen.Struct(profileSpecType(), map[string]gopter.Gen{
		"BudgetS":   gen.IntRange(60, 900), // tight enough that the knapsack binds
		"SlewRate4": gen.IntRange(2, 12),   // 0.5..3 deg/s
		"SettleS":   gen.IntRange(0, 15),
		"MaxRoll":   gen.IntRange(20, 60),
	})
}

func buildProblem(specs []candidateSpec, ps profileSpec) domain.Problem {
	p := domain.Problem{
		Key:       domain.RoundKey{SatelliteID: "CAPELLA-14", BucketStart: bucketStart},
		BucketEnd: bucketStart.Add(3 * time.Hour),
		Profile: domain.SatelliteProfile{
			Agility: domain.Agility{
				SlewRateDegS: float64(ps.SlewRate4) / 4,
				SettleTimeS:  float64(ps.SettleS),
				MaxRollDeg:   float64(ps.MaxRoll),
			},
			DutyCycleBudgetS: float64(ps.BudgetS),
		},
		Now: bucketStart,
	}
	for i, s := range specs {
		c := candidate(
			fmt.Sprintf("o%03d", i), fmt.Sprintf("r%03d", i),
			float64(s.Value),
			time.Duration(s.OpenMin)*time.Minute,
			time.Duration(s.DurationSec)*time.Second,
			float64(s.RollDeg),
		)
		c.AccessEnd = c.AccessStart.Add(time.Duration(s.SlackMin) * time.Minute)
		c.DutyCycleCostS = float64(s.DurationSec) * (1 + float64(s.DutyFactor)/4)
		// Deadline pressure: sometimes exactly the earliest finish, sometimes
		// generous. The knife-edge case is where an off-by-one lives.
		c.Deadline = c.AccessStart.
			Add(time.Duration(s.DurationSec) * time.Second).
			Add(time.Duration(s.DeadlineSlop) * time.Minute)
		p.Candidates = append(p.Candidates, c)
	}
	return p
}

// violations checks every invariant at once and names the first broken one.
// One checker for all six, so every policy faces the identical bar.
func violations(p domain.Problem, plan domain.Plan) string {
	byOpportunity := map[string]domain.ScoredCandidate{}
	requestIDs := map[string]bool{}
	for _, c := range p.Candidates {
		byOpportunity[c.OpportunityID] = c
		requestIDs[c.RequestID] = true
	}

	spent := map[int]float64{}
	holders := map[string]bool{}
	for i, a := range plan.Acquisitions {
		// At most one acquisition per request.
		if holders[a.RequestID] {
			return fmt.Sprintf("request %s holds two acquisitions", a.RequestID)
		}
		holders[a.RequestID] = true

		c, known := byOpportunity[a.OpportunityID]
		if !known {
			return fmt.Sprintf("acquisition %s was never a candidate", a.OpportunityID)
		}
		// Starts inside its window; finishes by its deadline.
		if a.Start.Before(c.AccessStart) || a.Start.After(c.AccessEnd) {
			return fmt.Sprintf("%s starts outside its access window", a.OpportunityID)
		}
		if a.End.After(c.Deadline) {
			return fmt.Sprintf("%s finishes after its deadline", a.OpportunityID)
		}
		spent[a.OrbitNumber] += a.DutyCycleCostS

		if i == 0 {
			continue
		}
		previous := plan.Acquisitions[i-1]
		// No overlap on one satellite.
		if a.Start.Before(previous.End) {
			return fmt.Sprintf("%s overlaps %s", a.OpportunityID, previous.OpportunityID)
		}
		// Every consecutive pair separated by at least slew_time(a, b).
		required := p.Profile.Agility.SlewTime(previous.Attitude, a.Attitude)
		if gap := a.Start.Sub(previous.End); gap < required {
			return fmt.Sprintf("%s follows %s after %s, needs %s of slew",
				a.OpportunityID, previous.OpportunityID, gap, required)
		}
	}
	// Per-orbit duty cycle never exceeded.
	for orbit, used := range spent {
		if used > p.Profile.DutyCycleBudgetS+1e-9 {
			return fmt.Sprintf("orbit %d spent %.2fs of a %.2fs budget", orbit, used, p.Profile.DutyCycleBudgetS)
		}
	}
	// Conservation: every candidate request appears exactly once. The invariant
	// that protects customers directly — a request that silently vanishes is
	// the worst failure mode this system has.
	var competed []string
	for id := range requestIDs {
		competed = append(competed, id)
	}
	if err := plan.Validate(competed); err != nil {
		return err.Error()
	}
	return ""
}

func TestInvariantsHoldForEveryPolicy(t *testing.T) {
	policies := []domain.AllocationPolicy{
		allocation.GreedyByBid{},
		allocation.GreedyByValueDensity{},
		allocation.VickreySealedBid{},
		allocation.NewExactDP(),
	}

	for _, policy := range policies {
		t.Run(policy.Name(), func(t *testing.T) {
			parameters := gopter.DefaultTestParameters()
			parameters.MinSuccessfulTests = 150
			parameters.Rng.Seed(1738) // reproducible: the benchmark's own rule
			properties := gopter.NewProperties(parameters)

			maxCandidates := 25
			if policy.Name() == "EXACT_DP" {
				// The exact search is factorial; its own limit is 14 and the
				// property suite stays inside it so every generated instance is
				// SOLVED, not refused.
				maxCandidates = 8
			}

			properties.Property("no plan violates any invariant", prop.ForAll(
				func(specs []candidateSpec, ps profileSpec) string {
					// Truncated INSIDE the property rather than filtered with
					// SuchThat: a filter discards most generated slices for the
					// exact solver's small cap and gopter gives up, while
					// truncation keeps every run AND keeps element-removal
					// shrinking intact.
					if len(specs) > maxCandidates {
						specs = specs[:maxCandidates]
					}
					p := buildProblem(specs, ps)
					plan := policy.Allocate(p)
					return violations(p, plan)
				},
				gen.SliceOf(genCandidate()),
				genProfile(),
			))

			properties.TestingRun(t)
		})
	}
}

// The checker itself must bite, or every green run above proves nothing. A
// hand-built plan violating each invariant in turn must be caught — the same
// discipline as every mutated gate in this repository.
func TestTheInvariantCheckerCatchesEachViolation(t *testing.T) {
	p := problem(
		candidate("o1", "r1", 100, 0, time.Minute, 0),
		candidate("o2", "r2", 100, 10*time.Minute, time.Minute, 30),
	)
	legal := allocation.GreedyByBid{}.Allocate(p)
	if violations(p, legal) != "" {
		t.Fatalf("a legal plan was reported violating: %s", violations(p, legal))
	}

	breakPlan := func(mutate func(*domain.Plan)) string {
		plan := allocation.GreedyByBid{}.Allocate(p)
		mutate(&plan)
		return violations(p, plan)
	}

	cases := []struct {
		name   string
		mutate func(*domain.Plan)
	}{
		{"overlap", func(plan *domain.Plan) {
			plan.Acquisitions[1].Start = plan.Acquisitions[0].Start
			plan.Acquisitions[1].End = plan.Acquisitions[0].End
		}},
		{"slew violated", func(plan *domain.Plan) {
			plan.Acquisitions[1].Start = plan.Acquisitions[0].End.Add(time.Second)
			plan.Acquisitions[1].End = plan.Acquisitions[1].Start.Add(time.Minute)
		}},
		{"duty cycle exceeded", func(plan *domain.Plan) {
			plan.Acquisitions[0].DutyCycleCostS = 10_000
		}},
		{"deadline missed", func(plan *domain.Plan) {
			plan.Acquisitions[0].End = plan.Acquisitions[0].End.Add(24 * time.Hour)
		}},
		{"double win", func(plan *domain.Plan) {
			plan.Acquisitions[1].RequestID = plan.Acquisitions[0].RequestID
		}},
		{"vanished request", func(plan *domain.Plan) {
			plan.Acquisitions = plan.Acquisitions[:1]
			plan.Unfulfilled = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if breakPlan(tc.mutate) == "" {
				t.Fatalf("the checker accepted a plan with %s; every green property run is meaningless", tc.name)
			}
		})
	}
}

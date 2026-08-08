package domain_test

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

var bucketStart = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func profile() domain.SatelliteProfile {
	return domain.SatelliteProfile{Agility: agility(), DutyCycleBudgetS: 600}
}

func schedule(t *testing.T, p domain.SatelliteProfile) *domain.Schedule {
	t.Helper()
	s, err := domain.NewSchedule(p)
	if err != nil {
		t.Fatalf("opening a schedule: %v", err)
	}
	return s
}

// scored builds a candidate whose access window opens at `open` and stays open
// for `slack`, requiring `duration` seconds of imaging at `roll` degrees.
func scored(id string, open time.Duration, slack, duration time.Duration, roll float64) domain.ScoredCandidate {
	orbit := 47110
	c := validCandidate()
	c.OpportunityID = id
	c.RequestID = "req-" + id
	c.OrbitNumber = &orbit
	c.AccessStart = bucketStart.Add(open)
	c.AccessEnd = bucketStart.Add(open + slack)
	c.AcquisitionDurationS = duration.Seconds()
	c.DutyCycleCostS = duration.Seconds()

	return domain.ScoredCandidate{
		Candidate:      c,
		CustomerID:     "acme",
		EffectiveValue: 100,
		Deadline:       bucketStart.Add(3 * time.Hour),
		Attitude:       domain.Attitude{RollDeg: roll, Mode: "STRIPMAP"},
	}
}

func mustPlace(t *testing.T, s *domain.Schedule, c domain.ScoredCandidate) domain.Placement {
	t.Helper()
	p, refusal, ok := s.TryPlace(c)
	if !ok {
		t.Fatalf("%s was refused: %s — %s", c.OpportunityID, refusal.ReasonCode, refusal.Explanation)
	}
	if err := s.Commit(p); err != nil {
		t.Fatalf("committing %s: %v", c.OpportunityID, err)
	}
	return p
}

func TestTheFirstAcquisitionStartsAsEarlyAsItMay(t *testing.T) {
	s := schedule(t, profile())
	c := scored("o1", time.Minute, 10*time.Minute, 30*time.Second, 20)

	p := mustPlace(t, s, c)

	if !p.Start.Equal(c.AccessStart) {
		t.Errorf("start = %s, want the window open at %s", p.Start, c.AccessStart)
	}
	if p.End.Sub(p.Start) != 30*time.Second {
		t.Errorf("span = %s, want 30s", p.End.Sub(p.Start))
	}
	// Nothing preceded it, so there is no transition to describe. A zero here
	// would claim an instantaneous slew from nowhere.
	if p.SlewFromPreviousS != nil || p.GapFromPreviousS != nil {
		t.Error("the first acquisition reports a transition from a predecessor that does not exist")
	}
}

// The second acquisition must wait out the slew, and the wait is a FUNCTION of
// the pair. This is what a constant gap cannot express.
func TestTheSecondAcquisitionWaitsOutTheSlew(t *testing.T) {
	s := schedule(t, profile()) // 1 deg/s, 5 s settling

	first := mustPlace(t, s, scored("o1", 0, 30*time.Minute, 30*time.Second, 0))
	// 40 degrees away: 40 s of slew plus 5 s settling.
	second := mustPlace(t, s, scored("o2", 0, 30*time.Minute, 30*time.Second, 40))

	wantEarliest := first.End.Add(45 * time.Second)
	if second.Start.Before(wantEarliest) {
		t.Errorf("second start %s is before the slew allows (%s)", second.Start, wantEarliest)
	}
	if !second.Start.Equal(wantEarliest) {
		t.Errorf("second start %s, want exactly %s — the engine is not packing left", second.Start, wantEarliest)
	}
	if second.SlewFromPreviousS == nil || *second.SlewFromPreviousS != 45 {
		t.Errorf("slew from previous = %v, want 45", second.SlewFromPreviousS)
	}
}

// BOTH neighbours matter. A start that clears the predecessor can still be
// infeasible because it leaves the SUCCESSOR too little room to slew away —
// and getting only one direction right is the plausible bug here.
func TestInsertionRespectsBothNeighbours(t *testing.T) {
	s := schedule(t, profile())

	// Two acquisitions 10 minutes apart at nadir.
	mustPlace(t, s, scored("early", 0, time.Second, 30*time.Second, 0))
	mustPlace(t, s, scored("late", 10*time.Minute, time.Second, 30*time.Second, 0))

	// A 40-degree candidate that could only go between them. It needs 45 s of
	// slew in AND 45 s out, plus its own 30 s — comfortably inside 10 minutes.
	middle := mustPlace(t, s, scored("middle", time.Minute, 5*time.Minute, 30*time.Second, 40))

	acquisitions := s.Acquisitions()
	if len(acquisitions) != 3 {
		t.Fatalf("scheduled %d acquisitions, want 3", len(acquisitions))
	}
	if acquisitions[1].OpportunityID != "middle" {
		t.Fatalf("time order is %s, %s, %s", acquisitions[0].OpportunityID, acquisitions[1].OpportunityID, acquisitions[2].OpportunityID)
	}

	// The successor's transition must have been RECOMPUTED. Leaving it stale
	// would put a number on the M4 timeline describing a plan that no longer
	// exists.
	successor := acquisitions[2]
	if successor.SlewFromPreviousS == nil || *successor.SlewFromPreviousS != 45 {
		t.Errorf("the successor still reports slew %v from its old predecessor, want 45 from the inserted one",
			successor.SlewFromPreviousS)
	}
	if !middle.End.Add(45 * time.Second).Before(successor.Start.Add(time.Nanosecond)) {
		t.Errorf("the inserted acquisition ends at %s and leaves less than its 45 s slew before %s",
			middle.End, successor.Start)
	}
}

// A candidate whose window is too tight for the slew in and out is refused,
// with slew named as the binding constraint.
func TestACandidateWithNoRoomIsRefusedForSlew(t *testing.T) {
	s := schedule(t, profile())

	mustPlace(t, s, scored("early", 0, time.Second, 30*time.Second, 0))
	mustPlace(t, s, scored("late", 90*time.Second, time.Second, 30*time.Second, 0))

	// A 40-degree candidate needing 45 s in and 45 s out plus 30 s of imaging —
	// 120 s in a gap of 60 s, and its window is only open in that gap.
	_, refusal, ok := s.TryPlace(scored("squeezed", 35*time.Second, 20*time.Second, 30*time.Second, 40))
	if ok {
		t.Fatal("a candidate that cannot fit between its neighbours was placed")
	}
	if refusal.ReasonCode != domain.ReasonBlockedBySlew {
		t.Errorf("reason = %s, want %s", refusal.ReasonCode, domain.ReasonBlockedBySlew)
	}
	if refusal.Explanation == "" {
		t.Error("no explanation; the why-panel would have nothing to show")
	}
}

func TestAnAcquisitionThatCannotFinishInTimeIsRefused(t *testing.T) {
	s := schedule(t, profile())

	c := scored("late", 0, time.Minute, 10*time.Minute, 0)
	c.Deadline = bucketStart.Add(5 * time.Minute) // finishes at +10m, too late

	_, refusal, ok := s.TryPlace(c)
	if ok {
		t.Fatal("an acquisition that finishes after its deadline was placed")
	}
	if refusal.ReasonCode != domain.ReasonDeadlinePassed {
		t.Errorf("reason = %s, want %s", refusal.ReasonCode, domain.ReasonDeadlinePassed)
	}
}

// The deadline binds on the FINISH, not the start. Feasibility clamped its
// search to the window so every candidate starts in time by construction; one
// that begins before the deadline can still end after it, and that check has no
// other home.
func TestTheDeadlineBindsOnTheFinishNotTheStart(t *testing.T) {
	s := schedule(t, profile())

	c := scored("tight", 0, 10*time.Minute, 60*time.Second, 0)
	// Starts legally at +0, but must finish by +30s.
	c.Deadline = bucketStart.Add(30 * time.Second)

	if _, _, ok := s.TryPlace(c); ok {
		t.Error("an acquisition starting in time but finishing late was placed")
	}
}

func TestAnExhaustedOrbitIsRefusedForDutyCycle(t *testing.T) {
	// A budget that fits one 300 s acquisition and not two.
	p := profile()
	p.DutyCycleBudgetS = 500
	s := schedule(t, p)

	mustPlace(t, s, scored("first", 0, time.Hour, 300*time.Second, 0))

	_, refusal, ok := s.TryPlace(scored("second", 0, time.Hour, 300*time.Second, 0))
	if ok {
		t.Fatal("a second acquisition was placed beyond the orbit's budget")
	}
	if refusal.ReasonCode != domain.ReasonDutyCycle {
		t.Errorf("reason = %s, want %s", refusal.ReasonCode, domain.ReasonDutyCycle)
	}
	// Actionable, per the contract: both numbers.
	if refusal.Explanation == "" {
		t.Error("no explanation of what was needed against what remained")
	}
}

func TestACandidateBeyondRollAuthorityIsRefused(t *testing.T) {
	s := schedule(t, profile()) // 45 degrees of authority

	_, refusal, ok := s.TryPlace(scored("steep", 0, time.Hour, 30*time.Second, 50))
	if ok {
		t.Fatal("a candidate beyond the spacecraft's roll authority was placed; the plan would not be flyable")
	}
	if refusal.ReasonCode != domain.ReasonBlockedBySlew {
		t.Errorf("reason = %s, want %s", refusal.ReasonCode, domain.ReasonBlockedBySlew)
	}
}

// At most one acquisition per request — do not image the same target twice.
func TestARequestCannotWinTwice(t *testing.T) {
	s := schedule(t, profile())

	first := scored("o1", 0, time.Hour, 30*time.Second, 0)
	mustPlace(t, s, first)

	second := scored("o2", 10*time.Minute, time.Hour, 30*time.Second, 0)
	second.RequestID = first.RequestID // same request, different opportunity

	_, refusal, ok := s.TryPlace(second)
	if ok {
		t.Fatal("one request won two slots; the same target would be imaged twice")
	}
	if refusal.ReasonCode != domain.ReasonLostToHigherValue {
		t.Errorf("reason = %s", refusal.ReasonCode)
	}
}

// TryPlace must NOT mutate. ExactDP explores placements it will not keep, and a
// TryPlace with side effects would make that impossible — it would also mean a
// policy could exhaust a budget by asking questions.
func TestTryPlaceDoesNotMutate(t *testing.T) {
	s := schedule(t, profile())

	c := scored("o1", 0, time.Hour, 300*time.Second, 0)
	for range 5 {
		if _, _, ok := s.TryPlace(c); !ok {
			t.Fatal("a candidate that fits was refused after repeated enquiries; TryPlace is spending the budget")
		}
	}
	if len(s.Acquisitions()) != 0 {
		t.Errorf("TryPlace committed %d acquisitions", len(s.Acquisitions()))
	}
}

// THE INVARIANT. For any sequence of accepted placements, the committed
// schedule never overlaps, never violates slew, never overruns an orbit and
// never misses a deadline.
//
// This is the domain half of what M2-12 asserts over committed plans, and it is
// the test that makes the whole allocation trustworthy: every policy proposes
// through TryPlace, so a policy cannot produce an infeasible plan without this
// failing first.
func TestNoAcceptedScheduleEverViolatesAConstraint(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22)) //nolint:gosec // deterministic by design

	for run := range 300 {
		p := profile()
		p.DutyCycleBudgetS = 200 + rng.Float64()*600
		p.Agility.SlewRateDegS = 0.5 + rng.Float64()*3
		p.Agility.SettleTimeS = rng.Float64() * 10
		s := schedule(t, p)

		var committed []domain.ScoredCandidate
		for i := range 40 {
			c := scored(
				fmt.Sprintf("r%d-o%d", run, i),
				time.Duration(rng.Int64N(int64(2*time.Hour))),
				time.Duration(rng.Int64N(int64(20*time.Minute))),
				time.Duration(10+rng.Int64N(120))*time.Second,
				rng.Float64()*80-40,
			)
			c.Deadline = bucketStart.Add(3 * time.Hour)

			placement, _, ok := s.TryPlace(c)
			if !ok {
				continue
			}
			if err := s.Commit(placement); err != nil {
				t.Fatalf("a placement TryPlace approved was refused on commit: %v", err)
			}
			committed = append(committed, c)
		}

		assertFeasible(t, s, p, committed)
	}
}

func assertFeasible(t *testing.T, s *domain.Schedule, p domain.SatelliteProfile, committed []domain.ScoredCandidate) {
	t.Helper()
	acquisitions := s.Acquisitions()

	byOpportunity := map[string]domain.ScoredCandidate{}
	for _, c := range committed {
		byOpportunity[c.OpportunityID] = c
	}

	seenRequests := map[string]bool{}
	for i, a := range acquisitions {
		if seenRequests[a.RequestID] {
			t.Fatalf("request %s holds two acquisitions", a.RequestID)
		}
		seenRequests[a.RequestID] = true

		c, ok := byOpportunity[a.OpportunityID]
		if !ok {
			t.Fatalf("acquisition %s was never committed", a.OpportunityID)
		}
		// It must start inside its own access window.
		if a.Start.Before(c.AccessStart) || a.Start.After(c.AccessEnd) {
			t.Fatalf("%s starts at %s, outside its window [%s, %s]",
				a.OpportunityID, a.Start, c.AccessStart, c.AccessEnd)
		}
		if a.End.After(c.Deadline) {
			t.Fatalf("%s finishes at %s, after its deadline %s", a.OpportunityID, a.End, c.Deadline)
		}
		if i == 0 {
			continue
		}

		previous := acquisitions[i-1]
		if a.Start.Before(previous.End) {
			t.Fatalf("%s starts at %s while %s runs until %s — overlapping acquisitions on one satellite",
				a.OpportunityID, a.Start, previous.OpportunityID, previous.End)
		}
		required := p.Agility.SlewTime(previous.Attitude, a.Attitude)
		if gap := a.Start.Sub(previous.End); gap < required {
			t.Fatalf("%s follows %s after %s, less than the %s slew it needs",
				a.OpportunityID, previous.OpportunityID, gap, required)
		}
	}

	for _, u := range s.Usage() {
		if u.SpentS > p.DutyCycleBudgetS {
			t.Fatalf("orbit %d spent %.2f s against a %.2f s budget", u.Orbit, u.SpentS, p.DutyCycleBudgetS)
		}
	}
}

func TestPlanConservation(t *testing.T) {
	plan := domain.Plan{
		Acquisitions: []domain.ScheduledAcquisition{{RequestID: "a"}},
		Unfulfilled:  []domain.Unfulfilment{{RequestID: "b", ReasonCode: domain.ReasonLostToHigherValue}},
	}

	if err := plan.Validate([]string{"a", "b"}); err != nil {
		t.Fatalf("a conserving plan was refused: %v", err)
	}

	// The worst possible failure mode for a customer, per the contract: a
	// request that silently vanishes.
	if err := plan.Validate([]string{"a", "b", "c"}); err == nil {
		t.Error("a request that competed and appears nowhere was accepted")
	}

	// And a request that both won and lost is a policy bug, not an outcome.
	doubled := domain.Plan{
		Acquisitions: []domain.ScheduledAcquisition{{RequestID: "a"}},
		Unfulfilled:  []domain.Unfulfilment{{RequestID: "a", ReasonCode: domain.ReasonLostToHigherValue}},
	}
	if err := doubled.Validate([]string{"a"}); err == nil {
		t.Error("a request reported as both an acquisition and an unfulfilment was accepted")
	}
}

func TestPlanValueSumsItsAwards(t *testing.T) {
	plan := domain.Plan{Acquisitions: []domain.ScheduledAcquisition{
		{AwardedValueCredits: 300}, {AwardedValueCredits: 250}, {AwardedValueCredits: 1},
	}}
	if got := plan.Value(); got != 551 {
		t.Errorf("Value = %d, want 551", got)
	}
	// An empty plan is worth nothing, not an error. A cadence sweep over an
	// empty bucket committing an empty plan is legal and meaningful.
	if got := (domain.Plan{}).Value(); got != 0 {
		t.Errorf("an empty plan is worth %d", got)
	}
}

// A schedule cannot be opened against a profile it could not schedule with —
// caught here rather than dividing by zero on the first transition.
func TestAScheduleRefusesAnUnusableProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile domain.SatelliteProfile
	}{
		{"no slew rate", domain.SatelliteProfile{
			Agility: domain.Agility{SlewRateDegS: 0, MaxRollDeg: 45}, DutyCycleBudgetS: 600,
		}},
		{"no power budget", domain.SatelliteProfile{Agility: agility(), DutyCycleBudgetS: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := domain.NewSchedule(tt.profile); err == nil {
				t.Fatalf("opened a schedule with %s", tt.name)
			}
		})
	}
}

// Commit is the second line behind TryPlace. A placement built by hand, or one
// held across other commits until it no longer fits, must be refused rather
// than trusted.
func TestCommitRefusesWhatNoLongerFits(t *testing.T) {
	p := profile()
	p.DutyCycleBudgetS = 400
	s := schedule(t, p)

	first := scored("o1", 0, time.Hour, 300*time.Second, 0)
	second := scored("o2", 30*time.Minute, time.Hour, 300*time.Second, 0)

	// Both approved against an empty schedule...
	firstPlacement, _, ok := s.TryPlace(first)
	if !ok {
		t.Fatal("the first candidate was refused")
	}
	secondPlacement, _, ok := s.TryPlace(second)
	if !ok {
		t.Fatal("the second candidate was refused against an empty schedule")
	}

	// ...but only one fits once the other is committed.
	if err := s.Commit(firstPlacement); err != nil {
		t.Fatalf("committing the first: %v", err)
	}
	if err := s.Commit(secondPlacement); err == nil {
		t.Fatal("a stale placement was committed beyond the budget; TryPlace's answer was trusted after it expired")
	}

	// And a duplicate request is refused at commit too, not only at TryPlace.
	duplicate := firstPlacement
	if err := s.Commit(duplicate); err == nil {
		t.Fatal("the same request was committed twice")
	}
}

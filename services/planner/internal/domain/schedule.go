package domain

import (
	"fmt"
	"sort"
	"time"
)

// The feasibility engine every policy shares.
//
// Shared deliberately. If each policy decided for itself whether an acquisition
// fits, the M2-13 comparison would be measuring four different notions of
// feasibility rather than four allocation strategies — and a policy that scored
// well by being slightly wrong about slew would look like the best algorithm.
// Every policy proposes; this decides.
//
// It is also where the problem's actual difficulty lives. The access window
// bounds where an acquisition may START, so placing one is a choice, and the
// cost of that choice depends on its neighbours in BOTH directions. That is the
// sequence-dependent setup time ADR-0007's strong NP-hardness reduction runs
// through.

// Schedule is a satellite's acquisitions within one bucket, kept in time order.
//
// One satellite, because that is the granularity of the invariant and of the
// advisory lock.
type Schedule struct {
	profile SatelliteProfile
	ledger  *DutyCycleLedger

	placed    []ScheduledAcquisition
	byRequest map[string]string // request -> opportunity already scheduled
}

// NewSchedule opens an empty schedule against one satellite's profile.
func NewSchedule(profile SatelliteProfile) (*Schedule, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	ledger, err := NewDutyCycleLedger(profile.DutyCycleBudgetS)
	if err != nil {
		return nil, err
	}
	return &Schedule{
		profile:   profile,
		ledger:    ledger,
		byRequest: map[string]string{},
	}, nil
}

// Placement is a feasible slot for one candidate.
type Placement struct {
	Candidate ScoredCandidate
	Start     time.Time
	End       time.Time
	Orbit     int

	// SlewFromPreviousS and GapFromPreviousS describe the transition into this
	// acquisition, or nil when it is first in the plan.
	SlewFromPreviousS *float64
	GapFromPreviousS  *float64
}

// Refusal says which constraint bound, so the unfulfilment reason is a fact
// rather than a guess.
type Refusal struct {
	ReasonCode  string
	Explanation string
}

// Acquisitions returns the schedule in time order.
func (s *Schedule) Acquisitions() []ScheduledAcquisition {
	out := make([]ScheduledAcquisition, len(s.placed))
	copy(out, s.placed)
	return out
}

// Usage reports duty-cycle consumption per orbit, for plan metrics.
func (s *Schedule) Usage() []Usage { return s.ledger.Usage() }

// HasRequest reports whether this request already won a slot.
func (s *Schedule) HasRequest(requestID string) bool {
	_, ok := s.byRequest[requestID]
	return ok
}

// TryPlace finds the EARLIEST feasible slot for a candidate without committing
// it, and says why when there is none.
//
// Earliest rather than best. A greedy policy wants the slot that leaves the most
// room for whatever comes next, and packing left is the cheap approximation of
// that; a policy wanting something else can propose candidates in a different
// order, which is exactly the freedom the Strategy interface exists to give.
func (s *Schedule) TryPlace(c ScoredCandidate) (Placement, Refusal, bool) {
	duration := time.Duration(c.AcquisitionDurationS * float64(time.Second))

	if s.HasRequest(c.RequestID) {
		// At most one acquisition per request — do not image the same target
		// twice. Enforced here so no policy has to remember it.
		return Placement{}, Refusal{
			ReasonCode:  ReasonLostToHigherValue,
			Explanation: fmt.Sprintf("request already scheduled as opportunity %s", s.byRequest[c.RequestID]),
		}, false
	}

	if !s.profile.Agility.WithinRollAuthority(c.Attitude) {
		// Not a computation error — a candidate that cannot be flown. Reported
		// as a slew refusal because the roll IS the manoeuvre, and it is the
		// closest truthful code among the contract's seven.
		return Placement{}, Refusal{
			ReasonCode: ReasonBlockedBySlew,
			Explanation: fmt.Sprintf("requires %.2f° of roll, beyond the spacecraft's %.2f°",
				c.Attitude.RollDeg, s.profile.Agility.MaxRollDeg),
		}, false
	}

	// The deadline is checked first because it is unambiguous and cheap: if the
	// acquisition cannot finish in time even starting at the first legal
	// instant, no arrangement of neighbours will help.
	if c.AccessStart.Add(duration).After(c.Deadline) {
		return Placement{}, Refusal{
			ReasonCode: ReasonDeadlinePassed,
			Explanation: fmt.Sprintf("earliest finish %s is after the deadline %s",
				c.AccessStart.Add(duration).Format(time.RFC3339), c.Deadline.Format(time.RFC3339)),
		}, false
	}

	orbit, hasOrbit := OrbitOf(c.Candidate)
	if !hasOrbit {
		// Held, not refused. See OrbitOf: a candidate with no orbit cannot be
		// charged against any budget, and none of the contract's reason codes
		// honestly describes a producer omitting an optional field. The round
		// never offers it, so it never reaches here from the real path — this is
		// the guard, not the policy.
		return Placement{}, Refusal{
			ReasonCode:  ReasonNoOpportunity,
			Explanation: "candidate carries no orbit number and cannot be charged against a duty-cycle budget",
		}, false
	}

	// Latest legal start across every constraint that does not involve
	// neighbours.
	latestStart := c.AccessEnd
	if deadlineStart := c.Deadline.Add(-duration); deadlineStart.Before(latestStart) {
		latestStart = deadlineStart
	}

	start, ok := s.earliestFeasibleStart(c, duration, latestStart)
	if !ok {
		return Placement{}, Refusal{
			ReasonCode: ReasonBlockedBySlew,
			Explanation: fmt.Sprintf(
				"no start in [%s, %s] leaves room for the slew to and from its neighbours",
				c.AccessStart.Format(time.RFC3339), latestStart.Format(time.RFC3339)),
		}, false
	}

	// Duty cycle last, because it is the only constraint whose refusal depends
	// on nothing but the orbit — checking it earlier would report an exhausted
	// budget for a candidate that could not have been placed anyway, and the
	// unfulfilment reason is supposed to name the binding constraint.
	if affordable, short := s.ledger.CanAfford(orbit, c.DutyCycleCostS); !affordable {
		return Placement{}, Refusal{
			ReasonCode:  ReasonDutyCycle,
			Explanation: short.String(),
		}, false
	}

	placement := Placement{
		Candidate: c,
		Start:     start,
		End:       start.Add(duration),
		Orbit:     orbit,
	}
	if previous, found := s.predecessorOf(start); found {
		slew := s.profile.Agility.SlewTime(previous.Attitude, c.Attitude).Seconds()
		gap := start.Sub(previous.End).Seconds()
		placement.SlewFromPreviousS = &slew
		placement.GapFromPreviousS = &gap
	}
	return placement, Refusal{}, true
}

// Commit accepts a placement, charging the budget and keeping time order.
//
// Separate from TryPlace so a policy can evaluate several candidates before
// choosing — ExactDP in particular explores placements it will not keep, and a
// TryPlace that mutated would make that impossible.
func (s *Schedule) Commit(p Placement) error {
	if s.HasRequest(p.Candidate.RequestID) {
		return fmt.Errorf("%w: request %s already scheduled", ErrInvalid, p.Candidate.RequestID)
	}
	if err := s.ledger.Charge(p.Orbit, p.Candidate.DutyCycleCostS); err != nil {
		return err
	}

	s.placed = append(s.placed, ScheduledAcquisition{
		OpportunityID:       p.Candidate.OpportunityID,
		RequestID:           p.Candidate.RequestID,
		CustomerID:          p.Candidate.CustomerID,
		Mode:                p.Candidate.Mode,
		OrbitNumber:         p.Orbit,
		Start:               p.Start,
		End:                 p.End,
		Attitude:            p.Candidate.Attitude,
		DutyCycleCostS:      p.Candidate.DutyCycleCostS,
		SlewFromPreviousS:   p.SlewFromPreviousS,
		GapFromPreviousS:    p.GapFromPreviousS,
		AwardedValueCredits: int64(p.Candidate.EffectiveValue),
		FootprintGeoJSON:    p.Candidate.FootprintGeoJSON,
		GeometryJSON:        p.Candidate.GeometryJSON,
		QualityScore:        p.Candidate.QualityScore,
	})
	s.byRequest[p.Candidate.RequestID] = p.Candidate.OpportunityID

	sort.Slice(s.placed, func(i, j int) bool { return s.placed[i].Start.Before(s.placed[j].Start) })
	// Inserting in the middle changes what the NEXT acquisition slewed from, so
	// the transition figures are recomputed over the whole sequence rather than
	// only for the new one. Leaving them stale would put numbers on the M4
	// timeline that describe a plan that no longer exists.
	s.recomputeTransitions()
	return nil
}

func (s *Schedule) recomputeTransitions() {
	for i := range s.placed {
		if i == 0 {
			s.placed[i].SlewFromPreviousS = nil
			s.placed[i].GapFromPreviousS = nil
			continue
		}
		previous := s.placed[i-1]
		slew := s.profile.Agility.SlewTime(previous.Attitude, s.placed[i].Attitude).Seconds()
		gap := s.placed[i].Start.Sub(previous.End).Seconds()
		s.placed[i].SlewFromPreviousS = &slew
		s.placed[i].GapFromPreviousS = &gap
	}
}

// earliestFeasibleStart scans the gaps between placed acquisitions.
//
// Both neighbours matter. A start that clears the predecessor's slew can still
// be infeasible because it leaves the SUCCESSOR too little time to slew away —
// and that asymmetry is why a constant gap is not a model of this problem.
func (s *Schedule) earliestFeasibleStart(c ScoredCandidate, duration time.Duration, latestStart time.Time) (time.Time, bool) {
	// One pass over len(placed)+1 gaps: before the first, between each pair,
	// and after the last.
	for i := 0; i <= len(s.placed); i++ {
		earliest := c.AccessStart
		if i > 0 {
			previous := s.placed[i-1]
			after := previous.End.Add(s.profile.Agility.SlewTime(previous.Attitude, c.Attitude))
			if after.After(earliest) {
				earliest = after
			}
		}

		latest := latestStart
		if i < len(s.placed) {
			next := s.placed[i]
			// The candidate must finish, then slew away, before the next one
			// starts.
			before := next.Start.Add(-s.profile.Agility.SlewTime(c.Attitude, next.Attitude)).Add(-duration)
			if before.Before(latest) {
				latest = before
			}
		}

		if !earliest.After(latest) {
			return earliest, true
		}
	}
	return time.Time{}, false
}

func (s *Schedule) predecessorOf(start time.Time) (ScheduledAcquisition, bool) {
	var found ScheduledAcquisition
	ok := false
	for _, a := range s.placed {
		if !a.End.After(start) {
			found, ok = a, true
		}
	}
	return found, ok
}

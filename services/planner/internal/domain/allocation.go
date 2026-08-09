package domain

import (
	"fmt"
	"time"
)

// The Strategy boundary. ADR-0007 argues that this interface is the whole point
// of the milestone: it turns "which algorithm?" from a guess into a measurement,
// because four policies over identical inputs are comparable and one policy is
// not.
//
// Allocate is a PURE FUNCTION — no context, no error, no database. That is not
// tidiness. It is what makes property-based testing possible at all (M2-12) and
// what lets the benchmark replay identical inputs through four policies
// (M2-13). A policy that could read anything could read something that differed
// between runs.

// AllocationPolicy decides which candidates fly.
type AllocationPolicy interface {
	// Name is the contract enum value recorded on every round and plan, so a
	// committed plan is attributable to a strategy after the fact.
	Name() string

	// Allocate returns the winners AND the losers with reasons.
	//
	// Unfulfilment reasons come back alongside the plan rather than being
	// derived afterwards. A policy that returned only winners could not power
	// the "why was my request rejected?" panel, and retrofitting explanations
	// onto an algorithm that has already discarded the information is far
	// harder than producing them as it goes.
	Allocate(Problem) Plan
}

// Problem is everything a policy is given, and the only thing it may read.
type Problem struct {
	Key       RoundKey
	BucketEnd time.Time
	Profile   SatelliteProfile

	// Now is passed in rather than read from the clock, so a policy is
	// reproducible. Deadlines are checked against it.
	Now time.Time

	Candidates []ScoredCandidate
}

// ScoredCandidate is a candidate joined to its request's value and deadline.
//
// The join happens before the policy sees it, so no policy reads a priority
// tier — ADR-0007 keeps fairness in EffectiveValue and out of the algorithms.
type ScoredCandidate struct {
	Candidate

	CustomerID string
	// EffectiveValue is what policies compete on. See value.go; it is bid
	// scaled by tier and age, computed in exactly one place.
	EffectiveValue float64
	// Deadline is the end of the request's window. An acquisition must FINISH
	// by it: feasibility clamped its search to the window so every candidate
	// starts in time, but one that begins before the deadline can still end
	// after it.
	Deadline time.Time
	// Attitude is the pointing state this acquisition requires, resolved by
	// AttitudeFor — the producer's roll when supplied, derived otherwise.
	Attitude Attitude
}

// Plan is what a policy produced.
type Plan struct {
	Acquisitions []ScheduledAcquisition
	Unfulfilled  []Unfulfilment
}

// ScheduledAcquisition is one winner, with the placement the policy chose.
type ScheduledAcquisition struct {
	// AcquisitionID is assigned by the application layer before commit, not by
	// the database, because the plan.committed event carries it and the event
	// is built before the INSERT runs.
	AcquisitionID string
	OpportunityID string
	RequestID     string
	CustomerID    string
	Mode          string
	OrbitNumber   int

	// Start is the instant chosen within the access window. It is a DECISION,
	// not an input — the window bounds where the acquisition may begin, and
	// choosing within it is the scheduling freedom the whole problem turns on.
	Start time.Time
	End   time.Time

	Attitude       Attitude
	DutyCycleCostS float64

	// SlewFromPreviousS and GapFromPreviousS are null on the first acquisition
	// of a plan, and are what makes the M4 timeline able to draw slew gaps
	// rather than assert them.
	SlewFromPreviousS *float64
	GapFromPreviousS  *float64

	AwardedValueCredits int64
	// ClearingPriceCredits is set only by VickreySealedBid. Computed and stored,
	// never settled — there is no billing, and that is a stated scope cut.
	ClearingPriceCredits *int64

	FootprintGeoJSON []byte
	// GeometryJSON is the AccessGeometry blob, carried through verbatim because
	// planning.acquisitions.geometry is NOT NULL and because the M4 why-panel
	// explains a rejection in terms of the geometry that caused it.
	GeometryJSON []byte
	QualityScore float64
}

// Unfulfilment explains one request that did not fly.
type Unfulfilment struct {
	RequestID   string
	CustomerID  string
	ReasonCode  string
	Explanation string

	// Detail is the structured half — numbers a customer can act on, per the
	// contract's explanation object.
	Detail RefusalDetail

	// OwnValueCredits is this request's effective value, shown next to the
	// winner's so the customer sees the size of the gap rather than only its
	// existence.
	OwnValueCredits int64

	// BestRejectedOpportunityID is the strongest candidate this request had.
	// The frontend renders it as a ghost on the timeline, so the de-confliction
	// decision is visible rather than only its result.
	BestRejectedOpportunityID string
}

// Reason codes, mirroring planning.request.unfulfilled.v1.
const (
	ReasonLostToHigherValue = "LOST_TO_HIGHER_VALUE"
	ReasonBlockedBySlew     = "BLOCKED_BY_SLEW_CONSTRAINT"
	ReasonDutyCycle         = "DUTY_CYCLE_EXHAUSTED"
	ReasonDeadlinePassed    = "DEADLINE_PASSED"
	ReasonNoOpportunity     = "NO_OPPORTUNITY_IN_BUCKET"
	ReasonSuperseded        = "SUPERSEDED"
	ReasonCancelled         = "CANCELLED_BY_CUSTOMER"
)

// Value is the plan's total, which is what the benchmark divides by ExactDP's.
func (p Plan) Value() int64 {
	var total int64
	for _, a := range p.Acquisitions {
		total += a.AwardedValueCredits
	}
	return total
}

// Validate checks the conservation property the contract asserts: every request
// that competed appears exactly once, as a winner or as an unfulfilment.
//
// Here rather than only in a test, because it is the property most likely to be
// broken by a policy that returns early, and the cheapest place to catch it is
// before the plan reaches the database.
func (p Plan) Validate(competed []string) error {
	seen := make(map[string]string, len(competed))

	for _, a := range p.Acquisitions {
		if previous, duplicate := seen[a.RequestID]; duplicate {
			return fmt.Errorf("%w: request %s appears twice (%s and as an acquisition)", ErrInvalid, a.RequestID, previous)
		}
		seen[a.RequestID] = "an acquisition"
	}
	for _, u := range p.Unfulfilled {
		if previous, duplicate := seen[u.RequestID]; duplicate {
			return fmt.Errorf("%w: request %s appears twice (%s and as %s)", ErrInvalid, u.RequestID, previous, u.ReasonCode)
		}
		seen[u.RequestID] = u.ReasonCode
	}

	for _, requestID := range competed {
		if _, ok := seen[requestID]; !ok {
			// The worst possible failure mode for a customer, per the contract:
			// a request that silently vanishes between rounds.
			return fmt.Errorf("%w: request %s competed and appears neither as an acquisition nor as an unfulfilment",
				ErrInvalid, requestID)
		}
	}
	return nil
}

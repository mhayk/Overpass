// Package domain holds the planner's rules, and nothing that talks to the
// outside world.
//
// It is deliberately small at this stage. The projections this package guards
// are the planner's INPUTS; the allocation logic that will make this package
// interesting arrives with M2-04 onward. What lives here now is the set of
// checks that must hold before a candidate or a snapshot is allowed into the
// planner's schema at all — because a round that reads a malformed row cannot
// tell it from a valid one, and the round is the component that holds the lock.
package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalid marks a payload the planner refuses to store.
//
// One sentinel rather than a type per failure, because every caller does the
// same thing with it: refuse the fold and say why. The detail is in the wrapped
// message, which is what an operator reads.
var ErrInvalid = errors.New("invalid projection input")

// PriorityTiers is the closed set the schema accepts.
//
// Restated here rather than imported from the generated types, and that is a
// real duplication worth defending. The generated binding emits a string type
// with no membership check — go-jsonschema does not turn an enum into a
// constrained Go type — so a payload carrying an unknown tier compiles, decodes
// and reaches the INSERT, where it fails against the CHECK constraint as an
// opaque database error at 3am. Checking it here turns that into a named
// rejection at the boundary.
//
// The JSON Schema remains the authority (CLAUDE.md). This is a restatement and
// must never be the wider of the two.
var PriorityTiers = map[string]struct{}{
	"GOVERNMENT":       {},
	"CIVIL_PROTECTION": {},
	"COMMERCIAL":       {},
	"BEST_EFFORT":      {},
}

// ImagingModes is the closed set planning.candidate_opportunities accepts.
var ImagingModes = map[string]struct{}{
	"SPOTLIGHT": {},
	"STRIPMAP":  {},
	"SCAN":      {},
}

// MaxBidCredits mirrors the schema's upper bound on bid_credits.
const MaxBidCredits int64 = 100_000_000

// Snapshot is the request half of a round's input.
//
// The five fields ADR-0015 chose, plus provenance. Not a copy of the request: a
// copy would drift, and every field not here is one the planner has been shown
// not to need.
type Snapshot struct {
	RequestID     string
	CustomerID    string
	PriorityTier  string
	BidCredits    int64
	WindowStart   time.Time
	WindowEnd     time.Time
	SubmittedAt   time.Time
	SourceEventID string
	OccurredAt    time.Time
}

// Validate refuses what the schema would refuse, before the schema sees it.
func (s Snapshot) Validate() error {
	if s.RequestID == "" {
		return fmt.Errorf("%w: request_id is empty", ErrInvalid)
	}
	if s.CustomerID == "" {
		return fmt.Errorf("%w: customer_id is empty", ErrInvalid)
	}
	if _, ok := PriorityTiers[s.PriorityTier]; !ok {
		return fmt.Errorf("%w: priority_tier %q is not one of the four tiers", ErrInvalid, s.PriorityTier)
	}
	if s.BidCredits < 0 || s.BidCredits > MaxBidCredits {
		return fmt.Errorf("%w: bid_credits %d is outside [0, %d]", ErrInvalid, s.BidCredits, MaxBidCredits)
	}
	// Not isempty, and not merely ordered: the schema's CHECK rejects an empty
	// range, and a zero-width window is empty. A request whose window cannot
	// contain any acquisition is not a request the planner can ever satisfy, so
	// it is refused here rather than stored and puzzled over later.
	if !s.WindowEnd.After(s.WindowStart) {
		return fmt.Errorf("%w: request_window is empty — end %s does not follow start %s",
			ErrInvalid, s.WindowEnd.Format(time.RFC3339), s.WindowStart.Format(time.RFC3339))
	}
	if s.SubmittedAt.IsZero() {
		return fmt.Errorf("%w: submitted_at is zero — the ageing factor is measured from it", ErrInvalid)
	}
	return nil
}

// Candidate is one opportunity the planner may allocate.
type Candidate struct {
	OpportunityID string
	RequestID     string
	SatelliteID   string
	Mode          string

	// AccessStart and AccessEnd bound when the acquisition may START. They do
	// not bound the acquisition itself — see AcquisitionDurationS. Collapsing
	// the two would delete the planner's main scheduling freedom, which is the
	// point 00006_planning_inputs.sql makes at the schema level and ADR-0007
	// leans on for the whole complexity argument.
	AccessStart          time.Time
	AccessEnd            time.Time
	AcquisitionDurationS float64

	// OrbitNumber is absent on a contract-valid opportunity, so it is a pointer
	// rather than a zero value. M2-03 decides what a candidate with no orbit is
	// charged against; this layer only preserves the distinction between "orbit
	// 0" and "no orbit", which an int would destroy.
	OrbitNumber *int

	DutyCycleCostS float64
	QualityScore   float64

	// GeometryJSON is the AccessGeometry blob, verbatim. The slew model (M2-02)
	// reads look_side, squint and roll out of it, so this is an INPUT to
	// scheduling rather than decoration — and it is carried unparsed because
	// roll_angle_deg is optional in the contract and M2-02 has not yet decided
	// whether to derive it or to argue for tightening the schema.
	GeometryJSON []byte

	// FootprintGeoJSON goes to PostGIS unchanged, as GeoJSON rather than WKT.
	// The contracts publish GeoJSON and ST_GeomFromGeoJSON parses it; a
	// hand-rolled WKT serialiser is exactly where ring closure and coordinate
	// order go quietly wrong.
	FootprintGeoJSON []byte

	ComputedAt    time.Time
	SourceEventID string
}

// Validate refuses what the schema would refuse.
func (c Candidate) Validate() error {
	if c.OpportunityID == "" {
		return fmt.Errorf("%w: opportunity_id is empty", ErrInvalid)
	}
	if c.RequestID == "" {
		return fmt.Errorf("%w: request_id is empty", ErrInvalid)
	}
	if c.SatelliteID == "" {
		return fmt.Errorf("%w: satellite_id is empty", ErrInvalid)
	}
	if _, ok := ImagingModes[c.Mode]; !ok {
		return fmt.Errorf("%w: mode %q is not one of SPOTLIGHT, STRIPMAP, SCAN", ErrInvalid, c.Mode)
	}
	if !c.AccessEnd.After(c.AccessStart) {
		return fmt.Errorf("%w: access_window is empty — end %s does not follow start %s",
			ErrInvalid, c.AccessEnd.Format(time.RFC3339), c.AccessStart.Format(time.RFC3339))
	}
	if c.AcquisitionDurationS <= 0 {
		return fmt.Errorf("%w: acquisition_duration_s must be positive, got %v", ErrInvalid, c.AcquisitionDurationS)
	}
	if c.DutyCycleCostS <= 0 {
		return fmt.Errorf("%w: duty_cycle_cost_s must be positive, got %v", ErrInvalid, c.DutyCycleCostS)
	}
	if c.QualityScore < 0 || c.QualityScore > 1 {
		return fmt.Errorf("%w: quality_score %v is outside [0, 1]", ErrInvalid, c.QualityScore)
	}
	if c.OrbitNumber != nil && *c.OrbitNumber < 0 {
		return fmt.Errorf("%w: orbit_number %d is negative", ErrInvalid, *c.OrbitNumber)
	}
	if len(c.GeometryJSON) == 0 {
		return fmt.Errorf("%w: geometry is empty — the slew model reads it", ErrInvalid)
	}
	if len(c.FootprintGeoJSON) == 0 {
		return fmt.Errorf("%w: footprint is empty — a winning candidate must become an acquisition", ErrInvalid)
	}
	return nil
}

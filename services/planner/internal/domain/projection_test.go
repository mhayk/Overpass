package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

func validSnapshot() domain.Snapshot {
	return domain.Snapshot{
		RequestID:     "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
		CustomerID:    "acme-imaging",
		PriorityTier:  "COMMERCIAL",
		BidCredits:    500,
		WindowStart:   time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC),
		WindowEnd:     time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC),
		SubmittedAt:   time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
		SourceEventID: "11111111-2222-4333-8444-555555555555",
		OccurredAt:    time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
	}
}

func TestSnapshotValidation(t *testing.T) {
	if err := validSnapshot().Validate(); err != nil {
		t.Fatalf("the baseline snapshot must be valid, or every case below proves nothing: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.Snapshot)
		want   string
	}{
		{"empty request id", func(s *domain.Snapshot) { s.RequestID = "" }, "request_id"},
		{"empty customer id", func(s *domain.Snapshot) { s.CustomerID = "" }, "customer_id"},
		{"tier outside the four", func(s *domain.Snapshot) { s.PriorityTier = "PLATINUM" }, "priority_tier"},
		{"tier lowercased", func(s *domain.Snapshot) { s.PriorityTier = "commercial" }, "priority_tier"},
		{"negative bid", func(s *domain.Snapshot) { s.BidCredits = -1 }, "bid_credits"},
		{"bid above the schema cap", func(s *domain.Snapshot) { s.BidCredits = domain.MaxBidCredits + 1 }, "bid_credits"},
		// A zero-width window is EMPTY to Postgres, so the schema's
		// isempty CHECK rejects it. Catching it here turns an opaque
		// constraint violation into a named refusal.
		{"zero-width window", func(s *domain.Snapshot) { s.WindowEnd = s.WindowStart }, "request_window"},
		{"window ends before it starts", func(s *domain.Snapshot) { s.WindowEnd = s.WindowStart.Add(-time.Hour) }, "request_window"},
		{"no submitted_at", func(s *domain.Snapshot) { s.SubmittedAt = time.Time{} }, "submitted_at"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSnapshot()
			tt.mutate(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("accepted %s", tt.name)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name the offending field %q", err, tt.want)
			}
		})
	}
}

// Every tier the schema accepts must pass, or the restatement in domain is
// NARROWER than the contract — which would reject valid events at the boundary
// and look exactly like a broken producer.
func TestEveryContractTierIsAccepted(t *testing.T) {
	for _, tier := range []string{"GOVERNMENT", "CIVIL_PROTECTION", "COMMERCIAL", "BEST_EFFORT"} {
		s := validSnapshot()
		s.PriorityTier = tier
		if err := s.Validate(); err != nil {
			t.Errorf("tier %s is in the schema's CHECK and was refused here: %v", tier, err)
		}
	}
}

func validCandidate() domain.Candidate {
	orbit := 47110
	return domain.Candidate{
		OpportunityID:        "55555555-6666-4777-8888-999999999999",
		RequestID:            "cbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
		SatelliteID:          "CAPELLA-14",
		Mode:                 "STRIPMAP",
		AccessStart:          time.Date(2026, 8, 7, 10, 14, 0, 0, time.UTC),
		AccessEnd:            time.Date(2026, 8, 7, 10, 16, 30, 0, time.UTC),
		AcquisitionDurationS: 18.5,
		OrbitNumber:          &orbit,
		DutyCycleCostS:       18.5,
		QualityScore:         0.87,
		GeometryJSON:         []byte(`{"look_side":"RIGHT"}`),
		FootprintGeoJSON:     []byte(`{"type":"Polygon","coordinates":[]}`),
		ComputedAt:           time.Date(2026, 8, 7, 9, 2, 0, 0, time.UTC),
		SourceEventID:        "31111111-2222-4333-8444-555555555555",
	}
}

func TestCandidateValidation(t *testing.T) {
	if err := validCandidate().Validate(); err != nil {
		t.Fatalf("the baseline candidate must be valid: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.Candidate)
		want   string
	}{
		{"empty opportunity id", func(c *domain.Candidate) { c.OpportunityID = "" }, "opportunity_id"},
		{"empty request id", func(c *domain.Candidate) { c.RequestID = "" }, "request_id"},
		{"empty satellite", func(c *domain.Candidate) { c.SatelliteID = "" }, "satellite_id"},
		{"mode outside the three", func(c *domain.Candidate) { c.Mode = "PANCHROMATIC" }, "mode"},
		{"zero-width access window", func(c *domain.Candidate) { c.AccessEnd = c.AccessStart }, "access_window"},
		{"zero duration", func(c *domain.Candidate) { c.AcquisitionDurationS = 0 }, "acquisition_duration_s"},
		{"negative duration", func(c *domain.Candidate) { c.AcquisitionDurationS = -1 }, "acquisition_duration_s"},
		{"zero duty-cycle cost", func(c *domain.Candidate) { c.DutyCycleCostS = 0 }, "duty_cycle_cost_s"},
		{"quality above one", func(c *domain.Candidate) { c.QualityScore = 1.01 }, "quality_score"},
		{"quality below zero", func(c *domain.Candidate) { c.QualityScore = -0.01 }, "quality_score"},
		{"negative orbit", func(c *domain.Candidate) { n := -1; c.OrbitNumber = &n }, "orbit_number"},
		{"no geometry", func(c *domain.Candidate) { c.GeometryJSON = nil }, "geometry"},
		{"no footprint", func(c *domain.Candidate) { c.FootprintGeoJSON = nil }, "footprint"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validCandidate()
			tt.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("accepted %s", tt.name)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name the offending field %q", err, tt.want)
			}
		})
	}
}

// A missing orbit_number is CONTRACT-VALID and must survive validation.
//
// 00006_planning_inputs.sql makes the column nullable for exactly this reason,
// and the comment there says declaring it NOT NULL would make a contract-valid
// event unstorable. Rejecting it here would reintroduce that from the other
// end, and it would look like a producer bug rather than a validator bug.
func TestCandidateWithoutAnOrbitIsAccepted(t *testing.T) {
	c := validCandidate()
	c.OrbitNumber = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("a candidate with no orbit_number was refused; M2-03 decides what to charge it against, not this layer: %v", err)
	}
}

// Orbit 0 is a real orbit and must not be confused with an absent one.
func TestOrbitZeroIsNotAbsent(t *testing.T) {
	c := validCandidate()
	zero := 0
	c.OrbitNumber = &zero
	if err := c.Validate(); err != nil {
		t.Fatalf("orbit 0 was refused: %v", err)
	}
}

func TestEveryContractModeIsAccepted(t *testing.T) {
	for _, mode := range []string{"SPOTLIGHT", "STRIPMAP", "SCAN"} {
		c := validCandidate()
		c.Mode = mode
		if err := c.Validate(); err != nil {
			t.Errorf("mode %s is in the schema's CHECK and was refused here: %v", mode, err)
		}
	}
}

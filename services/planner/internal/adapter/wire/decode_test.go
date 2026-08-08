package wire_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/adapter/wire"
)

// example reads a payload from contracts/examples/valid VERBATIM.
//
// Verbatim is the whole point. A payload retyped into this file is a payload
// that agrees with the decoder because both were written by the same hand at
// the same moment, which is precisely the agreement #112 had.
func example(t *testing.T, subject, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "..",
		"contracts", "examples", "valid", subject, name)
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return payload
}

func TestRequestReceivedDecodesARealPayload(t *testing.T) {
	got, err := wire.New().RequestReceived(example(t, "tasking.request.received.v1", "minimal.json"))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if got.EventID != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("event_id = %q, want the envelope's id", got.EventID)
	}
	if got.Snapshot.RequestID != "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff" {
		t.Errorf("request_id = %q", got.Snapshot.RequestID)
	}
	if got.Snapshot.CustomerID != "acme-imaging" {
		t.Errorf("customer_id = %q", got.Snapshot.CustomerID)
	}
	if got.Snapshot.PriorityTier != "BEST_EFFORT" {
		t.Errorf("priority_tier = %q", got.Snapshot.PriorityTier)
	}
	if got.Snapshot.BidCredits != 0 {
		t.Errorf("bid_credits = %d, want 0 — and 0 is a REAL value here, which is why every other field is asserted too", got.Snapshot.BidCredits)
	}

	wantStart := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	if !got.Snapshot.WindowStart.Equal(wantStart) {
		t.Errorf("window start = %s, want %s", got.Snapshot.WindowStart, wantStart)
	}
	wantEnd := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	if !got.Snapshot.WindowEnd.Equal(wantEnd) {
		t.Errorf("window end = %s, want %s", got.Snapshot.WindowEnd, wantEnd)
	}
	wantSubmitted := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	if !got.Snapshot.SubmittedAt.Equal(wantSubmitted) {
		t.Errorf("submitted_at = %s, want %s", got.Snapshot.SubmittedAt, wantSubmitted)
	}

	// Provenance travels from the envelope, not from data. A snapshot that
	// cannot name the event that produced it makes "the planner valued this
	// wrongly" an accusation rather than a diagnosable claim.
	if got.Snapshot.SourceEventID != got.EventID {
		t.Errorf("source_event_id = %q, want the envelope's event_id %q",
			got.Snapshot.SourceEventID, got.EventID)
	}

	// The decoded snapshot must survive the domain guard, or the fixture and
	// the validator disagree about what a valid request is.
	if err := got.Snapshot.Validate(); err != nil {
		t.Errorf("a valid contract example failed domain validation: %v", err)
	}
}

func TestOpportunitiesDecodesARealPayload(t *testing.T) {
	got, err := wire.New().Opportunities(
		example(t, "feasibility.opportunities.computed.v1", "two-passes.json"))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if got.RequestID != "cbbbbbbb-cccc-4ddd-8eee-ffffffffffff" {
		t.Errorf("request_id = %q", got.RequestID)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("decoded %d candidates, want 2", len(got.Candidates))
	}

	first := got.Candidates[0]
	if first.OpportunityID != "55555555-6666-4777-8888-999999999999" {
		t.Errorf("opportunity_id = %q", first.OpportunityID)
	}
	if first.SatelliteID != "CAPELLA-14" {
		t.Errorf("satellite_id = %q", first.SatelliteID)
	}
	if first.Mode != "STRIPMAP" {
		t.Errorf("mode = %q", first.Mode)
	}
	if first.AcquisitionDurationS != 18.5 {
		t.Errorf("acquisition_duration_s = %v, want 18.5", first.AcquisitionDurationS)
	}
	if first.DutyCycleCostS != 18.5 {
		t.Errorf("duty_cycle_cost_s = %v, want 18.5", first.DutyCycleCostS)
	}
	if first.QualityScore != 0.87 {
		t.Errorf("quality_score = %v, want 0.87", first.QualityScore)
	}
	if first.OrbitNumber == nil || *first.OrbitNumber != 47110 {
		t.Errorf("orbit_number = %v, want 47110", first.OrbitNumber)
	}

	// The item does not repeat request_id; it comes from the event's data. Get
	// this wrong and every candidate is stored against the empty request, which
	// no round would ever join.
	if first.RequestID != got.RequestID {
		t.Errorf("candidate request_id = %q, want the event's %q", first.RequestID, got.RequestID)
	}

	// The access window bounds the START. If this were the acquisition
	// interval, the window would be at least acquisition_duration_s wide by
	// construction; here it is 150s against an 18.5s acquisition, which is the
	// slack the planner spends absorbing slew.
	span := first.AccessEnd.Sub(first.AccessStart)
	if span != 150*time.Second {
		t.Errorf("access window span = %s, want 2m30s", span)
	}

	// Geometry and footprint must survive as usable JSON, because the slew
	// model reads one and PostGIS parses the other.
	var geometry map[string]any
	if err := json.Unmarshal(first.GeometryJSON, &geometry); err != nil {
		t.Fatalf("geometry is not JSON: %v", err)
	}
	for _, required := range []string{"incidence_angle_deg", "look_side", "squint_angle_deg"} {
		if _, ok := geometry[required]; !ok {
			t.Errorf("geometry lost %q in re-encoding — M2-02 reads it", required)
		}
	}
	var footprint map[string]any
	if err := json.Unmarshal(first.FootprintGeoJSON, &footprint); err != nil {
		t.Fatalf("footprint is not JSON: %v", err)
	}
	if footprint["type"] != "Polygon" {
		t.Errorf("footprint type = %v, want Polygon — ST_GeomFromGeoJSON needs it", footprint["type"])
	}

	for i, c := range got.Candidates {
		if err := c.Validate(); err != nil {
			t.Errorf("candidate %d of a valid contract example failed domain validation: %v", i+1, err)
		}
		if c.SourceEventID != got.EventID {
			t.Errorf("candidate %d source_event_id = %q, want %q", i+1, c.SourceEventID, got.EventID)
		}
	}

	// The two candidates must be distinct objects, not two views of the last
	// loop iteration. Aliasing here would produce a plausible plan with the
	// wrong orbits, which is the kind of wrong that survives review.
	second := got.Candidates[1]
	if first.OrbitNumber == second.OrbitNumber {
		t.Error("both candidates share one *int — the orbit pointers are aliased")
	}
	if second.OrbitNumber == nil || *second.OrbitNumber != 47133 {
		t.Errorf("second orbit_number = %v, want 47133", second.OrbitNumber)
	}
}

// TestTheNaiveDecodeWouldHaveFailed is the guard on the guard.
//
// #112 shipped because a projector unmarshalled real payloads into
// hand-written structs and got an all-zero result WITH NO ERROR. Nothing failed
// loudly; the read model was just always empty. This reproduces that decode
// against the same fixture the tests above use, and requires it to produce
// nothing — which is what proves those tests are actually asserting against the
// contract's shape rather than against a struct that happens to agree with
// them.
//
// If this test ever fails, the naive shape has become correct, and the tests
// above stopped being evidence of anything.
func TestTheNaiveDecodeWouldHaveFailed(t *testing.T) {
	// What a reasonable person writes: the domain's own field names, no
	// envelope, camel-free but not snake_case, decoded straight from the wire.
	type naive struct {
		RequestID  string `json:"requestId"`
		CustomerID string `json:"customerId"`
		BidCredits int64  `json:"bidCredits"`
	}

	var got naive
	if err := json.Unmarshal(example(t, "tasking.request.received.v1", "minimal.json"), &got); err != nil {
		t.Fatalf("the naive decode ERRORED, which would have been a mercy: %v", err)
	}

	if got.RequestID != "" || got.CustomerID != "" || got.BidCredits != 0 {
		t.Fatalf("the naive decode extracted %+v — this test no longer demonstrates the #112 failure mode", got)
	}
	// Reaching here means: real payload in, zero struct out, no error. That is
	// the defect, reproduced on demand.
}

func TestPayloadWithoutAnEnvelopeIsRefused(t *testing.T) {
	// #124's defect: a bare data object published as though it were an event.
	// It has no event_id, so it has no dedup key — and an empty dedup key makes
	// every such message look like a redelivery of the same one, so all but the
	// first would be silently dropped.
	bare := []byte(`{"request_id":"bbbbbbbb-cccc-4ddd-8eee-ffffffffffff","customer_id":"acme"}`)

	if _, err := wire.New().RequestReceived(bare); err == nil {
		t.Fatal("an unenveloped payload decoded without error; it would enter the ledger under an empty key")
	}

	bareOpportunities := []byte(`{"request_id":"cbbbbbbb-cccc-4ddd-8eee-ffffffffffff","opportunities":[]}`)
	if _, err := wire.New().Opportunities(bareOpportunities); err == nil {
		t.Fatal("an unenveloped opportunities payload decoded without error")
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	if _, err := wire.New().RequestReceived([]byte(`{"event_id":`)); err == nil {
		t.Fatal("truncated JSON decoded without error")
	}
	if _, err := wire.New().Opportunities([]byte(`not json at all`)); err == nil {
		t.Fatal("non-JSON decoded without error")
	}
}

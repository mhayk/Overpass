package wire_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/wire"
)

// These read payloads from contracts/examples/valid VERBATIM.
//
// That is the whole point. Every test in #26 built its input from the same Go
// struct it decoded into, so the field names always matched themselves and the
// suite could not fail even though nothing decoded. A payload the contract owns
// is the only input that can tell you whether the mapping is real.

func example(t *testing.T, subject, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "..",
		"contracts", "examples", "valid", subject, name)
	payload, err := os.ReadFile(path) //nolint:gosec // a fixed repo-relative path
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return payload
}

// TestAContractExampleDecodesToPopulatedFields is the regression test for #112.
//
// Before the fix this passed json.Unmarshal, returned no error, and produced an
// entirely zero-valued struct. Asserting on the VALUES rather than on the
// absence of an error is what makes it able to fail.
func TestAContractExampleDecodesToPopulatedFields(t *testing.T) {
	payload := example(t, "tasking.request.received.v1", "polygon-target.json")

	got, err := wire.New().RequestReceived(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.RequestID != "cbbbbbbb-cccc-4ddd-8eee-ffffffffffff" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
	if got.CustomerID != "port-authority-nl" {
		t.Errorf("CustomerID = %q", got.CustomerID)
	}
	if got.TargetName != "Port of Rotterdam" {
		t.Errorf("TargetName = %q", got.TargetName)
	}

	// occurred_at from the ENVELOPE, not submitted_at from the data. They differ
	// by a second in this example precisely so a mix-up is visible.
	wantAt := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	if !got.EventAt.Equal(wantAt) {
		t.Errorf("EventAt = %s, want the envelope's occurred_at %s", got.EventAt, wantAt)
	}

	wantStart := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	if !got.WindowStart.Equal(wantStart) || !got.WindowEnd.Equal(wantEnd) {
		t.Errorf("window = %s..%s, want %s..%s", got.WindowStart, got.WindowEnd, wantStart, wantEnd)
	}

	// GeoJSON, kept as GeoJSON. PostGIS parses it; converting to WKT here would
	// mean hand-rolling a serialiser for a format the database already reads.
	var geometry map[string]any
	if err := json.Unmarshal(got.TargetGeoJSON, &geometry); err != nil {
		t.Fatalf("target is not JSON: %v (%s)", err, got.TargetGeoJSON)
	}
	if geometry["type"] != "Polygon" {
		t.Errorf("target type = %v, want Polygon", geometry["type"])
	}
	// Longitude first. The ordering that trips everyone up once.
	rings, ok := geometry["coordinates"].([]any)
	if !ok || len(rings) == 0 {
		t.Fatalf("no coordinates: %s", got.TargetGeoJSON)
	}
	first, ok := rings[0].([]any)
	if !ok || len(first) == 0 {
		t.Fatalf("empty ring: %s", got.TargetGeoJSON)
	}
	position, ok := first[0].([]any)
	if !ok || len(position) != 2 {
		t.Fatalf("bad position: %v", first[0])
	}
	if position[0] != 4.02 || position[1] != 51.92 {
		t.Errorf("first position = %v, want [4.02, 51.92] — longitude first", position)
	}
}

// TestTheMinimalExampleDecodesToo covers the other end: no target_name, a Point
// target, a zero bid. Optional fields being absent must not be an error.
func TestTheMinimalExampleDecodesToo(t *testing.T) {
	payload := example(t, "tasking.request.received.v1", "minimal.json")

	got, err := wire.New().RequestReceived(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RequestID != "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
	if got.TargetName != "" {
		t.Errorf("TargetName = %q, want empty for an example that omits it", got.TargetName)
	}
	if len(got.TargetGeoJSON) == 0 {
		t.Fatal("no target geometry")
	}
}

// TestOpportunitiesDecodeWithTheirFootprints covers the event the map depends
// on. Every field here is one the UI reads, so a silent zero is a blank globe.
func TestOpportunitiesDecodeWithTheirFootprints(t *testing.T) {
	payload := example(t, "feasibility.opportunities.computed.v1", "two-passes.json")

	got, err := wire.New().Opportunities(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RequestID != "cbbbbbbb-cccc-4ddd-8eee-ffffffffffff" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
	if len(got.Opportunities) != 2 {
		t.Fatalf("got %d opportunities, want 2", len(got.Opportunities))
	}

	first := got.Opportunities[0]
	if first.SatelliteID != "CAPELLA-14" || first.Mode != "STRIPMAP" {
		t.Errorf("first = %s/%s", first.SatelliteID, first.Mode)
	}
	if first.AcquisitionDurationS != 18.5 {
		t.Errorf("duration = %v, want 18.5", first.AcquisitionDurationS)
	}
	if first.QualityScore != 0.87 {
		t.Errorf("quality = %v, want 0.87", first.QualityScore)
	}
	if first.OrbitNumber == nil || *first.OrbitNumber != 47110 {
		t.Errorf("orbit = %v, want 47110", first.OrbitNumber)
	}
	wantStart := time.Date(2026, 8, 7, 10, 14, 0, 0, time.UTC)
	if !first.AccessStart.Equal(wantStart) {
		t.Errorf("access start = %s, want %s", first.AccessStart, wantStart)
	}
	if len(first.FootprintGeoJSON) == 0 {
		t.Error("no footprint on the first opportunity")
	}

	// The second omits orbit_number and roll_angle_deg. Optional means optional.
	second := got.Opportunities[1]
	if second.Mode != "SCAN" {
		t.Errorf("second mode = %q, want SCAN", second.Mode)
	}
	if len(second.FootprintGeoJSON) == 0 {
		t.Error("no footprint on the second opportunity")
	}
}

// TestAPlanDecodesWithItsSupersessionAndSlew covers the plan event.
//
// supersedes_plan_id is the field ADR-0012's retention hangs on, and the slew
// and gap numbers are what the timeline draws. All three are optional in the
// schema and all three are present here on purpose.
func TestAPlanDecodesWithItsSupersessionAndSlew(t *testing.T) {
	payload := example(t, "planning.plan.committed.v1", "supersedes-earlier-version.json")

	got, err := wire.New().PlanCommitted(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PlanVersion != 2 {
		t.Errorf("PlanVersion = %d, want 2", got.PlanVersion)
	}
	if got.SupersedesPlanID == nil {
		t.Fatal("supersedes_plan_id was dropped; ADR-0012's history would be unreachable")
	}
	if *got.SupersedesPlanID != "23333333-4444-4555-8666-777777777777" {
		t.Errorf("SupersedesPlanID = %q", *got.SupersedesPlanID)
	}
	// SCREAMING_SNAKE, from the schema's enum. Everything written from memory
	// had this as GreedyByBid, which the validator rejected.
	if got.Policy != "GREEDY_BY_BID" {
		t.Errorf("Policy = %q, want GREEDY_BY_BID", got.Policy)
	}
	if got.SatelliteID != "CAPELLA-14" {
		t.Errorf("SatelliteID = %q", got.SatelliteID)
	}

	if len(got.Acquisitions) != 1 {
		t.Fatalf("got %d acquisitions, want 1", len(got.Acquisitions))
	}
	acq := got.Acquisitions[0]
	if acq.AwardedValueCredits != 12000 {
		t.Errorf("AwardedValueCredits = %d, want 12000", acq.AwardedValueCredits)
	}
	if acq.SlewTimeFromPreviousS == nil || *acq.SlewTimeFromPreviousS != 12.5 {
		t.Errorf("slew = %v, want 12.5", acq.SlewTimeFromPreviousS)
	}
	if acq.GapFromPreviousS == nil || *acq.GapFromPreviousS != 41.0 {
		t.Errorf("gap = %v, want 41", acq.GapFromPreviousS)
	}
	if acq.CustomerID != "port-authority-nl" {
		t.Errorf("CustomerID = %q", acq.CustomerID)
	}
	if len(acq.FootprintGeoJSON) == 0 {
		t.Error("no footprint on the acquisition")
	}

	// The metrics block travels whole. It is what the plan-quality panel reads,
	// and picking fields out here would mean editing this mapping every time a
	// metric is added.
	var metrics map[string]any
	if err := json.Unmarshal(got.MetricsJSON, &metrics); err != nil {
		t.Fatalf("metrics is not JSON: %v", err)
	}
	if metrics["requests_unfulfilled"] != 2.0 {
		t.Errorf("requests_unfulfilled = %v, want 2", metrics["requests_unfulfilled"])
	}
}

// TestAPlanWithNoSupersessionReportsNil pins the nullable case.
//
// The schema types supersedes_plan_id as ["string","null"], which the generator
// emits as interface{}. A nil that came back as the string "<nil>" would create
// a link to a plan that does not exist.
func TestAPlanWithNoSupersessionReportsNil(t *testing.T) {
	payload := example(t, "planning.plan.committed.v1", "supersedes-earlier-version.json")

	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatal("no data object")
	}
	data["supersedes_plan_id"] = nil
	mutated, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("re-encoding: %v", err)
	}

	got, err := wire.New().PlanCommitted(mutated)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SupersedesPlanID != nil {
		t.Errorf("SupersedesPlanID = %q, want nil for a first version", *got.SupersedesPlanID)
	}
}

// TestAnUnfulfilmentKeepsItsExplanation guards the numbers.
//
// "Requests unfulfilled by reason" is a domain metric and a bid suggestion is
// only possible if the shortfall survives as a number. Keeping only the reason
// code would leave the UI with a string it cannot chart.
func TestAnUnfulfilmentKeepsItsExplanation(t *testing.T) {
	payload := example(t, "planning.request.unfulfilled.v1", "lost-to-higher-value.json")

	got, err := wire.New().Unfulfilled(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RequestID == "" {
		t.Error("RequestID is empty")
	}
	if got.EventAt.IsZero() {
		t.Error("EventAt is zero")
	}

	var reason map[string]any
	if err := json.Unmarshal(got.ReasonJSON, &reason); err != nil {
		t.Fatalf("reason is not JSON: %v", err)
	}
	if reason["reason_code"] == nil {
		t.Errorf("no reason_code in %s", got.ReasonJSON)
	}
	explanation, ok := reason["explanation"].(map[string]any)
	if !ok {
		t.Fatalf("no explanation object in %s", got.ReasonJSON)
	}
	if _, present := explanation["shortfall_credits"]; !present {
		t.Errorf("shortfall_credits was dropped: %v", explanation)
	}
}

// TestAFlatPayloadIsRejected is the shape the OLD code accepted silently.
//
// Unwrapped, snake_case at the top level: exactly what a naive producer would
// send, and exactly what used to decode to an empty struct with no error.
func TestAFlatPayloadIsRejected(t *testing.T) {
	flat := []byte(`{"request_id":"bbbbbbbb-cccc-4ddd-8eee-ffffffffffff","customer_id":"acme"}`)

	got, err := wire.New().RequestReceived(flat)
	if err == nil {
		t.Fatalf("an unenveloped payload decoded cleanly to %+v; this is exactly #112", got)
	}
	if !errors.Is(err, wire.ErrMalformed) {
		t.Errorf("want ErrMalformed, got %v", err)
	}
}

// TestAnUnknownFieldIsRejected pins DisallowUnknownFields.
//
// additionalProperties is false in every schema, so a field nobody declared is
// a producer talking a version this consumer does not speak. Accepting it
// silently is how the next #112 happens.
func TestAnUnknownFieldIsRejected(t *testing.T) {
	payload := example(t, "tasking.request.received.v1", "minimal.json")

	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	envelope["invented_field"] = "surprise"
	mutated, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("re-encoding: %v", err)
	}

	if _, err := wire.New().RequestReceived(mutated); err == nil {
		t.Fatal("an undeclared field was accepted")
	}
}

func TestGarbageIsRejected(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"not json", `not json at all`},
		{"empty", ``},
		{"null", `null`},
		{"array", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := wire.New().RequestReceived([]byte(tc.payload)); err == nil {
				t.Fatalf("%q was accepted", tc.payload)
			}
		})
	}
}

// TestEphemerisSamplesResolveToAbsoluteInstants is the one decode in this
// package that computes rather than copies.
//
// The event carries offsets from an epoch, because a timestamp per sample would
// be more bytes than the position it labels. Everything downstream — the
// projection's primary key, the range query the renderer reads through — works
// in absolute instants, so the resolution has to happen exactly once, and the
// boundary is where.
func TestEphemerisSamplesResolveToAbsoluteInstants(t *testing.T) {
	payload := example(t, "feasibility.ephemeris.computed.v1", "ascending-pass.json")

	got, err := wire.New().Ephemeris(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.SatelliteID != "SENTINEL-1A" {
		t.Errorf("SatelliteID = %q", got.SatelliteID)
	}
	// From the reference, not from occurred_at. It is what lets a fresher
	// element set overwrite an older track for the same instants.
	wantTLE := time.Date(2026, 8, 6, 21, 41, 12, 0, time.UTC)
	if !got.TleEpoch.Equal(wantTLE) {
		t.Errorf("TleEpoch = %s, want %s", got.TleEpoch, wantTLE)
	}
	if len(got.Samples) != 6 {
		t.Fatalf("got %d samples, want 6", len(got.Samples))
	}

	epoch := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	if !got.Samples[0].At.Equal(epoch) {
		t.Errorf("first sample at %s, want the epoch %s", got.Samples[0].At, epoch)
	}
	if want := epoch.Add(50 * time.Second); !got.Samples[5].At.Equal(want) {
		t.Errorf("last sample at %s, want %s", got.Samples[5].At, want)
	}

	// Longitude first. A swap here relocates the whole constellation and
	// renders without complaint.
	if got.Samples[0].LongitudeDeg != 12.401883 {
		t.Errorf("LongitudeDeg = %v, want the second element of the tuple", got.Samples[0].LongitudeDeg)
	}
	if got.Samples[0].LatitudeDeg != 51.203114 {
		t.Errorf("LatitudeDeg = %v, want the third element of the tuple", got.Samples[0].LatitudeDeg)
	}
	if got.Samples[0].AltitudeM != 693412.8 {
		t.Errorf("AltitudeM = %v", got.Samples[0].AltitudeM)
	}
}

// TestAnEphemerisSampleOfTheWrongLengthIsRejected covers the gap the generated
// type cannot: `prefixItems` renders as [][]float64, so a three-element sample
// decodes cleanly into a slice of three. Reading it positionally without
// checking would index out of range at best and silently shift longitude into
// latitude at worst.
func TestAnEphemerisSampleOfTheWrongLengthIsRejected(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "..",
		"contracts", "examples", "invalid", "feasibility.ephemeris.computed.v1",
		"sample-missing-altitude.json")
	payload, err := os.ReadFile(path) //nolint:gosec // a fixed repo-relative path
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	if _, err := wire.New().Ephemeris(payload); !errors.Is(err, wire.ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

// TestAnEphemerisTrackCrossingTheAntimeridianIsNotNormalised pins a decision by
// showing it: longitude is carried through as published, including the jump
// from +179 to -179. Wrapping it to a continuous 0..360 range here would make
// the numbers look tidier and put the track in the wrong place for every
// consumer that expects WGS84.
func TestAnEphemerisTrackCrossingTheAntimeridianIsNotNormalised(t *testing.T) {
	payload := example(t, "feasibility.ephemeris.computed.v1", "crosses-the-antimeridian.json")

	got, err := wire.New().Ephemeris(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Samples[0].LongitudeDeg <= 179 || got.Samples[1].LongitudeDeg >= -179 {
		t.Errorf("longitudes %v and %v were normalised across the antimeridian",
			got.Samples[0].LongitudeDeg, got.Samples[1].LongitudeDeg)
	}
}

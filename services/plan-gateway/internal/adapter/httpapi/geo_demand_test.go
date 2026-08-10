package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

const geoWindow = "?window_start=2026-03-01T00:00:00Z&window_end=2026-03-02T00:00:00Z"

func getGeo(t *testing.T, reads *fakeReads, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	serve(t, reads, nil).ServeHTTP(recorder,
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody))
	return recorder
}

func decodeCollection(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decoding GeoJSON: %v\n%s", err, body)
	}
	return doc
}

// A target's geometry passes through verbatim, Point or Polygon.
//
// The read model stores both in one geometry column and PostGIS has already
// serialised it. Re-decoding into a Go type here would mean a union every layer
// above has to discriminate again, and would risk changing coordinates on the
// way through — which is the one thing a geospatial API must never do quietly.
func TestATargetsGeometryPassesThroughUntouched(t *testing.T) {
	point := `{"type":"Point","coordinates":[4.4,51.9]}`
	polygon := `{"type":"Polygon","coordinates":[[[4.0,51.0],[4.1,51.0],[4.1,51.1],[4.0,51.0]]]}`

	reads := &fakeReads{targets: []port.TargetView{
		{RequestID: "r1", CustomerID: "acme", State: "RECEIVED", GeoJSON: []byte(point)},
		{RequestID: "r2", CustomerID: "acme", State: "PLANNED", GeoJSON: []byte(polygon)},
	}}

	recorder := getGeo(t, reads, "/v1/geo/targets"+geoWindow)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body)
	}

	doc := decodeCollection(t, recorder.Body.Bytes())
	features, _ := doc["features"].([]any) //nolint:errcheck // len(nil) is 0 and the next line reports it
	if len(features) != 2 {
		t.Fatalf("features = %d, want 2", len(features))
	}

	kinds := []string{}
	for _, raw := range features {
		feature, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("feature is %T, want an object", raw)
		}
		geometry, ok := feature["geometry"].(map[string]any)
		if !ok {
			t.Fatalf("geometry is %T, want an object", feature["geometry"])
		}
		kind, ok := geometry["type"].(string)
		if !ok {
			t.Fatalf("geometry type is %T, want a string", geometry["type"])
		}
		kinds = append(kinds, kind)
	}
	if kinds[0] != "Point" || kinds[1] != "Polygon" {
		t.Errorf("geometry types = %v, want [Point Polygon] — a density layer that "+
			"dropped polygons would under-report exactly the largest requests", kinds)
	}
}

// An empty result renders as [], never null.
//
// A nil slice marshals to `null`, and deck.gl iterating over null throws where
// iterating over [] is a no-op. The difference never shows up in Go and always
// shows up in the browser.
func TestAnEmptyCollectionRendersAsAnArray(t *testing.T) {
	for _, path := range []string{"/v1/geo/targets", "/v1/geo/opportunities"} {
		t.Run(path, func(t *testing.T) {
			recorder := getGeo(t, &fakeReads{}, path+geoWindow)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			doc := decodeCollection(t, recorder.Body.Bytes())
			features, ok := doc["features"].([]any)
			if !ok {
				t.Fatalf("features is %T, want an array — null throws in deck.gl", doc["features"])
			}
			if len(features) != 0 {
				t.Errorf("features = %d, want 0", len(features))
			}
		})
	}
}

// `awarded: false` must survive serialisation.
//
// It is the field the contention endpoint exists for, and false is the
// INTERESTING value: an omitempty here would drop the flag from exactly the
// features the conflict layer counts, leaving a client unable to tell a loser
// from a winner whose flag was elided.
func TestAwardedFalseIsSerialisedRatherThanOmitted(t *testing.T) {
	reads := &fakeReads{footprints: []port.OpportunityFootprintView{
		{OpportunityID: "o1", RequestID: "r1", SatelliteID: "SENTINEL-1A", Mode: "SCAN",
			Awarded: false, GeoJSON: []byte(`{"type":"Polygon","coordinates":[]}`)},
	}}

	recorder := getGeo(t, reads, "/v1/geo/opportunities"+geoWindow)
	doc := decodeCollection(t, recorder.Body.Bytes())
	features, _ := doc["features"].([]any) //nolint:errcheck // len(nil) is 0 and the next line reports it
	if len(features) != 1 {
		t.Fatalf("features = %d, want 1", len(features))
	}
	feature, ok := features[0].(map[string]any)
	if !ok {
		t.Fatalf("feature is %T, want an object", features[0])
	}
	properties, ok := feature["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties is %T, want an object", feature["properties"])
	}

	awarded, present := properties["awarded"]
	if !present {
		t.Fatal("awarded is absent; a loser is indistinguishable from an elided winner")
	}
	if awarded != false {
		t.Errorf("awarded = %v, want false", awarded)
	}
}

// Absent `awarded` means BOTH, because the ratio is the interesting quantity
// and cannot be computed from one half.
func TestAbsentAwardedFilterAsksForBoth(t *testing.T) {
	reads := &fakeReads{}
	getGeo(t, reads, "/v1/geo/opportunities"+geoWindow)

	if reads.lastFootprintQuery.Awarded != nil {
		t.Errorf("Awarded = %v, want nil — absent must mean both",
			*reads.lastFootprintQuery.Awarded)
	}
}

func TestAwardedFilterIsPassedThrough(t *testing.T) {
	for _, want := range []bool{true, false} {
		reads := &fakeReads{}
		getGeo(t, reads, "/v1/geo/opportunities"+geoWindow+"&awarded="+boolText(want))

		got := reads.lastFootprintQuery.Awarded
		if got == nil || *got != want {
			t.Errorf("awarded=%v produced %v", want, got)
		}
	}
}

// A malformed `awarded` is refused, not ignored.
//
// Ignoring it would return BOTH halves for a query that asked for one, which
// looks like a working request and quietly doubles the answer.
func TestAMalformedAwardedIsRefused(t *testing.T) {
	recorder := getGeo(t, &fakeReads{}, "/v1/geo/opportunities"+geoWindow+"&awarded=maybe")
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
}

// An unknown state is refused for the same reason.
//
// Treated as "no filter" it returns EVERY target, which is the opposite of
// what was asked for and indistinguishable from success.
func TestAnUnknownStateIsRefused(t *testing.T) {
	recorder := getGeo(t, &fakeReads{}, "/v1/geo/targets"+geoWindow+"&state=NOT_A_STATE")
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
}

// A backwards window is refused rather than answered with nothing.
//
// window_end before window_start produces an empty tstzrange, which Postgres
// answers with zero rows — a confident, fast, wrong "nothing here".
func TestABackwardsWindowIsRefused(t *testing.T) {
	backwards := "?window_start=2026-03-02T00:00:00Z&window_end=2026-03-01T00:00:00Z"
	for _, path := range []string{"/v1/geo/targets", "/v1/geo/opportunities"} {
		recorder := getGeo(t, &fakeReads{}, path+backwards)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, recorder.Code)
		}
	}
}

// Truncation is DETECTED, not assumed.
//
// The handler asks for limit+1 so it can tell "exactly limit rows exist" from
// "more were cut". Reporting truncated on an exact fit would make a client
// narrow a window that did not need narrowing.
func TestTruncationIsDetectedRatherThanAssumed(t *testing.T) {
	exactly := make([]port.TargetView, 3)
	for i := range exactly {
		exactly[i] = port.TargetView{
			RequestID: "r", State: "RECEIVED",
			GeoJSON: []byte(`{"type":"Point","coordinates":[0,0]}`),
		}
	}

	reads := &fakeReads{targets: exactly}
	recorder := getGeo(t, reads, "/v1/geo/targets"+geoWindow+"&limit=3")
	doc := decodeCollection(t, recorder.Body.Bytes())

	if doc["truncated"] != false {
		t.Error("truncated on an exact fit; the client would narrow a window that fits")
	}
	if reads.lastTargetQuery.Limit != 4 {
		t.Errorf("asked the read model for %d, want limit+1 so truncation is detectable",
			reads.lastTargetQuery.Limit)
	}
}

// The window filters on the REQUEST'S window, and reaches the query intact.
func TestTheWindowReachesTheReadModel(t *testing.T) {
	reads := &fakeReads{}
	getGeo(t, reads, "/v1/geo/targets"+geoWindow)

	wantStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if !reads.lastTargetQuery.WindowStart.Equal(wantStart) {
		t.Errorf("WindowStart = %s, want %s", reads.lastTargetQuery.WindowStart, wantStart)
	}
}

// Both documents carry staleness, so a client can tell a quiet region from a
// stale read model.
func TestGeoDocumentsCarryStaleness(t *testing.T) {
	for _, path := range []string{"/v1/geo/targets", "/v1/geo/opportunities"} {
		doc := decodeCollection(t, getGeo(t, &fakeReads{}, path+geoWindow).Body.Bytes())
		staleness, ok := doc["staleness"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no staleness block", path)
		}
		if staleness["as_of"] == "" {
			t.Errorf("%s: staleness carries no as_of", path)
		}
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

func getWith(t *testing.T, h http.Handler, target, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const czmlPath = "/v1/geo/plans/SAT-1/2026-03-01T00:00:00Z/czml"

const footprintsPath = "/v1/geo/footprints" +
	"?window_start=2026-03-01T00:00:00Z&window_end=2026-03-02T00:00:00Z"

// TestAMatchingETagGets304WithNoBody is the payload budget's main lever.
//
// A globe polls the same horizon every few seconds and the answer only changes
// when a plan is committed. Without this, every poll re-sends an unchanged
// document.
func TestAMatchingETagGets304WithNoBody(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"czml", czmlPath},
		{"footprints", footprintsPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := serve(t, &fakeReads{plan: populated(), acquisitions: populated().Acquisitions}, nil)

			first := getWith(t, h, tc.path, "")
			if first.Code != http.StatusOK {
				t.Fatalf("first request: %d %s", first.Code, first.Body.String())
			}
			tag := first.Header().Get("ETag")
			if tag == "" {
				t.Fatal("no ETag on the first response; a client has nothing to revalidate with")
			}
			if first.Body.Len() == 0 {
				t.Fatal("the first response has no body")
			}

			second := getWith(t, h, tc.path, tag)
			if second.Code != http.StatusNotModified {
				t.Fatalf("revalidation returned %d, want 304", second.Code)
			}
			if second.Body.Len() != 0 {
				t.Errorf("the 304 carries %d bytes; that defeats the point", second.Body.Len())
			}
			// Repeated so a client that lost its copy recovers without a second
			// round trip.
			if second.Header().Get("ETag") != tag {
				t.Errorf("the 304 reports ETag %q, want %q", second.Header().Get("ETag"), tag)
			}
		})
	}
}

// TestADifferentRenderingGetsADifferentETag is the guard on the guard.
//
// If every response shared one tag the test above would pass and the endpoint
// would serve 304 for content the client has never seen.
func TestADifferentRenderingGetsADifferentETag(t *testing.T) {
	plan := populated()
	h := serve(t, &fakeReads{plan: plan}, nil)
	original := getWith(t, h, czmlPath, "").Header().Get("ETag")

	changed := populated()
	changed.Acquisitions[0].Status = "SUPERSEDED"
	h2 := serve(t, &fakeReads{plan: changed}, nil)
	after := getWith(t, h2, czmlPath, "").Header().Get("ETag")

	if original == after {
		t.Fatalf("a superseded acquisition rendered under the same tag %s; "+
			"clients would never see the change", original)
	}

	// And the stale tag must not satisfy the new content.
	rec := getWith(t, h2, czmlPath, original)
	if rec.Code != http.StatusOK {
		t.Fatalf("a stale tag got %d, want a fresh 200", rec.Code)
	}
}

// TestTwoWindowsOverTheSameDataDoNotShareATag pins that the validator covers
// the RENDERING and not the read model. Different queries are different
// documents.
func TestTwoWindowsOverTheSameDataDoNotShareATag(t *testing.T) {
	reads := &fakeReads{acquisitions: populated().Acquisitions}
	h := serve(t, reads, nil)

	narrow := getWith(t, h, footprintsPath, "").Header().Get("ETag")

	reads.acquisitions = nil // a different window legitimately finds nothing
	wide := getWith(t, h, "/v1/geo/footprints"+
		"?window_start=2026-03-01T00:00:00Z&window_end=2026-03-09T00:00:00Z", "").Header().Get("ETag")

	if narrow == wide {
		t.Fatalf("an empty result shares tag %s with a populated one", narrow)
	}
}

func TestConditionalRequestEdgeCases(t *testing.T) {
	h := serve(t, &fakeReads{plan: populated()}, nil)
	tag := getWith(t, h, czmlPath, "").Header().Get("ETag")

	for _, tc := range []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"exact match", tag, http.StatusNotModified},
		// RFC 9110 §13.1.2: a client may hold several representations.
		{"one of several", `"deadbeef", ` + tag, http.StatusNotModified},
		// A proxy is allowed to weaken a strong tag in transit. Refusing to
		// match then would silently disable the whole mechanism.
		{"weakened by a proxy", "W/" + tag, http.StatusNotModified},
		{"wildcard", "*", http.StatusNotModified},
		{"no match", `"not-the-tag"`, http.StatusOK},
		{"absent", "", http.StatusOK},
		{"garbage", "!!!", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := getWith(t, h, czmlPath, tc.header); rec.Code != tc.wantStatus {
				t.Fatalf("If-None-Match %q gave %d, want %d", tc.header, rec.Code, tc.wantStatus)
			}
		})
	}
}

// TestGeoJSONUsesItsRegisteredMediaType matters because readers dispatch on it,
// and a proxy that "helpfully" transforms application/json will reorder a
// coordinate array.
func TestGeoJSONUsesItsRegisteredMediaType(t *testing.T) {
	h := serve(t, &fakeReads{acquisitions: populated().Acquisitions}, nil)
	rec := getWith(t, h, footprintsPath, "")
	if got := rec.Header().Get("Content-Type"); got != httpapi.MediaTypeGeoJSON {
		t.Errorf("Content-Type = %q, want %q", got, httpapi.MediaTypeGeoJSON)
	}
}

// TestTheFootprintsPageIsBoundedAndSaysSo is the difference between "there is
// no coverage here" and "I stopped counting". A viewport cannot tell them apart.
func TestTheFootprintsPageIsBoundedAndSaysSo(t *testing.T) {
	// Far more than the endpoint's limit, all identical but for their ids.
	many := make([]port.AcquisitionView, 0, 400)
	template := populated().Acquisitions[0]
	for i := range 400 {
		clone := template
		clone.AcquisitionID = string(rune('a'+i%26)) + "-acq"
		many = append(many, clone)
	}
	reads := &fakeReads{acquisitions: many}
	h := serve(t, reads, nil)

	body := decodeBody(t, getWith(t, h, footprintsPath, ""))
	if body["truncated"] != true {
		t.Errorf("truncated = %v with 400 rows behind a 250 limit", body["truncated"])
	}
	features, ok := body["features"].([]any)
	if !ok {
		t.Fatalf("features is %T", body["features"])
	}
	if len(features) > 250 {
		t.Errorf("returned %d features, over the endpoint's own limit", len(features))
	}

	// The handler must ask the read model for one MORE than it will return, or
	// it cannot distinguish "exactly the limit exists" from "more were cut".
	if reads.lastAcqQuery.Limit <= len(features) {
		t.Errorf("queried for %d and returned %d; truncation would be guessed, not detected",
			reads.lastAcqQuery.Limit, len(features))
	}
}

// TestAnExactFitIsNotReportedAsTruncated is the other half. Reporting
// truncation on a page that happens to fill exactly would send a client
// narrowing a window that did not need it.
func TestAnExactFitIsNotReportedAsTruncated(t *testing.T) {
	exact := make([]port.AcquisitionView, 0, 3)
	template := populated().Acquisitions[0]
	for i := range 3 {
		clone := template
		clone.AcquisitionID = string(rune('a'+i)) + "-acq"
		exact = append(exact, clone)
	}
	h := serve(t, &fakeReads{acquisitions: exact}, nil)

	body := decodeBody(t, getWith(t, h, footprintsPath+"&limit=3", ""))
	if body["truncated"] != false {
		t.Errorf("truncated = %v for a page that fits exactly", body["truncated"])
	}
}

// TestUnrenderableGeometryIsNotA503 keeps two very different failures apart.
//
// A footprint that will not parse is a data problem in one plan; the read model
// is fine and every other endpoint still works. Reporting it as unavailable
// would send an operator looking at Postgres.
func TestUnrenderableGeometryIsNotA503(t *testing.T) {
	plan := populated()
	plan.Acquisitions[0].FootprintGeoJSON = []byte(`{"type":"Point","coordinates":[4,51]}`)
	h := serve(t, &fakeReads{plan: plan}, nil)

	rec := getWith(t, h, czmlPath, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if body := decodeBody(t, rec); body["type"] != httpapi.ProblemBase+"unrenderable-geometry" {
		t.Errorf("problem type = %v", body["type"])
	}
}

func TestGeoEndpointsRejectMalformedParameters(t *testing.T) {
	h := serve(t, &fakeReads{plan: populated()}, nil)
	for _, tc := range []struct{ name, path string }{
		{"bad bucket", "/v1/geo/plans/SAT-1/nonsense/czml"},
		{"bad version", czmlPath + "?plan_version=0"},
		{"no window", "/v1/geo/footprints"},
		{"inverted window", "/v1/geo/footprints?window_start=2026-03-02T00:00:00Z&window_end=2026-03-01T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := getWith(t, h, tc.path, ""); rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", rec.Code)
			}
		})
	}
}

func TestAMissingPlanCZMLIs404(t *testing.T) {
	h := serve(t, &fakeReads{err: port.ErrNotFound}, nil)
	if rec := getWith(t, h, czmlPath, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestABrokenReadModelIs503OnTheGeoEndpoints(t *testing.T) {
	h := serve(t, &fakeReads{err: errors.New("connection refused")}, nil)
	for _, path := range []string{czmlPath, footprintsPath} {
		if rec := getWith(t, h, path, ""); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s gave %d, want 503", path, rec.Code)
		}
	}
}

// TestTheCZMLIsLoadableAsAPacketStream is a shape check at the HTTP boundary:
// Cesium expects an array whose first entry is the document packet, and
// anything else fails inside the viewer rather than here.
func TestTheCZMLIsLoadableAsAPacketStream(t *testing.T) {
	h := serve(t, &fakeReads{plan: populated()}, nil)
	rec := getWith(t, h, czmlPath, "")

	var packets []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &packets); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, rec.Body.String())
	}
	if len(packets) == 0 || packets[0]["id"] != "document" {
		t.Fatalf("first packet is not the document packet: %v", packets)
	}
	if packets[0]["version"] != "1.0" {
		t.Errorf("CZML version = %v, want 1.0", packets[0]["version"])
	}
	clock, ok := packets[0]["clock"].(map[string]any)
	if !ok {
		t.Fatal("the document packet has no clock; scrubbing would do nothing")
	}
	if clock["interval"] != "2026-03-01T00:00:00Z/2026-03-01T03:00:00Z" {
		t.Errorf("clock interval = %v, want the bucket", clock["interval"])
	}
}

// TestTheCZMLValidatorTracksThePlanNotTheGlobalCursor.
//
// A CZML document renders ONE plan. Tying its validator to a cursor that moves
// whenever any unrelated stream advances would invalidate it constantly for no
// change in content — the cache would appear to work and never hit.
func TestTheCZMLValidatorTracksThePlanNotTheGlobalCursor(t *testing.T) {
	plan := populated()
	reads := &fakeReads{plan: plan}
	h := serve(t, reads, nil)
	tag := getWith(t, h, czmlPath, "").Header().Get("ETag")

	// The global cursor moves; this plan does not change.
	lastEvent = lastEvent.Add(time.Hour)
	t.Cleanup(func() { lastEvent = lastEvent.Add(-time.Hour) })

	if rec := getWith(t, h, czmlPath, tag); rec.Code != http.StatusNotModified {
		t.Fatalf("an unrelated cursor move invalidated the document (%d)", rec.Code)
	}
}

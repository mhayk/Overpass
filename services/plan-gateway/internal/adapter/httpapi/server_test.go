package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// pinnedNow keeps staleness deterministic. A test that reads the wall clock
// asserts on a number that changes between runs, so it ends up asserting
// nothing.
var pinnedNow = time.Date(2026, 3, 1, 12, 0, 30, 0, time.UTC)

var lastEvent = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// fakeReads returns whatever it is told to, including errors.
type fakeReads struct {
	plans         []port.PlanView
	plan          port.PlanView
	acquisitions  []port.AcquisitionView
	request       port.RequestView
	opportunities []port.OpportunityView
	ephemeris     []port.EphemerisSample
	targets       []port.TargetView
	footprints    []port.OpportunityFootprintView

	lastTargetQuery    port.TargetQuery
	lastFootprintQuery port.OpportunityFootprintQuery
	constellation      map[string][]port.EphemerisSample
	err                error

	lastSatelliteFilter string

	lastPlanQuery port.PlanQuery
	lastAcqQuery  port.AcquisitionQuery
}

func (f *fakeReads) Plans(_ context.Context, q port.PlanQuery) ([]port.PlanView, port.Cursor, error) {
	f.lastPlanQuery = q
	return f.plans, port.Cursor{LastEventAt: lastEvent}, f.err
}

func (f *fakeReads) Plan(_ context.Context, _ string, _ time.Time, _ *int) (port.PlanView, error) {
	return f.plan, f.err
}

func (f *fakeReads) Acquisitions(
	_ context.Context, q port.AcquisitionQuery,
) ([]port.AcquisitionView, port.Cursor, error) {
	f.lastAcqQuery = q
	return f.acquisitions, port.Cursor{LastEventAt: lastEvent}, f.err
}

func (f *fakeReads) Request(_ context.Context, _ string) (port.RequestView, error) {
	return f.request, f.err
}

func (f *fakeReads) RequestOpportunities(
	_ context.Context, _ string,
) ([]port.OpportunityView, port.Cursor, error) {
	return f.opportunities, port.Cursor{LastEventAt: lastEvent}, f.err
}

// Ephemeris returns whatever the fixture holds, including nothing. Nothing is
// the interesting case for the handler: a plan whose bucket the sweep has not
// reached still has to render.
func (f *fakeReads) Ephemeris(
	_ context.Context, _ string, _, _ time.Time,
) ([]port.EphemerisSample, error) {
	return f.ephemeris, f.err
}

func (f *fakeReads) Targets(
	_ context.Context, q port.TargetQuery,
) ([]port.TargetView, port.Cursor, error) {
	f.lastTargetQuery = q
	return f.targets, port.Cursor{LastEventAt: lastEvent}, f.err
}

func (f *fakeReads) OpportunityFootprints(
	_ context.Context, q port.OpportunityFootprintQuery,
) ([]port.OpportunityFootprintView, port.Cursor, error) {
	f.lastFootprintQuery = q
	return f.footprints, port.Cursor{LastEventAt: lastEvent}, f.err
}

func (f *fakeReads) Constellation(
	_ context.Context, satelliteID string, _, _ time.Time,
) (map[string][]port.EphemerisSample, port.Cursor, error) {
	f.lastSatelliteFilter = satelliteID
	return f.constellation, port.Cursor{LastEventAt: lastEvent}, f.err
}

// Generous on purpose. These tests are about routing and rendering, not about
// the deadline; a tight value here would make an unrelated slow CI runner look
// like a read-model failure. The deadline's own behaviour is asserted in
// deadline_test.go, where the timeout is the subject rather than the backdrop.
const testReadTimeout = 30 * time.Second

func serve(t *testing.T, reads port.Reads, health func() error) http.Handler {
	t.Helper()
	if health == nil {
		health = func() error { return nil }
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return httpapi.New(reads, health, func() time.Time { return pinnedNow }, testReadTimeout, log).Routes()
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return body
}

// TestAMissIs404NotA503 is the regression test for a real defect.
//
// The handler originally compared against a sentinel it declared itself, while
// the postgres adapter returned its own. Nothing ever matched, so every "no
// such plan" fell through to the generic error branch and rendered as 503 —
// telling a client the read model was DOWN when it was working perfectly and
// the answer was simply "there isn't one". The sentinel now lives in port so
// both sides name the same value; this test is what keeps them there.
func TestAMissIs404NotA503(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
	}{
		{"plan", "/v1/plans/SAT-1/2026-03-01T00:00:00Z"},
		{"request", "/v1/requests/11111111-1111-4111-8111-111111111111"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := serve(t, &fakeReads{err: port.ErrNotFound}, nil)
			rec := get(t, h, tc.target)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("want 404 for a miss, got %d: %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("want a problem document, got Content-Type %q", ct)
			}
		})
	}
}

// TestABrokenReadModelIs503 is the other half of the pair.
//
// If both a miss and an outage rendered the same way the test above would pass
// for the wrong reason.
func TestABrokenReadModelIs503(t *testing.T) {
	h := serve(t, &fakeReads{err: errors.New("connection refused")}, nil)
	rec := get(t, h, "/v1/plans/SAT-1/2026-03-01T00:00:00Z")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when the read model is unreachable, got %d", rec.Code)
	}
	if body := decodeBody(t, rec); body["type"] != httpapi.ProblemBase+"read-model-unavailable" {
		t.Errorf("want the unavailable problem type, got %v", body["type"])
	}
}

// TestStalenessIsOnEveryCollectionResponse is the acceptance criterion for #26:
// surfaced rather than hidden.
func TestStalenessIsOnEveryCollectionResponse(t *testing.T) {
	reads := &fakeReads{}
	h := serve(t, reads, nil)

	for _, target := range []string{
		"/v1/plans",
		"/v1/acquisitions?window_start=2026-03-01T00:00:00Z&window_end=2026-03-02T00:00:00Z",
		"/v1/requests/11111111-1111-4111-8111-111111111111/opportunities",
	} {
		t.Run(target, func(t *testing.T) {
			rec := get(t, h, target)
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			staleness, ok := decodeBody(t, rec)["staleness"].(map[string]any)
			if !ok {
				t.Fatal("no staleness on the response; the client is left to guess")
			}
			// 12:00:30 pinned minus a 12:00:00 last event.
			if staleness["lag_seconds"] != 30.0 {
				t.Errorf("lag_seconds = %v, want 30", staleness["lag_seconds"])
			}
			if staleness["as_of"] != "2026-03-01T12:00:00Z" {
				t.Errorf("as_of = %v", staleness["as_of"])
			}
		})
	}
}

// TestAnEmptyCollectionIsAnEmptyArray guards a JSON shape, not a Go value.
//
// A nil slice marshals to `null`, and a client that does `for (const p of
// body.items)` throws on null and iterates fine over []. The difference never
// shows up in Go and always shows up in the browser.
func TestAnEmptyCollectionIsAnEmptyArray(t *testing.T) {
	h := serve(t, &fakeReads{}, nil)
	rec := get(t, h, "/v1/plans")
	items, ok := decodeBody(t, rec)["items"].([]any)
	if !ok {
		t.Fatalf("items is not an array: %s", rec.Body.String())
	}
	if len(items) != 0 {
		t.Fatalf("want an empty array, got %d items", len(items))
	}
}

func TestMalformedParametersAreRejectedBeforeTheDatabase(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"bad bucket", "/v1/plans/SAT-1/not-a-timestamp"},
		{"bad plan_version", "/v1/plans/SAT-1/2026-03-01T00:00:00Z?plan_version=0"},
		{"missing window", "/v1/acquisitions"},
		{"inverted window", "/v1/acquisitions?window_start=2026-03-02T00:00:00Z&window_end=2026-03-01T00:00:00Z"},
		{"bad after", "/v1/plans?bucket_start_after=yesterday"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The fake would answer happily; a 400 proves the handler stopped
			// first rather than passing nonsense down to SQL.
			h := serve(t, &fakeReads{}, nil)
			if rec := get(t, h, tc.target); rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestQueryParametersReachTheReadModel stops the handler silently ignoring a
// filter. Accepting `satellite_id` and then returning every satellite is worse
// than rejecting it.
func TestQueryParametersReachTheReadModel(t *testing.T) {
	reads := &fakeReads{}
	h := serve(t, reads, nil)

	get(t, h, "/v1/plans?satellite_id=SAT-7&include_superseded=true&limit=5")
	if reads.lastPlanQuery.SatelliteID != "SAT-7" {
		t.Errorf("satellite_id did not reach the query: %+v", reads.lastPlanQuery)
	}
	if !reads.lastPlanQuery.IncludeSuperseded {
		t.Error("include_superseded=true did not reach the query")
	}
	if reads.lastPlanQuery.Limit != 5 {
		t.Errorf("limit = %d, want 5", reads.lastPlanQuery.Limit)
	}

	get(t, h, "/v1/acquisitions?window_start=2026-03-01T00:00:00Z"+
		"&window_end=2026-03-02T00:00:00Z&status=ACTIVE&status=SUPERSEDED")
	if len(reads.lastAcqQuery.Statuses) != 2 {
		t.Errorf("repeated status did not reach the query: %+v", reads.lastAcqQuery.Statuses)
	}
}

// realPolygon is a closed ring with actual positions.
//
// Not `{"type":"Polygon","coordinates":[]}`, which is what these fixtures
// originally carried. It is valid JSON and valid-looking GeoJSON, and it made
// every geo rendering emit ZERO acquisition entities — so a test asserting that
// two different plans render differently compared one empty document against
// another and could not fail. Found by exactly that test.
var realPolygon = []byte(`{"type":"Polygon","coordinates":[[[4.01,51.9],[4.19,51.9],[4.19,52],[4.01,52],[4.01,51.9]]]}`)

// populated is a plan with everything optional actually set, so the rendering
// paths run against real values rather than zero ones.
func populated() port.PlanView {
	supersedes := "old-plan"
	slew := 12.5
	gap := 40.0
	return port.PlanView{
		PlanID:      "plan-1",
		SatelliteID: "SAT-1",
		BucketStart: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		BucketEnd:   time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC),
		PlanVersion: 2, SupersedesPlanID: &supersedes, Superseded: false,
		Policy:      "GreedyByBid",
		MetricsJSON: []byte(`{"requests_fulfilled":1}`),
		CommittedAt: lastEvent,
		Acquisitions: []port.AcquisitionView{{
			AcquisitionID: "acq-1", PlanID: "plan-1", RequestID: "req-1",
			CustomerID: "cust-1", SatelliteID: "SAT-1", Mode: "STRIPMAP",
			WindowStart:           lastEvent,
			WindowEnd:             lastEvent.Add(8 * time.Second),
			Status:                "ACTIVE",
			FootprintGeoJSON:      realPolygon,
			SlewTimeFromPreviousS: &slew, GapFromPreviousS: &gap,
			AwardedValueCredits: 500,
		}},
	}
}

func populatedOpportunity() port.OpportunityView {
	orbit := 4711
	return port.OpportunityView{
		OpportunityID: "opp-1", SatelliteID: "SAT-1", Mode: "STRIPMAP",
		AccessStart: lastEvent, AccessEnd: lastEvent.Add(10 * time.Minute),
		AcquisitionDurationS: 8, OrbitNumber: &orbit, QualityScore: 0.87,
		FootprintGeoJSON: realPolygon,
		Won:              true,
	}
}

// TestAPopulatedPlanRendersEveryField is the shape contract the web client
// codes against. Rendering was only ever exercised against empty lists before,
// which runs none of it.
func TestAPopulatedPlanRendersEveryField(t *testing.T) {
	h := serve(t, &fakeReads{plan: populated()}, nil)
	body := decodeBody(t, get(t, h, "/v1/plans/SAT-1/2026-03-01T00:00:00Z"))

	for _, key := range []string{
		"plan_id", "satellite_id", "bucket", "plan_version", "superseded",
		"policy", "committed_at", "acquisitions", "supersedes_plan_id",
		"metrics", "staleness",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("%s missing from the plan document", key)
		}
	}

	bucketRange, ok := body["bucket"].(map[string]any)
	if !ok {
		t.Fatal("bucket is not an object")
	}
	// A bare start/end pair rather than two sibling keys, because a range is
	// one thing and splitting it invites half of it being dropped.
	if bucketRange["start"] != "2026-03-01T00:00:00Z" || bucketRange["end"] != "2026-03-01T03:00:00Z" {
		t.Errorf("bucket = %v", bucketRange)
	}

	acqs, ok := body["acquisitions"].([]any)
	if !ok || len(acqs) != 1 {
		t.Fatalf("acquisitions = %v", body["acquisitions"])
	}
	acq, ok := acqs[0].(map[string]any)
	if !ok {
		t.Fatal("acquisition is not an object")
	}
	for _, key := range []string{
		"acquisition_id", "request_id", "customer_id", "satellite_id", "mode",
		"window", "status", "footprint", "awarded_value_credits",
		"slew_time_from_previous_s", "gap_from_previous_s",
	} {
		if _, ok := acq[key]; !ok {
			t.Errorf("%s missing from the acquisition", key)
		}
	}
	// Embedded as an object, not a JSON string. A double-encoded footprint
	// parses fine and renders nothing.
	if _, ok := acq["footprint"].(map[string]any); !ok {
		t.Errorf("footprint is %T, want an embedded object", acq["footprint"])
	}
}

// TestOptionalFieldsAreOmittedRatherThanNulled keeps a first acquisition from
// claiming a slew time of zero seconds, which is a real number and a lie.
func TestOptionalFieldsAreOmittedRatherThanNulled(t *testing.T) {
	plan := populated()
	plan.SupersedesPlanID = nil
	plan.MetricsJSON = nil
	plan.Acquisitions[0].SlewTimeFromPreviousS = nil
	plan.Acquisitions[0].GapFromPreviousS = nil

	h := serve(t, &fakeReads{plan: plan}, nil)
	body := decodeBody(t, get(t, h, "/v1/plans/SAT-1/2026-03-01T00:00:00Z"))

	for _, key := range []string{"supersedes_plan_id", "metrics"} {
		if _, present := body[key]; present {
			t.Errorf("%s is present when it has no value", key)
		}
	}
	acq, ok := body["acquisitions"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatal("acquisition is not an object")
	}
	for _, key := range []string{"slew_time_from_previous_s", "gap_from_previous_s"} {
		if _, present := acq[key]; present {
			t.Errorf("%s is present on the first acquisition, which has no previous", key)
		}
	}
}

func TestRequestAndOpportunityDocumentsCarryTheirFields(t *testing.T) {
	reads := &fakeReads{
		request: port.RequestView{
			RequestID: "req-1", CustomerID: "cust-1", TargetName: "Rotterdam",
			State:       "PLANNED",
			WindowStart: lastEvent, WindowEnd: lastEvent.Add(6 * time.Hour),
			OpportunityCount: 3,
			UnfulfilmentJSON: []byte(`{"reason":"NO_ACCESS"}`),
			LastEventAt:      lastEvent,
		},
		opportunities: []port.OpportunityView{populatedOpportunity()},
	}
	h := serve(t, reads, nil)

	body := decodeBody(t, get(t, h, "/v1/requests/req-1"))
	for _, key := range []string{
		"request_id", "customer_id", "target_name", "state", "window",
		"opportunity_count", "unfulfilment", "staleness",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("%s missing from the request document", key)
		}
	}

	list := decodeBody(t, get(t, h, "/v1/requests/req-1/opportunities"))
	items, ok := list["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %v", list["items"])
	}
	opp, ok := items[0].(map[string]any)
	if !ok {
		t.Fatal("opportunity is not an object")
	}
	// won is the field that makes a plan explainable: the losers are why the
	// winner won.
	if opp["won"] != true {
		t.Errorf("won = %v, want true", opp["won"])
	}
	if opp["orbit_number"] != 4711.0 {
		t.Errorf("orbit_number = %v", opp["orbit_number"])
	}
}

// TestListPlansRendersItsItems covers the list path, which the empty-array test
// deliberately does not.
func TestListPlansRendersItsItems(t *testing.T) {
	h := serve(t, &fakeReads{plans: []port.PlanView{populated(), populated()}}, nil)
	items, ok := decodeBody(t, get(t, h, "/v1/plans"))["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %v, want 2 plans", items)
	}
}

func TestAcquisitionListRendersItsItems(t *testing.T) {
	h := serve(t, &fakeReads{acquisitions: populated().Acquisitions}, nil)
	rec := get(t, h, "/v1/acquisitions?window_start=2026-03-01T00:00:00Z&window_end=2026-03-02T00:00:00Z")
	items, ok := decodeBody(t, rec)["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
}

// TestAnUnavailableReadModelIs503OnEveryEndpoint stops one handler treating a
// broken database as an empty result.
func TestAnUnavailableReadModelIs503OnEveryEndpoint(t *testing.T) {
	h := serve(t, &fakeReads{err: errors.New("connection refused")}, nil)
	for _, target := range []string{
		"/v1/plans",
		"/v1/acquisitions?window_start=2026-03-01T00:00:00Z&window_end=2026-03-02T00:00:00Z",
		"/v1/requests/req-1",
		"/v1/requests/req-1/opportunities",
	} {
		t.Run(target, func(t *testing.T) {
			if rec := get(t, h, target); rec.Code != http.StatusServiceUnavailable {
				t.Errorf("got %d, want 503 — an outage rendered as an empty result", rec.Code)
			}
		})
	}
}

// TestLivenessIgnoresPostgres records a deliberate difference from readiness.
//
// Tying liveness to the database turns a Postgres blip into a restart loop,
// which is how a recoverable outage becomes an unrecoverable one.
func TestLivenessIgnoresPostgres(t *testing.T) {
	down := func() error { return errors.New("postgres is gone") }
	h := serve(t, &fakeReads{}, down)

	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("liveness = %d, want 200 even with Postgres down", rec.Code)
	}
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness = %d, want 503 with Postgres down", rec.Code)
	}
}

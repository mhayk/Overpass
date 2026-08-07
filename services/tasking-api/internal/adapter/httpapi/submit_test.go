package httpapi_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/tasking-api/internal/app"
	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

type fakeStore struct {
	err    error
	saved  int
	lastID string
}

func (f *fakeStore) Save(_ context.Context, req port.StoredRequest, _ port.OutboxEvent) error {
	if f.err != nil {
		return f.err
	}
	f.saved++
	f.lastID = req.RequestID
	return nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var submitNow = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func submitServer(t *testing.T, store *fakeStore) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	health := app.NewHealthService("test", time.Second)
	submitter := app.NewSubmitService(
		store, fixedClock{submitNow}, domain.ConfiguredSensors(), domain.DefaultValidationPolicy(),
	)
	return httpapi.New(health, submitter, logger).Routes()
}

const validBody = `{
  "customer_id": "acme",
  "target_name": "Port of Rotterdam",
  "target": {"type": "Point", "coordinates": [4.4, 51.9]},
  "window": {"start": "2026-08-07T13:00:00Z", "end": "2026-08-08T13:00:00Z"},
  "priority_tier": "COMMERCIAL",
  "bid_credits": 1200,
  "requested_modes": ["STRIPMAP"]
}`

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/tasking-requests",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAValidSubmissionIs202AndIsStored(t *testing.T) {
	store := &fakeStore{}
	rec := post(t, submitServer(t, store), validBody)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202: %s", rec.Code, rec.Body)
	}
	if store.saved != 1 {
		t.Fatalf("stored %d times", store.saved)
	}
	body := decode(t, rec.Body.Bytes())
	if body["state"] != "RECEIVED" {
		t.Fatalf("state is %v", body["state"])
	}
	if body["request_id"] != store.lastID {
		t.Fatalf("the response id %v is not the one stored %q", body["request_id"], store.lastID)
	}
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, store.lastID) {
		t.Fatalf("Location %q does not point at the created request", loc)
	}
}

func TestAValidRequestThatCannotBeStoredIs503AndNot202(t *testing.T) {
	// The single most important behaviour on this endpoint. Acknowledging a
	// request we dropped is unrecoverable: the customer believes an image is
	// coming and nothing in the system knows about it.
	store := &fakeStore{err: errors.New("connection refused")}
	rec := post(t, submitServer(t, store), validBody)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
	if store.saved != 0 {
		t.Fatal("the store reported failure but counted a save")
	}
	if strings.Contains(rec.Body.String(), "RECEIVED") {
		t.Fatal("a failed submission was described as received")
	}
}

func TestMalformedJSONIs400AndNot422(t *testing.T) {
	// 400 is "I could not parse this", 422 is "I understood and refuse". A
	// client cannot tell whether to fix its serialiser or its values otherwise.
	rec := post(t, submitServer(t, &fakeStore{}), `{"customer_id":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type is %q, want application/problem+json", ct)
	}
}

func TestAnUnknownFieldIsRejected(t *testing.T) {
	// A customer who typed "bid_credit" and got a 202 has been told their bid
	// was accepted, at zero.
	body := strings.Replace(validBody, `"bid_credits"`, `"bid_credit"`, 1)
	if rec := post(t, submitServer(t, &fakeStore{}), body); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for an unknown field", rec.Code)
	}
}

func TestTrailingContentIsRejected(t *testing.T) {
	rec := post(t, submitServer(t, &fakeStore{}), validBody+`{"extra":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestSemanticFailuresAre422WithFieldPointers(t *testing.T) {
	// Window inverted: understood perfectly, and refused.
	body := strings.Replace(validBody,
		`"start": "2026-08-07T13:00:00Z", "end": "2026-08-08T13:00:00Z"`,
		`"start": "2026-08-08T13:00:00Z", "end": "2026-08-07T13:00:00Z"`, 1)

	rec := post(t, submitServer(t, &fakeStore{}), body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422: %s", rec.Code, rec.Body)
	}

	problem := decode(t, rec.Body.Bytes())
	if problem["reason_code"] != "WINDOW_INVERTED" {
		t.Fatalf("reason_code is %v", problem["reason_code"])
	}
	errs, ok := problem["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("no field errors: %v", problem)
	}
	first, ok := errs[0].(map[string]any)
	if !ok || first["pointer"] != "/window" {
		t.Fatalf("the pointer does not locate the field: %v", errs[0])
	}
}

func TestEveryFieldErrorIsReportedNotJustTheFirst(t *testing.T) {
	body := `{
	  "customer_id": "",
	  "target_name": "",
	  "target": {"type": "Point", "coordinates": [4.4, 51.9]},
	  "window": {"start": "2026-08-07T13:00:00Z", "end": "2026-08-08T13:00:00Z"},
	  "priority_tier": "COMMERCIAL",
	  "bid_credits": 10,
	  "requested_modes": ["HOLOGRAM"]
	}`
	rec := post(t, submitServer(t, &fakeStore{}), body)
	problem := decode(t, rec.Body.Bytes())
	errs, ok := problem["errors"].([]any)
	if !ok {
		t.Fatalf("no errors array in the problem: %v", problem)
	}
	if len(errs) < 3 {
		t.Fatalf("expected at least 3 field errors, got %v", problem["errors"])
	}
}

func TestAPolygonTargetIsAccepted(t *testing.T) {
	body := strings.Replace(validBody,
		`{"type": "Point", "coordinates": [4.4, 51.9]}`,
		`{"type": "Polygon", "coordinates": [[[4.0,51.9],[4.0,52.0],[4.2,52.0],[4.2,51.9],[4.0,51.9]]]}`, 1)

	if rec := post(t, submitServer(t, &fakeStore{}), body); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202: %s", rec.Code, rec.Body)
	}
}

func TestPolygonHolesAreRefusedRatherThanIgnored(t *testing.T) {
	// Accepting and dropping the hole would image the hole.
	holed := `{"type": "Polygon", "coordinates": [` +
		`[[4.0,51.9],[4.0,52.0],[4.2,52.0],[4.0,51.9]],` +
		`[[4.05,51.95],[4.05,51.96],[4.06,51.96],[4.05,51.95]]]}`
	body := strings.Replace(validBody, `{"type": "Point", "coordinates": [4.4, 51.9]}`, holed, 1)

	if rec := post(t, submitServer(t, &fakeStore{}), body); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for a polygon with a hole", rec.Code)
	}
}

func TestTheProblemCarriesTheCorrelationId(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/tasking-requests",
		strings.NewReader(`not json`))
	req.Header.Set(httpapi.CorrelationHeader, "trace-me")
	rec := httptest.NewRecorder()
	submitServer(t, &fakeStore{}).ServeHTTP(rec, req)

	problem := decode(t, rec.Body.Bytes())
	if problem["correlation_id"] != "trace-me" {
		t.Fatalf("the problem cannot be tied to the request: %v", problem)
	}
}

func TestTheProblemTypeIsAStableURI(t *testing.T) {
	// Clients branch on type, never on title. A title gets reworded; a client
	// keyed on one breaks silently the day someone improves the wording.
	rec := post(t, submitServer(t, &fakeStore{}), `not json`)
	problem := decode(t, rec.Body.Bytes())
	typ, ok := problem["type"].(string)
	if !ok {
		t.Fatalf("the problem has no type: %v", problem)
	}
	if !strings.HasPrefix(typ, httpapi.ProblemBase) {
		t.Fatalf("type %q is not under the problem namespace", typ)
	}
}

func TestAnOversizedBodyIsRefused(t *testing.T) {
	huge := `{"customer_id": "` + strings.Repeat("a", 2<<20) + `"}`
	if rec := post(t, submitServer(t, &fakeStore{}), huge); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for an oversized body", rec.Code)
	}
}

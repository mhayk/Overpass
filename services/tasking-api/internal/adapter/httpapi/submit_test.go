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
	// claims mimics the unique constraint: key -> fingerprint and the request
	// it created. A map is not a substitute for the real constraint under
	// concurrency, which is why the concurrency test runs against Postgres.
	claims map[string]claimRecord
}

type claimRecord struct {
	fingerprint string
	requestID   string
}

func (f *fakeStore) Save(
	_ context.Context, claim port.IdempotencyClaim, req port.StoredRequest, _ port.OutboxEvent,
) (port.Replay, error) {
	if f.err != nil {
		return port.Replay{}, f.err
	}
	if f.claims == nil {
		f.claims = map[string]claimRecord{}
	}
	if existing, taken := f.claims[claim.CustomerID+"/"+claim.Key]; taken {
		if existing.fingerprint != claim.Fingerprint {
			return port.Replay{}, port.ErrIdempotencyConflict
		}
		return port.Replay{
			Replayed: true, RequestID: existing.requestID,
			State: "RECEIVED", SubmittedAt: submitNow,
		}, nil
	}
	f.claims[claim.CustomerID+"/"+claim.Key] = claimRecord{claim.Fingerprint, req.RequestID}
	f.saved++
	f.lastID = req.RequestID
	return port.Replay{}, nil
}

func (f *fakeStore) PurgeExpiredKeys(context.Context, time.Time) (int64, error) { return 0, nil }

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
	return httpapi.New(health, submitter, 5*time.Second, logger).Routes()
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
	return postWithKey(t, h, body, "idem-key-00000001")
}

func postWithKey(t *testing.T, h http.Handler, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/tasking-requests",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
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

func TestTheIdempotencyKeyIsRequired(t *testing.T) {
	// Required, not optional. Optional means the default is unsafe under retry
	// and clients find out in production.
	if rec := postWithKey(t, submitServer(t, &fakeStore{}), validBody, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 with no Idempotency-Key", rec.Code)
	}
}

func TestAMalformedIdempotencyKeyIsRejected(t *testing.T) {
	for _, key := range []string{"short", strings.Repeat("a", 129), "has spaces", "has/slash"} {
		if rec := postWithKey(t, submitServer(t, &fakeStore{}), validBody, key); rec.Code != http.StatusBadRequest {
			t.Errorf("key %q got %d, want 400", key, rec.Code)
		}
	}
}

func TestAnIdenticalRetryReplaysTheOriginalResponse(t *testing.T) {
	store := &fakeStore{}
	server := submitServer(t, store)

	first := postWithKey(t, server, validBody, "retry-key-000001")
	second := postWithKey(t, server, validBody, "retry-key-000001")

	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("got %d then %d, want 202 both times", first.Code, second.Code)
	}
	if store.saved != 1 {
		t.Fatalf("a retry created %d requests", store.saved)
	}
	if decode(t, first.Body.Bytes())["request_id"] != decode(t, second.Body.Bytes())["request_id"] {
		t.Fatal("the retry returned a different request_id")
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("the replay was not declared, so a client cannot tell it from a first submission")
	}
	if first.Header().Get("Idempotency-Replayed") != "" {
		t.Fatal("the first submission was labelled a replay")
	}
}

func TestAReorderedButIdenticalBodyStillReplays(t *testing.T) {
	// Many HTTP clients reserialise before retrying. A digest over raw bytes
	// would turn that into a 409 for a retry that is genuinely identical.
	reordered := `{
	  "requested_modes": ["STRIPMAP"],
	  "bid_credits": 1200,
	  "priority_tier": "COMMERCIAL",
	  "window": {"end": "2026-08-08T13:00:00Z", "start": "2026-08-07T13:00:00Z"},
	  "target": {"coordinates": [4.4, 51.9], "type": "Point"},
	  "target_name": "Port of Rotterdam",
	  "customer_id": "acme"
	}`
	store := &fakeStore{}
	server := submitServer(t, store)

	postWithKey(t, server, validBody, "reorder-key-0001")
	second := postWithKey(t, server, reordered, "reorder-key-0001")

	if second.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202: %s", second.Code, second.Body)
	}
	if store.saved != 1 {
		t.Fatalf("a reordered retry created %d requests", store.saved)
	}
}

func TestTheSameKeyWithADifferentBodyIs409(t *testing.T) {
	// A client bug that must surface. Silently replaying would discard a
	// request the customer believes they submitted.
	store := &fakeStore{}
	server := submitServer(t, store)
	different := strings.Replace(validBody, `"bid_credits": 1200`, `"bid_credits": 9999`, 1)

	postWithKey(t, server, validBody, "conflict-key-001")
	second := postWithKey(t, server, different, "conflict-key-001")

	if second.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", second.Code, second.Body)
	}
	if decode(t, second.Body.Bytes())["reason_code"] != "IDEMPOTENCY_KEY_CONFLICT" {
		t.Fatalf("wrong reason code: %s", second.Body)
	}
	if store.saved != 1 {
		t.Fatalf("the conflicting submission was stored anyway (%d saves)", store.saved)
	}
}

func TestDifferentKeysCreateDifferentRequests(t *testing.T) {
	store := &fakeStore{}
	server := submitServer(t, store)

	postWithKey(t, server, validBody, "key-aaaaaaaa0001")
	postWithKey(t, server, validBody, "key-bbbbbbbb0002")

	if store.saved != 2 {
		t.Fatalf("two distinct keys produced %d requests", store.saved)
	}
}

// blockingStore never answers, which is what an exhausted connection pool looks
// like from the handler: the acquire simply does not return.
type blockingStore struct{ fakeStore }

func (b *blockingStore) Save(
	ctx context.Context, _ port.IdempotencyClaim, _ port.StoredRequest, _ port.OutboxEvent,
) (port.Replay, error) {
	<-ctx.Done()
	return port.Replay{}, ctx.Err()
}

// A submission that cannot reach the database must be REFUSED, not held.
//
// #50's acceptance criterion is that ingress degrades to 503 under pool
// exhaustion. Without a deadline on the submit path the handler waits for a
// connection that is not coming, the client's socket stays open, and the
// symptom is a service that looks alive and answers nothing — the failure mode
// an operator finds hardest to attribute, because every dashboard is green and
// the request count simply stops.
//
// 503 is already the mapping for a failed submission; what was missing is
// something that makes the attempt fail at all.
func TestASubmissionThatCannotReachTheDatabaseIsRefusedNotHeld(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	health := app.NewHealthService("test", time.Second)
	submitter := app.NewSubmitService(
		&blockingStore{}, fixedClock{submitNow}, domain.ConfiguredSensors(), domain.DefaultValidationPolicy(),
	)
	handler := httpapi.New(health, submitter, 200*time.Millisecond, logger).Routes()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- post(t, handler, validBody) }()

	select {
	case rec := <-done:
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 — a request that never reached the database must not be accepted", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never answered; ingress hangs instead of degrading, which is the failure mode #50 asks about")
	}
}

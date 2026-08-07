package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/tasking-api/internal/app"
	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

// The use case, tested without HTTP and without Postgres. That is the layering
// paying for itself: the interesting decisions here — what to do with a replay,
// what to do with a conflict, what NOT to do with a store failure — are all
// exercised against a fake the app layer's own interface permits.

type storeStub struct {
	replay  port.Replay
	err     error
	saves   int
	purged  int64
	lastReq port.StoredRequest
}

func (s *storeStub) Save(
	_ context.Context, _ port.IdempotencyClaim, req port.StoredRequest, _ port.OutboxEvent,
) (port.Replay, error) {
	s.saves++
	s.lastReq = req
	return s.replay, s.err
}

func (s *storeStub) PurgeExpiredKeys(context.Context, time.Time) (int64, error) {
	return s.purged, nil
}

type stubClock struct{ t time.Time }

func (c stubClock) Now() time.Time { return c.t }

var appNow = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func service(store *storeStub) *app.SubmitService {
	return app.NewSubmitService(
		store, stubClock{appNow}, domain.ConfiguredSensors(), domain.DefaultValidationPolicy(),
	)
}

func goodRequest() domain.SubmitRequest {
	return domain.SubmitRequest{
		CustomerID:     "acme",
		TargetName:     "target",
		Target:         domain.Target{Kind: domain.TargetPoint, Point: domain.Position{Lon: 4.4, Lat: 51.9}},
		WindowStart:    appNow.Add(time.Hour),
		WindowEnd:      appNow.Add(25 * time.Hour),
		PriorityTier:   "COMMERCIAL",
		BidCredits:     100,
		RequestedModes: []string{"STRIPMAP"},
	}
}

func TestAValidSubmissionIsStoredAndReported(t *testing.T) {
	store := &storeStub{}
	result, validation, err := service(store).Submit(t.Context(), goodRequest(), "key-00000001", domain.Fingerprint{})

	if err != nil || !validation.OK() {
		t.Fatalf("unexpected failure: err=%v validation=%+v", err, validation.Errors)
	}
	if store.saves != 1 {
		t.Fatalf("stored %d times", store.saves)
	}
	if result.RequestID == "" || result.State != "RECEIVED" || result.Replayed {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestValidationFailureNeverReachesTheStore(t *testing.T) {
	// Cheap and local, and it short-circuits. A request that cannot be accepted
	// must not cost a database round trip.
	bad := goodRequest()
	bad.WindowStart, bad.WindowEnd = bad.WindowEnd, bad.WindowStart

	store := &storeStub{}
	_, validation, err := service(store).Submit(t.Context(), bad, "key-00000001", domain.Fingerprint{})

	if err != nil {
		t.Fatalf("validation failure surfaced as an error: %v", err)
	}
	if validation.OK() {
		t.Fatal("an inverted window was accepted")
	}
	if store.saves != 0 {
		t.Fatal("an invalid request reached the store")
	}
}

func TestAReplayIsReportedAsSuchWithTheOriginalIdentity(t *testing.T) {
	store := &storeStub{replay: port.Replay{
		Replayed: true, RequestID: "original-id", State: "PLANNED",
		SubmittedAt: appNow.Add(-time.Hour),
	}}

	result, _, err := service(store).Submit(t.Context(), goodRequest(), "key-00000001", domain.Fingerprint{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Replayed {
		t.Fatal("a replay was not reported as one")
	}
	if result.RequestID != "original-id" {
		t.Fatalf("the replay returned %q, not the original id", result.RequestID)
	}
	// The ORIGINAL state and time, not fresh ones. A replay that reported
	// RECEIVED for a request already planned would tell the customer their
	// request had been reset.
	if result.State != "PLANNED" || !result.SubmittedAt.Equal(appNow.Add(-time.Hour)) {
		t.Fatalf("the replay invented a new state or time: %+v", result)
	}
}

func TestAConflictIsPropagatedAndNotWrapped(t *testing.T) {
	// The handler branches on this to return 409, so it must survive intact
	// rather than being folded into "could not persist".
	store := &storeStub{err: port.ErrIdempotencyConflict}
	_, _, err := service(store).Submit(t.Context(), goodRequest(), "key-00000001", domain.Fingerprint{})

	if !errors.Is(err, port.ErrIdempotencyConflict) {
		t.Fatalf("got %v, want ErrIdempotencyConflict", err)
	}
	if errors.Is(err, app.ErrNotPersisted) {
		t.Fatal("a conflict was reported as a persistence failure, which would become a 503")
	}
}

func TestAStoreFailureBecomesErrNotPersisted(t *testing.T) {
	// And it must, because the handler turns exactly this into a 503 rather
	// than a 202. Acknowledging a dropped request is unrecoverable.
	store := &storeStub{err: errors.New("connection refused")}
	_, _, err := service(store).Submit(t.Context(), goodRequest(), "key-00000001", domain.Fingerprint{})

	if !errors.Is(err, app.ErrNotPersisted) {
		t.Fatalf("got %v, want ErrNotPersisted", err)
	}
	// The cause survives, so the log line says why.
	if err.Error() == app.ErrNotPersisted.Error() {
		t.Fatal("the underlying cause was discarded")
	}
}

func TestTheStoredRequestCarriesTheGeometryAsWKT(t *testing.T) {
	store := &storeStub{}
	req := goodRequest()
	if _, _, err := service(store).Submit(t.Context(), req, "key-00000001", domain.Fingerprint{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Longitude first. A swap here relocates the target to another hemisphere
	// and every downstream answer is confidently wrong.
	if store.lastReq.TargetWKT != "POINT(4.4 51.9)" {
		t.Fatalf("WKT is %q", store.lastReq.TargetWKT)
	}
}

func TestTheClaimExpiryIsTheTTLFromNow(t *testing.T) {
	// Checked through the store, because the claim is what the store sees.
	captured := &claimCapturingStore{}
	svc := app.NewSubmitService(captured, stubClock{appNow}, domain.ConfiguredSensors(),
		domain.DefaultValidationPolicy())

	if _, _, err := svc.Submit(t.Context(), goodRequest(), "key-00000001", domain.Fingerprint{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := appNow.Add(app.KeyTTL)
	if !captured.claim.ExpiresAt.Equal(want) {
		t.Fatalf("expiry is %v, want %v", captured.claim.ExpiresAt, want)
	}
	if captured.claim.Key != "key-00000001" {
		t.Fatalf("the key was not passed through: %q", captured.claim.Key)
	}
}

type claimCapturingStore struct {
	claim port.IdempotencyClaim
}

func (s *claimCapturingStore) Save(
	_ context.Context, claim port.IdempotencyClaim, _ port.StoredRequest, _ port.OutboxEvent,
) (port.Replay, error) {
	s.claim = claim
	return port.Replay{}, nil
}

func (s *claimCapturingStore) PurgeExpiredKeys(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func TestPurgeDelegatesWithTheServiceClock(t *testing.T) {
	store := &storeStub{purged: 7}
	got, err := service(store).PurgeExpiredKeys(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 7 {
		t.Fatalf("purged %d, want 7", got)
	}
}

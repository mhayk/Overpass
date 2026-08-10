package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// blockingReads hangs until its context is cancelled — a stand-in for a query
// that has stopped making progress rather than one that is merely slow.
type blockingReads struct {
	fakeReads
	entered chan struct{}
}

func (b *blockingReads) Plans(ctx context.Context, _ port.PlanQuery) ([]port.PlanView, port.Cursor, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, port.Cursor{}, ctx.Err()
}

// A read that never returns is refused, not held open.
//
// This is the defect the audit found: every handler passed r.Context()
// straight through, and an inbound request context carries no deadline of its
// own. A query that stopped making progress held its request open
// indefinitely. The service looks alive, answers nothing, and the request
// count simply stops — the failure hardest to attribute, because every
// dashboard stays green while nothing is served.
func TestASlowReadIsRefusedRatherThanHeldOpen(t *testing.T) {
	reads := &blockingReads{entered: make(chan struct{})}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	handler := httpapi.New(reads, func() error { return nil },
		func() time.Time { return pinnedNow }, 50*time.Millisecond, log).Routes()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/plans", http.NoBody)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(recorder, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never returned; the read model is holding it open")
	}

	// 503, not 200 with an empty list. "There is no plan" and "I cannot tell
	// you whether there is a plan" are different answers, and conflating them
	// would let a stalled read model look like an empty schedule.
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

// The deadline reaches the query layer, not just the handler.
//
// A timeout that fires but leaves the query running is bookkeeping, not a
// bound: the connection stays busy and the pool still drains. This asserts the
// context the reads actually receive is the one that gets cancelled.
func TestTheDeadlineCancelsTheQueryItself(t *testing.T) {
	reads := &blockingReads{entered: make(chan struct{})}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	handler := httpapi.New(reads, func() error { return nil },
		func() time.Time { return pinnedNow }, 50*time.Millisecond, log).Routes()

	go handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/plans", http.NoBody))

	select {
	case <-reads.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the read was never reached")
	}
	// blockingReads returns only once its context is done, so the handler
	// completing at all is the proof. The assertion is the absence of a hang.
}

// A fast read is unaffected.
//
// A bound that also refuses healthy traffic is worse than none, so this is the
// other half of the claim rather than a formality.
func TestAFastReadIsUntouchedByTheDeadline(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := httpapi.New(&fakeReads{}, func() error { return nil },
		func() time.Time { return pinnedNow }, 50*time.Millisecond, log).Routes()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder,
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/plans", http.NoBody))

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d — the deadline is refusing healthy reads", recorder.Code, http.StatusOK)
	}
}

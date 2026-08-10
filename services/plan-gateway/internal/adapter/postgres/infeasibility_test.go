package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// The SQL is the thing under test, so these run against a real Postgres for
// projection_test.go's reasons. The interesting mistake here is not in Go: it
// is a CASE arm in the wrong order, which would compile, pass a unit test
// against a fake, and quietly resurrect a terminal state.

func failed(requestID string, at time.Time) port.FeasibilityFailed {
	return port.FeasibilityFailed{
		EventAt:    at,
		RequestID:  requestID,
		Retryable:  false,
		ReasonJSON: []byte(`{"reason_code":"NO_ACCESS_IN_HORIZON","retryable":false,"satellites_evaluated":9}`),
	}
}

func freshProjection(t *testing.T) (*postgres.Projection, fixture) {
	t.Helper()
	pr := postgres.NewProjection(pool(t))
	f := newFixture()
	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(func() {
		if err := pr.Reset(context.Background()); err != nil {
			t.Errorf("reset failed; later tests will see this test's rows: %v", err)
		}
	})
	if err := pr.ProjectRequestReceived(t.Context(), f.received()); err != nil {
		t.Fatalf("request: %v", err)
	}
	return pr, f
}

func stateOf(t *testing.T, requestID string) (string, []byte) {
	t.Helper()
	reads := postgres.NewReads(pool(t))
	view, err := reads.Request(t.Context(), requestID)
	if err != nil {
		t.Fatalf("reading request: %v", err)
	}
	return view.State, view.InfeasibilityJSON
}

// THE DEFECT, AT THE LAYER THAT HELD IT. A refused request reads as INFEASIBLE.
//
// Before #207 it read as RECEIVED forever: the event was published, delivered
// and consumed, and the projector had no case for it. Nothing errored.
func TestAFailedRequestReadsAsInfeasible(t *testing.T) {
	pr, f := freshProjection(t)

	if err := pr.ProjectFeasibilityFailed(t.Context(), failed(f.requestID, epoch.Add(time.Minute))); err != nil {
		t.Fatalf("projecting failure: %v", err)
	}

	state, reason := stateOf(t, f.requestID)
	if state != "INFEASIBLE" {
		t.Errorf("state = %q, want INFEASIBLE", state)
	}
	if len(reason) == 0 {
		t.Error("no reason stored; INFEASIBLE with no explanation is not an answer")
	}
}

// A TERMINAL DECISION MUST SURVIVE THE DERIVATION THAT RUNS AFTER IT.
//
// reconcileRequest recomputes state from the rows that exist, on EVERY event
// touching the request. If INFEASIBLE were assigned rather than derived from
// the stored fact, the very next event would recompute it back to RECEIVED —
// and the bug would look fixed in a unit test and be broken in the stack.
//
// Opportunities arriving after a refusal is not hypothetical: the two streams
// race by design, and a redelivery can put them in any order.
func TestAnInfeasibleRequestIsNotResurrectedByALaterEvent(t *testing.T) {
	pr, f := freshProjection(t)

	if err := pr.ProjectFeasibilityFailed(t.Context(), failed(f.requestID, epoch.Add(time.Minute))); err != nil {
		t.Fatalf("projecting failure: %v", err)
	}
	// Anything that triggers a reconcile. Opportunities are the most awkward
	// case, because the derivation would otherwise read them as
	// AWAITING_PLANNING.
	if err := pr.ProjectOpportunities(t.Context(), f.opportunities()); err != nil {
		t.Fatalf("projecting opportunities: %v", err)
	}

	if state, _ := stateOf(t, f.requestID); state != "INFEASIBLE" {
		t.Errorf("state = %q after a later event; a terminal decision was overwritten by a derivation", state)
	}
}

// An untouched request is unaffected — the guard must not paint everything
// INFEASIBLE.
func TestARequestWithNoFailureKeepsItsDerivedState(t *testing.T) {
	pr, f := freshProjection(t)

	if err := pr.ProjectOpportunities(t.Context(), f.opportunities()); err != nil {
		t.Fatalf("projecting opportunities: %v", err)
	}

	state, reason := stateOf(t, f.requestID)
	if state != "AWAITING_PLANNING" {
		t.Errorf("state = %q, want AWAITING_PLANNING", state)
	}
	if len(reason) != 0 {
		t.Errorf("infeasibility = %s on a request that never failed", reason)
	}
}

// An older redelivery must not overwrite a newer verdict, matching the guard
// unfulfilment already carries.
func TestAnOlderFailureDoesNotOverwriteANewerOne(t *testing.T) {
	pr, f := freshProjection(t)

	newer := failed(f.requestID, epoch.Add(time.Hour))
	newer.ReasonJSON = []byte(`{"reason_code":"CONSTRAINTS_TOO_NARROW","retryable":false}`)
	if err := pr.ProjectFeasibilityFailed(t.Context(), newer); err != nil {
		t.Fatalf("projecting newer failure: %v", err)
	}

	older := failed(f.requestID, epoch.Add(time.Minute))
	if err := pr.ProjectFeasibilityFailed(t.Context(), older); err != nil {
		t.Fatalf("projecting older failure: %v", err)
	}

	_, reason := stateOf(t, f.requestID)
	if got := string(reason); !strings.Contains(got, "CONSTRAINTS_TOO_NARROW") {
		t.Errorf("reason = %s; a redelivered older verdict overwrote the newer one", got)
	}
}

package domain_test

import (
	"testing"

	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
)

// The domain tests import nothing but the domain. That is the property the
// layering exists to protect, and running them proves it holds rather than
// merely asserting it.

func TestReadinessWithNoChecksIsOK(t *testing.T) {
	if got := (domain.Readiness{}).Status(); got != domain.StatusOK {
		t.Fatalf("got %q, want %q", got, domain.StatusOK)
	}
}

func TestOneFailedCheckMakesTheWholeServiceUnavailable(t *testing.T) {
	// Not "degraded". Readiness is a binary question an orchestrator asks
	// before sending traffic, and "degraded" is read as yes.
	r := domain.Readiness{Checks: []domain.Check{
		{Name: "postgres", Healthy: false, Detail: "connection refused"},
		{Name: "other", Healthy: true},
	}}
	if r.Ready() {
		t.Fatal("a service that cannot reach Postgres reported itself ready")
	}
	if got := r.Status(); got != domain.StatusUnavailable {
		t.Fatalf("got %q, want %q", got, domain.StatusUnavailable)
	}
}

func TestAllHealthyIsReady(t *testing.T) {
	r := domain.Readiness{Checks: []domain.Check{
		{Name: "postgres", Healthy: true, LatencyMS: 1.2},
	}}
	if !r.Ready() {
		t.Fatal("all checks healthy but not ready")
	}
}

func TestStatusDoesNotDependOnCheckOrder(t *testing.T) {
	failing := domain.Check{Name: "postgres", Healthy: false}
	passing := domain.Check{Name: "other", Healthy: true}

	first := domain.Readiness{Checks: []domain.Check{failing, passing}}
	second := domain.Readiness{Checks: []domain.Check{passing, failing}}

	if first.Status() != second.Status() {
		t.Fatalf("order changed the answer: %q vs %q", first.Status(), second.Status())
	}
}

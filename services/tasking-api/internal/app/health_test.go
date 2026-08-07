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

type probe struct {
	name  string
	err   error
	delay time.Duration
	calls int
}

func (p *probe) Name() string { return p.name }

func (p *probe) Check(ctx context.Context) error {
	p.calls++
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.err
}

func TestLivenessNeverConsultsADependency(t *testing.T) {
	// The probe below would fail if it were called. Liveness must not call it:
	// a liveness probe that fails on a dependency outage causes a restart loop.
	p := &probe{name: "postgres", err: errors.New("down")}
	svc := app.NewHealthService("v1", time.Second, p)

	if got := svc.Live(); got != domain.StatusOK {
		t.Fatalf("got %q, want ok", got)
	}
	if p.calls != 0 {
		t.Fatalf("liveness consulted a dependency %d times", p.calls)
	}
}

func TestReadinessConsultsEveryProbe(t *testing.T) {
	a := &probe{name: "postgres"}
	b := &probe{name: "other"}
	svc := app.NewHealthService("v1", time.Second, a, b)

	result := svc.Ready(t.Context())
	if len(result.Checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(result.Checks))
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("probes called %d and %d times", a.calls, b.calls)
	}
}

func TestReadinessReportsTheFailureDetail(t *testing.T) {
	svc := app.NewHealthService("v1", time.Second, &probe{name: "postgres", err: errors.New("connection refused")})

	result := svc.Ready(t.Context())
	if result.Ready() {
		t.Fatal("reported ready with a failing dependency")
	}
	if result.Checks[0].Detail == "" {
		t.Fatal("the reason was not recorded, so /readyz cannot explain itself")
	}
}

func TestASlowProbeIsBoundedByTheTimeout(t *testing.T) {
	// A readiness probe that can hang turns a slow dependency into an
	// unresponsive service, and an orchestrator waiting on it never gets the
	// 503 that would take this instance out of rotation.
	svc := app.NewHealthService("v1", 50*time.Millisecond, &probe{name: "slow", delay: 5 * time.Second})

	started := time.Now()
	result := svc.Ready(t.Context())
	elapsed := time.Since(started)

	if elapsed > time.Second {
		t.Fatalf("readiness took %v; the timeout did not bound it", elapsed)
	}
	if result.Ready() {
		t.Fatal("a probe that timed out was treated as healthy")
	}
}

func TestLatencyIsRecorded(t *testing.T) {
	svc := app.NewHealthService("v1", time.Second, &probe{name: "postgres", delay: 10 * time.Millisecond})

	result := svc.Ready(t.Context())
	if result.Checks[0].LatencyMS < 5 {
		t.Fatalf("latency recorded as %vms for a 10ms probe", result.Checks[0].LatencyMS)
	}
}

func TestNoProbesMeansReady(t *testing.T) {
	svc := app.NewHealthService("v1", time.Second)
	if !svc.Ready(t.Context()).Ready() {
		t.Fatal("a service with no dependencies reported itself not ready")
	}
}

func TestVersionIsReported(t *testing.T) {
	if got := app.NewHealthService("v1.2.3", time.Second).Version(); got != "v1.2.3" {
		t.Fatalf("got %q", got)
	}
}

var _ port.DependencyProbe = (*probe)(nil)

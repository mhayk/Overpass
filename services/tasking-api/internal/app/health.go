// Package app holds the use cases. It depends on domain and port, never on
// adapter.
package app

import (
	"context"
	"time"

	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

// HealthService answers the liveness and readiness questions.
type HealthService struct {
	probes  []port.DependencyProbe
	timeout time.Duration
	version string
}

// NewHealthService wires the probes readiness will consult.
func NewHealthService(version string, timeout time.Duration, probes ...port.DependencyProbe) *HealthService {
	return &HealthService{probes: probes, timeout: timeout, version: version}
}

// Version reports the build this process is running.
func (s *HealthService) Version() string { return s.version }

// Live answers the liveness probe.
//
// Checks nothing external, on purpose. A liveness probe that fails on a
// dependency outage causes a restart loop, and a restart loop during an outage
// makes the outage worse.
func (s *HealthService) Live() domain.Status { return domain.StatusOK }

// Ready consults every probe and aggregates the result.
//
// Probes run sequentially against a shared deadline rather than concurrently.
// There are two of them at most, and a sequential loop is one fewer thing to
// reason about than a fan-out whose cancellation semantics somebody has to
// check.
func (s *HealthService) Ready(ctx context.Context) domain.Readiness {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	checks := make([]domain.Check, 0, len(s.probes))
	for _, probe := range s.probes {
		started := time.Now()
		err := probe.Check(ctx)
		check := domain.Check{
			Name:      probe.Name(),
			Healthy:   err == nil,
			LatencyMS: float64(time.Since(started).Microseconds()) / 1000.0,
		}
		if err != nil {
			check.Detail = err.Error()
		}
		checks = append(checks, check)
	}
	return domain.Readiness{Checks: checks}
}

// Package domain holds the business rules, and imports nothing from adapter.
//
// The rule is enforced by internal/arch, not by convention. What it buys is
// specific and worth stating plainly: the state machine and, later, the
// allocation logic are testable without Postgres or NATS, which is what makes
// property-based testing practical at all. "Clean architecture" is not the
// justification.
package domain

// Status is the health of a component or of the service as a whole.
// Mirrors the HealthStatus enum in the OpenAPI document.
type Status string

const (
	StatusOK          Status = "ok"
	StatusDegraded    Status = "degraded"
	StatusUnavailable Status = "unavailable"
)

// Check is one dependency's contribution to readiness.
type Check struct {
	Name      string
	Healthy   bool
	Detail    string
	LatencyMS float64
}

// Readiness aggregates dependency checks into one answer.
//
// A pure function of its inputs, so the aggregation rule is testable without
// standing up anything it describes.
type Readiness struct {
	Checks []Check
}

// Status is unavailable if ANY required dependency is down.
//
// Not "degraded": readiness is a binary question the orchestrator asks before
// sending traffic, and answering "degraded" to it means answering "yes". A
// request this service cannot persist must not be accepted, because returning
// 202 for a request we dropped is the worst thing it could do.
func (r Readiness) Status() Status {
	if len(r.Checks) == 0 {
		return StatusOK
	}
	for _, c := range r.Checks {
		if !c.Healthy {
			return StatusUnavailable
		}
	}
	return StatusOK
}

// Ready reports whether traffic should be accepted.
func (r Readiness) Ready() bool { return r.Status() == StatusOK }

// Package domain holds the projection rules, and imports nothing from adapter.
//
// What that buys here specifically: the ordering guards and the staleness
// calculation are the two things most likely to be subtly wrong, and both are
// pure functions of their inputs. Neither needs Postgres, NATS, or a clock that
// moves.
package domain

import "time"

// Staleness is how current an answer is.
//
// On every projection response, because a read model that cannot say how far
// behind it is forces the UI to guess — and the UI guesses optimistically,
// showing a plan as current when it is already superseded.
type Staleness struct {
	AsOf       time.Time
	LagSeconds float64
}

// StalenessAt computes lag from the last folded event.
//
// `now` is passed rather than read, so a test can pin it and so a replay
// produces the same answer twice.
func StalenessAt(lastEventAt, now time.Time) Staleness {
	lag := now.Sub(lastEventAt).Seconds()
	if lag < 0 {
		// A negative lag means the last event is stamped in the future —
		// clock skew between services, not a bug here. Reporting a negative
		// number would be reporting a projection that is ahead of reality.
		lag = 0
	}
	return Staleness{AsOf: lastEventAt, LagSeconds: lag}
}

// PlanKey identifies the bucket a plan version belongs to.
type PlanKey struct {
	SatelliteID string
	BucketStart time.Time
}

// VersionDecision is what to do with an arriving plan.
type VersionDecision int

const (
	// ApplyAsCurrent means this version is newer than anything seen.
	ApplyAsCurrent VersionDecision = iota
	// ApplyAsSuperseded means it is valid history arriving after its successor.
	// Stored, but not marked current — the retained history from ADR-0012 is
	// only reachable if late arrivals are kept rather than dropped.
	ApplyAsSuperseded
	// Ignore means it has already been folded.
	Ignore
)

// DecidePlanVersion resolves a plan arriving against what is already known.
//
// Version, not arrival order. JetStream orders per subject, consumers run
// concurrently, and redelivery is out of band — so a lower plan_version
// arriving after a higher one is stale, and the contract says consumers must
// drop it as such. "Drop" here means "do not make it current", not "discard":
// the history is worth keeping and a read can name it.
func DecidePlanVersion(arriving, knownCurrent int) VersionDecision {
	switch {
	case knownCurrent == 0:
		return ApplyAsCurrent
	case arriving > knownCurrent:
		return ApplyAsCurrent
	case arriving == knownCurrent:
		return Ignore
	default:
		return ApplyAsSuperseded
	}
}

// ShouldApplyEvent guards a projection update on when the event happened.
//
// Strictly older events are ignored. Equal timestamps apply, for the same
// reason the request state machine accepts them: clock resolution can collapse
// two genuinely distinct events onto one instant, and dropping the second would
// lose a real update.
func ShouldApplyEvent(eventAt, lastAppliedAt time.Time) bool {
	return !eventAt.Before(lastAppliedAt)
}

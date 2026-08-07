// Package port declares the interfaces the application layer depends on.
//
// Declared HERE, next to the code that uses them, rather than next to the
// implementations. That direction is the whole point of the layout: app depends
// on an interface it owns, and adapter satisfies it, so the dependency arrow
// points inward and Postgres can be swapped for a fake in a test without the
// application layer knowing.
package port

import (
	"context"
	"time"
)

// DependencyProbe reports whether one external dependency is usable.
type DependencyProbe interface {
	// Name identifies the dependency in the readiness response.
	Name() string
	// Check returns nil when the dependency is usable. The context carries the
	// timeout — a readiness probe that can hang is a readiness probe that turns
	// a slow dependency into an unresponsive service.
	Check(ctx context.Context) error
}

// Clock exists so that anything time-dependent can be tested without sleeping.
type Clock interface {
	Now() time.Time
}

// SubmissionStore persists a tasking request and its outbox event atomically.
//
// One method, and it takes both, because they must commit together. An
// interface with Save and Publish as separate calls is an interface that
// permits the dual-write problem — the adapter could satisfy it correctly, but
// nothing would stop the next one from not.
type SubmissionStore interface {
	Save(ctx context.Context, req StoredRequest, event OutboxEvent) error
}

// StoredRequest is what the write model needs, already validated.
type StoredRequest struct {
	RequestID       string
	CustomerID      string
	TargetName      string
	TargetWKT       string
	WindowStart     time.Time
	WindowEnd       time.Time
	PriorityTier    string
	BidCredits      int64
	RequestedModes  []string
	ConstraintsJSON []byte
	SubmittedAt     time.Time
}

// OutboxEvent is the event that must exist if and only if the request does.
type OutboxEvent struct {
	EventID       string
	EventType     string
	SchemaVersion string
	Subject       string
	PayloadJSON   []byte
	HeadersJSON   []byte
	OccurredAt    time.Time
}

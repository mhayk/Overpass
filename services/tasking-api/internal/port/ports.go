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
	"errors"
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

// SubmissionStore persists a tasking request, its idempotency claim, and its
// outbox event — atomically.
//
// One method taking all three, because they must commit together. Separate
// Save and Publish calls would permit the dual-write problem; separate Claim
// and Save calls would leave a crash window in which the key exists and the
// request does not, permanently swallowing that submission.
type SubmissionStore interface {
	// Save returns Replay when the key has already been used with the SAME
	// body, and ErrIdempotencyConflict when it was used with a different one.
	Save(ctx context.Context, claim IdempotencyClaim, req StoredRequest, event OutboxEvent) (Replay, error)

	// PurgeExpiredKeys removes claims past their expiry. Returns how many.
	PurgeExpiredKeys(ctx context.Context, now time.Time) (int64, error)
}

// IdempotencyClaim is the client's key and the fingerprint of what it sent.
type IdempotencyClaim struct {
	CustomerID  string
	Key         string
	Fingerprint string
	ExpiresAt   time.Time
}

// Replay describes an earlier response being served again.
type Replay struct {
	Replayed    bool
	RequestID   string
	State       string
	SubmittedAt time.Time
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

// ErrIdempotencyConflict means the key was reused with a different body.
//
// A client bug, and it must surface. Silently treating it as a replay discards
// a request the customer believes they submitted, and they find out when the
// image never arrives.
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different body")

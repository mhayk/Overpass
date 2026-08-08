package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

// ErrNotPersisted means the request was valid but could not be stored.
//
// Distinct from a validation failure because the response is different in the
// way that matters: the client should retry this one. Acknowledging a request
// we dropped is unrecoverable business damage; a 503 the client retries is not.
var ErrNotPersisted = errors.New("request could not be persisted")

// SubmitResult is what the handler needs to build a 202.
type SubmitResult struct {
	RequestID   string
	State       string
	SubmittedAt time.Time
	// Replayed is true when this response came from a stored idempotency claim
	// rather than from work done now. The header the contract defines exists so
	// a client can tell the difference; without it a retry looks identical to a
	// first submission and nobody can debug a double charge.
	Replayed bool
}

// KeyTTL is how long an idempotency claim is honoured.
//
// 24 hours, matching the contract. Long enough for any retry a sane client
// makes, short enough that the table does not grow without bound. Past it a
// repeat is a new submission, which is the right answer — a client retrying a
// day later has almost certainly given up and started again.
const KeyTTL = 24 * time.Hour

// SubmitService accepts tasking requests.
type SubmitService struct {
	store   port.SubmissionStore
	clock   port.Clock
	sensors []domain.SensorCapability
	policy  domain.ValidationPolicy
	newID   func() string
}

// NewSubmitService wires the use case.
func NewSubmitService(
	store port.SubmissionStore,
	clock port.Clock,
	sensors []domain.SensorCapability,
	policy domain.ValidationPolicy,
) *SubmitService {
	return &SubmitService{
		store:   store,
		clock:   clock,
		sensors: sensors,
		policy:  policy,
		newID:   uuid.NewString,
	}
}

// Submit validates and persists.
//
// Returns the validation result when it fails, so the handler can render every
// field error rather than the first. A valid request that cannot be stored
// returns ErrNotPersisted and NOT a result — there is no partial success here.
func (s *SubmitService) Submit(
	ctx context.Context,
	req domain.SubmitRequest,
	key string,
	fingerprint domain.Fingerprint,
	traceHeaders map[string]string,
) (SubmitResult, domain.ValidationResult, error) {
	now := s.clock.Now()

	if result := domain.Validate(req, now, s.sensors, s.policy); !result.OK() {
		return SubmitResult{}, result, nil
	}

	requestID := s.newID()
	eventID := s.newID()

	// The correlation id travels in BOTH the envelope and the NATS headers, and
	// that is not redundant: the envelope is what a consumer reads after the
	// message is stored and replayed, and the header is what it reads while
	// deciding whether to bother. The schema requires it in the envelope.
	payload, err := buildReceivedEvent(eventID, requestID, correlationID(traceHeaders), req, now)
	if err != nil {
		return SubmitResult{}, domain.ValidationResult{}, err
	}
	constraints, err := json.Marshal(req.Constraints)
	if err != nil {
		return SubmitResult{}, domain.ValidationResult{}, err
	}
	if traceHeaders == nil {
		traceHeaders = map[string]string{}
	}
	headers, err := json.Marshal(traceHeaders)
	if err != nil {
		return SubmitResult{}, domain.ValidationResult{}, err
	}

	stored := port.StoredRequest{
		RequestID:       requestID,
		CustomerID:      req.CustomerID,
		TargetName:      req.TargetName,
		TargetWKT:       domain.TargetWKT(req.Target),
		WindowStart:     req.WindowStart,
		WindowEnd:       req.WindowEnd,
		PriorityTier:    req.PriorityTier,
		BidCredits:      req.BidCredits,
		RequestedModes:  req.RequestedModes,
		ConstraintsJSON: constraints,
		SubmittedAt:     now,
	}

	event := port.OutboxEvent{
		// The SAME id that is inside the envelope. They were independent before,
		// so the outbox row and the payload disagreed about the event's identity
		// — and consumer-side dedup keys on the payload's.
		EventID:       eventID,
		EventType:     "tasking.request.received.v1",
		SchemaVersion: EventSchemaVersion,
		Subject:       "tasking.request.received.v1",
		PayloadJSON:   payload,
		HeadersJSON:   headers,
		OccurredAt:    now,
	}

	claim := port.IdempotencyClaim{
		CustomerID:  req.CustomerID,
		Key:         key,
		Fingerprint: fingerprint.String(),
		ExpiresAt:   now.Add(KeyTTL),
	}

	replay, err := s.store.Save(ctx, claim, stored, event)
	switch {
	case errors.Is(err, port.ErrIdempotencyConflict):
		// Surfaced, never swallowed. The handler turns this into a 409.
		return SubmitResult{}, domain.ValidationResult{}, err
	case err != nil:
		return SubmitResult{}, domain.ValidationResult{}, fmt.Errorf("%w: %w", ErrNotPersisted, err)
	case replay.Replayed:
		return SubmitResult{
			RequestID:   replay.RequestID,
			State:       replay.State,
			SubmittedAt: replay.SubmittedAt,
			Replayed:    true,
		}, domain.ValidationResult{}, nil
	}

	return SubmitResult{
		RequestID:   requestID,
		State:       string(domain.StateReceived),
		SubmittedAt: now,
	}, domain.ValidationResult{}, nil
}

// PurgeExpiredKeys drops claims past their TTL.
//
// Called on a timer from main. Without it the table grows forever, and its
// primary key is customer-supplied — an unbounded row count driven by client
// input is a slow-motion outage.
func (s *SubmitService) PurgeExpiredKeys(ctx context.Context) (int64, error) {
	return s.store.PurgeExpiredKeys(ctx, s.clock.Now())
}

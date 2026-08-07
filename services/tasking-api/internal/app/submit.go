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
}

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
func (s *SubmitService) Submit(ctx context.Context, req domain.SubmitRequest) (SubmitResult, domain.ValidationResult, error) {
	now := s.clock.Now()

	if result := domain.Validate(req, now, s.sensors, s.policy); !result.OK() {
		return SubmitResult{}, result, nil
	}

	requestID := s.newID()
	payload, err := receivedEventPayload(requestID, req, now)
	if err != nil {
		return SubmitResult{}, domain.ValidationResult{}, err
	}
	constraints, err := json.Marshal(req.Constraints)
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
		EventID:       s.newID(),
		EventType:     "tasking.request.received.v1",
		SchemaVersion: "1.0.0",
		Subject:       "tasking.request.received.v1",
		PayloadJSON:   payload,
		HeadersJSON:   []byte(`{}`),
		OccurredAt:    now,
	}

	if err := s.store.Save(ctx, stored, event); err != nil {
		return SubmitResult{}, domain.ValidationResult{}, fmt.Errorf("%w: %w", ErrNotPersisted, err)
	}

	return SubmitResult{
		RequestID:   requestID,
		State:       string(domain.StateReceived),
		SubmittedAt: now,
	}, domain.ValidationResult{}, nil
}

func receivedEventPayload(requestID string, req domain.SubmitRequest, now time.Time) ([]byte, error) {
	return json.Marshal(map[string]any{
		"request_id":      requestID,
		"customer_id":     req.CustomerID,
		"target_name":     req.TargetName,
		"priority_tier":   req.PriorityTier,
		"bid_credits":     req.BidCredits,
		"requested_modes": req.RequestedModes,
		"submitted_at":    now.UTC().Format(time.RFC3339Nano),
		"window": map[string]any{
			"start": req.WindowStart.UTC().Format(time.RFC3339Nano),
			"end":   req.WindowEnd.UTC().Format(time.RFC3339Nano),
		},
	})
}

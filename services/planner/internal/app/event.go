package app

import (
	"encoding/json"
	"fmt"

	"github.com/mhayk/overpass/gen/go/events"

	"github.com/mhayk/overpass/services/planner/internal/port"
)

// The published event, built from the GENERATED contract types.
//
// Not from a hand-written map, for the reason #124 records: tasking-api's first
// version was one, and it produced a bare `data` object with no envelope and no
// target — nine schema violations — which every test agreed with because every
// test built the payload with the same helper it asserted against.
//
// Going through gen/go/events means a field the contract adds is a compile error
// here rather than a message somebody's consumer terms at 3am.

// EventSchemaVersion is the version this service publishes.
const EventSchemaVersion = "1.0.0"

// SubjectRoundTriggered is where a round is announced.
const SubjectRoundTriggered = "planning.round.triggered.v1"

// buildRoundTriggeredEvent renders planning.round.triggered.v1.
func buildRoundTriggeredEvent(r port.Round) ([]byte, error) {
	requestIDs := make([]events.Uuid, 0, len(r.CandidateRequestIDs))
	for _, id := range r.CandidateRequestIDs {
		requestIDs = append(requestIDs, events.Uuid(id))
	}

	budget := events.DurationSeconds(r.DutyCycleBudgetS)

	data := events.PlanningRoundTriggeredData{
		RoundId:     events.Uuid(r.RoundID),
		SatelliteId: events.SatelliteId(r.Key.SatelliteID),
		Bucket: events.TimeWindow{
			Start: events.Timestamp(r.Key.BucketStart.UTC()),
			End:   events.Timestamp(r.BucketEnd.UTC()),
		},
		Trigger:                   events.PlanningRoundTriggeredDataTrigger(r.Trigger),
		Policy:                    events.PlanningRoundTriggeredDataPolicy(r.Policy),
		CandidateOpportunityCount: r.CandidateOpportunityCount,
		CandidateRequestIds:       requestIDs,
		DutyCycleBudgetS:          &budget,
		TriggeredAt:               events.Timestamp(r.TriggeredAt.UTC()),
	}
	// SupersedesPlanId is interface{} in the binding because the schema is a
	// oneOf over a uuid and null. Left unset when there is nothing to supersede
	// — assigning a typed nil pointer into an interface{} would marshal as a
	// present-but-null field on a `omitzero` tag that expects absence, which is
	// the difference between "first plan" and "superseding nothing".
	if r.SupersedesPlanID != nil {
		data.SupersedesPlanId = *r.SupersedesPlanID
	}

	envelope := events.PlanningRoundTriggered{
		EventId:       events.EventId(r.EventID),
		EventType:     events.PlanningRoundTriggeredEventTypePlanningRoundTriggeredV1,
		SchemaVersion: events.SchemaVersion(EventSchemaVersion),
		OccurredAt:    events.OccurredAt(r.TriggeredAt.UTC()),
		// A round aggregates many requests, so it cannot inherit any one
		// request's correlation_id — the contract says so and gives it a fresh
		// one. The per-request threads are re-joined through
		// candidate_request_ids and through the request_id on each unfulfilment
		// event.
		CorrelationId: events.CorrelationId(r.CorrelationID),
		// Null when the staleness ceiling fired, the tipping event's id
		// otherwise. Because REPLAN overloads the trigger field, this is the
		// ONLY thing distinguishing a cadence-driven replan from a
		// debounce-driven one.
		CausationId: r.CausationID,
		Producer:    events.PlanningRoundTriggeredProducerPlannerService,
		Data:        data,
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encoding the round event: %w", err)
	}
	return payload, nil
}

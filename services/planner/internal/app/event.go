package app

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mhayk/overpass/gen/go/events"

	"github.com/mhayk/overpass/services/planner/internal/domain"
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

// SubjectPlanCommitted and SubjectUnfulfilled are where a plan announces itself.
const (
	SubjectPlanCommitted = "planning.plan.committed.v1"
	SubjectUnfulfilled   = "planning.request.unfulfilled.v1"
)

// buildPlanCommittedEvent renders planning.plan.committed.v1.
func buildPlanCommittedEvent(r port.Round, plan port.PlanCommit, metrics events.PlanCommittedDataMetrics) ([]byte, error) {
	data := events.PlanCommittedData{
		PlanId:      events.Uuid(plan.PlanID),
		RoundId:     events.Uuid(r.RoundID),
		SatelliteId: events.SatelliteId(r.Key.SatelliteID),
		Bucket: events.TimeWindow{
			Start: events.Timestamp(r.Key.BucketStart.UTC()),
			End:   events.Timestamp(r.BucketEnd.UTC()),
		},
		PlanVersion: plan.PlanVersion,
		Policy:      events.PlanCommittedDataPolicy(plan.Policy),
		CommittedAt: events.Timestamp(plan.CommittedAt.UTC()),
		// Required by the schema, never nil: an empty plan carries an empty
		// list, which is a meaningful statement that the satellite is idle.
		Acquisitions: make([]events.PlanCommittedDataAcquisitionsElem, 0, len(plan.Acquisitions)),
		Metrics:      metrics,
	}
	if plan.SupersedesPlanID != nil {
		data.SupersedesPlanId = *plan.SupersedesPlanID
	}

	for _, a := range plan.Acquisitions {
		elem, err := acquisitionElem(a)
		if err != nil {
			return nil, err
		}
		data.Acquisitions = append(data.Acquisitions, elem)
	}

	envelope := events.PlanCommitted{
		EventId:       events.EventId(plan.PlanEventID),
		EventType:     events.PlanCommittedEventTypePlanningPlanCommittedV1,
		SchemaVersion: events.SchemaVersion(EventSchemaVersion),
		OccurredAt:    events.OccurredAt(plan.CommittedAt.UTC()),
		CorrelationId: events.CorrelationId(r.CorrelationID),
		// The round is what caused this plan, and the chain back to the
		// tipping opportunity event runs through it.
		CausationId: causationOf(r.EventID),
		Producer:    events.PlanCommittedProducerPlannerService,
		Data:        data,
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encoding the plan event: %w", err)
	}
	return payload, nil
}

func acquisitionElem(a domain.ScheduledAcquisition) (events.PlanCommittedDataAcquisitionsElem, error) {
	// The geometry travels verbatim from feasibility through the projection to
	// here, decoded into the generated type so a contract-invalid blob fails
	// NOW rather than at a consumer.
	var geometry events.AccessGeometry
	if err := json.Unmarshal(a.GeometryJSON, &geometry); err != nil {
		return events.PlanCommittedDataAcquisitionsElem{},
			fmt.Errorf("acquisition %s carries undecodable geometry: %w", a.AcquisitionID, err)
	}

	elem := events.PlanCommittedDataAcquisitionsElem{
		AcquisitionId: events.Uuid(a.AcquisitionID),
		RequestId:     events.Uuid(a.RequestID),
		OpportunityId: events.Uuid(a.OpportunityID),
		Mode:          events.ImagingMode(a.Mode),
		Window: events.TimeWindow{
			Start: events.Timestamp(a.Start.UTC()),
			End:   events.Timestamp(a.End.UTC()),
		},
		Geometry:            geometry,
		AwardedValueCredits: events.Credits(a.AwardedValueCredits),
	}
	if a.CustomerID != "" {
		customer := events.CustomerId(a.CustomerID)
		elem.CustomerId = &customer
	}
	duty := events.DurationSeconds(a.DutyCycleCostS)
	elem.DutyCycleCostS = &duty
	if a.SlewFromPreviousS != nil {
		slew := events.DurationSeconds(*a.SlewFromPreviousS)
		elem.SlewTimeFromPreviousS = &slew
	}
	if a.GapFromPreviousS != nil {
		gap := events.DurationSeconds(*a.GapFromPreviousS)
		elem.GapFromPreviousS = &gap
	}
	if a.ClearingPriceCredits != nil {
		elem.ClearingPriceCredits = *a.ClearingPriceCredits
	}
	var footprint events.Polygon
	if err := json.Unmarshal(a.FootprintGeoJSON, &footprint); err == nil {
		elem.Footprint = &footprint
	}
	return elem, nil
}

// buildUnfulfilledEvent renders planning.request.unfulfilled.v1 with the
// STRUCTURED explanation — numbers, never only a message string, because a
// shortfall a customer can act on has to be a number.
func buildUnfulfilledEvent(r port.Round, u domain.Unfulfilment, inputs port.RoundInputs, now time.Time) (port.OutboxEvent, error) {
	explanation := &events.PlanningRequestUnfulfilledDataExplanation{}
	populated := false

	if u.OwnValueCredits > 0 {
		own := events.Credits(u.OwnValueCredits)
		explanation.OwnEffectiveValueCredits = &own
		populated = true
	}
	if u.BestRejectedOpportunityID != "" {
		best := events.Uuid(u.BestRejectedOpportunityID)
		explanation.BestRejectedOpportunityId = &best
		populated = true
	}
	if u.Detail.WinningRequestID != "" {
		winner := events.Uuid(u.Detail.WinningRequestID)
		winning := events.Credits(u.Detail.WinningValueCredits)
		explanation.WinningRequestId = &winner
		explanation.WinningEffectiveValueCredits = &winning
		// The single most useful number a losing customer gets: how much more
		// effective value would have been needed.
		if u.Detail.WinningValueCredits > u.OwnValueCredits {
			shortfall := events.Credits(u.Detail.WinningValueCredits - u.OwnValueCredits)
			explanation.ShortfallCredits = &shortfall
		}
		populated = true
	}
	if u.Detail.BlockingAcquisitionID != "" {
		blocking := events.Uuid(u.Detail.BlockingAcquisitionID)
		explanation.BlockingAcquisitionId = &blocking
		populated = true
	}
	if u.Detail.RequiredSlewS > 0 {
		required := events.DurationSeconds(u.Detail.RequiredSlewS)
		available := events.DurationSeconds(u.Detail.AvailableGapS)
		explanation.RequiredSlewS = &required
		explanation.AvailableGapS = &available
		populated = true
	}
	if u.Detail.DutyCycleRequiredS > 0 {
		required := events.DurationSeconds(u.Detail.DutyCycleRequiredS)
		remaining := events.DurationSeconds(u.Detail.DutyCycleRemainingS)
		explanation.DutyCycleRequiredS = &required
		explanation.DutyCycleRemainingS = &remaining
		populated = true
	}
	if !u.Detail.Deadline.IsZero() {
		deadline := events.Timestamp(u.Detail.Deadline.UTC())
		explanation.Deadline = &deadline
		populated = true
	}
	if u.Detail.SupersededByPlanID != "" {
		superseded := events.Uuid(u.Detail.SupersededByPlanID)
		explanation.SupersededByPlanId = &superseded
		populated = true
	}

	data := events.PlanningRequestUnfulfilledData{
		RequestId:  events.Uuid(u.RequestID),
		RoundId:    events.Uuid(r.RoundID),
		ReasonCode: events.PlanningRequestUnfulfilledDataReasonCode(u.ReasonCode),
		// True for the competitive losses — exactly the ones the ageing factor
		// exists to eventually resolve. False for the terminal ones.
		EligibleForRetry:       u.ReasonCode != domain.ReasonDeadlinePassed && u.ReasonCode != domain.ReasonCancelled,
		DecidedAt:              events.Timestamp(now.UTC()),
		SatelliteIdsConsidered: []events.SatelliteId{events.SatelliteId(r.Key.SatelliteID)},
	}
	if u.CustomerID != "" {
		customer := events.CustomerId(u.CustomerID)
		data.CustomerId = &customer
	}
	// Prior rounds in this bucket plus this one: how many rounds have now
	// considered the request. An OBSERVABILITY counter, not a fairness input —
	// ageing is by time, and the M2-09 commit records why the contract's
	// description is out of step.
	age := inputs.AgeRounds[u.RequestID] + 1
	data.AgeRounds = &age
	if populated {
		data.Explanation = explanation
	}

	envelope := events.PlanningRequestUnfulfilled{
		EventId:       events.EventId(uuid.NewString()),
		EventType:     events.PlanningRequestUnfulfilledEventTypePlanningRequestUnfulfilledV1,
		SchemaVersion: events.SchemaVersion(EventSchemaVersion),
		OccurredAt:    events.OccurredAt(now.UTC()),
		CorrelationId: events.CorrelationId(r.CorrelationID),
		CausationId:   causationOf(r.EventID),
		Producer:      events.PlanningRequestUnfulfilledProducerPlannerService,
		Data:          data,
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return port.OutboxEvent{}, fmt.Errorf("encoding the unfulfilment for %s: %w", u.RequestID, err)
	}
	return port.OutboxEvent{
		EventID:   string(envelope.EventId),
		EventType: SubjectUnfulfilled,
		Subject:   SubjectUnfulfilled,
		Payload:   payload,
	}, nil
}

func causationOf(eventID string) events.CausationId {
	id := eventID
	return events.CausationId(&id)
}

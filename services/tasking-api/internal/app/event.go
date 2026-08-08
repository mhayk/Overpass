package app

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mhayk/overpass/gen/go/events"

	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
)

// The published event, built from the GENERATED contract types.
//
// Not from a hand-written map. The first version was one, and it produced a
// bare `data` object with no envelope and no target — nine schema violations —
// which feasibility-service correctly refused as undeliverable and
// plan-gateway's decoder would have rejected outright. Every test agreed with
// it because every test built the payload with the same helper it asserted
// against (#124, and #112 is the same blind spot seen from the consuming side).
//
// Going through gen/go/events means a field the contract adds is a compile
// error here rather than a message somebody's consumer terms at 3am.

// EventSchemaVersion is the version this service publishes.
//
// Stated once, here, rather than at the call site. It travels in the envelope
// and it is the thing a consumer branches on when the shape changes.
const EventSchemaVersion = "1.0.0"

// buildReceivedEvent renders tasking.request.received.v1.
//
// correlationID comes from the request context. It is REQUIRED by the schema,
// so an empty one would produce an invalid event — the caller supplies the id
// the ingress middleware assigned, which always exists.
func buildReceivedEvent(
	eventID, requestID, correlationID string,
	req domain.SubmitRequest,
	now time.Time,
) ([]byte, error) {
	target, err := targetGeometry(req.Target)
	if err != nil {
		return nil, err
	}

	modes := make([]events.ImagingMode, 0, len(req.RequestedModes))
	for _, mode := range req.RequestedModes {
		modes = append(modes, events.ImagingMode(mode))
	}

	data := events.TaskingRequestReceivedData{
		RequestId:      events.Uuid(requestID),
		CustomerId:     events.CustomerId(req.CustomerID),
		Target:         target,
		PriorityTier:   events.PriorityTier(req.PriorityTier),
		BidCredits:     events.Credits(req.BidCredits),
		RequestedModes: modes,
		SubmittedAt:    events.Timestamp(now.UTC()),
		Window: events.TimeWindow{
			Start: events.Timestamp(req.WindowStart.UTC()),
			End:   events.Timestamp(req.WindowEnd.UTC()),
		},
	}
	// Optional in the schema, and display-only. An empty name is absent rather
	// than an empty string: "" is a label somebody typed, nothing is a label
	// nobody gave.
	if req.TargetName != "" {
		name := req.TargetName
		data.TargetName = &name
	}
	if constraints := acquisitionConstraints(req.Constraints); constraints != nil {
		data.Constraints = constraints
	}

	envelope := events.TaskingRequestReceived{
		EventId:       events.EventId(eventID),
		EventType:     events.TaskingRequestReceivedEventTypeTaskingRequestReceivedV1,
		SchemaVersion: events.SchemaVersion(EventSchemaVersion),
		OccurredAt:    events.OccurredAt(now.UTC()),
		CorrelationId: events.CorrelationId(correlationID),
		// Always null, and the schema says so. This is the root of the causation
		// tree: nothing inside Overpass caused a customer submission.
		CausationId: nil,
		Producer:    events.TaskingRequestReceivedProducerTaskingApi,
		Data:        data,
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encoding the received event: %w", err)
	}
	return payload, nil
}

// correlationHeader is the header the ingress middleware sets. Duplicated as a
// constant here rather than imported: app must not depend on the HTTP adapter,
// and one string is a cheaper coupling than an inverted dependency arrow.
const correlationHeader = "X-Correlation-Id"

// correlationID pulls the id out of the trace headers the handler collected.
//
// The schema REQUIRES it, so an absent one would produce an invalid event. It
// is always present in practice — the middleware assigns one when the caller
// does not — and the fallback exists so a direct call in a test cannot silently
// emit an event the contract rejects.
func correlationID(traceHeaders map[string]string) string {
	if id := traceHeaders[correlationHeader]; id != "" {
		return id
	}
	return uuid.NewString()
}

// targetGeometry renders the target as GeoJSON, longitude first.
//
// The contract types make TargetGeometry an interface{} because the schema is a
// oneOf over Point and Polygon, so the discrimination happens here. A Point is
// a zero-area target; a Polygon carries its exterior ring only, which is what
// the domain holds and what the schema accepts.
func targetGeometry(target domain.Target) (events.TargetGeometry, error) {
	switch target.Kind {
	case domain.TargetPoint:
		return events.Point{
			Type:        events.PointTypePoint,
			Coordinates: position(target.Point),
		}, nil
	case domain.TargetPolygon:
		ring := make(events.LinearRing, 0, len(target.Ring))
		for _, p := range target.Ring {
			ring = append(ring, position(p))
		}
		return events.Polygon{
			Type:        events.PolygonTypePolygon,
			Coordinates: []events.LinearRing{ring},
		}, nil
	default:
		// Unreachable through Submit, which validates first. Explicit anyway:
		// silently publishing an event with no target is how a request that
		// cannot be planned looks exactly like one that can.
		return nil, fmt.Errorf("cannot render target of kind %q", target.Kind)
	}
}

// position is [longitude, latitude]. Longitude first, which trips up everyone
// at least once and relocates a target to another hemisphere when it is wrong.
func position(p domain.Position) events.Position {
	return events.Position{p.Lon, p.Lat}
}

func acquisitionConstraints(c domain.RequestConstraints) *events.AcquisitionConstraints {
	// Absent rather than an empty object when the customer narrowed nothing. An
	// empty constraints object and no constraints object mean the same thing to
	// the planner, and only one of them is worth bytes.
	if c.LookSide == "" && c.MinIncidenceDeg == nil &&
		c.MaxIncidenceDeg == nil && c.MaxSquintDeg == nil {
		return nil
	}
	out := &events.AcquisitionConstraints{
		MinIncidenceDeg: c.MinIncidenceDeg,
		MaxIncidenceDeg: c.MaxIncidenceDeg,
		MaxSquintDeg:    c.MaxSquintDeg,
	}
	if c.LookSide != "" {
		out.LookSide = events.LookSideConstraint(c.LookSide)
	}
	return out
}

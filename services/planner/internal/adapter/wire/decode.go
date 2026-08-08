// Package wire decodes contract payloads into the planner's own types.
//
// It exists because of #112, and that defect is worth restating rather than
// linking. plan-gateway's projector called json.Unmarshal on its own structs.
// Every real payload — wrapped in an envelope, snake_case throughout — decoded
// into an ALL-ZERO STRUCT AND RETURNED NO ERROR. Nothing logged, nothing
// retried; the read model was simply always empty, and it stayed that way
// through review because the code looked obviously correct.
//
// The defence is not care. It is decoding through the GENERATED types, which
// are regenerated from the schemas and drift-gated by `make contracts-verify`,
// and then converting explicitly. A field that moves in the contract becomes a
// compile error here instead of a silent zero value.
package wire

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mhayk/overpass/gen/go/events"

	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// Decoder is the contract-typed implementation of port.Decoder.
type Decoder struct{}

// New builds a decoder. Stateless; the type exists to satisfy the port.
func New() Decoder { return Decoder{} }

// RequestReceived decodes tasking.request.received.v1.
func (Decoder) RequestReceived(payload []byte) (port.RequestReceived, error) {
	var e events.TaskingRequestReceived
	if err := json.Unmarshal(payload, &e); err != nil {
		return port.RequestReceived{}, fmt.Errorf("unmarshalling tasking.request.received.v1: %w", err)
	}
	// The envelope is required by the schema, so an empty event_id means the
	// payload was not a contract event at all — most likely a bare `data`
	// object published by something that skipped the envelope. That is #124's
	// defect, and it is checked here rather than trusted, because an empty
	// event_id would become an empty dedup key and every such message would
	// look like a redelivery of the same one.
	if e.EventId == "" {
		return port.RequestReceived{}, fmt.Errorf("payload has no event_id — not an enveloped contract event")
	}

	return port.RequestReceived{
		EventID: string(e.EventId),
		EventAt: time.Time(e.OccurredAt),
		Snapshot: domain.Snapshot{
			RequestID:     string(e.Data.RequestId),
			CustomerID:    string(e.Data.CustomerId),
			PriorityTier:  string(e.Data.PriorityTier),
			BidCredits:    int64(e.Data.BidCredits),
			WindowStart:   time.Time(e.Data.Window.Start),
			WindowEnd:     time.Time(e.Data.Window.End),
			SubmittedAt:   time.Time(e.Data.SubmittedAt),
			SourceEventID: string(e.EventId),
			OccurredAt:    time.Time(e.OccurredAt),
		},
	}, nil
}

// Opportunities decodes feasibility.opportunities.computed.v1.
func (Decoder) Opportunities(payload []byte) (port.OpportunitiesComputed, error) {
	var e events.FeasibilityOpportunitiesComputed
	if err := json.Unmarshal(payload, &e); err != nil {
		return port.OpportunitiesComputed{}, fmt.Errorf("unmarshalling feasibility.opportunities.computed.v1: %w", err)
	}
	if e.EventId == "" {
		return port.OpportunitiesComputed{}, fmt.Errorf("payload has no event_id — not an enveloped contract event")
	}

	out := port.OpportunitiesComputed{
		EventID:    string(e.EventId),
		EventAt:    time.Time(e.OccurredAt),
		RequestID:  string(e.Data.RequestId),
		Candidates: make([]domain.Candidate, 0, len(e.Data.Opportunities)),
	}

	for i, item := range e.Data.Opportunities {
		// Re-marshalled from the generated types rather than sliced out of the
		// raw payload with a second decode. Round-tripping through the
		// contract-typed struct is what the gen/go/contracttest suite already
		// exercises, so a geometry that survives this survives the contract;
		// hand-cut JSON would be a second parser to keep correct.
		geometry, err := json.Marshal(item.Geometry)
		if err != nil {
			return port.OpportunitiesComputed{}, fmt.Errorf("re-encoding geometry of opportunity %d: %w", i+1, err)
		}
		footprint, err := json.Marshal(item.Footprint)
		if err != nil {
			return port.OpportunitiesComputed{}, fmt.Errorf("re-encoding footprint of opportunity %d: %w", i+1, err)
		}

		out.Candidates = append(out.Candidates, domain.Candidate{
			OpportunityID: string(item.OpportunityId),
			// From the event's data, not from the item: an opportunities event
			// carries candidates for exactly one request, and the item does not
			// repeat the id.
			RequestID:            string(e.Data.RequestId),
			SatelliteID:          string(item.SatelliteId),
			Mode:                 string(item.Mode),
			AccessStart:          time.Time(item.AccessWindow.Start),
			AccessEnd:            time.Time(item.AccessWindow.End),
			AcquisitionDurationS: float64(item.AcquisitionDurationS),
			// Copied, not aliased. `item` is the range variable; taking
			// &item.OrbitNumber's target without copying would leave every
			// candidate pointing at the same storage in Go versions where the
			// loop variable is reused, which is the kind of bug that produces a
			// plausible plan with the wrong orbits.
			OrbitNumber:      copyIntPtr(item.OrbitNumber),
			DutyCycleCostS:   float64(item.DutyCycleCostS),
			QualityScore:     float64(item.QualityScore),
			GeometryJSON:     geometry,
			FootprintGeoJSON: footprint,
			ComputedAt:       time.Time(e.Data.ComputedAt),
			SourceEventID:    string(e.EventId),
		})
	}
	return out, nil
}

func copyIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

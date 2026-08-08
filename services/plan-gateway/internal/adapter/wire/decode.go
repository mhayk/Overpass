// Package wire decodes contract events into the projector's types.
//
// It decodes into the GENERATED types in gen/go/events and maps from there. Not
// into hand-written structs: a hand-written copy of a contract is a copy that
// drifts, and the generated ones are regenerated from the schemas and held
// honest by `make contracts-verify`.
//
// This package exists because the first version skipped it. The projector
// called json.Unmarshal on its own untagged structs, and Go's decoder matches
// field names case-insensitively without mapping snake_case — so
// {"request_id":"A"} produced {RequestID:""} and returned no error at all. Every
// real event folded an empty row and the service reported success (#112).
package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mhayk/overpass/gen/go/events"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// ErrMalformed marks a payload that cannot be understood.
var ErrMalformed = errors.New("malformed payload")

// Decoder maps contract events onto the projector's types.
type Decoder struct{}

// New returns a decoder. Stateless; the type exists to satisfy the port.
func New() *Decoder { return &Decoder{} }

// RequestReceived decodes tasking.request.received.v1.
func (Decoder) RequestReceived(payload []byte) (port.RequestReceived, error) {
	var e events.TaskingRequestReceived
	if err := unmarshal(payload, &e, "tasking.request.received.v1"); err != nil {
		return port.RequestReceived{}, err
	}
	geometry, err := geoJSON(e.Data.Target)
	if err != nil {
		return port.RequestReceived{}, fmt.Errorf("%w: target: %w", ErrMalformed, err)
	}

	return port.RequestReceived{
		EventAt:    time.Time(e.OccurredAt).UTC(),
		RequestID:  string(e.Data.RequestId),
		CustomerID: string(e.Data.CustomerId),
		// Display only, and optional in the schema. An absent name is not an
		// error; a request with no label is still a request.
		TargetName:    derefOr(e.Data.TargetName, ""),
		WindowStart:   time.Time(e.Data.Window.Start).UTC(),
		WindowEnd:     time.Time(e.Data.Window.End).UTC(),
		TargetGeoJSON: geometry,
	}, nil
}

// Opportunities decodes feasibility.opportunities.computed.v1.
func (Decoder) Opportunities(payload []byte) (port.OpportunitiesComputed, error) {
	var e events.FeasibilityOpportunitiesComputed
	if err := unmarshal(payload, &e, "feasibility.opportunities.computed.v1"); err != nil {
		return port.OpportunitiesComputed{}, err
	}

	out := port.OpportunitiesComputed{
		EventAt:   time.Time(e.OccurredAt).UTC(),
		RequestID: string(e.Data.RequestId),
	}
	for i, o := range e.Data.Opportunities {
		footprint, err := geoJSON(o.Footprint)
		if err != nil {
			return port.OpportunitiesComputed{},
				fmt.Errorf("%w: opportunity %d footprint: %w", ErrMalformed, i, err)
		}
		out.Opportunities = append(out.Opportunities, port.Opportunity{
			OpportunityID:        string(o.OpportunityId),
			SatelliteID:          string(o.SatelliteId),
			Mode:                 string(o.Mode),
			AccessStart:          time.Time(o.AccessWindow.Start).UTC(),
			AccessEnd:            time.Time(o.AccessWindow.End).UTC(),
			AcquisitionDurationS: float64(o.AcquisitionDurationS),
			OrbitNumber:          o.OrbitNumber,
			QualityScore:         float64(o.QualityScore),
			FootprintGeoJSON:     footprint,
		})
	}
	return out, nil
}

// PlanCommitted decodes planning.plan.committed.v1.
func (Decoder) PlanCommitted(payload []byte) (port.PlanCommitted, error) {
	var e events.PlanCommitted
	if err := unmarshal(payload, &e, "planning.plan.committed.v1"); err != nil {
		return port.PlanCommitted{}, err
	}

	metrics, err := json.Marshal(e.Data.Metrics)
	if err != nil {
		return port.PlanCommitted{}, fmt.Errorf("re-encoding metrics: %w", err)
	}

	out := port.PlanCommitted{
		EventAt:          time.Time(e.OccurredAt).UTC(),
		PlanID:           string(e.Data.PlanId),
		SatelliteID:      string(e.Data.SatelliteId),
		BucketStart:      time.Time(e.Data.Bucket.Start).UTC(),
		BucketEnd:        time.Time(e.Data.Bucket.End).UTC(),
		PlanVersion:      e.Data.PlanVersion,
		SupersedesPlanID: optionalString(e.Data.SupersedesPlanId),
		Policy:           string(e.Data.Policy),
		MetricsJSON:      metrics,
		CommittedAt:      time.Time(e.Data.CommittedAt).UTC(),
	}

	for i, a := range e.Data.Acquisitions {
		// Optional in the schema, and its absence is not an error the projector
		// can do anything about: an acquisition with no footprint still
		// occupies its window and still de-conflicts.
		var footprint []byte
		if a.Footprint != nil {
			if footprint, err = geoJSON(*a.Footprint); err != nil {
				return port.PlanCommitted{},
					fmt.Errorf("%w: acquisition %d footprint: %w", ErrMalformed, i, err)
			}
		}
		opportunityID := string(a.OpportunityId)
		out.Acquisitions = append(out.Acquisitions, port.Acquisition{
			AcquisitionID:         string(a.AcquisitionId),
			RequestID:             string(a.RequestId),
			OpportunityID:         &opportunityID,
			CustomerID:            derefOr((*string)(a.CustomerId), ""),
			Mode:                  string(a.Mode),
			WindowStart:           time.Time(a.Window.Start).UTC(),
			WindowEnd:             time.Time(a.Window.End).UTC(),
			FootprintGeoJSON:      footprint,
			SlewTimeFromPreviousS: (*float64)(a.SlewTimeFromPreviousS),
			GapFromPreviousS:      (*float64)(a.GapFromPreviousS),
			AwardedValueCredits:   int64(a.AwardedValueCredits),
		})
	}
	return out, nil
}

// Unfulfilled decodes planning.request.unfulfilled.v1.
//
// The whole data object is kept, not just the reason code. The explanation is
// the point of this event — "which constraint bound, and by how much" — and
// discarding the numbers would leave the UI with a string it cannot chart.
func (Decoder) Unfulfilled(payload []byte) (port.RequestUnfulfilled, error) {
	var e events.PlanningRequestUnfulfilled
	if err := unmarshal(payload, &e, "planning.request.unfulfilled.v1"); err != nil {
		return port.RequestUnfulfilled{}, err
	}
	reason, err := json.Marshal(e.Data)
	if err != nil {
		return port.RequestUnfulfilled{}, fmt.Errorf("re-encoding explanation: %w", err)
	}
	return port.RequestUnfulfilled{
		EventAt:    time.Time(e.OccurredAt).UTC(),
		RequestID:  string(e.Data.RequestId),
		ReasonJSON: reason,
	}, nil
}

// unmarshal rejects unknown fields.
//
// DisallowUnknownFields, deliberately. The failure this package exists to fix
// was silent: a payload whose names matched nothing decoded to zeroes and
// looked fine. Refusing to accept a field nobody asked for turns a future
// version of that mistake into an error rather than a quiet empty row.
//
// The cost is that a producer adding a field breaks this consumer, which is why
// the contracts set additionalProperties: false and version rather than extend.
func unmarshal(payload []byte, into any, subject string) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMalformed, subject, err)
	}
	return nil
}

// geoJSON re-encodes a decoded geometry back to bytes for PostGIS.
//
// Round-tripping rather than slicing the original payload: the generated types
// have already validated the shape, and finding the right byte range inside the
// envelope would mean a second parser.
func geoJSON(geometry any) ([]byte, error) {
	if geometry == nil {
		return nil, errors.New("geometry is absent")
	}
	out, err := json.Marshal(geometry)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 || string(out) == "null" {
		return nil, errors.New("geometry encoded to null")
	}
	return out, nil
}

func derefOr[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

// optionalString handles the schema's nullable uuid, which the generator emits
// as interface{} because it is `["string", "null"]`.
func optionalString(v any) *string {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	return &s
}

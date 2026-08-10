package render

import (
	"encoding/json"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// FeatureCollection is RFC 7946 with two foreign members.
//
// §6.1 permits foreign members and every reader ignores them, so `truncated`
// and `staleness` ride on the collection rather than in a wrapper object. That
// keeps the response loadable by deck.gl directly, with no unwrapping step in
// the client — and a client that has to unwrap is a client that will eventually
// forget to.
type FeatureCollection struct {
	Type      string    `json:"type"`
	Features  []Feature `json:"features"`
	Truncated bool      `json:"truncated"`
	Staleness Staleness `json:"staleness"`
}

// Feature is one footprint.
type Feature struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Geometry   json.RawMessage `json:"geometry"`
	Properties FeatureProps    `json:"properties"`
}

// FeatureProps is what the coverage view colours and filters by.
type FeatureProps struct {
	AcquisitionID       string  `json:"acquisition_id"`
	PlanID              string  `json:"plan_id"`
	RequestID           string  `json:"request_id"`
	CustomerID          string  `json:"customer_id"`
	SatelliteID         string  `json:"satellite_id"`
	Mode                string  `json:"mode"`
	Status              string  `json:"status"`
	WindowStart         string  `json:"window_start"`
	WindowEnd           string  `json:"window_end"`
	DurationS           float64 `json:"duration_s"`
	AwardedValueCredits int64   `json:"awarded_value_credits"`
}

// Staleness mirrors the field on every other response.
type Staleness struct {
	AsOf       string  `json:"as_of"`
	LagSeconds float64 `json:"lag_seconds"`
}

// FootprintsGeoJSON renders acquisitions as a FeatureCollection.
//
// Truncation is reported, never silent. A coverage view that draws a gap where
// there is really a limit is a view confidently showing emptiness over a fully
// tasked region — and the reader has no way to tell the difference.
func FootprintsGeoJSON(
	acquisitions []port.AcquisitionView,
	truncated bool,
	cursor port.Cursor,
	now time.Time,
) ([]byte, error) {
	lag := now.Sub(cursor.LastEventAt).Seconds()
	if lag < 0 {
		lag = 0
	}

	// Never nil. A nil slice marshals to `null`, and deck.gl iterating over
	// null throws where iterating over [] is a no-op — the difference never
	// shows up in Go and always shows up in the browser.
	features := make([]Feature, 0, len(acquisitions))
	for _, a := range acquisitions {
		if len(a.FootprintGeoJSON) == 0 {
			// Nothing to draw, and the contract makes the footprint optional.
			// A feature with a null geometry is valid GeoJSON and renders as an
			// invisible hole in the layer's indexing, which is harder to
			// diagnose than an absent feature.
			continue
		}
		features = append(features, Feature{
			Type:     "Feature",
			ID:       a.AcquisitionID,
			Geometry: json.RawMessage(a.FootprintGeoJSON),
			Properties: FeatureProps{
				AcquisitionID:       a.AcquisitionID,
				PlanID:              a.PlanID,
				RequestID:           a.RequestID,
				CustomerID:          a.CustomerID,
				SatelliteID:         a.SatelliteID,
				Mode:                a.Mode,
				Status:              a.Status,
				WindowStart:         a.WindowStart.UTC().Format(time.RFC3339),
				WindowEnd:           a.WindowEnd.UTC().Format(time.RFC3339),
				DurationS:           a.WindowEnd.Sub(a.WindowStart).Seconds(),
				AwardedValueCredits: a.AwardedValueCredits,
			},
		})
	}

	return json.Marshal(FeatureCollection{
		Type:      "FeatureCollection",
		Features:  features,
		Truncated: truncated,
		Staleness: Staleness{
			AsOf:       cursor.LastEventAt.UTC().Format(time.RFC3339Nano),
			LagSeconds: lag,
		},
	})
}

// TargetFeatureProps is the demand layer's per-feature payload.
//
// A named struct rather than a map, for the reason writeConditional documents:
// the ETag is computed over the rendered BYTES, and Go randomises map iteration
// per run, so a map-rendered document would produce a different tag on every
// request and the validator would never match.
type TargetFeatureProps struct {
	RequestID        string `json:"request_id"`
	CustomerID       string `json:"customer_id"`
	TargetName       string `json:"target_name,omitempty"`
	State            string `json:"state"`
	Window           Window `json:"window"`
	OpportunityCount int    `json:"opportunity_count"`
}

// Window is a time range, spelled as the contract's TimeWindow.
type Window struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// TargetFeature carries a Point OR a Polygon, so its geometry stays raw.
type TargetFeature struct {
	Type       string             `json:"type"`
	ID         string             `json:"id,omitempty"`
	Geometry   json.RawMessage    `json:"geometry"`
	Properties TargetFeatureProps `json:"properties"`
}

// TargetCollection is the demand layer's document.
type TargetCollection struct {
	Type      string          `json:"type"`
	Features  []TargetFeature `json:"features"`
	Truncated bool            `json:"truncated"`
	Staleness Staleness       `json:"staleness"`
}

// TargetsGeoJSON renders the target-density layer.
func TargetsGeoJSON(
	targets []port.TargetView,
	truncated bool,
	cursor port.Cursor,
	now time.Time,
) ([]byte, error) {
	// Never nil: a nil slice marshals to `null`, and deck.gl iterating over
	// null throws where iterating over [] is a no-op. The difference never
	// shows up in Go and always shows up in the browser.
	features := make([]TargetFeature, 0, len(targets))
	for _, t := range targets {
		if len(t.GeoJSON) == 0 {
			continue
		}
		features = append(features, TargetFeature{
			Type:     "Feature",
			ID:       t.RequestID,
			Geometry: json.RawMessage(t.GeoJSON),
			Properties: TargetFeatureProps{
				RequestID:  t.RequestID,
				CustomerID: t.CustomerID,
				TargetName: t.TargetName,
				State:      t.State,
				Window: Window{
					Start: t.WindowStart.UTC().Format(time.RFC3339),
					End:   t.WindowEnd.UTC().Format(time.RFC3339),
				},
				OpportunityCount: t.OpportunityCount,
			},
		})
	}

	return json.Marshal(TargetCollection{
		Type:      "FeatureCollection",
		Features:  features,
		Truncated: truncated,
		Staleness: stalenessOf(cursor, now),
	})
}

// OpportunityFeatureProps is the contention layer's per-feature payload.
type OpportunityFeatureProps struct {
	OpportunityID string  `json:"opportunity_id"`
	RequestID     string  `json:"request_id"`
	SatelliteID   string  `json:"satellite_id"`
	Mode          string  `json:"mode"`
	WindowStart   string  `json:"window_start"`
	WindowEnd     string  `json:"window_end"`
	QualityScore  float64 `json:"quality_score"`
	// Awarded is the field the whole endpoint exists for. NOT omitempty: false
	// is the interesting value here, and omitempty would drop it from exactly
	// the features the conflict layer is counting.
	Awarded bool `json:"awarded"`
}

// OpportunityFeature is one candidate's footprint.
type OpportunityFeature struct {
	Type       string                  `json:"type"`
	ID         string                  `json:"id,omitempty"`
	Geometry   json.RawMessage         `json:"geometry"`
	Properties OpportunityFeatureProps `json:"properties"`
}

// OpportunityCollection is the contention layer's document.
type OpportunityCollection struct {
	Type      string               `json:"type"`
	Features  []OpportunityFeature `json:"features"`
	Truncated bool                 `json:"truncated"`
	Staleness Staleness            `json:"staleness"`
}

// OpportunityFootprintsGeoJSON renders candidate footprints, won and lost.
func OpportunityFootprintsGeoJSON(
	opportunities []port.OpportunityFootprintView,
	truncated bool,
	cursor port.Cursor,
	now time.Time,
) ([]byte, error) {
	features := make([]OpportunityFeature, 0, len(opportunities))
	for _, o := range opportunities {
		if len(o.GeoJSON) == 0 {
			continue
		}
		features = append(features, OpportunityFeature{
			Type:     "Feature",
			ID:       o.OpportunityID,
			Geometry: json.RawMessage(o.GeoJSON),
			Properties: OpportunityFeatureProps{
				OpportunityID: o.OpportunityID,
				RequestID:     o.RequestID,
				SatelliteID:   o.SatelliteID,
				Mode:          o.Mode,
				WindowStart:   o.WindowStart.UTC().Format(time.RFC3339),
				WindowEnd:     o.WindowEnd.UTC().Format(time.RFC3339),
				QualityScore:  o.QualityScore,
				Awarded:       o.Awarded,
			},
		})
	}

	return json.Marshal(OpportunityCollection{
		Type:      "FeatureCollection",
		Features:  features,
		Truncated: truncated,
		Staleness: stalenessOf(cursor, now),
	})
}

// stalenessOf is the staleness block every geo document carries.
//
// Extracted because three renderers computed it identically and a fourth would
// have copied it again. Lag is clamped at zero: a cursor timestamped slightly
// ahead of this process's clock is a clock-skew artefact, and a negative
// staleness on a dashboard reads as a bug in the read model rather than in NTP.
func stalenessOf(cursor port.Cursor, now time.Time) Staleness {
	lag := now.Sub(cursor.LastEventAt).Seconds()
	if lag < 0 {
		lag = 0
	}
	return Staleness{
		AsOf:       cursor.LastEventAt.UTC().Format(time.RFC3339Nano),
		LagSeconds: lag,
	}
}

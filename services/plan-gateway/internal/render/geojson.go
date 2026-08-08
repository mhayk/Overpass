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

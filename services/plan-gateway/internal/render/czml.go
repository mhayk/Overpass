// Package geo renders read models into the formats the two view libraries
// consume: CZML for Cesium, GeoJSON for deck.gl.
//
// Server-side, per ADR-0009. The alternative ships raw geometry and lets each
// library derive shapes for itself, which is slow, duplicated twice, and a
// second and third place for the geometry to be wrong. Rendering here means a
// footprint that disagrees between the globe and the coverage view has exactly
// one culprit, and it is covered by golden files.
package render

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// CZMLTimeFormat is ISO 8601 as Cesium's clock parses it.
//
// Second resolution, not nanosecond. Cesium accepts fractional seconds, but a
// document whose every interval carries nine decimal places is noticeably
// larger for precision no viewer can display — and acquisition windows are
// scheduled to the second in the first place.
const CZMLTimeFormat = "2006-01-02T15:04:05Z"

// Packet is one CZML packet. Rendered through an ordered struct rather than a
// map so the output is byte-stable, which is what makes both the golden files
// and the ETag meaningful.
type Packet struct {
	ID           string       `json:"id"`
	Name         string       `json:"name,omitempty"`
	Description  string       `json:"description,omitempty"`
	Version      string       `json:"version,omitempty"`
	Clock        *Clock       `json:"clock,omitempty"`
	Availability string       `json:"availability,omitempty"`
	Polygon      *PolygonGfx  `json:"polygon,omitempty"`
	Properties   *PlanProps   `json:"properties,omitempty"`
	Label        *LabelGfx    `json:"label,omitempty"`
	Position     *PositionGfx `json:"position,omitempty"`
}

// Clock drives the timeline. Cesium scrubs against this rather than re-querying.
type Clock struct {
	Interval    string  `json:"interval"`
	CurrentTime string  `json:"currentTime"`
	Multiplier  float64 `json:"multiplier"`
	Range       string  `json:"range"`
	Step        string  `json:"step"`
}

// PolygonGfx is a footprint on the ellipsoid.
type PolygonGfx struct {
	Positions    Positions `json:"positions"`
	Material     Material  `json:"material"`
	Outline      bool      `json:"outline"`
	OutlineWidth float64   `json:"outlineWidth,omitempty"`
	OutlineColor *Material `json:"outlineColor,omitempty"`
	// Clamped to the terrain rather than floating at an altitude. A swath is a
	// region OF the ground; drawing it above the surface makes the projection
	// look like a decal and breaks the illusion the globe exists to create.
	HeightReference string `json:"heightReference"`
	ArcType         string `json:"arcType"`
}

// Positions carries degrees, not radians, and longitude first.
type Positions struct {
	CartographicDegrees []float64 `json:"cartographicDegrees"`
}

// Material is a solid colour. Cesium's colour model is RGBA 0-255.
type Material struct {
	SolidColor *SolidColor `json:"solidColor,omitempty"`
	RGBA       []int       `json:"rgba,omitempty"`
}

// SolidColor wraps an RGBA for the material form.
type SolidColor struct {
	Color Material `json:"color"`
}

// LabelGfx annotates an acquisition on the globe.
type LabelGfx struct {
	Text        string      `json:"text"`
	Show        bool        `json:"show"`
	FillColor   Material    `json:"fillColor"`
	Font        string      `json:"font"`
	Scale       float64     `json:"scale,omitempty"`
	PixelOffset *Cartesian2 `json:"pixelOffset,omitempty"`
}

// Cartesian2 is a screen-space offset.
type Cartesian2 struct {
	Cartesian2 []float64 `json:"cartesian2"`
}

// PositionGfx anchors a label.
type PositionGfx struct {
	CartographicDegrees []float64 `json:"cartographicDegrees"`
}

// PlanProps carries plan metadata Cesium ignores and the UI reads.
type PlanProps struct {
	PlanID      string  `json:"plan_id"`
	SatelliteID string  `json:"satellite_id"`
	PlanVersion int     `json:"plan_version"`
	Superseded  bool    `json:"superseded"`
	Policy      string  `json:"policy"`
	AsOf        string  `json:"as_of"`
	LagSeconds  float64 `json:"lag_seconds"`
}

// Colours, as RGBA. Status is encoded in colour because a timeline with fifty
// acquisitions cannot carry fifty labels, and the one distinction that has to
// survive at a glance is live versus superseded.
var (
	colourActive     = []int{64, 196, 255, 100}
	colourExecuted   = []int{80, 220, 140, 120}
	colourSuperseded = []int{160, 160, 170, 45}
	colourOutline    = []int{255, 255, 255, 160}
)

// PlanCZML renders one plan as a CZML packet stream.
//
// What it does NOT contain: the satellite's position track and its ground
// track. Those need ephemeris, and the read model holds none — it holds the
// acquisitions the planner committed, not the orbit they were derived from.
// Interpolating a path through the footprint centroids would produce a curve
// that looks like an orbit and is not one, which is worse than an absent layer:
// a viewer would believe it. The path arrives when an ephemeris projection
// does, and the document packet's clock is already shaped to carry it.
func PlanCZML(plan port.PlanView, staleness port.Cursor, now time.Time) ([]byte, error) {
	interval := czmlInterval(plan.BucketStart, plan.BucketEnd)
	lag := now.Sub(staleness.LastEventAt).Seconds()
	if lag < 0 {
		lag = 0
	}

	packets := []Packet{{
		ID:      "document",
		Name:    fmt.Sprintf("Overpass plan %s v%d", plan.SatelliteID, plan.PlanVersion),
		Version: "1.0",
		Clock: &Clock{
			Interval:    interval,
			CurrentTime: plan.BucketStart.UTC().Format(CZMLTimeFormat),
			// Sixty times real time. A three-hour bucket then scrubs in three
			// minutes, which is roughly how long anyone watches a demo before
			// wanting to skip.
			Multiplier: 60,
			Range:      "LOOP_STOP",
			Step:       "SYSTEM_CLOCK_MULTIPLIER",
		},
		Properties: &PlanProps{
			PlanID:      plan.PlanID,
			SatelliteID: plan.SatelliteID,
			PlanVersion: plan.PlanVersion,
			Superseded:  plan.Superseded,
			Policy:      plan.Policy,
			AsOf:        staleness.LastEventAt.UTC().Format(time.RFC3339Nano),
			LagSeconds:  lag,
		},
	}}

	for _, a := range plan.Acquisitions {
		packet, err := acquisitionPacket(a)
		if err != nil {
			return nil, err
		}
		if packet != nil {
			packets = append(packets, *packet)
		}
	}

	return json.Marshal(packets)
}

func acquisitionPacket(a port.AcquisitionView) (*Packet, error) {
	// An acquisition with no footprint is legitimate — the contract makes it
	// optional — and there is nothing to draw. Skipping is right; emitting a
	// polygon with no positions makes Cesium throw and takes the whole scene
	// down with it.
	if len(a.FootprintGeoJSON) == 0 {
		return nil, nil //nolint:nilnil // no footprint is not an error
	}
	positions, err := polygonToCartographicDegrees(a.FootprintGeoJSON)
	if err != nil {
		return nil, fmt.Errorf("acquisition %s footprint: %w", a.AcquisitionID, err)
	}
	if len(positions) == 0 {
		return nil, nil //nolint:nilnil // an empty ring draws nothing
	}

	return &Packet{
		ID:   "acquisition/" + a.AcquisitionID,
		Name: a.SatelliteID + " " + a.Mode,
		Description: fmt.Sprintf("request %s, %s, %d credits",
			a.RequestID, a.Status, a.AwardedValueCredits),
		// availability is what makes the timeline mean something: the polygon
		// exists only while the acquisition is being taken. Without it Cesium
		// draws every footprint at every instant and the scrub does nothing.
		Availability: czmlInterval(a.WindowStart, a.WindowEnd),
		Polygon: &PolygonGfx{
			Positions:       Positions{CartographicDegrees: positions},
			Material:        Material{SolidColor: &SolidColor{Color: Material{RGBA: statusColour(a.Status)}}},
			Outline:         true,
			OutlineWidth:    2,
			OutlineColor:    &Material{RGBA: colourOutline},
			HeightReference: "CLAMP_TO_GROUND",
			// Geodesic, not straight lines in projected space. A swath edge
			// spanning degrees of longitude is a great-circle arc, and RHUMB or
			// NONE would draw a visibly wrong shape at high latitude — where a
			// sun-synchronous constellation spends most of its passes.
			ArcType: "GEODESIC",
		},
	}, nil
}

func statusColour(status string) []int {
	switch status {
	case "EXECUTED":
		return colourExecuted
	case "SUPERSEDED":
		return colourSuperseded
	default:
		return colourActive
	}
}

func czmlInterval(start, end time.Time) string {
	return start.UTC().Format(CZMLTimeFormat) + "/" + end.UTC().Format(CZMLTimeFormat)
}

// polygonToCartographicDegrees flattens a GeoJSON polygon's exterior ring.
//
// CZML wants a flat [lon, lat, height, lon, lat, height, ...] array with height
// in metres. Height is zero throughout because the polygon is CLAMP_TO_GROUND
// and Cesium ignores it — but the triples are still required, and omitting the
// zeroes silently shifts every coordinate by one position, which puts the
// footprint somewhere else entirely.
//
// The exterior ring only. GeoJSON holes would need a separate CZML mechanism
// and no footprint this system produces has one; emitting them as part of the
// same ring would draw a shape that crosses itself.
func polygonToCartographicDegrees(geojson []byte) ([]float64, error) {
	var polygon struct {
		Type        string          `json:"type"`
		Coordinates [][][]float64   `json:"coordinates"`
		Geometries  json.RawMessage `json:"geometries"`
	}
	if err := json.Unmarshal(geojson, &polygon); err != nil {
		return nil, fmt.Errorf("not GeoJSON: %w", err)
	}
	if polygon.Type != "Polygon" {
		return nil, fmt.Errorf("footprint is %q, want Polygon", polygon.Type)
	}
	if len(polygon.Coordinates) == 0 {
		return nil, nil
	}

	ring := polygon.Coordinates[0]
	out := make([]float64, 0, len(ring)*3)
	for i, position := range ring {
		if len(position) < 2 {
			return nil, fmt.Errorf("position %d has %d coordinates, want at least 2", i, len(position))
		}
		// Longitude first, in both formats. Stated because the failure is
		// silent: a swap relocates the footprint to another hemisphere and
		// still renders happily.
		out = append(out, position[0], position[1], 0)
	}
	return out, nil
}

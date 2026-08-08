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
	"math"
	"sort"
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
	ID           string      `json:"id"`
	Name         string      `json:"name,omitempty"`
	Description  string      `json:"description,omitempty"`
	Version      string      `json:"version,omitempty"`
	Clock        *Clock      `json:"clock,omitempty"`
	Availability string      `json:"availability,omitempty"`
	Polygon      *PolygonGfx `json:"polygon,omitempty"`
	Properties   *PlanProps  `json:"properties,omitempty"`
	// Staleness alone, for a document that has no plan to describe.
	Freshness *Freshness   `json:"freshness,omitempty"`
	Label     *LabelGfx    `json:"label,omitempty"`
	Position  *PositionGfx `json:"position,omitempty"`
	Path      *PathGfx     `json:"path,omitempty"`
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

// PositionGfx is a position, constant or sampled.
//
// Both forms in one struct because CZML uses one property for both, and the
// difference is entirely whether `epoch` is set. Constant: three numbers, no
// epoch. Sampled: `[t, lon, lat, height]` repeated, with the offsets in seconds
// after `epoch` — which is exactly the shape the ephemeris event carries, so
// the renderer copies rather than converts.
type PositionGfx struct {
	// Epoch is the origin for the sample time offsets. Absent for a constant
	// position, and its absence is what tells Cesium to read the array as one
	// triple rather than as samples.
	Epoch string `json:"epoch,omitempty"`

	CartographicDegrees []float64 `json:"cartographicDegrees"`

	// LAGRANGE, not Cesium's default LINEAR. Straight lines between samples cut
	// the corner of a curving orbit; at ten-second spacing that is small over
	// most of a pass and visible where the track turns hardest, which for a
	// sun-synchronous constellation is the poles — where it spends most of its
	// time.
	InterpolationAlgorithm string `json:"interpolationAlgorithm,omitempty"`
	InterpolationDegree    int    `json:"interpolationDegree,omitempty"`

	// FIXED, i.e. Earth-fixed. The samples are geodetic longitude and latitude
	// on a rotating Earth; INERTIAL would make the track drift westward across
	// the globe at fifteen degrees an hour and still render perfectly happily.
	ReferenceFrame string `json:"referenceFrame,omitempty"`
}

// PathGfx draws the orbit track swept by a sampled position.
type PathGfx struct {
	Material   Material `json:"material"`
	Width      float64  `json:"width"`
	LeadTime   float64  `json:"leadTime"`
	TrailTime  float64  `json:"trailTime"`
	Resolution float64  `json:"resolution"`
}

// Freshness is how current a document is, with nothing else attached.
//
// A separate type from PlanProps rather than PlanProps with empty fields. The
// constellation document has no plan, and rendering one with plan_id "" and
// plan_version 0 does not describe the absence of a plan — it describes a plan,
// wrongly, in a shape a UI would happily read. Caught by the golden file.
type Freshness struct {
	AsOf       string  `json:"as_of"`
	LagSeconds float64 `json:"lag_seconds"`
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
	// The orbit track. Warm against the cool footprints, so the two layers are
	// distinguishable at a glance without a legend — the path crosses the
	// footprints constantly and a similar hue would read as one shape.
	colourPath = []int{255, 190, 90, 220}
)

// PlanCZML renders one plan as a CZML packet stream.
//
// The satellite's path comes from `plan.Track` — sampled positions projected
// from feasibility.ephemeris.computed.v1 — or it is absent. There is no third
// option, and that is the decision #27 made and #128 kept: interpolating a
// curve through the committed footprint centroids would produce something that
// looks like an orbit and is not one, which is worse than an absent layer
// because a viewer would believe it.
//
// So a plan whose bucket the ephemeris sweep has not reached renders exactly as
// it did before: footprints and a clock, no path.
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

	// The satellite before the acquisitions. Packet order is not semantic in
	// CZML, but a reader opening the document sees the entity that owns the
	// scene first, and the golden file is easier to review for it.
	if packet := satellitePacket(plan); packet != nil {
		packets = append(packets, *packet)
	}

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

// satellitePacket renders the orbit track, or nothing at all.
//
// Two samples is the floor, and it is not arbitrary: one sample is a position,
// not a path, and Cesium given a single sample holds the satellite there for
// the whole interval — a stationary satellite, rendered confidently.
func satellitePacket(plan port.PlanView) *Packet {
	if len(plan.Track) < 2 {
		return nil
	}

	epoch := plan.Track[0].At
	values := make([]float64, 0, len(plan.Track)*4)
	for _, sample := range plan.Track {
		values = append(values,
			sample.At.Sub(epoch).Seconds(),
			round(sample.LongitudeDeg, 6),
			round(sample.LatitudeDeg, 6),
			// Whole metres. The propagator's own error is metres and no viewer
			// can display less; the fractional part is pure payload on the
			// largest array in the document.
			round(sample.AltitudeM, 0),
		)
	}

	// Availability spans the SAMPLES, not the plan's bucket. They are usually
	// the same interval and must not be assumed to be: a bucket the sweep has
	// only partly covered would otherwise ask Cesium for positions it does not
	// have, and Cesium answers by holding the last one — a satellite frozen in
	// place, which reads as a bug in the physics rather than a gap in the data.
	last := plan.Track[len(plan.Track)-1].At

	return &Packet{
		ID:           "satellite/" + plan.SatelliteID,
		Name:         plan.SatelliteID,
		Description:  fmt.Sprintf("%d sampled positions", len(plan.Track)),
		Availability: czmlInterval(epoch, last),
		Position: &PositionGfx{
			Epoch:                  epoch.UTC().Format(CZMLTimeFormat),
			CartographicDegrees:    values,
			InterpolationAlgorithm: "LAGRANGE",
			// Five, Cesium's own default for sampled positions. Higher degrees
			// fit the samples more closely and ring between them, which on an
			// orbit shows up as a track that wobbles across its own path.
			InterpolationDegree: 5,
			ReferenceFrame:      "FIXED",
		},
		Path: &PathGfx{
			Material: Material{SolidColor: &SolidColor{Color: Material{RGBA: colourPath}}},
			Width:    2,
			// Half an orbit of trail and none of lead. Drawing the whole bucket
			// at once makes three hours of ground track cross itself repeatedly
			// and tells a viewer nothing about where the satellite is now;
			// showing where it is going would also pre-announce acquisitions
			// the timeline is about to reach.
			LeadTime:  0,
			TrailTime: 2700,
			// Cesium subdivides the path to this many seconds when the samples
			// are further apart. Ours are ten seconds apart, so this never
			// fires — it is here so the path stays smooth if the sample
			// interval is ever widened for payload.
			Resolution: 60,
		},
	}
}

// round to n decimal places.
//
// Coordinate precision is decided HERE rather than in SQL, unlike footprints —
// those go through ST_AsGeoJSON, which takes a precision argument, so there is
// no reason not to let PostGIS do it. Nothing rounds a float8 column on the way
// out, and this is the largest array in the document, so the last layer before
// the bytes is where it has to happen. The budget test in this package is what
// measures the result.
func round(v float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(v*scale) / scale
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

// ConstellationCZML renders every satellite's orbit track over a window.
//
// The constellation, not a plan's satellite. PlanCZML draws the orbit of the
// one satellite its plan belongs to, which is the right answer when a plan is
// what you are looking at and the wrong one for a globe: the constellation
// exists before the first plan is committed and after the last is superseded,
// and a viewer who sees satellites appear only once something has been
// scheduled has been told something false about the system.
//
// An empty result is a document packet and a clock. That is the honest
// rendering of a window the ephemeris sweep has not reached — the sweep runs on
// its own timer — and it keeps the timeline's span meaningful when there is
// nothing to draw on it yet.
func ConstellationCZML(
	tracks map[string][]port.EphemerisSample,
	from, to time.Time,
	staleness port.Cursor,
	now time.Time,
) ([]byte, error) {
	lag := now.Sub(staleness.LastEventAt).Seconds()
	if lag < 0 {
		lag = 0
	}

	packets := []Packet{{
		ID:      "document",
		Name:    "Overpass constellation",
		Version: "1.0",
		Clock: &Clock{
			Interval:    czmlInterval(from, to),
			CurrentTime: from.UTC().Format(CZMLTimeFormat),
			Multiplier:  60,
			Range:       "LOOP_STOP",
			Step:        "SYSTEM_CLOCK_MULTIPLIER",
		},
		Freshness: &Freshness{
			AsOf:       staleness.LastEventAt.UTC().Format(time.RFC3339Nano),
			LagSeconds: lag,
		},
	}}

	// Sorted, because Go randomises map iteration per run and the ETag is
	// computed over these exact bytes. A map-ordered document would hash
	// differently on every request and the validator would never match —
	// silently, with the endpoint merely appearing to have a very cold cache.
	for _, satelliteID := range sortedKeys(tracks) {
		if packet := satellitePacket(port.PlanView{
			SatelliteID: satelliteID,
			Track:       tracks[satelliteID],
		}); packet != nil {
			packets = append(packets, *packet)
		}
	}

	return json.Marshal(packets)
}

func sortedKeys(tracks map[string][]port.EphemerisSample) []string {
	out := make([]string, 0, len(tracks))
	for id := range tracks {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

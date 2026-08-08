package render_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
	"github.com/mhayk/overpass/services/plan-gateway/internal/render"
)

// Golden files, because the OpenAPI document deliberately does not specify CZML.
//
// It says so in as many words: CZML is a Cesium document format with its own
// structure outside JSON Schema, and a partial schema would be a contract that
// looks binding and is not. So the shape is pinned here instead — and a golden
// file is a stronger guarantee than an incomplete declaration precisely because
// it fails on any change at all, including the ones nobody thought to specify.

var update = flag.Bool("update", false, "rewrite the golden files")

var (
	bucketStart = time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	bucketEnd   = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	committedAt = time.Date(2026, 8, 7, 9, 5, 0, 0, time.UTC)
	// Five minutes after the commit, so the rendered lag is a number a reader
	// can check by hand rather than whatever the clock happened to say.
	renderedAt = time.Date(2026, 8, 7, 9, 10, 0, 0, time.UTC)
)

const footprint = `{"type":"Polygon","coordinates":[[[4.01,51.9],[4.19,51.9],[4.19,52],[4.01,52],[4.01,51.9]]]}`

func samplePlan() port.PlanView {
	slew := 12.5
	gap := 41.0
	return port.PlanView{
		PlanID:      "33333333-4444-4555-8666-777777777777",
		SatelliteID: "CAPELLA-14",
		BucketStart: bucketStart,
		BucketEnd:   bucketEnd,
		PlanVersion: 2,
		Superseded:  false,
		Policy:      "GREEDY_BY_BID",
		CommittedAt: committedAt,
		Acquisitions: []port.AcquisitionView{
			{
				AcquisitionID:         "66666666-7777-4888-8999-000000000000",
				PlanID:                "33333333-4444-4555-8666-777777777777",
				RequestID:             "cbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
				CustomerID:            "port-authority-nl",
				SatelliteID:           "CAPELLA-14",
				Mode:                  "STRIPMAP",
				WindowStart:           time.Date(2026, 8, 7, 10, 14, 0, 0, time.UTC),
				WindowEnd:             time.Date(2026, 8, 7, 10, 14, 18, 0, time.UTC),
				Status:                "ACTIVE",
				FootprintGeoJSON:      []byte(footprint),
				SlewTimeFromPreviousS: &slew,
				GapFromPreviousS:      &gap,
				AwardedValueCredits:   12000,
			},
			{
				AcquisitionID:       "76666666-7777-4888-8999-000000000000",
				PlanID:              "33333333-4444-4555-8666-777777777777",
				RequestID:           "dbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
				CustomerID:          "acme-imaging",
				SatelliteID:         "CAPELLA-14",
				Mode:                "SCAN",
				WindowStart:         time.Date(2026, 8, 7, 11, 2, 0, 0, time.UTC),
				WindowEnd:           time.Date(2026, 8, 7, 11, 2, 31, 0, time.UTC),
				Status:              "SUPERSEDED",
				FootprintGeoJSON:    []byte(footprint),
				AwardedValueCredits: 4000,
			},
		},
	}
}

// golden compares against testdata, or rewrites it under -update.
//
// Indented before comparison so a diff is readable. The bytes on the wire are
// compact; a golden file nobody can read is a golden file people re-record
// instead of reviewing, which turns it into a record of whatever the code does.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got, "", "  "); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, got)
	}
	pretty.WriteByte('\n')

	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o600); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
		t.Logf("rewrote %s", path)
		return
	}

	want, err := os.ReadFile(path) //nolint:gosec // a fixed testdata path
	if err != nil {
		t.Fatalf("reading golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), pretty.Bytes()) {
		t.Errorf("%s changed.\n--- want ---\n%s\n--- got ---\n%s", path, want, pretty.String())
	}
}

func TestPlanCZMLGolden(t *testing.T) {
	got, err := render.PlanCZML(samplePlan(), port.Cursor{LastEventAt: committedAt}, renderedAt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	golden(t, "plan.czml.json", got)
}

func TestFootprintsGeoJSONGolden(t *testing.T) {
	got, err := render.FootprintsGeoJSON(samplePlan().Acquisitions, false,
		port.Cursor{LastEventAt: committedAt}, renderedAt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	golden(t, "footprints.geojson.json", got)
}

// TestTheRenderingIsByteStable is what makes the ETag mean anything.
//
// Both renderers marshal through ordered structs rather than maps for exactly
// this reason: Go randomises map iteration per run, so a map-rendered document
// would hash differently on every request and the validator would never match —
// silently, with the endpoint merely appearing to have a very cold cache.
func TestTheRenderingIsByteStable(t *testing.T) {
	plan := samplePlan()
	cursor := port.Cursor{LastEventAt: committedAt}

	first, err := render.PlanCZML(plan, cursor, renderedAt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for i := range 20 {
		again, err := render.PlanCZML(plan, cursor, renderedAt)
		if err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("render %d differed from the first; the ETag would never match", i)
		}
	}
}

// TestAvailabilityIsPerAcquisition is what makes the timeline mean something.
//
// Without it Cesium draws every footprint at every instant and scrubbing does
// nothing — the scene looks populated and conveys no schedule at all.
func TestAvailabilityIsPerAcquisition(t *testing.T) {
	got, err := render.PlanCZML(samplePlan(), port.Cursor{LastEventAt: committedAt}, renderedAt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var packets []map[string]any
	if err := json.Unmarshal(got, &packets); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(packets) != 3 {
		t.Fatalf("got %d packets, want a document packet and two acquisitions", len(packets))
	}
	if packets[0]["id"] != "document" {
		t.Errorf("first packet is %v, want the document packet", packets[0]["id"])
	}
	if _, present := packets[0]["availability"]; present {
		t.Error("the document packet carries an availability; it is not a drawable entity")
	}
	if got := packets[1]["availability"]; got != "2026-08-07T10:14:00Z/2026-08-07T10:14:18Z" {
		t.Errorf("availability = %v, want the acquisition's own window", got)
	}
	if got := packets[2]["availability"]; got != "2026-08-07T11:02:00Z/2026-08-07T11:02:31Z" {
		t.Errorf("availability = %v", got)
	}
}

// TestSupersededAcquisitionsRenderDifferently keeps the one distinction that
// has to survive at a glance. A timeline with fifty acquisitions cannot carry
// fifty labels; colour is what tells live from replaced.
func TestSupersededAcquisitionsRenderDifferently(t *testing.T) {
	got, err := render.PlanCZML(samplePlan(), port.Cursor{LastEventAt: committedAt}, renderedAt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var packets []struct {
		Polygon struct {
			Material struct {
				SolidColor struct {
					Color struct {
						RGBA []int `json:"rgba"`
					} `json:"color"`
				} `json:"solidColor"`
			} `json:"material"`
		} `json:"polygon"`
	}
	if err := json.Unmarshal(got, &packets); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	active := packets[1].Polygon.Material.SolidColor.Color.RGBA
	superseded := packets[2].Polygon.Material.SolidColor.Color.RGBA
	if len(active) != 4 || len(superseded) != 4 {
		t.Fatalf("colours are %v and %v, want RGBA quads", active, superseded)
	}
	if active[3] == superseded[3] {
		t.Errorf("both render at alpha %d; a superseded footprint must recede", active[3])
	}
}

// TestLongitudeComesFirst is the one that would relocate a target to another
// hemisphere and still render happily.
func TestLongitudeComesFirst(t *testing.T) {
	got, err := render.PlanCZML(samplePlan(), port.Cursor{LastEventAt: committedAt}, renderedAt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var packets []struct {
		Polygon struct {
			Positions struct {
				CartographicDegrees []float64 `json:"cartographicDegrees"`
			} `json:"positions"`
		} `json:"polygon"`
	}
	if err := json.Unmarshal(got, &packets); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	degrees := packets[1].Polygon.Positions.CartographicDegrees

	// Five ring positions as [lon, lat, height] triples. The height entries are
	// required even though the polygon is clamped: dropping them shifts every
	// coordinate by one place and puts the footprint somewhere else entirely.
	if len(degrees) != 15 {
		t.Fatalf("got %d values, want 15 (5 positions × lon,lat,height)", len(degrees))
	}
	if degrees[0] != 4.01 || degrees[1] != 51.9 || degrees[2] != 0 {
		t.Errorf("first position = %v, want [4.01, 51.9, 0] — longitude first", degrees[:3])
	}
	// The Netherlands. If these were swapped the point would be off Somalia,
	// which is a perfectly valid coordinate and completely wrong.
	for i := 0; i < len(degrees); i += 3 {
		if degrees[i] > 10 || degrees[i+1] < 40 {
			t.Errorf("position %d = [%v, %v] looks transposed", i/3, degrees[i], degrees[i+1])
		}
	}
}

// TestAnAcquisitionWithNoFootprintIsSkipped covers a legitimate shape.
//
// The contract makes the footprint optional. Emitting a polygon with no
// positions makes Cesium throw and takes the whole scene down with it, so the
// entity has to be absent rather than empty.
func TestAnAcquisitionWithNoFootprintIsSkipped(t *testing.T) {
	plan := samplePlan()
	plan.Acquisitions[1].FootprintGeoJSON = nil

	got, err := render.PlanCZML(plan, port.Cursor{LastEventAt: committedAt}, renderedAt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var packets []map[string]any
	if err := json.Unmarshal(got, &packets); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(packets) != 2 {
		t.Fatalf("got %d packets, want the document and one drawable acquisition", len(packets))
	}
}

func TestUnrenderableGeometryIsAnError(t *testing.T) {
	for _, tc := range []struct{ name, geometry string }{
		{"not json", `not json`},
		{"a point, not a polygon", `{"type":"Point","coordinates":[4,51]}`},
		{"a position with one number", `{"type":"Polygon","coordinates":[[[4]]]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := samplePlan()
			plan.Acquisitions[0].FootprintGeoJSON = []byte(tc.geometry)
			if _, err := render.PlanCZML(plan, port.Cursor{LastEventAt: committedAt}, renderedAt); err == nil {
				t.Fatal("rendered a footprint that is not a usable polygon")
			}
		})
	}
}

// TestAnEmptyCollectionIsStillAFeatureCollection guards the JSON shape.
//
// A nil slice marshals to `null`, and deck.gl iterating over null throws where
// iterating over [] is a no-op. The difference never shows up in Go.
func TestAnEmptyCollectionIsStillAFeatureCollection(t *testing.T) {
	got, err := render.FootprintsGeoJSON(nil, false, port.Cursor{LastEventAt: committedAt}, renderedAt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var collection map[string]any
	if err := json.Unmarshal(got, &collection); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if collection["type"] != "FeatureCollection" {
		t.Errorf("type = %v", collection["type"])
	}
	features, ok := collection["features"].([]any)
	if !ok {
		t.Fatalf("features is %T, want an array even when empty", collection["features"])
	}
	if len(features) != 0 {
		t.Errorf("got %d features from no acquisitions", len(features))
	}
}

// TestTruncationIsReported is the difference between "there is no coverage
// here" and "I stopped counting". A viewport cannot tell them apart on its own.
func TestTruncationIsReported(t *testing.T) {
	got, err := render.FootprintsGeoJSON(samplePlan().Acquisitions, true,
		port.Cursor{LastEventAt: committedAt}, renderedAt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var collection map[string]any
	if err := json.Unmarshal(got, &collection); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if collection["truncated"] != true {
		t.Errorf("truncated = %v, want true", collection["truncated"])
	}
}

// TestStalenessIsOnTheGeoRenderings too. A globe is the surface most likely to
// be left open on a wall display, and the one where "this is current" is
// assumed hardest.
func TestStalenessIsOnTheGeoRenderings(t *testing.T) {
	czml, err := render.PlanCZML(samplePlan(), port.Cursor{LastEventAt: committedAt}, renderedAt)
	if err != nil {
		t.Fatalf("czml: %v", err)
	}
	var packets []struct {
		Properties *struct {
			AsOf       string  `json:"as_of"`
			LagSeconds float64 `json:"lag_seconds"`
		} `json:"properties"`
	}
	if czmlErr := json.Unmarshal(czml, &packets); czmlErr != nil {
		t.Fatalf("not JSON: %v", czmlErr)
	}
	if packets[0].Properties == nil {
		t.Fatal("the document packet carries no properties")
	}
	// renderedAt is five minutes after committedAt.
	if packets[0].Properties.LagSeconds != 300 {
		t.Errorf("lag = %v, want 300", packets[0].Properties.LagSeconds)
	}

	collection, err := render.FootprintsGeoJSON(samplePlan().Acquisitions, false,
		port.Cursor{LastEventAt: committedAt}, renderedAt)
	if err != nil {
		t.Fatalf("geojson: %v", err)
	}
	var fc struct {
		Staleness struct {
			LagSeconds float64 `json:"lag_seconds"`
		} `json:"staleness"`
	}
	if err := json.Unmarshal(collection, &fc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if fc.Staleness.LagSeconds != 300 {
		t.Errorf("lag = %v, want 300", fc.Staleness.LagSeconds)
	}
}

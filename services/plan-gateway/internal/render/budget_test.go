package render_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
	"github.com/mhayk/overpass/services/plan-gateway/internal/render"
)

// Payload size is a first-class constraint, so it is a test rather than a
// paragraph.
//
// A globe that takes eight seconds to populate reads as broken regardless of
// how correct it is. The numbers below are measured by this file and written
// into docs/payload-budget.md by running it with -update; a budget nobody
// measures is a budget that has already been exceeded.

// The ceilings. Deliberately generous against what is measured today — the
// point is to catch a change that makes a document several times larger, not to
// fail on a field being renamed.
// Set from the measurements below, with roughly 10% headroom.
//
// Twice wrong before this. First guessed at 700 and 900 against actual figures
// of 1031 and 1194 — a budget that fails on day one. Then loosened to 1300 and
// 1500, which was the opposite mistake: restoring PostGIS's default nine
// decimal places, a 30% payload regression, sailed straight through. A ceiling
// generous enough never to fire is not a budget, it is a comment.
//
// 10% catches a real regression and tolerates a renamed field. Re-measure and
// move them deliberately rather than nudging them when they complain.
const (
	maxCZMLBytesPerAcquisition = 1150
	maxGeoJSONBytesPerFeature  = 1300
	// A full page at the endpoint's own 250-feature limit. 500 features measured
	// 597 kB, and a globe that parses 600 kB before drawing anything reads as
	// broken however correct it is.
	maxFootprintResponseBytes = 350 << 10

	// The orbit track (#128). Each sample is `[t, lon, lat, height]` — four
	// numbers at the precision the renderer rounds to, plus separators.
	//
	// This is the ceiling that decides the sample interval, because the
	// interval is the only knob: a three-hour bucket at ten seconds is 1080
	// samples and at sixty seconds is 180, and the payload is linear in that.
	// The table below measures all three so the choice is a measurement rather
	// than a preference.
	maxCZMLBytesPerEphemerisSample = 36

	// A whole plan document with a path in it: the thing a globe actually
	// fetches. Set from the measured figure with the same ~10% headroom as the
	// others.
	maxPlanWithTrackBytes = 85 << 10
)

// ephemerisTrack is a plausible LEO arc of `n` samples at `interval`.
//
// The coordinates VARY, and that matters for a size measurement: a repeated
// constant would compress differently and, more to the point, would round to
// far fewer characters than a real track's six decimal places.
func ephemerisTrack(n int, interval time.Duration) []port.EphemerisSample {
	out := make([]port.EphemerisSample, 0, n)
	for i := range n {
		out = append(out, port.EphemerisSample{
			At: bucketStart.Add(time.Duration(i) * interval),
			// Wrapped, because a three-hour track genuinely crosses the
			// antimeridian and a longitude that walks off to 400 degrees would
			// measure a character wider than the real thing.
			LongitudeDeg: math.Mod(4.0+float64(i)*0.612437+180.0, 360.0) - 180.0,
			LatitudeDeg:  82.0 * math.Sin(float64(i)*0.0058178),
			AltitudeM:    693412.83219 + float64(i%17),
		})
	}
	return out
}

// realisticFootprint is a swath polygon with the vertex count the geodesic
// footprint code actually produces, not a four-corner rectangle.
//
// It matters: size is dominated by coordinates, and budgeting against a
// simplified shape would set a ceiling the real thing sails past on its first
// day.
func realisticFootprint(vertices int) []byte {
	out := make([]byte, 0, vertices*26+40)
	out = append(out, []byte(`{"type":"Polygon","coordinates":[[`)...)
	for i := range vertices {
		if i > 0 {
			out = append(out, ',')
		}
		// Six decimal places, matching what the read layer now asks PostGIS for.
		// PostGIS DEFAULTS to nine, and this fixture originally used six against
		// that default — so it measured a payload roughly 30% smaller than the
		// system actually emitted. Measured:
		//
		//   ST_AsGeoJSON(POINT(4.123456789012 51.987654321098))
		//     -> [4.123456789,51.987654321]
		out = append(out, []byte(fmt.Sprintf("[%.6f,%.6f]",
			4.0+float64(i)*0.0013, 51.9+float64(i)*0.0007))...)
	}
	out = append(out, []byte(`]]}`)...)
	return out
}

func acquisitionsOfSize(n, vertices int) []port.AcquisitionView {
	footprint := realisticFootprint(vertices)
	out := make([]port.AcquisitionView, 0, n)
	for i := range n {
		out = append(out, port.AcquisitionView{
			AcquisitionID:       fmt.Sprintf("66666666-7777-4888-8999-%012d", i),
			PlanID:              "33333333-4444-4555-8666-777777777777",
			RequestID:           fmt.Sprintf("cbbbbbbb-cccc-4ddd-8eee-%012d", i),
			CustomerID:          "port-authority-nl",
			SatelliteID:         "CAPELLA-14",
			Mode:                "STRIPMAP",
			WindowStart:         bucketStart.Add(time.Duration(i) * time.Minute),
			WindowEnd:           bucketStart.Add(time.Duration(i)*time.Minute + 18*time.Second),
			Status:              "ACTIVE",
			FootprintGeoJSON:    footprint,
			AwardedValueCredits: 12000,
		})
	}
	return out
}

type measurement struct {
	label      string
	features   int
	vertices   int
	bytes      int
	perFeature int
}

func measureAll(t *testing.T) []measurement {
	t.Helper()
	cursor := port.Cursor{LastEventAt: committedAt}
	var out []measurement

	for _, tc := range []struct {
		label           string
		count, vertices int
		czml            bool
	}{
		{"CZML, one plan, 12 acquisitions, 5-vertex footprints", 12, 5, true},
		{"CZML, one plan, 40 acquisitions, 33-vertex footprints", 40, 33, true},
		{"GeoJSON, 100 footprints, 33 vertices", 100, 33, false},
		{"GeoJSON, 250 footprints (the limit), 33 vertices", 250, 33, false},
	} {
		acquisitions := acquisitionsOfSize(tc.count, tc.vertices)

		var body []byte
		var err error
		if tc.czml {
			plan := samplePlan()
			plan.Acquisitions = acquisitions
			body, err = render.PlanCZML(plan, cursor, renderedAt)
		} else {
			body, err = render.FootprintsGeoJSON(acquisitions, false, cursor, renderedAt)
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		out = append(out, measurement{
			label: tc.label, features: tc.count, vertices: tc.vertices,
			bytes: len(body), perFeature: len(body) / tc.count,
		})
	}

	return append(out, measureEphemeris(t)...)
}

// measureEphemeris is what turns the sample interval from a preference into a
// decision.
//
// A three-hour bucket is the unit the CZML endpoint serves, and the interval is
// the only knob on its size — the payload is linear in the sample count. So the
// three candidate intervals are measured against the same bucket, and the
// argument for the one chosen is in
// docs/decisions/0016-ephemeris-sampling-and-horizon.md rather than in a
// comment here.
func measureEphemeris(t *testing.T) []measurement {
	t.Helper()
	cursor := port.Cursor{LastEventAt: committedAt}
	var out []measurement

	for _, tc := range []struct {
		label    string
		interval time.Duration
	}{
		{"CZML, a 3-hour orbit track at 10 s (the chosen interval)", 10 * time.Second},
		{"CZML, the same track at 30 s", 30 * time.Second},
		{"CZML, the same track at 60 s", 60 * time.Second},
	} {
		samples := int((3 * time.Hour) / tc.interval)

		// Measured as the DIFFERENCE the track makes to a document, not as a
		// document of its own. What a client pays for the orbit is what the
		// path adds to the plan it was already fetching.
		bare := samplePlan()
		bare.Acquisitions = acquisitionsOfSize(40, 33)
		withTrack := bare
		withTrack.Track = ephemerisTrack(samples, tc.interval)

		before, err := render.PlanCZML(bare, cursor, renderedAt)
		if err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		after, err := render.PlanCZML(withTrack, cursor, renderedAt)
		if err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}

		out = append(out, measurement{
			label: tc.label, features: samples,
			bytes: len(after) - len(before), perFeature: (len(after) - len(before)) / samples,
		})
	}
	return out
}

func TestPayloadsStayWithinBudget(t *testing.T) {
	for _, m := range measureAll(t) {
		t.Logf("%-52s %7d bytes  (%d per feature)", m.label, m.bytes, m.perFeature)
	}

	cursor := port.Cursor{LastEventAt: committedAt}

	plan := samplePlan()
	plan.Acquisitions = acquisitionsOfSize(40, 33)
	czml, err := render.PlanCZML(plan, cursor, renderedAt)
	if err != nil {
		t.Fatalf("czml: %v", err)
	}
	if per := len(czml) / 40; per > maxCZMLBytesPerAcquisition {
		t.Errorf("CZML is %d bytes per acquisition, over the %d budget", per, maxCZMLBytesPerAcquisition)
	}

	collection, err := render.FootprintsGeoJSON(acquisitionsOfSize(250, 33), true, cursor, renderedAt)
	if err != nil {
		t.Fatalf("geojson: %v", err)
	}
	if per := len(collection) / 250; per > maxGeoJSONBytesPerFeature {
		t.Errorf("GeoJSON is %d bytes per feature, over the %d budget", per, maxGeoJSONBytesPerFeature)
	}
	// The one a client actually feels: a full page at the endpoint's own limit.
	if len(collection) > maxFootprintResponseBytes {
		t.Errorf("a full footprints page is %d bytes, over the %d budget",
			len(collection), maxFootprintResponseBytes)
	}

	// The orbit track, at the interval actually shipped.
	withTrack := samplePlan()
	withTrack.Acquisitions = acquisitionsOfSize(40, 33)
	withTrack.Track = ephemerisTrack(int((3*time.Hour)/(10*time.Second)), 10*time.Second)
	document, err := render.PlanCZML(withTrack, cursor, renderedAt)
	if err != nil {
		t.Fatalf("czml with track: %v", err)
	}
	if per := (len(document) - len(czml)) / len(withTrack.Track); per > maxCZMLBytesPerEphemerisSample {
		t.Errorf("an ephemeris sample costs %d bytes, over the %d budget",
			per, maxCZMLBytesPerEphemerisSample)
	}
	if len(document) > maxPlanWithTrackBytes {
		t.Errorf("a plan document with an orbit track is %d bytes, over the %d budget",
			len(document), maxPlanWithTrackBytes)
	}
}

// TestTheBudgetDocumentIsCurrent keeps docs/payload-budget.md honest.
//
// Regenerated with -update, and compared otherwise. A budget written once and
// never re-measured is a number that describes the code as it was on the day
// somebody typed it.
func TestTheBudgetDocumentIsCurrent(t *testing.T) {
	measurements := measureAll(t)

	body := "<!-- Generated by services/plan-gateway/internal/render/budget_test.go.\n" +
		"     Run `go test ./internal/render/... -update` to refresh. -->\n\n" +
		"# Geo payload budget\n\n" +
		"Payload size is a first-class constraint for the geo endpoints: a globe\n" +
		"that takes eight seconds to populate reads as broken regardless of how\n" +
		"correct it is.\n\n" +
		"These are measured, not estimated. The table is rewritten from the same\n" +
		"test that enforces the ceilings, so it cannot drift from what the code\n" +
		"actually emits.\n\n" +
		"| Rendering | Bytes | Per feature |\n|---|---:|---:|\n"
	for _, m := range measurements {
		body += fmt.Sprintf("| %s | %d | %d |\n", m.label, m.bytes, m.perFeature)
	}
	body += fmt.Sprintf(`
## Ceilings

| Limit | Value |
|---|---:|
| CZML bytes per acquisition | %d |
| GeoJSON bytes per feature | %d |
| A full footprints page (250 features) | %d |
| CZML bytes per ephemeris sample | %d |
| A plan document carrying a 3-hour orbit track | %d |

Deliberately generous against what is measured above. The point is to catch a
change that makes a document several times larger, not to fail when a field is
renamed.

## The sample interval is the ephemeris knob

The orbit track is the largest single thing the globe loads, and its size is
linear in one number: how often the satellite is sampled. The table above
measures the same three-hour bucket at three intervals so the choice is a
measurement rather than a preference.

**Ten seconds is what ships.** A LEO SAR satellite covers about 66 km of ground
track in ten seconds. Cesium interpolates between samples, and the error of that
interpolation grows with the gap — at sixty seconds the samples are 400 km apart
and the drawn curve visibly cuts the corner where the track turns hardest, which
for a sun-synchronous constellation is over the poles, where it spends most of
its time. Six times the payload buys a path that is right where it is most
looked at. The argument in full, including what would make us revisit it, is in
[ADR-0016](decisions/0016-ephemeris-sampling-and-horizon.md).

## What keeps these numbers down

**Conditional requests.** Both endpoints emit a strong `+"`ETag`"+` and answer `+"`304`"+`
to a matching `+"`If-None-Match`"+`. A globe polls the same horizon every few seconds
and the answer only changes when a plan is committed, so in the steady state the
cost is a header exchange rather than a document. This is the largest single
lever and it is the reason the budget above is affordable at all.

**A bounded collection.** `+"`/v1/geo/footprints`"+` caps at 250 features and reports
`+"`truncated`"+` rather than silently cutting. An unbounded geometry endpoint is a
denial of service waiting for a client to ask for a year.

**Second-resolution timestamps in CZML.** Cesium accepts fractional seconds, but
nine decimal places on every interval is measurable size for precision no viewer
displays — and acquisition windows are scheduled to the second anyway.

**Ephemeris samples as offsets, rounded.** A sample is
`+"`[seconds_after_epoch, lon, lat, height_m]`"+` rather than a timestamped object:
field names would be most of the payload at this cardinality, and an RFC 3339
timestamp per sample costs more than the position it labels. Coordinates are
rounded to six decimal places (~0.1 m, the same precision the read layer asks
PostGIS for on footprints) and heights to whole metres. Measured: full float64
precision costs 49 bytes per sample against 33, about 50%% more, for digits no
viewer can display and a propagator whose own error is metres cannot justify.

**No path without ephemeris.** A plan whose bucket the ephemeris sweep has not
reached renders footprints and a clock and nothing else. That is a correctness
decision rather than a size one — interpolating a path through footprint
centroids would draw a curve that looks like an orbit and is not one — but it is
also why a plan document without a track stays small.

## What is not measured here

Compression. Every one of these documents is highly repetitive JSON and gzip
takes roughly a fifth of it, so the wire cost in front of a real proxy is well
under these figures. The budget is deliberately stated uncompressed: it is the
number that does not depend on somebody else's configuration.
`, maxCZMLBytesPerAcquisition, maxGeoJSONBytesPerFeature, maxFootprintResponseBytes,
		maxCZMLBytesPerEphemerisSample, maxPlanWithTrackBytes)

	// internal/render -> services/plan-gateway -> services -> repo root.
	// Four, not five: the package moved up a level out of internal/adapter and
	// this path came with it.
	path := filepath.Join("..", "..", "..", "..", "docs", "payload-budget.md")
	if *update {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing the budget document: %v", err)
		}
		t.Logf("rewrote %s", path)
		return
	}

	want, err := os.ReadFile(path) //nolint:gosec // a fixed repo-relative path
	if err != nil {
		t.Fatalf("reading the budget document (run with -update to create): %v", err)
	}
	if normalise(string(want)) != normalise(body) {
		t.Errorf("docs/payload-budget.md is out of date; run the geo tests with -update")
	}
}

func normalise(s string) string {
	out := make([]byte, 0, len(s))
	for i := range len(s) {
		if s[i] != '\r' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

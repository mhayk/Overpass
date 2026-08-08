package app

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhayk/overpass/gen/go/events"

	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
)

// This file writes the bytes this service would actually publish into
// testdata/published-events/, where scripts/contracts-validate.sh validates them
// against the JSON Schema.
//
// That indirection is the whole point. Go's type system cannot enforce a
// contract — CLAUDE.md says so, and #124 is what it looks like when you assume
// otherwise: the old builder was a hand-written map that compiled, vetted,
// linted, and produced an event with nine schema violations. Every test agreed
// with it because every test built the payload with the same helper it asserted
// against.
//
// The schema is the authority. Getting the producer's real output in front of
// it is the only check that could have failed.

var update = flag.Bool("update", false, "rewrite the published-event fixtures")

var fixedTime = time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)

const (
	fixedEventID       = "11111111-2222-4333-8444-555555555555"
	fixedRequestID     = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	fixedCorrelationID = "66666666-7777-4888-8999-aaaaaaaaaaaa"
)

func pointRequest() domain.SubmitRequest {
	return domain.SubmitRequest{
		CustomerID:     "acme-imaging",
		Target:         domain.Target{Kind: domain.TargetPoint, Point: domain.Position{Lon: 0, Lat: 0}},
		WindowStart:    fixedTime.Add(time.Hour),
		WindowEnd:      fixedTime.Add(25 * time.Hour),
		PriorityTier:   "BEST_EFFORT",
		BidCredits:     0,
		RequestedModes: []string{"SCAN"},
	}
}

func polygonRequest() domain.SubmitRequest {
	minIncidence := 20.0
	maxIncidence := 40.0
	return domain.SubmitRequest{
		CustomerID: "port-authority-nl",
		TargetName: "Port of Rotterdam",
		Target: domain.Target{
			Kind: domain.TargetPolygon,
			Ring: []domain.Position{
				{Lon: 4.02, Lat: 51.92}, {Lon: 4.18, Lat: 51.92},
				{Lon: 4.18, Lat: 51.99}, {Lon: 4.02, Lat: 51.99},
				{Lon: 4.02, Lat: 51.92},
			},
		},
		WindowStart:    fixedTime,
		WindowEnd:      fixedTime.Add(7 * 24 * time.Hour),
		PriorityTier:   "COMMERCIAL",
		BidCredits:     12000,
		RequestedModes: []string{"SCAN", "STRIPMAP"},
		Constraints: domain.RequestConstraints{
			LookSide:        "RIGHT",
			MinIncidenceDeg: &minIncidence,
			MaxIncidenceDeg: &maxIncidence,
		},
	}
}

// publishedFixture writes what the service would publish, for the schema
// validator to check, and compares against it otherwise.
func publishedFixture(t *testing.T, name string, payload []byte) {
	t.Helper()

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, payload, "", "  "); err != nil {
		t.Fatalf("the published payload is not JSON: %v\n%s", err, payload)
	}
	pretty.WriteByte('\n')

	dir := filepath.Join("..", "..", "testdata", "published-events",
		"tasking.request.received.v1")
	path := filepath.Join(dir, name)

	if *update {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o600); err != nil {
			t.Fatalf("writing the fixture: %v", err)
		}
		t.Logf("rewrote %s", path)
		return
	}

	want, err := os.ReadFile(path) //nolint:gosec // a fixed testdata path
	if err != nil {
		t.Fatalf("reading the fixture (run with -update to create): %v", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), pretty.Bytes()) {
		t.Errorf("the published event changed shape.\n--- fixture ---\n%s\n--- now ---\n%s",
			want, pretty.String())
	}
}

func TestPublishedPointEventMatchesItsFixture(t *testing.T) {
	payload, err := buildReceivedEvent(fixedEventID, fixedRequestID, fixedCorrelationID,
		pointRequest(), fixedTime)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	publishedFixture(t, "point-target.json", payload)
}

func TestPublishedPolygonEventMatchesItsFixture(t *testing.T) {
	payload, err := buildReceivedEvent(fixedEventID, fixedRequestID, fixedCorrelationID,
		polygonRequest(), fixedTime)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	publishedFixture(t, "polygon-target.json", payload)
}

// TestThePublishedEventIsAnEnvelope is the #124 regression, asserted here as
// well as by the schema validator.
//
// The validator is the authority and runs in a different job; this fails in the
// same test run as the change, which is where a developer is actually looking.
func TestThePublishedEventIsAnEnvelope(t *testing.T) {
	payload, err := buildReceivedEvent(fixedEventID, fixedRequestID, fixedCorrelationID,
		pointRequest(), fixedTime)
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("not JSON: %v", err)
	}

	// Every field the schema requires at the top level. The old builder had
	// none of them and shipped.
	for _, field := range []string{
		"event_id", "event_type", "schema_version", "occurred_at",
		"correlation_id", "causation_id", "producer", "data",
	} {
		if _, present := envelope[field]; !present {
			t.Errorf("%s is missing; feasibility-service terms a message it cannot "+
				"deduplicate and plan-gateway refuses one it cannot decode", field)
		}
	}

	if envelope["event_id"] != fixedEventID {
		t.Errorf("event_id = %v, want the id the outbox row carries", envelope["event_id"])
	}
	if envelope["event_type"] != "tasking.request.received.v1" {
		t.Errorf("event_type = %v", envelope["event_type"])
	}
	if envelope["causation_id"] != nil {
		t.Errorf("causation_id = %v, want null — a submission is the root of the tree",
			envelope["causation_id"])
	}

	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is %T, want an object", envelope["data"])
	}
	if _, present := data["target"]; !present {
		t.Error("data.target is missing — the thing the whole request is about")
	}
}

// TestTheEnvelopeIdMatchesTheOutboxRow guards a subtle version of the same bug.
//
// The two ids were generated independently before, so the outbox row and the
// payload disagreed about the event's identity. The broker dedups on the row's,
// consumers dedup on the payload's, and the two would drift apart silently.
func TestTheEnvelopeIdMatchesTheOutboxRow(t *testing.T) {
	payload, err := buildReceivedEvent(fixedEventID, fixedRequestID, fixedCorrelationID,
		pointRequest(), fixedTime)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	var decoded events.TaskingRequestReceived
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("the published event does not decode into its own generated type: %v", err)
	}
	if string(decoded.EventId) != fixedEventID {
		t.Errorf("EventId = %q, want %q", decoded.EventId, fixedEventID)
	}
}

// TestThePublishedEventDecodesStrictly is the same check gen/go/contracttest
// applies to fixtures, aimed at the producer.
//
// DisallowUnknownFields is what makes additionalProperties:false observable in
// Go. It cannot catch a MISSING required field — which is exactly why the
// schema validator has to see these bytes too.
func TestThePublishedEventDecodesStrictly(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  domain.SubmitRequest
	}{
		{"point", pointRequest()},
		{"polygon", polygonRequest()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := buildReceivedEvent(fixedEventID, fixedRequestID,
				fixedCorrelationID, tc.req, fixedTime)
			if err != nil {
				t.Fatalf("building: %v", err)
			}
			decoder := json.NewDecoder(bytes.NewReader(payload))
			decoder.DisallowUnknownFields()
			var decoded events.TaskingRequestReceived
			if err := decoder.Decode(&decoded); err != nil {
				t.Fatalf("strict decode failed: %v\n%s", err, payload)
			}
		})
	}
}

// TestLongitudeComesFirstInTheTarget. A swap is silent and relocates the target.
func TestLongitudeComesFirstInTheTarget(t *testing.T) {
	req := pointRequest()
	req.Target.Point = domain.Position{Lon: 4.4, Lat: 51.9}

	payload, err := buildReceivedEvent(fixedEventID, fixedRequestID, fixedCorrelationID,
		req, fixedTime)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	var envelope struct {
		Data struct {
			Target struct {
				Type        string    `json:"type"`
				Coordinates []float64 `json:"coordinates"`
			} `json:"target"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if envelope.Data.Target.Type != "Point" {
		t.Errorf("target type = %q", envelope.Data.Target.Type)
	}
	if len(envelope.Data.Target.Coordinates) != 2 {
		t.Fatalf("coordinates = %v", envelope.Data.Target.Coordinates)
	}
	// 4.4E 51.9N is the Netherlands. Transposed it is off Somalia — a
	// perfectly valid coordinate and completely wrong.
	if envelope.Data.Target.Coordinates[0] != 4.4 || envelope.Data.Target.Coordinates[1] != 51.9 {
		t.Errorf("coordinates = %v, want [4.4, 51.9] — longitude first",
			envelope.Data.Target.Coordinates)
	}
}

// TestOptionalFieldsAreAbsentRatherThanEmpty.
//
// "" is a label somebody typed; nothing is a label nobody gave. An empty
// constraints object and no constraints object mean the same to the planner,
// and only one costs bytes on every message.
func TestOptionalFieldsAreAbsentRatherThanEmpty(t *testing.T) {
	payload, err := buildReceivedEvent(fixedEventID, fixedRequestID, fixedCorrelationID,
		pointRequest(), fixedTime)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, field := range []string{"target_name", "constraints"} {
		if _, present := envelope.Data[field]; present {
			t.Errorf("%s is present on a request that did not supply one", field)
		}
	}
}

func TestATargetWithNoKindIsRefused(t *testing.T) {
	req := pointRequest()
	req.Target.Kind = ""
	if _, err := buildReceivedEvent(fixedEventID, fixedRequestID, fixedCorrelationID,
		req, fixedTime); err == nil {
		t.Fatal("published an event with no target rather than failing")
	}
}

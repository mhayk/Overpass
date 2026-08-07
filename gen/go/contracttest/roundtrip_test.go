// Package contracttest checks that the generated Go types actually agree with
// the contract fixtures.
//
// This is not a test of the generators. It is a test of the claim the whole
// contracts-first approach rests on: that a payload the JSON Schema accepts is
// a payload the Go types can hold, and one it rejects is one they refuse. The
// Python side of the same claim is checked by scripts/contracts-smoke.sh.
//
// It lives outside gen/go/events on purpose — `make contracts-generate` deletes
// and rewrites that directory, so a test file placed there would vanish.
package contracttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhayk/overpass/gen/go/events"
)

// fixtureRoot resolves contracts/examples relative to this file, so the test
// works from any working directory.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "contracts", "examples"))
	if err != nil {
		t.Fatalf("resolving fixture root: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("fixture root %s: %v", root, err)
	}
	return root
}

// decoderFor maps a fixture directory name (which is the event type) onto a
// function that strictly decodes into the corresponding generated struct.
//
// Strictly: DisallowUnknownFields is what makes the additionalProperties:false
// contract observable in Go. Without it, an undeclared field is silently
// dropped and the negative fixture proving strictness would pass for the wrong
// reason — the decode would "succeed" by ignoring the very thing under test.
var decoders = map[string]func(*json.Decoder) error{
	"tasking.request.received.v1": func(d *json.Decoder) error {
		var v events.TaskingRequestReceived
		return d.Decode(&v)
	},
	"tasking.request.rejected.v1": func(d *json.Decoder) error {
		var v events.TaskingRequestRejected
		return d.Decode(&v)
	},
	"planning.plan.committed.v1": func(d *json.Decoder) error {
		var v events.PlanCommitted
		return d.Decode(&v)
	},
	"planning.request.unfulfilled.v1": func(d *json.Decoder) error {
		var v events.PlanningRequestUnfulfilled
		return d.Decode(&v)
	},
}

func walkFixtures(t *testing.T, expectation string, fn func(name, eventType string, raw []byte)) {
	t.Helper()
	dir := filepath.Join(fixtureRoot(t), expectation)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		fn(filepath.Base(path), filepath.Base(filepath.Dir(path)), raw)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
}

// TestValidFixturesDecode asserts that every payload the schema accepts also
// fits the generated Go types — the forward direction of the contract.
func TestValidFixturesDecode(t *testing.T) {
	walkFixtures(t, "valid", func(name, eventType string, raw []byte) {
		t.Run(eventType+"/"+name, func(t *testing.T) {
			decode, known := decoders[eventType]
			if !known {
				t.Skipf("no generated decoder wired for %s", eventType)
			}
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := decode(dec); err != nil {
				t.Fatalf("valid fixture failed to decode into the generated type: %v", err)
			}
		})
	})
}

// TestInvalidFixturesAreRejected covers the reverse direction, but only for the
// subset of violations Go's type system can express.
//
// Being precise about the gap matters. encoding/json enforces structure and
// undeclared fields; it does NOT enforce enum membership, numeric bounds, or
// string patterns, because the generator emits those as plain named string and
// int types. So `unknown-tier` and `plan-version-zero` decode cleanly here even
// though the schema rejects them — and that is correct behaviour, not a bug.
//
// The consequence is a real architectural statement: the JSON Schema is the
// authority on validity, and services validate against it at the boundary. The
// Go types are a convenience for handling data already known to be valid. A
// reader who assumed "it compiles, therefore it is valid" would be wrong, so
// this test documents exactly where that assumption breaks.
func TestInvalidFixturesAreRejected(t *testing.T) {
	// Violations Go can catch by itself.
	structural := map[string]bool{
		"undeclared-extra-field.json":     true, // additionalProperties: false
		"missing-required-data.json":      false,
		"unknown-tier.json":               false,
		"plan-version-zero.json":          false,
		"empty-requested-modes.json":      false,
		"longitude-out-of-range.json":     false,
		"latitude-longitude-swapped.json": false,
		"wrong-event-type.json":           false,
	}

	walkFixtures(t, "invalid", func(name, eventType string, raw []byte) {
		t.Run(eventType+"/"+name, func(t *testing.T) {
			decode, known := decoders[eventType]
			if !known {
				t.Skipf("no generated decoder wired for %s", eventType)
			}
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			err := decode(dec)

			mustFail, listed := structural[name]
			if !listed {
				t.Fatalf("fixture %s is not classified — add it to the structural map "+
					"and state whether Go alone can reject it", name)
			}
			if mustFail && err == nil {
				t.Fatalf("expected the generated type to reject %s, but it decoded cleanly", name)
			}
			if !mustFail && err != nil {
				t.Logf("note: Go also rejected %s (%v) — schema validation is still "+
					"the authority, this is a bonus", name, err)
			}
		})
	})
}

// TestEnumConstantsMatchTheContract pins the generated enum constants.
//
// These are the values that travel over the wire and get persisted. If a
// generator upgrade or a schema edit renamed one, every consumer would break
// and the drift check alone would not say why — it reports that bytes changed,
// not that a wire value changed. This test names the ones that matter.
func TestEnumConstantsMatchTheContract(t *testing.T) {
	cases := []struct {
		got  string
		want string
	}{
		{string(events.ImagingModeSPOTLIGHT), "SPOTLIGHT"},
		{string(events.ImagingModeSTRIPMAP), "STRIPMAP"},
		{string(events.ImagingModeSCAN), "SCAN"},
		{string(events.LookSideLEFT), "LEFT"},
		{string(events.LookSideRIGHT), "RIGHT"},
		{string(events.PriorityTierGOVERNMENT), "GOVERNMENT"},
		{string(events.PriorityTierCIVILPROTECTION), "CIVIL_PROTECTION"},
		{string(events.PriorityTierCOMMERCIAL), "COMMERCIAL"},
		{string(events.PriorityTierBESTEFFORT), "BEST_EFFORT"},
		{string(events.TleStalenessFRESH), "FRESH"},
		{string(events.TleStalenessAGING), "AGING"},
		{string(events.TleStalenessSTALE), "STALE"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("wire value drifted: got %q, contract says %q", c.got, c.want)
		}
	}
}

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Events are built by loading a contract example and patching the ids.
//
// Not by hand. A hand-built payload is a payload that agrees with whatever the
// test expects, which is precisely how #112 shipped: every unit test in #26
// constructed its input from the same Go struct it decoded into, so the field
// names always matched themselves and the suite could not fail even though
// nothing decoded. Starting from a file the contract owns removes that freedom.

func loadExample(t *testing.T, subject, name string) map[string]any {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	path := filepath.Join(root, "contracts", "examples", "valid", subject, name)
	raw, err := os.ReadFile(path) //nolint:gosec // a repo-relative path built by the test
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s is not JSON: %v", path, err)
	}
	return out
}

func data(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	d, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatal("example has no data object")
	}
	return d
}

func encode(t *testing.T, envelope map[string]any) []byte {
	t.Helper()
	out, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return out
}

func rfc3339(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

func (f fixture) receivedEvent(t *testing.T, at time.Time) (string, []byte) {
	t.Helper()
	e := loadExample(t, "tasking.request.received.v1", "polygon-target.json")
	e["occurred_at"] = rfc3339(at)
	d := data(t, e)
	d["request_id"] = f.requestID
	d["window"] = map[string]any{
		"start": rfc3339(f.bucketStart),
		"end":   rfc3339(f.bucketStart.Add(6 * time.Hour)),
	}
	d["submitted_at"] = rfc3339(at)
	return "tasking.request.received.v1", encode(t, e)
}

// opportunitiesEvent trims or repeats the example's opportunities to reach n.
//
// Repeating with fresh ids rather than inventing shapes: the shape stays the
// contract's, only the identity changes.
func (f fixture) opportunitiesEvent(t *testing.T, at time.Time, n int) (string, []byte) {
	t.Helper()
	e := loadExample(t, "feasibility.opportunities.computed.v1", "two-passes.json")
	e["occurred_at"] = rfc3339(at)
	d := data(t, e)
	d["request_id"] = f.requestID
	d["computed_at"] = rfc3339(at)

	template, ok := d["opportunities"].([]any)
	if !ok || len(template) == 0 {
		t.Fatal("the example has no opportunities to copy")
	}
	out := make([]any, 0, n)
	for i := range n {
		src, ok := template[i%len(template)].(map[string]any)
		if !ok {
			t.Fatal("opportunity is not an object")
		}
		clone := map[string]any{}
		for k, v := range src {
			clone[k] = v
		}
		clone["opportunity_id"] = f.derivedID('a', i)
		clone["satellite_id"] = f.satelliteID
		clone["access_window"] = map[string]any{
			"start": rfc3339(f.bucketStart.Add(time.Duration(i) * time.Hour)),
			"end":   rfc3339(f.bucketStart.Add(time.Duration(i)*time.Hour + 10*time.Minute)),
		}
		out = append(out, clone)
	}
	d["opportunities"] = out
	d["opportunity_count"] = n
	// tle_references name the satellite this fixture uses, or the event
	// contradicts itself.
	if refs, ok := d["tle_references"].([]any); ok {
		for _, r := range refs {
			if ref, ok := r.(map[string]any); ok {
				ref["satellite_id"] = f.satelliteID
			}
		}
	}
	return "feasibility.opportunities.computed.v1", encode(t, e)
}

func (f fixture) planEvent(t *testing.T, at time.Time, version int) (string, []byte) {
	t.Helper()
	e := loadExample(t, "planning.plan.committed.v1", "supersedes-earlier-version.json")
	e["occurred_at"] = rfc3339(at)
	d := data(t, e)
	d["plan_id"] = f.derivedID('b', version)
	d["satellite_id"] = f.satelliteID
	d["plan_version"] = version
	d["committed_at"] = rfc3339(at)
	d["bucket"] = map[string]any{
		"start": rfc3339(f.bucketStart),
		"end":   rfc3339(f.bucketStart.Add(3 * time.Hour)),
	}
	if version == 1 {
		// A first version supersedes nothing. Leaving the example's value in
		// would point at a plan that does not exist.
		d["supersedes_plan_id"] = nil
	} else {
		d["supersedes_plan_id"] = f.derivedID('b', version-1)
	}

	acqs, ok := d["acquisitions"].([]any)
	if !ok || len(acqs) == 0 {
		t.Fatal("the example has no acquisitions")
	}
	acq, ok := acqs[0].(map[string]any)
	if !ok {
		t.Fatal("acquisition is not an object")
	}
	acq["acquisition_id"] = f.derivedID('c', version)
	acq["request_id"] = f.requestID
	acq["opportunity_id"] = f.derivedID('a', 0)
	acq["window"] = map[string]any{
		"start": rfc3339(f.bucketStart),
		"end":   rfc3339(f.bucketStart.Add(8 * time.Second)),
	}
	return "planning.plan.committed.v1", encode(t, e)
}

// derivedID builds a schema-valid uuid that is unique to this fixture and
// deterministic within it.
//
// Both halves matter. Deterministic, so plan version 2 can name version 1 as
// its predecessor without the test threading ids around. Fixture-scoped,
// because it is not — two sub-runs of the same test shared a plan id and the
// second one died on plan_views_pkey while the first one's rows were still
// there, which read as "the projection dropped an event" rather than "the test
// reused an id".
//
// The nonce is a real uuid, so the version and variant nibbles are already
// correct; only the final node is replaced.
func (f fixture) derivedID(kind byte, n int) string {
	const hex = "0123456789abcdef"
	node := []byte("000000000000")
	node[0] = kind
	for i := len(node) - 1; i > 0 && n > 0; i-- {
		node[i] = hex[n%16]
		n /= 16
	}
	return f.nonce[:len(f.nonce)-12] + string(node)
}

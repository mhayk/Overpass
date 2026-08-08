package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The vertical slice, end to end, through every real component.
//
// This is the test M1 was for and could not have passed until #131: a customer
// request submitted over HTTP, published through tasking-api's outbox, swept by
// a real SGP4 propagation of the real seeded constellation, published back
// through feasibility's outbox, and projected into the read model the globe
// reads. Before #131 the worker's handler did no domain work at all, so the
// chain stopped at the second hop and nothing downstream ever existed.
//
// Nothing here is stubbed. A fake propagation would assert that this file and
// the handler agree about a dictionary; what is in doubt is whether a request
// the API really publishes becomes opportunities the gateway can really read.

// A high-latitude target, because the seeded constellation is near-polar and
// passes over Svalbard many times a day. A mid-latitude target makes the test a
// coin flip on the horizon, and a flaky end-to-end test is one people disable.
const (
	svalbardLon = 15.6267
	svalbardLat = 78.2232
)

func TestASubmittedRequestBecomesOpportunitiesOnTheReadModel(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}

	gw, _ := gateway(t)
	seedCustomer(t)
	seedConstellation(t, root)
	startFeasibilityWorker(t, root)

	api, err := start(env.taskingAPIBin, "tasking-api", nil)
	if err != nil {
		t.Fatalf("starting tasking-api: %v", err)
	}
	t.Cleanup(func() {
		if killErr := api.Kill(); killErr != nil {
			t.Errorf("killing tasking-api: %v", killErr)
		}
	})

	requestID := submitSweepRequest(t, api)

	// The whole chain, given time for four hops: the API's relay, the worker's
	// SGP4 sweep, feasibility's relay, and the gateway's projector. The sweep is
	// the slow one — a real constellation over a real day.
	eventually(t, "the opportunities to reach the read model", 120*time.Second, func() bool {
		return opportunityRows(t, requestID) > 0
	})

	body := getJSON(t, gw.baseURL+"/v1/requests/"+requestID+"/opportunities")
	items, ok := body["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("the gateway served no opportunities: %v", body)
	}

	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("opportunity is not an object: %v", items[0])
	}
	// The three things a viewer needs and the planner allocates over. Each was
	// unreachable before #131 for a different reason: no event carried them, no
	// projection stored them, and the ids were not even UUIDs.
	if _, present := first["footprint"]; !present {
		t.Error("the opportunity carries no footprint; the globe has nothing to draw")
	}
	if _, err := uuid.Parse(fmt.Sprint(first["opportunity_id"])); err != nil {
		t.Errorf("opportunity_id %v is not a uuid: %v", first["opportunity_id"], err)
	}
	if satellite := fmt.Sprint(first["satellite_id"]); !isSeededSatellite(t, satellite) {
		t.Errorf("satellite_id %q does not join to reference.satellites", satellite)
	}

	// And the request's own state follows from the projection rather than from
	// a message: an opportunity existing is what makes it AWAITING_PLANNING.
	view := getJSON(t, gw.baseURL+"/v1/requests/"+requestID)
	if state := fmt.Sprint(view["state"]); state != "AWAITING_PLANNING" {
		t.Errorf("request state is %q, want AWAITING_PLANNING once candidates exist", state)
	}
}

// seedConstellation puts the frozen snapshot into reference.*.
//
// The harness migrates and does not seed, which is correct for every other test
// in this package — they build the rows they read. This one propagates real
// element sets, and an empty reference schema makes the sweep refuse with
// TLE_UNAVAILABLE rather than compute anything. Found by writing this test:
// the first run timed out with a worker that had done exactly what it was told.
//
// Through scripts/seed.sh rather than a Go reimplementation, so the constellation
// under test is the one the demo and the golden tests use — ADR-0011.
func seedConstellation(t *testing.T, root string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "scripts/seed.sh")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "DATABASE_URL="+env.dsn)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding the constellation: %v\n%s", err, output)
	}
}

// isSeededSatellite is the check that catches a satellite_id built from a
// Celestrak display name — "CAPELLA-11 (ACADIA-1)" joins to nothing and the
// contract's own pattern rejects it.
func isSeededSatellite(t *testing.T, satelliteID string) bool {
	t.Helper()
	var count int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM reference.satellites WHERE satellite_id = $1`,
		satelliteID).Scan(&count); err != nil {
		t.Fatalf("checking the satellite id: %v", err)
	}
	return count == 1
}

// submitSweepRequest posts a request the sweep can actually satisfy.
//
// A 24-hour window over Svalbard, in STRIPMAP and SPOTLIGHT. The window is
// anchored to now rather than to a fixed date because the element sets are
// checked for staleness against the clock, and a request in 2026 swept in 2027
// is refused rather than computed — correctly, and uselessly for this test.
func submitSweepRequest(t *testing.T, api *service) string {
	t.Helper()

	start := time.Now().UTC().Add(time.Minute)
	body := map[string]any{
		"customer_id":     traceCustomerID,
		"target_name":     "Svalbard",
		"target":          map[string]any{"type": "Point", "coordinates": []float64{svalbardLon, svalbardLat}},
		"window":          map[string]any{"start": rfc3339(start), "end": rfc3339(start.Add(24 * time.Hour))},
		"priority_tier":   "BEST_EFFORT",
		"bid_credits":     0,
		"requested_modes": []string{"STRIPMAP", "SPOTLIGHT"},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the request: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		api.baseURL+"/v1/tasking-requests", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("submitting: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only

	var accepted map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		t.Fatalf("submit returned %d: %v", resp.StatusCode, accepted)
	}
	return fmt.Sprint(accepted["request_id"])
}

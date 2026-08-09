package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The bulkhead: ingress and background work do not share a connection pool.
//
// Two pools that are separate in the code and identical on the wire would be a
// bulkhead in name only, so this asserts against pg_stat_activity — the same
// view an operator would read while wondering which half is holding the
// connections.
//
// What this does NOT claim: that background work can starve ingress in this
// topology today. It cannot be shown from outside, because the relay and the
// submit path write the same table, so anything that blocks one blocks the
// other. The separation is the mechanism; the starvation it prevents is the one
// that arrives with the first read endpoint or the first slow background job,
// and the bulkhead is cheaper to have before that than after.

func connectionsNamed(t *testing.T, applicationName string) int {
	t.Helper()
	return rowCount(t, `SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, applicationName)
}

func TestIngressAndBackgroundWorkHoldSeparatePools(t *testing.T) {
	seedCustomer(t)

	api, err := start(env.taskingAPIBin, "tasking-api", map[string]string{
		"DB_INGRESS_MAX_CONNS":    "3",
		"DB_BACKGROUND_MAX_CONNS": "2",
	})
	if err != nil {
		t.Fatalf("starting tasking-api: %v", err)
	}
	t.Cleanup(func() {
		if killErr := api.Kill(); killErr != nil {
			t.Errorf("killing tasking-api: %v", killErr)
		}
	})

	// A submission, so the ingress pool has opened at least one connection —
	// pgxpool connects lazily, and asserting before any traffic would measure
	// the absence of work rather than the presence of a pool.
	if status, _ := submitStatus(t, api, uuid.NewString(), chaosRequestBody(t)); status != http.StatusAccepted {
		t.Fatalf("submit returned %d, want 202 before measuring anything", status)
	}

	if !waitFor(15*time.Second, func() bool {
		return connectionsNamed(t, "overpass-tasking-api-ingress") > 0 &&
			connectionsNamed(t, "overpass-tasking-api-background") > 0
	}) {
		t.Fatalf("expected both pools to be connected; ingress=%d background=%d — "+
			"one pool serving both paths is the coupling #51 removes",
			connectionsNamed(t, "overpass-tasking-api-ingress"),
			connectionsNamed(t, "overpass-tasking-api-background"))
	}

	// Sized independently. A single pool renamed twice would pass the check
	// above; it cannot pass this one, because the caps differ.
	if got := connectionsNamed(t, "overpass-tasking-api-background"); got > 2 {
		t.Errorf("%d background connections against a cap of 2 — the background pool is not the one it says it is", got)
	}
	if got := connectionsNamed(t, "overpass-tasking-api-ingress"); got > 3 {
		t.Errorf("%d ingress connections against a cap of 3", got)
	}
}

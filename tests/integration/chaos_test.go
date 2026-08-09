package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// The #48 chaos claim: duplicates injected at EVERY hop leave the end state
// identical.
//
// Each hop has its own dedup mechanism, and each mechanism has its own way of
// silently not existing: the ingress key could be stored and never checked,
// the broker's dedup window could be missing from the topology, a consumer
// ledger could be claiming on the wrong column. The per-hop tests prove each
// mechanism in isolation; this test proves they COMPOSE — that a message
// duplicated at the ingress, at the relay, and at redelivery all at once still
// converges to the state one clean delivery would have produced.
//
// The duplicates are injected AFTER a clean pass has settled, which is the
// adversarial ordering: every consumer's ledger already holds a claim, every
// read-model row already exists, and anything not idempotent gets a second
// chance to do visible damage.
func TestDuplicatesInjectedAtEveryHopLeaveTheEndStateIdentical(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}

	gw, gwPrefix := gateway(t)
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

	// ---- Hop 1: the HTTP ingress. The same body, the same Idempotency-Key,
	// submitted twice — a client retrying a request whose response it lost.
	key := uuid.NewString()
	first := submitChaosRequest(t, api, key)
	second := submitChaosRequest(t, api, key)
	if first != second {
		t.Fatalf("a retried submission created a second request: %s then %s", first, second)
	}
	requestID := first

	if got := outboxEvents(t, "tasking.outbox", requestID); got != 1 {
		t.Fatalf("%d request events in the outbox after a retried submission, want 1", got)
	}

	// One clean pass through the whole pipeline, and a settled baseline.
	eventually(t, "the opportunities to reach the read model", 120*time.Second, func() bool {
		return opportunityRows(t, requestID) > 0
	})
	baselineOpportunities := opportunityRows(t, requestID)
	baselineView := getJSON(t, gw.baseURL+"/v1/requests/"+requestID)
	baselineSweeps := outboxEvents(t, "feasibility.outbox", requestID)

	// ---- Hop 2: the relay -> broker hop. The relay's crash window is "published
	// but not yet marked", and its recovery is publishing the same event again.
	// Replayed twice: once with the SAME Nats-Msg-Id (the broker's dedup window
	// must absorb it) and once with a FRESH one (the broker cannot help, and the
	// feasibility worker's ledger must). The payload comes from the outbox row
	// itself — the real bytes, not this test's idea of them.
	replayOutboxEvent(t, "tasking.outbox", requestID, "tasking.request.received.v1")

	// ---- Hop 3: the feasibility -> consumers hop, same two injections. The
	// gateway's projector has already folded this event once; its ledger claim
	// is what stands between the replay and a doubled read model.
	replayOutboxEvent(t, "feasibility.outbox", requestID, "feasibility.opportunities.computed.v1")

	// The replays are consumed when both consumers report nothing pending and
	// nothing awaiting ack — the broker's own account, not a guessed sleep.
	eventually(t, "the worker to absorb the replayed request event", 30*time.Second,
		drained("TASKING", "feasibility-worker"))
	eventually(t, "the projector to absorb the replayed opportunities event", 30*time.Second,
		drained("FEASIBILITY", gwPrefix+"-feasibility"))
	// Then a settle window, so any second sweep or doubled fold that IS coming
	// has time to land before the assertions below would forgive it.
	time.Sleep(3 * time.Second)

	// ---- The end state is identical: nothing doubled, nothing re-swept.
	if got := opportunityRows(t, requestID); got != baselineOpportunities {
		t.Errorf("opportunity rows went from %d to %d under duplicate delivery", baselineOpportunities, got)
	}
	if got := outboxEvents(t, "feasibility.outbox", requestID); got != baselineSweeps {
		t.Errorf("the sweep ran again: %d feasibility events, want %d", got, baselineSweeps)
	}
	if got := outboxEvents(t, "tasking.outbox", requestID); got != 1 {
		t.Errorf("%d request events in the tasking outbox, want 1", got)
	}
	view := getJSON(t, gw.baseURL+"/v1/requests/"+requestID)
	for _, field := range []string{"state", "opportunity_count"} {
		if fmt.Sprint(view[field]) != fmt.Sprint(baselineView[field]) {
			t.Errorf("request %s changed under duplicate delivery: %v -> %v",
				field, baselineView[field], view[field])
		}
	}
}

// submitChaosRequest posts the sweep request with a caller-owned Idempotency-Key,
// so the test can submit the same logical request twice.
func submitChaosRequest(t *testing.T, api *service, idempotencyKey string) string {
	t.Helper()

	start := time.Now().UTC().Truncate(time.Second).Add(time.Minute)
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
	req.Header.Set("Idempotency-Key", idempotencyKey)

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

// outboxEvents counts a request's events in one service's outbox. Both outbox
// tables carry the request id inside the payload's data object.
func outboxEvents(t *testing.T, table, requestID string) int {
	t.Helper()
	var n int
	//nolint:gosec // table is one of two literals owned by this file
	query := `SELECT count(*) FROM ` + table + ` WHERE payload->'data'->>'request_id' = $1`
	if err := env.pool.QueryRow(context.Background(), query, requestID).Scan(&n); err != nil {
		t.Fatalf("counting %s events: %v", table, err)
	}
	return n
}

// drained reports whether a durable consumer has consumed and settled
// everything the stream holds for it.
func drained(stream, durable string) func() bool {
	return func() bool {
		info, err := env.js.ConsumerInfo(stream, durable)
		if err != nil {
			return false
		}
		return info.NumPending == 0 && info.NumAckPending == 0
	}
}

// replayOutboxEvent re-publishes a stored event twice: once with the same
// Nats-Msg-Id the relay used (broker dedup must absorb it) and once with a
// fresh one (only the consumer's ledger can).
func replayOutboxEvent(t *testing.T, table, requestID, eventType string) {
	t.Helper()

	var eventID string
	var payload []byte
	var occurredAt time.Time
	//nolint:gosec // table is one of two literals owned by this file
	query := `SELECT event_id, payload, occurred_at FROM ` + table + `
		WHERE payload->'data'->>'request_id' = $1 AND event_type = $2
		ORDER BY id LIMIT 1`
	if err := env.pool.QueryRow(context.Background(), query, requestID, eventType).
		Scan(&eventID, &payload, &occurredAt); err != nil {
		t.Fatalf("reading %s from %s: %v", eventType, table, err)
	}

	for _, msgID := range []string{eventID, uuid.NewString()} {
		msg := &nats.Msg{
			Subject: eventType,
			Data:    payload,
			Header: nats.Header{
				"Nats-Msg-Id": []string{msgID},
				"Occurred-At": []string{occurredAt.UTC().Format(time.RFC3339Nano)},
			},
		}
		if _, err := env.js.PublishMsg(msg); err != nil {
			t.Fatalf("replaying %s: %v", eventType, err)
		}
	}
}

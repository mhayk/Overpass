package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// The delivery guarantees, against a real broker, a real database, and the
// real service binaries.
//
// Mocks here would assert that our understanding of the infrastructure is
// self-consistent, which is not the thing in doubt. What is in doubt is whether
// Postgres and JetStream behave the way the code assumes when a message arrives
// twice, arrives late, or arrives while the previous attempt is half-committed.

// gateway starts a plan-gateway bound to this test's own durable consumers.
//
// Its own consumers, not the shared gateway-projector-* set: two tests pulling
// from one durable consumer would each see half the messages, and which half
// would depend on timing.
func gateway(t *testing.T) (*service, string) {
	t.Helper()
	prefix := "itest-" + uuid.NewString()[:8]
	declareGatewayConsumers(t, prefix)

	svc, err := start(env.planGatewayBin, "plan-gateway", map[string]string{
		"PLAN_GATEWAY_DURABLE_PREFIX": prefix,
		"FETCH_WAIT":                  "1",
		"FETCH_BATCH":                 "16",
	})
	if err != nil {
		t.Fatalf("starting plan-gateway: %v", err)
	}
	t.Cleanup(func() {
		if killErr := svc.Kill(); killErr != nil {
			t.Errorf("killing plan-gateway: %v", killErr)
		}
		cleanupGatewayConsumers(t, prefix)
	})
	return svc, prefix
}

func declareGatewayConsumers(t *testing.T, prefix string) {
	t.Helper()
	for _, c := range consumers {
		if c.name[:len("gateway-projector")] != "gateway-projector" {
			continue
		}
		cfg := &nats.ConsumerConfig{
			Durable:       prefix + c.name[len("gateway-projector"):],
			DeliverPolicy: nats.DeliverNewPolicy, // only what this test publishes
			AckPolicy:     nats.AckExplicitPolicy,
			AckWait:       c.ackWait,
			MaxDeliver:    c.maxDeliver,
			MaxAckPending: c.maxPending,
			ReplayPolicy:  nats.ReplayInstantPolicy,
		}
		if len(c.filters) == 1 {
			cfg.FilterSubject = c.filters[0]
		} else {
			cfg.FilterSubjects = c.filters
		}
		if _, err := env.js.AddConsumer(c.stream, cfg); err != nil {
			t.Fatalf("declaring %s/%s: %v", c.stream, cfg.Durable, err)
		}
	}
}

func cleanupGatewayConsumers(t *testing.T, prefix string) {
	t.Helper()
	for _, c := range consumers {
		if c.name[:len("gateway-projector")] != "gateway-projector" {
			continue
		}
		durable := prefix + c.name[len("gateway-projector"):]
		if err := env.js.DeleteConsumer(c.stream, durable); err != nil {
			t.Logf("deleting %s/%s: %v", c.stream, durable, err)
		}
	}
}

// fixture is one isolated scenario. Every id is fresh, so tests sharing a
// database do not share state and can run without a per-test container.
type fixture struct {
	requestID   string
	satelliteID string
	bucketStart time.Time

	// nonce scopes every derived id to this fixture. See derivedID.
	nonce string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{
		requestID:   uuid.NewString(),
		satelliteID: "ITEST-" + uuid.NewString()[:8],
		bucketStart: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		nonce:       uuid.NewString(),
	}
	t.Cleanup(func() { f.cleanup(t) })
	return f
}

// cleanup removes only this fixture's rows. A blanket delete would take out the
// rows of every other test in the package.
func (f fixture) cleanup(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, st := range []struct {
		sql string
		arg any
	}{
		{`DELETE FROM readmodel.acquisition_views WHERE satellite_id = $1`, f.satelliteID},
		{`DELETE FROM readmodel.plan_views WHERE satellite_id = $1`, f.satelliteID},
		{`DELETE FROM readmodel.opportunity_views WHERE request_id = $1`, f.requestID},
		{`DELETE FROM readmodel.request_views WHERE request_id = $1`, f.requestID},
	} {
		if _, err := env.pool.Exec(ctx, st.sql, st.arg); err != nil {
			t.Errorf("cleanup %q: %v", st.sql, err)
		}
	}
}

// publish sends one event with the headers the outbox relay would set.
func publish(t *testing.T, subject string, payload []byte, occurredAt time.Time) string {
	t.Helper()
	eventID := uuid.NewString()
	msg := &nats.Msg{
		Subject: subject,
		Data:    payload,
		Header: nats.Header{
			"Nats-Msg-Id": []string{eventID},
			"Occurred-At": []string{occurredAt.UTC().Format(time.RFC3339Nano)},
		},
	}
	if _, err := env.js.PublishMsg(msg); err != nil {
		t.Fatalf("publishing %s: %v", subject, err)
	}
	return eventID
}

// eventually polls until the condition holds. Projection is asynchronous by
// design; a fixed sleep is either flaky on CI or wasted time on a laptop.
func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitFor is eventually() without the fatal, so the caller can report detail.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

func planCount(t *testing.T, f fixture) int {
	t.Helper()
	return rowCount(t, `SELECT count(*) FROM readmodel.plan_views WHERE satellite_id = $1`, f.satelliteID)
}

func rowCount(t *testing.T, query string, arg any) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(context.Background(), query, arg).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	return n
}

func opportunityRows(t *testing.T, requestID string) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM readmodel.opportunity_views WHERE request_id = $1`,
		requestID).Scan(&n); err != nil {
		t.Fatalf("counting opportunities: %v", err)
	}
	return n
}

// TestDuplicateDeliveryProducesExactlyOneStateChange is the effectively-once
// claim, tested rather than asserted.
//
// Redelivery is routine: an ack lost on the wire, an ack_wait that expires
// while a fold is still running, a consumer restarted mid-batch. If folding
// twice doubled a count, the read model would drift further from the truth
// every time the network hiccupped, and nothing would report it.
func TestDuplicateDeliveryProducesExactlyOneStateChange(t *testing.T) {
	gw, _ := gateway(t)
	f := newFixture(t)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	subject, payload := f.receivedEvent(t, at)
	publish(t, subject, payload, at)

	oppSubject, oppPayload := f.opportunitiesEvent(t, at.Add(time.Minute), 3)
	// The SAME payload four times. Different Nats-Msg-Ids, because JetStream's
	// own dedup window would otherwise absorb them at the broker and the fold
	// would never be asked to be idempotent — which is the thing under test.
	for range 4 {
		publish(t, oppSubject, oppPayload, at.Add(time.Minute))
	}

	eventually(t, "the opportunities to project", 30*time.Second, func() bool {
		return opportunityRows(t, f.requestID) >= 3
	})
	// Settle: give any further duplicate a chance to do damage before asserting.
	time.Sleep(2 * time.Second)

	if got := opportunityRows(t, f.requestID); got != 3 {
		t.Fatalf("%d opportunity rows after four identical deliveries, want 3", got)
	}

	body := getJSON(t, gw.baseURL+"/v1/requests/"+f.requestID)
	count, ok := body["opportunity_count"].(float64)
	if !ok || count != 3 {
		t.Fatalf("opportunity_count = %v after four deliveries, want 3", body["opportunity_count"])
	}
}

// TestOutOfOrderDeliveryConverges is the property that actually matters.
//
// JetStream orders per subject; it does not order across subjects, and
// redelivery is out of band entirely. So a plan can arrive before the request
// it belongs to, and version 1 can arrive after version 2. Both orderings must
// land in the same state, or the read model is a function of the network rather
// than of the events.
func TestOutOfOrderDeliveryConverges(t *testing.T) {
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	run := func(t *testing.T, reversed bool) string {
		t.Helper()
		gw, _ := gateway(t)
		f := newFixture(t)

		type event struct {
			subject string
			payload []byte
			at      time.Time
		}
		s1, p1 := f.receivedEvent(t, at)
		s2, p2 := f.opportunitiesEvent(t, at.Add(1*time.Minute), 2)
		s3, p3 := f.planEvent(t, at.Add(2*time.Minute), 1)
		s4, p4 := f.planEvent(t, at.Add(3*time.Minute), 2)
		events := []event{
			{s1, p1, at},
			{s2, p2, at.Add(1 * time.Minute)},
			{s3, p3, at.Add(2 * time.Minute)},
			{s4, p4, at.Add(3 * time.Minute)},
		}
		if reversed {
			for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
				events[i], events[j] = events[j], events[i]
			}
		}
		for _, e := range events {
			publish(t, e.subject, e.payload, e.at)
			// Published one at a time and confirmed, so "reversed" means
			// reversed arrival and not merely reversed publish order.
			time.Sleep(300 * time.Millisecond)
		}

		if !waitFor(30*time.Second, func() bool { return planCount(t, f) == 2 }) {
			t.Fatalf("only %d of 2 plan versions projected (reversed=%t)\n"+
				"requests=%d opportunities=%d acquisitions=%d\n--- gateway log ---\n%s",
				planCount(t, f), reversed,
				rowCount(t, `SELECT count(*) FROM readmodel.request_views WHERE request_id = $1`, f.requestID),
				rowCount(t, `SELECT count(*) FROM readmodel.opportunity_views WHERE request_id = $1`, f.requestID),
				rowCount(t, `SELECT count(*) FROM readmodel.acquisition_views WHERE satellite_id = $1`, f.satelliteID),
				gw.logs.String())
		}
		time.Sleep(2 * time.Second)

		return observable(t, f)
	}

	forward := run(t, false)
	reversed := run(t, true)

	if forward != reversed {
		t.Fatalf("arrival order changed the outcome:\n  in order %s\n  reversed %s", forward, reversed)
	}
}

// observable renders the parts of the state a client can see, with the
// fixture's random ids left out so two runs are comparable.
func observable(t *testing.T, f fixture) string {
	t.Helper()
	ctx := context.Background()

	var state string
	var opportunities int
	if err := env.pool.QueryRow(ctx,
		`SELECT state, opportunity_count FROM readmodel.request_views WHERE request_id = $1`,
		f.requestID).Scan(&state, &opportunities); err != nil {
		t.Fatalf("reading request: %v", err)
	}

	var topVersion int
	var supersededTop bool
	if err := env.pool.QueryRow(ctx, `
		SELECT plan_version, superseded FROM readmodel.plan_views
		WHERE satellite_id = $1 ORDER BY plan_version DESC LIMIT 1
	`, f.satelliteID).Scan(&topVersion, &supersededTop); err != nil {
		t.Fatalf("reading plan: %v", err)
	}

	var live, dead int
	if err := env.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status <> 'SUPERSEDED'),
		       count(*) FILTER (WHERE status =  'SUPERSEDED')
		FROM readmodel.acquisition_views WHERE satellite_id = $1
	`, f.satelliteID).Scan(&live, &dead); err != nil {
		t.Fatalf("reading acquisitions: %v", err)
	}

	return fmt.Sprintf("state=%s opportunities=%d top_version=%d top_superseded=%t live=%d superseded=%d",
		state, opportunities, topVersion, supersededTop, live, dead)
}

// TestAProjectorKilledMidStreamLosesNothing is the crash-safety scenario, and
// the reason these tests drive real processes.
//
// SIGKILL, not a graceful stop: the process gets no chance to ack what it has
// folded or to finish what it has not. On restart, every event must be
// accounted for — the unacked ones redelivered and folded, the acked ones not
// folded twice.
func TestAProjectorKilledMidStreamLosesNothing(t *testing.T) {
	prefix := "itest-" + uuid.NewString()[:8]
	declareGatewayConsumers(t, prefix)
	t.Cleanup(func() { cleanupGatewayConsumers(t, prefix) })

	f := newFixture(t)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	first, err := start(env.planGatewayBin, "plan-gateway", map[string]string{
		"PLAN_GATEWAY_DURABLE_PREFIX": prefix,
		"FETCH_WAIT":                  "1",
		"FETCH_BATCH":                 "4",
	})
	if err != nil {
		t.Fatalf("starting plan-gateway: %v", err)
	}

	subject, payload := f.receivedEvent(t, at)
	publish(t, subject, payload, at)
	oppSubject, oppPayload := f.opportunitiesEvent(t, at.Add(time.Minute), 5)
	publish(t, oppSubject, oppPayload, at.Add(time.Minute))

	// Kill as soon as it has started folding, so some work is done and some is
	// not. Waiting for completion first would test a clean restart, which is
	// not the interesting case.
	eventually(t, "the projector to start folding", 30*time.Second, func() bool {
		return rowCount(t, `SELECT count(*) FROM readmodel.request_views WHERE request_id = $1`,
			f.requestID) == 1
	})
	if killErr := first.Kill(); killErr != nil {
		t.Fatalf("killing: %v", killErr)
	}

	// More events arrive while nothing is running. They must not be lost:
	// that is what a durable consumer is for.
	planSubject, planPayload := f.planEvent(t, at.Add(2*time.Minute), 1)
	publish(t, planSubject, planPayload, at.Add(2*time.Minute))

	second, err := start(env.planGatewayBin, "plan-gateway", map[string]string{
		"PLAN_GATEWAY_DURABLE_PREFIX": prefix,
		"FETCH_WAIT":                  "1",
		"FETCH_BATCH":                 "4",
	})
	if err != nil {
		t.Fatalf("restarting plan-gateway: %v", err)
	}
	t.Cleanup(func() {
		if killErr := second.Kill(); killErr != nil {
			t.Errorf("killing the restarted projector: %v", killErr)
		}
	})

	eventually(t, "the plan published during the outage to project", 45*time.Second, func() bool {
		return planCount(t, f) == 1
	})
	time.Sleep(2 * time.Second)

	// Nothing lost.
	if got := opportunityRows(t, f.requestID); got != 5 {
		t.Errorf("%d opportunities after a kill and restart, want 5 — events were lost", got)
	}
	// And nothing doubled: a redelivered batch must fold to the same state.
	var acqs int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM readmodel.acquisition_views WHERE satellite_id = $1`,
		f.satelliteID).Scan(&acqs); err != nil {
		t.Fatalf("counting acquisitions: %v", err)
	}
	if acqs != 1 {
		t.Errorf("%d acquisitions after a kill and restart, want 1 — redelivery was folded twice", acqs)
	}
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %d: %v", url, resp.StatusCode, body)
	}
	return body
}

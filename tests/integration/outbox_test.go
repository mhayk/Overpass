package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// The outbox relay, restarted mid-drain.
//
// This is the claim the whole persistence design rests on: a state change and
// its event commit together or not at all, and the event reaches the broker
// exactly once however many times the relay dies trying. Everything else in the
// system assumes it.

// enqueue writes an unpublished outbox row, the way tasking-api would inside
// the transaction that persisted the request.
func enqueue(t *testing.T, subject string, occurredAt time.Time) string {
	t.Helper()
	eventID := uuid.NewString()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO tasking.outbox
			(event_id, event_type, schema_version, subject, payload, headers, occurred_at)
		VALUES ($1, $2, '1.0.0', $2, $3::jsonb, '{}'::jsonb, $4)
	`, eventID, subject,
		`{"outbox_test":true,"event_id":"`+eventID+`"}`,
		occurredAt); err != nil {
		t.Fatalf("enqueueing: %v", err)
	}
	return eventID
}

func unpublishedCount(t *testing.T, eventIDs []string) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tasking.outbox
		 WHERE event_id = ANY($1::uuid[]) AND published_at IS NULL`,
		eventIDs).Scan(&n); err != nil {
		t.Fatalf("counting unpublished: %v", err)
	}
	return n
}

// collect drains a subject into a map of event id to delivery count.
//
// An ephemeral consumer from the start of the stream, so it sees every copy of
// every message — including any the relay published twice. Binding to a durable
// would let an earlier test's cursor hide exactly what this one is looking for.
func collect(t *testing.T, subject string, want int, timeout time.Duration) map[string]int {
	t.Helper()
	sub, err := env.js.SubscribeSync(subject, nats.DeliverAll(), nats.AckNone())
	if err != nil {
		t.Fatalf("subscribing to %s: %v", subject, err)
	}
	t.Cleanup(func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Logf("unsubscribing: %v", err)
		}
	})

	seen := map[string]int{}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msg, err := sub.NextMsg(500 * time.Millisecond)
		if err != nil {
			if len(seen) >= want {
				break
			}
			continue
		}
		seen[msg.Header.Get("Nats-Msg-Id")]++
	}
	return seen
}

// TestTheRelayPublishesEveryPendingEventExactlyOnceAcrossARestart is the one
// that matters.
//
// SIGKILL mid-drain, not a graceful stop. A relay that only survives a clean
// shutdown has not been tested against the case it exists for: the process
// dying between publishing a message and recording that it did.
//
// Both halves are asserted, and they fail in opposite directions. Nothing lost
// means every row ends up published; nothing duplicated means each event
// reaches the stream once. A relay that marked rows published before sending
// would pass the second and fail the first; one that never marked them would
// pass the first and fail the second.
func TestTheRelayPublishesEveryPendingEventExactlyOnceAcrossARestart(t *testing.T) {
	const subject = "tasking.request.received.v1"
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	// Enough that the relay cannot finish before the kill lands.
	//
	// The number is derived, not guessed: outbox.DefaultConfig drains 32 rows
	// every 250ms, so 1200 rows take roughly 38 batches — about nine seconds of
	// draining for the kill to land inside. Forty rows finished in under a
	// second and the test silently exercised restart-AFTER-drain instead, which
	// is the easy case and not the one being claimed.
	const events = 1200
	ids := make([]string, 0, events)
	for i := range events {
		ids = append(ids, enqueue(t, subject, at.Add(time.Duration(i)*time.Second)))
	}
	t.Cleanup(func() {
		if _, err := env.pool.Exec(context.Background(),
			`DELETE FROM tasking.outbox WHERE event_id = ANY($1::uuid[])`, ids); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	watcher := collect(t, subject, events, 45*time.Second)
	_ = watcher // started before the relay, so nothing is missed

	first, err := start(env.taskingAPIBin, "tasking-api", nil)
	if err != nil {
		t.Fatalf("starting tasking-api: %v", err)
	}

	// Kill once it has published SOME but not all. Waiting for zero remaining
	// would be testing a clean run; waiting for none published would be testing
	// a start-up failure.
	if !waitFor(30*time.Second, func() bool { return unpublishedCount(t, ids) < events }) {
		_ = first.Kill() //nolint:errcheck // already failing
		t.Fatalf("the relay published nothing in 30s\n%s", first.logs.String())
	}
	remainingAtKill := unpublishedCount(t, ids)
	if killErr := first.Kill(); killErr != nil {
		t.Fatalf("killing: %v", killErr)
	}

	// Reported, not asserted. The relay drains in batches and can finish all of
	// them before the kill lands, in which case this run exercised a restart
	// AFTER the drain rather than during it. Asserting on it would be flaky;
	// saying nothing would let the test claim a guarantee it did not exercise.
	if remainingAtKill == 0 {
		t.Logf("the relay finished before the kill landed: this run tested "+
			"restart-after-drain, not restart-mid-drain (%d events)", events)
	} else {
		t.Logf("killed mid-drain with %d of %d events still unpublished", remainingAtKill, events)
	}

	second, err := start(env.taskingAPIBin, "tasking-api", nil)
	if err != nil {
		t.Fatalf("restarting tasking-api: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Kill(); err != nil {
			t.Errorf("killing the restarted relay: %v", err)
		}
	})

	if !waitFor(60*time.Second, func() bool { return unpublishedCount(t, ids) == 0 }) {
		t.Fatalf("%d of %d events never published after the restart\n%s",
			unpublishedCount(t, ids), events, second.logs.String())
	}

	// Nothing duplicated. Every delivery is counted, so a second publish of the
	// same event shows up as a count of two.
	delivered := collect(t, subject, events, 20*time.Second)
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var missing, duplicated []string
	for id := range wanted {
		switch n := delivered[id]; {
		case n == 0:
			missing = append(missing, id)
		case n > 1:
			duplicated = append(duplicated, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d events were marked published but never reached the stream: %v",
			len(missing), missing[:min(3, len(missing))])
	}
	if len(duplicated) > 0 {
		t.Errorf("%d events reached the stream more than once: %v",
			len(duplicated), duplicated[:min(3, len(duplicated))])
	}
}

// TestARelayThatCannotReachTheBrokerLeavesTheRowsAlone is the other side of the
// outbox bargain.
//
// Acceptance survives a broker outage: the request is persisted and the event
// waits. A relay that marked rows published on a failed send would turn a
// recoverable delay into permanent data loss, and a relay that refused to start
// would turn it into refused customer traffic.
func TestARelayThatCannotReachTheBrokerLeavesTheRowsAlone(t *testing.T) {
	const subject = "tasking.request.received.v1"
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

	ids := []string{enqueue(t, subject, at), enqueue(t, subject, at.Add(time.Second))}
	t.Cleanup(func() {
		if _, err := env.pool.Exec(context.Background(),
			`DELETE FROM tasking.outbox WHERE event_id = ANY($1::uuid[])`, ids); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// Port 1 refuses. The service must still come up and serve.
	svc, err := start(env.taskingAPIBin, "tasking-api", map[string]string{
		"NATS_URL": "nats://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("tasking-api refused to start without a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := svc.Kill(); err != nil {
			t.Errorf("killing: %v", err)
		}
	})

	time.Sleep(3 * time.Second)
	if got := unpublishedCount(t, ids); got != len(ids) {
		t.Fatalf("%d of %d rows are still unpublished; the relay marked rows it never sent",
			got, len(ids))
	}
}

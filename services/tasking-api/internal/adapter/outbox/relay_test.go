package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/outbox"
)

// Everything here runs against a real Postgres, because what is under test is
// FOR UPDATE SKIP LOCKED, a transaction that spans a network call, and a row
// that survives a failed publish. None of those are properties of Go code.
//
// The publisher is a stub rather than a broker, which is the opposite choice
// from the feasibility relay's tests — and deliberate. There the question was
// "does the event reach the stream"; here it is "what happens to the row when
// publishing fails", and a stub can fail on command where a broker cannot.

func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv("OVERPASS_TEST_DSN")
	if v == "" {
		t.Skip("set OVERPASS_TEST_DSN to run the relay integration tests")
	}
	return v
}

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(t.Context(), dsn(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	t.Cleanup(func() {
		if _, err := p.Exec(context.Background(),
			`DELETE FROM tasking.outbox WHERE event_type = 'relaytest.v1'`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return p
}

func discardLog() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// stubPublisher records what it was asked to send and can be told to fail.
type stubPublisher struct {
	mu       sync.Mutex
	sent     []sentMessage
	failWith error
}

type sentMessage struct {
	subject string
	payload []byte
	headers map[string]string
}

func (s *stubPublisher) Publish(subject string, data []byte, headers map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	s.sent = append(s.sent, sentMessage{subject, data, headers})
	return nil
}

// byID finds what was published for one event.
//
// Indexed by id rather than by position, because `go test` runs packages in
// parallel and the postgres package writes to the same outbox table. Asserting
// on sent[0] made this suite fail intermittently against correct code — the
// first message was simply somebody else's row.
func (s *stubPublisher) byID(t *testing.T, eventID string) sentMessage {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.sent {
		if m.headers["Nats-Msg-Id"] == eventID {
			return m
		}
	}
	t.Fatalf("event %s was never published", eventID)
	return sentMessage{}
}

// countOurs counts only the ids this test enqueued.
func (s *stubPublisher) countOurs(ids ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	n := 0
	for _, m := range s.sent {
		if wanted[m.headers["Nats-Msg-Id"]] {
			n++
		}
	}
	return n
}

func enqueue(t *testing.T, p *pgxpool.Pool, headers map[string]string) string {
	t.Helper()
	eventID := uuid.NewString()
	raw, err := json.Marshal(headers)
	if err != nil {
		t.Fatalf("marshalling headers: %v", err)
	}
	if _, err := p.Exec(t.Context(), `
		INSERT INTO tasking.outbox
			(event_id, event_type, schema_version, subject, payload, headers, occurred_at)
		VALUES ($1, 'relaytest.v1', '1.0.0', 'tasking.request.received.v1',
		        '{"probe":true}'::jsonb, $2::jsonb, now())
	`, eventID, string(raw)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return eventID
}

func unpublished(t *testing.T, p *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := p.QueryRow(t.Context(),
		`SELECT count(*) FROM tasking.outbox WHERE event_type = 'relaytest.v1' AND published_at IS NULL`,
	).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	return n
}

func TestAnEnqueuedEventIsPublishedAndMarked(t *testing.T) {
	p := pool(t)
	id := enqueue(t, p, nil)
	publisher := &stubPublisher{}
	relay := outbox.New(p, publisher, outbox.DefaultConfig(), discardLog())

	_, failed, err := relay.DrainOnce(t.Context())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if failed != 0 {
		t.Fatalf("failed=%d", failed)
	}
	if publisher.countOurs(id) != 1 {
		t.Fatalf("our event was published %d times", publisher.countOurs(id))
	}
	if unpublished(t, p) != 0 {
		t.Fatal("the row was published but not marked, so it will go again forever")
	}
}

func TestAPublishedEventIsNotPublishedTwice(t *testing.T) {
	p := pool(t)
	id := enqueue(t, p, nil)
	publisher := &stubPublisher{}
	relay := outbox.New(p, publisher, outbox.DefaultConfig(), discardLog())

	if _, _, err := relay.DrainOnce(t.Context()); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if _, _, err := relay.DrainOnce(t.Context()); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if publisher.countOurs(id) != 1 {
		t.Fatalf("the event was published %d times", publisher.countOurs(id))
	}
}

func TestTheEventIdIsStableAcrossRetries(t *testing.T) {
	// The whole basis of consumer-side deduplication. An id minted per publish
	// attempt would make every retry a new event, and every consumer would
	// process it again.
	p := pool(t)
	eventID := enqueue(t, p, nil)
	publisher := &stubPublisher{failWith: errors.New("broker down")}
	relay := outbox.New(p, publisher, outbox.DefaultConfig(), discardLog())

	_, failed, err := relay.DrainOnce(t.Context())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if failed < 1 {
		t.Fatal("the failing publish was not counted")
	}

	publisher.failWith = nil
	if _, _, err := relay.DrainOnce(t.Context()); err != nil {
		t.Fatalf("retry drain: %v", err)
	}
	if publisher.countOurs(eventID) != 1 {
		t.Fatalf("published %d times after one failure and one success",
			publisher.countOurs(eventID))
	}
	if got := publisher.byID(t, eventID).headers["Nats-Msg-Id"]; got != eventID {
		t.Fatalf("Nats-Msg-Id is %q, want the stable event id %q", got, eventID)
	}
}

func TestAFailedPublishLeavesTheRowAndCountsTheAttempt(t *testing.T) {
	// A row dropped on the first network blip is an event that, as far as
	// everyone downstream is concerned, never happened.
	p := pool(t)
	enqueue(t, p, nil)
	relay := outbox.New(p, &stubPublisher{failWith: errors.New("connection refused")},
		outbox.DefaultConfig(), discardLog())

	published, failed, err := relay.DrainOnce(t.Context())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if published != 0 || failed < 1 {
		t.Fatalf("published=%d failed=%d", published, failed)
	}
	if unpublished(t, p) != 1 {
		t.Fatal("a failed publish was marked published")
	}

	var attempts int
	var lastError *string
	if err := p.QueryRow(t.Context(),
		`SELECT attempts, last_error FROM tasking.outbox
		 WHERE event_type = 'relaytest.v1' ORDER BY id DESC LIMIT 1`,
	).Scan(&attempts, &lastError); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if attempts != 1 || lastError == nil {
		t.Fatalf("attempts=%d last_error=%v", attempts, lastError)
	}
}

func TestTheTraceparentSurvivesToThePublish(t *testing.T) {
	// Captured at WRITE time and carried here. Capturing it in the relay would
	// attribute the event to the poll loop and sever the trace at exactly the
	// hop this project claims to preserve.
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	p := pool(t)
	id := enqueue(t, p, map[string]string{"traceparent": traceparent, "X-Correlation-Id": "corr-1"})
	publisher := &stubPublisher{}
	relay := outbox.New(p, publisher, outbox.DefaultConfig(), discardLog())

	if _, _, err := relay.DrainOnce(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	headers := publisher.byID(t, id).headers
	if headers["traceparent"] != traceparent {
		t.Fatalf("traceparent is %q", headers["traceparent"])
	}
	if headers["X-Correlation-Id"] != "corr-1" {
		t.Fatalf("correlation id is %q", headers["X-Correlation-Id"])
	}
}

func TestTwoRelaysDoNotPublishTheSameRowTwice(t *testing.T) {
	// What SKIP LOCKED buys. Two instances must partition the work rather than
	// duplicate it, with no coordination between them.
	p := pool(t)
	ids := make([]string, 0, 6)
	for range 6 {
		ids = append(ids, enqueue(t, p, nil))
	}

	a := &stubPublisher{}
	b := &stubPublisher{}
	relayA := outbox.New(p, a, outbox.DefaultConfig(), discardLog())
	relayB := outbox.New(p, b, outbox.DefaultConfig(), discardLog())

	var wg sync.WaitGroup
	wg.Add(2)
	for _, relay := range []*outbox.Relay{relayA, relayB} {
		go func() {
			defer wg.Done()
			if _, _, err := relay.DrainOnce(context.Background()); err != nil {
				t.Errorf("drain: %v", err)
			}
		}()
	}
	wg.Wait()

	if total := a.countOurs(ids...) + b.countOurs(ids...); total != 6 {
		t.Fatalf("six events were published %d times in total", total)
	}
	if unpublished(t, p) != 0 {
		t.Fatalf("%d events left unpublished", unpublished(t, p))
	}
}

func TestARestartPublishesEverythingExactlyOnce(t *testing.T) {
	// The acceptance test: a relay stopped mid-flight and started again must
	// leave every event published exactly once. Stopping it here means running
	// a bounded loop and building a new relay, which is what a restart is.
	p := pool(t)
	ids := make([]string, 0, 5)
	for range 5 {
		ids = append(ids, enqueue(t, p, nil))
	}

	first := &stubPublisher{failWith: errors.New("killed mid-publish")}
	relay := outbox.New(p, first, outbox.Config{
		Batch: 2, PollInterval: time.Millisecond,
		BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond,
	}, discardLog())
	if err := relay.Run(t.Context(), 2); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if unpublished(t, p) != 5 {
		t.Fatalf("a failing relay published something: %d left", unpublished(t, p))
	}

	// Restart with a working publisher.
	second := &stubPublisher{}
	restarted := outbox.New(p, second, outbox.Config{
		Batch: 2, PollInterval: time.Millisecond,
		BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond,
	}, discardLog())
	if err := restarted.Run(t.Context(), 6); err != nil {
		t.Fatalf("restart: %v", err)
	}

	if unpublished(t, p) != 0 {
		t.Fatalf("%d events still unpublished after restart", unpublished(t, p))
	}
	if second.countOurs(ids...) != 5 {
		t.Fatalf("five events were published %d times", second.countOurs(ids...))
	}

	// Exactly once, checked by id rather than by count alone.
	seen := map[string]int{}
	for _, id := range ids {
		seen[id] = second.countOurs(id)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("event %s published %d times", id, n)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("%d distinct events published, want 5", len(seen))
	}
}

func TestMetricsReportLagAndFailures(t *testing.T) {
	// Lag is the number to alert on: it says whether anyone downstream has
	// heard about what already happened.
	p := pool(t)
	enqueue(t, p, nil)
	relay := outbox.New(p, &stubPublisher{failWith: errors.New("down")},
		outbox.DefaultConfig(), discardLog())

	if _, _, err := relay.DrainOnce(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	m := relay.Metrics.Snapshot()
	// Lower bounds, not exact counts. Packages run in parallel and another
	// suite's rows can share the batch; asserting exact numbers made this fail
	// against correct code.
	if m.Failed < 1 || m.Batches != 1 || m.LastBatch < 1 {
		t.Fatalf("metrics: %+v", m)
	}
	if m.OldestUnsent <= 0 {
		t.Fatal("lag was not measured")
	}
}

func TestDrainingIsNotAnErrorWhenThereIsNothingOfOurs(t *testing.T) {
	// Only the error is asserted. The counts cannot be: go test runs packages
	// in parallel and the postgres suite writes to the same outbox table, so
	// "the outbox is empty" is not a state this test can arrange. Asserting
	// published==0 failed here against correct code.
	p := pool(t)
	relay := outbox.New(p, &stubPublisher{}, outbox.DefaultConfig(), discardLog())
	if _, _, err := relay.DrainOnce(t.Context()); err != nil {
		t.Fatalf("draining errored: %v", err)
	}
}

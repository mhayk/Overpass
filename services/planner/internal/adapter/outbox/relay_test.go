package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/planner/internal/adapter/outbox"
)

// Against a real Postgres, because the claim is about a transaction: the row
// must stay unsent when the publish fails, and the publish must happen inside
// the transaction that marks it sent.

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("OVERPASS_TEST_DSN")
	if dsn == "" {
		t.Skip("set OVERPASS_TEST_DSN to run the relay tests")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// stub records what it was asked to publish and can be told to fail.
type stub struct {
	mu       sync.Mutex
	sent     []string
	failWith error
}

func (s *stub) Publish(_ string, _ []byte, headers map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	s.sent = append(s.sent, headers["Nats-Msg-Id"])
	return nil
}

func (s *stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func enqueue(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	eventID := uuid.NewString()
	_, err := p.Exec(context.Background(), `
		INSERT INTO planning.outbox (event_id, event_type, schema_version, subject, payload, headers, occurred_at)
		VALUES ($1, 'planning.round.triggered.v1', '1.0.0', 'planning.round.triggered.v1',
		        $2::jsonb, '{}'::jsonb, now())
	`, eventID, fmt.Sprintf(`{"event_id":%q}`, eventID))
	if err != nil {
		t.Fatalf("enqueueing: %v", err)
	}
	return eventID
}

func published(t *testing.T, p *pgxpool.Pool, eventID string) bool {
	t.Helper()
	var sent bool
	if err := p.QueryRow(context.Background(),
		`SELECT published_at IS NOT NULL FROM planning.outbox WHERE event_id = $1`,
		eventID).Scan(&sent); err != nil {
		t.Fatalf("reading the outbox row: %v", err)
	}
	return sent
}

func TestARowIsPublishedAndMarked(t *testing.T) {
	p := pool(t)
	eventID := enqueue(t, p)
	publisher := &stub{}

	relay := outbox.New(p, publisher, outbox.DefaultConfig(), discard())
	sent, failed, err := relay.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("draining: %v", err)
	}
	if sent == 0 || failed != 0 {
		t.Fatalf("published=%d failed=%d", sent, failed)
	}
	if publisher.count() == 0 {
		t.Fatal("nothing reached the publisher")
	}
	if !published(t, p, eventID) {
		t.Error("the row was published but not marked; it would go out again on every poll forever")
	}
}

// The stable event_id doubles as the broker's dedup key. Losing it would make
// at-least-once delivery indistinguishable from duplicate events downstream.
func TestTheEventIDTravelsAsTheDedupHeader(t *testing.T) {
	p := pool(t)
	eventID := enqueue(t, p)
	publisher := &stub{}

	if _, _, err := outbox.New(p, publisher, outbox.DefaultConfig(), discard()).
		DrainOnce(context.Background()); err != nil {
		t.Fatalf("draining: %v", err)
	}

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	for _, id := range publisher.sent {
		if id == eventID {
			return
		}
	}
	t.Errorf("Nats-Msg-Id never carried %s; the broker's dedup has nothing to work with", eventID)
}

// A failed publish must leave the row UNSENT. A row dropped on the first
// network blip is an event that, as far as everyone downstream is concerned,
// never happened.
func TestAFailedPublishLeavesTheRowUnsent(t *testing.T) {
	p := pool(t)
	eventID := enqueue(t, p)
	publisher := &stub{failWith: errors.New("no responders")}

	sent, failed, err := outbox.New(p, publisher, outbox.DefaultConfig(), discard()).
		DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("draining: %v", err)
	}
	if sent != 0 || failed == 0 {
		t.Fatalf("published=%d failed=%d, want a failure", sent, failed)
	}
	if published(t, p, eventID) {
		t.Fatal("a row whose publish failed was marked published; the event is lost and nothing will retry it")
	}

	var attempts int
	var lastError *string
	if err := p.QueryRow(context.Background(),
		`SELECT attempts, last_error FROM planning.outbox WHERE event_id = $1`,
		eventID).Scan(&attempts, &lastError); err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if lastError == nil || *lastError == "" {
		t.Error("no last_error recorded; an operator has nothing to diagnose from")
	}
}

// Run must return on cancellation rather than spin, and must be exercised by
// running to completion rather than only by being cancelled.
func TestRunStopsAfterItsIterationBudget(t *testing.T) {
	p := pool(t)
	enqueue(t, p)

	relay := outbox.New(p, &stub{}, outbox.DefaultConfig(), discard())
	done := make(chan error, 1)
	go func() { done <- relay.Run(context.Background(), 2) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its iteration budget")
	}
}

func TestRunReturnsOnCancellation(t *testing.T) {
	p := pool(t)
	ctx, cancel := context.WithCancel(context.Background())
	relay := outbox.New(p, &stub{failWith: errors.New("down")}, outbox.DefaultConfig(), discard())

	done := make(chan error, 1)
	go func() { done <- relay.Run(ctx, 0) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation is a clean stop, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run ignored cancellation")
	}
}

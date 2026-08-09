// Package outbox drains the transactional outbox onto NATS.
//
// The business transaction writes the event; this publishes it. Never the other
// way round and never together: a publish inside the transaction would succeed
// and the transaction could still roll back, announcing a fact that never
// became true. Nothing downstream could tell.
//
// What that buys is at-least-once publication with a stable event_id, which is
// exactly what makes consumer-side deduplication possible — and every consumer
// in this system already does it.
package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/mhayk/overpass/services/tasking-api/internal/telemetry"
	"github.com/nats-io/nats.go"
)

// Config controls the relay loop.
type Config struct {
	// Batch is small on purpose: the publish happens with row locks held, so a
	// large batch is a long transaction blocking nothing useful.
	Batch int
	// PollInterval is how long to wait when the table was empty.
	PollInterval time.Duration
	// BackoffBase and BackoffMax bound the exponential retry.
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

// DefaultConfig is tuned for a laptop and a demo, not for a fleet.
func DefaultConfig() Config {
	return Config{
		Batch:        32,
		PollInterval: 250 * time.Millisecond,
		BackoffBase:  200 * time.Millisecond,
		BackoffMax:   30 * time.Second,
	}
}

// Stats is what an operator needs to know the relay is healthy.
//
// Lag is the one that matters. Batch size and failure rate describe how the
// relay is working; lag describes whether anyone downstream has heard about
// what already happened, and it is the number to alert on.
//
// A plain value with no lock, separate from the counter that guards it. The
// first version had one type doing both and Snapshot returned it by value —
// copying a mutex, which go vet's copylocks catches and which would have been
// a genuine data race under a scraper.
type Stats struct {
	Published    int64
	Failed       int64
	Batches      int64
	LastBatch    int
	OldestUnsent time.Duration
}

// Metrics accumulates Stats under a lock.
type Metrics struct {
	mu    sync.Mutex
	stats Stats
}

// Snapshot returns a consistent copy, safe to read from another goroutine.
func (m *Metrics) Snapshot() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

func (m *Metrics) record(published, failed, batch int, lag time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.Published += int64(published)
	m.stats.Failed += int64(failed)
	m.stats.Batches++
	m.stats.LastBatch = batch
	m.stats.OldestUnsent = lag
}

// Publisher is the narrow slice of NATS the relay needs.
//
// Declared here so the relay can be driven by a stub that fails on demand —
// the backoff and the "row stays unsent" behaviour are the interesting parts
// and neither needs a broker to exercise.
type Publisher interface {
	Publish(subject string, data []byte, headers map[string]string) error
}

// NATSPublisher adapts a JetStream context.
type NATSPublisher struct {
	js nats.JetStreamContext
}

// NewNATSPublisher wraps a JetStream context.
func NewNATSPublisher(js nats.JetStreamContext) *NATSPublisher {
	return &NATSPublisher{js: js}
}

// publishTimeout bounds one publish, explicitly.
//
// AUDITED, not assumed (#51). An unoptioned PublishMsg is NOT unbounded — but
// its bound is the library's, and it is not the five seconds a reader would
// guess from the call site: nats.go waits defaultRequestWait (5s) and retries
// DefaultPubRetryAttempts (2) more times with DefaultPubRetryWait (250ms)
// between, so a stalled broker holds this call for roughly fifteen seconds per
// message. The relay drains batches, so that is fifteen seconds MULTIPLIED BY
// the batch size before the loop notices anything is wrong.
//
// Stated here instead. The number is deliberately close to the library's single
// attempt: the outbox is the reason a broker outage does not refuse customer
// traffic, so the relay's job when the broker is slow is to give up quickly and
// leave the rows for the next tick — not to hold a connection hoping.
const publishTimeout = 5 * time.Second

// Publish sends one message with its headers intact.
func (p *NATSPublisher) Publish(subject string, data []byte, headers map[string]string) error {
	msg := nats.NewMsg(subject)
	msg.Data = data
	for k, v := range headers {
		msg.Header.Set(k, v)
	}
	// A deadline over the WHOLE call, retries included. nats.AckWait would
	// bound one attempt and leave the total as attempts x wait, which is the
	// same implicit arithmetic this constant exists to remove.
	//
	// context.Background() rather than a caller's context: this interface does
	// not carry one, and the bound here is about the broker rather than about
	// the caller's lifetime — shutdown is handled by the loop that calls this,
	// which stops fetching rows.
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()

	if _, err := p.js.PublishMsg(msg, nats.Context(ctx)); err != nil {
		return fmt.Errorf("publishing to %s: %w", subject, err)
	}
	return nil
}

// Relay drains tasking.outbox.
type Relay struct {
	pool      *pgxpool.Pool
	publisher Publisher
	cfg       Config
	log       *slog.Logger

	// Metrics is exported so main can serve it and M3 can scrape it.
	Metrics Metrics
}

// New builds a relay.
func New(pool *pgxpool.Pool, publisher Publisher, cfg Config, log *slog.Logger) *Relay {
	return &Relay{pool: pool, publisher: publisher, cfg: cfg, log: log}
}

type pending struct {
	id         int64
	eventID    string
	subject    string
	payload    []byte
	headers    map[string]string
	occurredAt time.Time
}

// DrainOnce publishes at most one batch. Returns how many went out.
//
// The transaction spans the publish, deliberately. Crash after publishing and
// before committing, and the row stays unsent and goes again — at-least-once,
// which every consumer absorbs. Mark-first would turn the same crash into a
// LOST event, and a lost event is invisible.
func (r *Relay) DrainOnce(ctx context.Context) (published, failed int, err error) {
	txErr := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := claim(ctx, tx, r.cfg.Batch)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}

		var lag time.Duration
		if age := time.Since(rows[0].occurredAt); age > 0 {
			lag = age
		}

		for _, row := range rows {
			// The producing span, resumed from the row.
			//
			// This is the join that makes an outbox traceable at all. The
			// traceparent was captured at ENQUEUE time, inside the transaction
			// that persisted the request, so it names the ingress span of the
			// HTTP call that caused this event. Resuming it here and starting a
			// child means the publish appears under the request that produced
			// it — even though the publish happens later, on a different
			// goroutine, possibly after a restart, possibly in a different
			// process.
			//
			// Without this the relay's spans would be roots: a trace per
			// publish, unconnected to anything, which is the shape almost every
			// outbox implementation ships with.
			publishCtx, span := telemetry.Tracer().Start(
				telemetry.Extract(ctx, row.headers),
				"tasking.outbox publish",
				trace.WithSpanKind(trace.SpanKindProducer),
				trace.WithAttributes(
					semconv.MessagingSystemKey.String("nats"),
					semconv.MessagingDestinationName(row.subject),
					semconv.MessagingMessageIDKey.String(row.eventID),
				),
			)

			// The stable event_id doubles as the broker's dedup key. Ours is
			// the outbox row; this is a second line of defence, and it expires
			// where ours does not.
			headers := map[string]string{"Nats-Msg-Id": row.eventID}
			for k, v := range row.headers {
				headers[k] = v
			}
			// Overwrites the stored traceparent with THIS span's, deliberately.
			// The consumer should hang off the publish, not off the ingress: the
			// publish is what actually delivered the message, and a consumer
			// parented directly to the ingress would show the broker hop as
			// taking no time at all.
			telemetry.Inject(publishCtx, headers)

			if perr := r.publisher.Publish(row.subject, row.payload, headers); perr != nil {
				r.log.Warn("publish failed",
					slog.String("event_id", row.eventID),
					slog.String("subject", row.subject),
					slog.Any("error", perr),
				)
				span.RecordError(perr)
				span.SetStatus(codes.Error, "publish failed")
				span.End()
				if merr := markFailed(ctx, tx, row.id, perr.Error()); merr != nil {
					return merr
				}
				failed++
				continue
			}
			span.End()
			if merr := markPublished(ctx, tx, row.id); merr != nil {
				return merr
			}
			published++
		}

		r.Metrics.record(published, failed, len(rows), lag)
		return nil
	})
	return published, failed, txErr
}

// Run drains until the context is cancelled.
//
// maxIterations exists so a test can run the real loop to completion rather
// than cancelling it. A loop only ever stopped by cancellation is a loop whose
// shutdown path is never exercised.
func (r *Relay) Run(ctx context.Context, maxIterations int) error {
	consecutiveFailures := 0

	for i := 0; maxIterations <= 0 || i < maxIterations; i++ {
		published, failed, err := r.DrainOnce(ctx)

		switch {
		case err != nil:
			r.log.Error("relay batch failed", slog.Any("error", err))
			consecutiveFailures++
		case failed > 0:
			consecutiveFailures++
		default:
			consecutiveFailures = 0
		}

		wait := r.cfg.PollInterval
		if consecutiveFailures > 0 {
			// Exponential, capped. A broker that is down should not be hammered
			// once per poll interval for as long as it stays down — that turns
			// an outage into two.
			wait = r.backoff(consecutiveFailures)
		} else if published > 0 {
			// There was work; look again immediately rather than sleeping
			// through a backlog.
			wait = 0
		}

		if wait > 0 {
			select {
			case <-ctx.Done():
				// A cancelled context is a clean stop, not a failure. The
				// caller asked us to finish.
				return nil //nolint:nilerr // cancellation is not an error here
			case <-time.After(wait):
			}
		} else if ctx.Err() != nil {
			return nil //nolint:nilerr // same
		}
	}
	return nil
}

func (r *Relay) backoff(attempt int) time.Duration {
	shift := min(attempt-1, 16) // guard the shift, not the result
	wait := time.Duration(float64(r.cfg.BackoffBase) * math.Pow(2, float64(shift)))
	return min(wait, r.cfg.BackoffMax)
}

func claim(ctx context.Context, tx pgx.Tx, limit int) ([]pending, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, event_id::text, subject, payload::text, headers, occurred_at
		FROM tasking.outbox
		WHERE published_at IS NULL
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("claiming outbox rows: %w", err)
	}
	defer rows.Close()

	var out []pending
	for rows.Next() {
		var (
			row     pending
			payload string
		)
		if err := rows.Scan(&row.id, &row.eventID, &row.subject, &payload,
			&row.headers, &row.occurredAt); err != nil {
			return nil, fmt.Errorf("scanning outbox row: %w", err)
		}
		row.payload = []byte(payload)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading outbox rows: %w", err)
	}
	return out, nil
}

func markPublished(ctx context.Context, tx pgx.Tx, id int64) error {
	if _, err := tx.Exec(ctx,
		`UPDATE tasking.outbox SET published_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("marking outbox row %d published: %w", id, err)
	}
	return nil
}

func markFailed(ctx context.Context, tx pgx.Tx, id int64, reason string) error {
	// The row stays unpublished. A row dropped on the first network blip is an
	// event that, as far as everyone downstream is concerned, never happened.
	if _, err := tx.Exec(ctx, `
		UPDATE tasking.outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1
	`, id, truncate(reason, 2000)); err != nil {
		return fmt.Errorf("recording outbox failure for %d: %w", id, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Package outbox drains planning.outbox onto NATS.
//
// The business transaction writes the event; this publishes it. Never the other
// way round and never together: a publish inside the transaction would succeed
// and the transaction could still roll back, announcing a round that never
// opened, and nothing downstream could tell.
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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/mhayk/overpass/lib/go/telemetry/outboxmetrics"
)

// Config controls the relay loop.
type Config struct {
	// Batch is small on purpose: the publish happens with row locks held, so a
	// large batch is a long transaction blocking nothing useful.
	Batch        int
	PollInterval time.Duration
	BackoffBase  time.Duration
	BackoffMax   time.Duration
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

// Publisher is the narrow slice of NATS the relay needs.
//
// Declared here so the relay can be driven by a stub that fails on demand — the
// backoff and the "row stays unsent" behaviour are the interesting parts and
// neither needs a broker to exercise.
type Publisher interface {
	Publish(subject string, data []byte, headers map[string]string) error
}

// NATSPublisher adapts a JetStream context.
type NATSPublisher struct{ js nats.JetStreamContext }

// NewNATSPublisher wraps a JetStream context.
func NewNATSPublisher(js nats.JetStreamContext) *NATSPublisher { return &NATSPublisher{js: js} }

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

// Relay drains planning.outbox.
type Relay struct {
	pool      *pgxpool.Pool
	publisher Publisher
	cfg       Config
	log       *slog.Logger

	// Instruments exports backlog age and publish outcomes. Optional and
	// nil-safe: a relay built without a meter publishes exactly as it
	// otherwise would.
	//
	// This relay had NO metrics at all before #53 — not even the in-process
	// Stats tasking-api's carries — so planning.outbox could fall arbitrarily
	// far behind with nothing but log lines to say so.
	Instruments *outboxmetrics.Instruments
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
			// Zero lag, reported rather than skipped. An empty batch means
			// nothing is waiting; without this the observable gauge would stay
			// pinned at the last non-empty measurement and OutboxRelayLagging
			// would keep firing after the backlog had cleared.
			r.Instruments.RecordBatch(ctx, 0, 0, 0)
			return nil
		}

		// The oldest row in the batch, which is the oldest unpublished row in
		// the table: claim() orders by id and id is monotonic with insertion.
		var lag time.Duration
		if age := time.Since(rows[0].occurredAt); age > 0 {
			lag = age
		}

		for _, row := range rows {
			// The stable event_id doubles as the broker's dedup key. Ours is
			// the outbox row; this is a second line of defence, and it expires
			// where ours does not.
			headers := map[string]string{
				"Nats-Msg-Id": row.eventID,
				"Occurred-At": row.occurredAt.UTC().Format(time.RFC3339Nano),
			}
			if perr := r.publisher.Publish(row.subject, row.payload, headers); perr != nil {
				r.log.Warn("publish failed",
					slog.String("event_id", row.eventID),
					slog.String("subject", row.subject),
					slog.Any("error", perr))
				if merr := markFailed(ctx, tx, row.id, perr.Error()); merr != nil {
					return merr
				}
				failed++
				continue
			}
			if merr := markPublished(ctx, tx, row.id); merr != nil {
				return merr
			}
			published++
		}
		r.Instruments.RecordBatch(ctx, published, failed, lag)
		return nil
	})
	return published, failed, txErr
}

// Run drains until the context is cancelled.
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
			wait = 0 // there was work; look again rather than sleeping through a backlog
		}

		if wait > 0 {
			select {
			case <-ctx.Done():
				return nil //nolint:nilerr // cancellation is a clean stop
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
		SELECT id, event_id::text, subject, payload::text, occurred_at
		FROM planning.outbox
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
		if err := rows.Scan(&row.id, &row.eventID, &row.subject, &payload, &row.occurredAt); err != nil {
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
		`UPDATE planning.outbox SET published_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("marking outbox row %d published: %w", id, err)
	}
	return nil
}

func markFailed(ctx context.Context, tx pgx.Tx, id int64, reason string) error {
	// The row stays unpublished. A row dropped on the first network blip is an
	// event that, as far as everyone downstream is concerned, never happened.
	if _, err := tx.Exec(ctx, `
		UPDATE planning.outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1
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

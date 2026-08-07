package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

// Submissions writes tasking requests and their outbox events.
type Submissions struct {
	pool *pgxpool.Pool
}

// NewSubmissions wraps a pool.
func NewSubmissions(pool *pgxpool.Pool) *Submissions { return &Submissions{pool: pool} }

// Save writes the request and the event in ONE transaction.
//
// This is the transactional outbox, and the single transaction is the whole of
// it. Two transactions — persist, then publish — is the dual-write problem: a
// crash between them either loses the event for a request that exists, or
// announces a request that was rolled back. Both are silent, and both are found
// much later by someone holding a plan that references a request nobody has.
//
// The event is not published here. A relay drains the table afterwards, which
// makes publication at-least-once — exactly what every consumer already handles.
func (s *Submissions) Save(ctx context.Context, req port.StoredRequest, event port.OutboxEvent) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO tasking.tasking_requests
				(request_id, customer_id, target_name, target, request_window,
				 priority_tier, bid_credits, requested_modes, constraints,
				 state, submitted_at)
			VALUES ($1, $2, $3, ST_GeomFromText($4, 4326), tstzrange($5, $6, '[)'),
			        $7, $8, $9, $10, 'RECEIVED', $11)
		`,
			req.RequestID, req.CustomerID, req.TargetName, req.TargetWKT,
			req.WindowStart, req.WindowEnd,
			req.PriorityTier, req.BidCredits, req.RequestedModes, req.ConstraintsJSON,
			req.SubmittedAt,
		); err != nil {
			return fmt.Errorf("inserting tasking request: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO tasking.outbox
				(event_id, event_type, schema_version, subject, payload, headers, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`,
			event.EventID, event.EventType, event.SchemaVersion, event.Subject,
			event.PayloadJSON, event.HeadersJSON, event.OccurredAt,
		); err != nil {
			return fmt.Errorf("inserting outbox event: %w", err)
		}

		return nil
	})
}

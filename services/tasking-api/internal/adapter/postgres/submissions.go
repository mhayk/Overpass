package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

// uniqueViolation is the SQLSTATE Postgres raises for a duplicate key.
const uniqueViolation = "23505"

// Submissions writes tasking requests, their idempotency claims, and their
// outbox events.
type Submissions struct {
	pool *pgxpool.Pool
}

// NewSubmissions wraps a pool.
func NewSubmissions(pool *pgxpool.Pool) *Submissions { return &Submissions{pool: pool} }

// Save writes the claim, the request and the event in ONE transaction.
//
// Three things, one commit, and each pairing matters:
//
//	Request and event together is the transactional outbox. Two transactions
//	would either lose the event for a request that exists, or announce one that
//	was rolled back.
//
//	Claim and request together closes the window where a key exists and its
//	request does not — a crash there would swallow that submission forever,
//	because every retry would look like a replay of something that never
//	happened.
//
// The REQUEST goes in first, then the claim, and the ordering is forced rather
// than chosen: idempotency_keys.request_id is a foreign key, so a claim written
// before its request violates it. Found by the concurrency test on its first
// run, which is the sort of thing no unit test with a map for a store could
// have noticed.
//
// It costs nothing. Under contention the losing transactions do one wasted
// INSERT and then roll it back whole — the unique constraint on the claim is
// still what decides, and it decides after the work rather than before.
//
// What matters is that it IS the constraint deciding. Check-then-insert is a
// race: two concurrent identical submissions both see no key, both insert, and
// the customer pays twice. The concurrency test exists to prove which one this
// is, and a fake store cannot tell them apart.
func (s *Submissions) Save(
	ctx context.Context,
	claim port.IdempotencyClaim,
	req port.StoredRequest,
	event port.OutboxEvent,
) (port.Replay, error) {
	var replay port.Replay

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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

		_, err := tx.Exec(ctx, `
			INSERT INTO tasking.idempotency_keys
				(customer_id, idempotency_key, request_digest, request_id,
				 response_status, expires_at)
			VALUES ($1, $2, decode($3, 'hex'), $4, 202, $5)
		`, claim.CustomerID, claim.Key, claim.Fingerprint, req.RequestID, claim.ExpiresAt)

		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			// The key is taken. Whether that is a replay or a client bug depends
			// entirely on whether the body matches — and either way this
			// transaction rolls back, undoing the request inserted above.
			//
			// The lookup runs in a SEPARATE transaction, because this one is
			// already aborted: Postgres refuses every further statement on a
			// transaction that has hit an error.
			return errKeyTaken
		}
		if err != nil {
			return fmt.Errorf("claiming idempotency key: %w", err)
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
	if errors.Is(err, errKeyTaken) {
		return existingClaim(ctx, s.pool, claim)
	}
	if err != nil {
		return port.Replay{}, err
	}
	return replay, nil
}

// errKeyTaken signals, inside the transaction, that the claim already exists.
//
// A sentinel rather than the outcome itself, because the transaction is aborted
// by the time we know: Postgres refuses every further statement on it, so the
// lookup has to happen after the rollback.
var errKeyTaken = errors.New("idempotency key already claimed")

// existingClaim decides replay versus conflict for a key already taken.
func existingClaim(ctx context.Context, db *pgxpool.Pool, claim port.IdempotencyClaim) (port.Replay, error) {
	var (
		storedDigest string
		requestID    string
		state        string
		submittedAt  time.Time
	)
	err := db.QueryRow(ctx, `
		SELECT encode(k.request_digest, 'hex'), k.request_id::text, r.state, r.submitted_at
		FROM tasking.idempotency_keys k
		JOIN tasking.tasking_requests r ON r.request_id = k.request_id
		WHERE k.customer_id = $1 AND k.idempotency_key = $2
	`, claim.CustomerID, claim.Key).Scan(&storedDigest, &requestID, &state, &submittedAt)
	if err != nil {
		return port.Replay{}, fmt.Errorf("reading the existing idempotency claim: %w", err)
	}

	if storedDigest != claim.Fingerprint {
		// A client bug, and it must surface. Treating it as a replay would
		// discard a request the customer believes they submitted.
		return port.Replay{}, port.ErrIdempotencyConflict
	}

	return port.Replay{
		Replayed:    true,
		RequestID:   requestID,
		State:       state,
		SubmittedAt: submittedAt,
	}, nil
}

// PurgeExpiredKeys removes claims past their expiry.
func (s *Submissions) PurgeExpiredKeys(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM tasking.idempotency_keys WHERE expires_at < $1`, now)
	if err != nil {
		return 0, fmt.Errorf("purging expired idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

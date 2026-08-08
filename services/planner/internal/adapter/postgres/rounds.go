package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// Rounds is the pgx-backed round ledger and advisory lock.
type Rounds struct {
	pool *pgxpool.Pool
}

// NewRounds wraps a pool.
func NewRounds(pool *pgxpool.Pool) *Rounds { return &Rounds{pool: pool} }

// DirtyBuckets derives which buckets have candidates a round has not seen.
//
// The whole rule in one query, because splitting it would mean holding a
// partial answer across statements while candidates keep arriving.
//
// Bucketing is done with to_timestamp(floor(epoch / seconds) * seconds), which
// is the SQL spelling of domain.BucketStart. The two must agree or the planner
// would lock one key and read another — so the duration comes from the same
// configured value, validated once at startup by domain.ValidBucketDuration.
func (r *Rounds) DirtyBuckets(ctx context.Context, q port.BucketQuery) ([]domain.BucketState, error) {
	if err := domain.ValidBucketDuration(q.BucketDuration); err != nil {
		return nil, err
	}
	seconds := q.BucketDuration.Seconds()

	rows, err := r.pool.Query(ctx, `
		WITH bucketed AS (
			SELECT
				c.satellite_id,
				to_timestamp(floor(extract(epoch FROM lower(c.access_window)) / $1) * $1) AS bucket_start,
				c.created_at,
				c.source_event_id
			FROM planning.candidate_opportunities c
			WHERE lower(c.access_window) >= $2
			  AND lower(c.access_window) <  $3
		),
		last_round AS (
			SELECT b.satellite_id, b.bucket_start, MAX(r.triggered_at) AS triggered_at
			FROM (SELECT DISTINCT satellite_id, bucket_start FROM bucketed) b
			LEFT JOIN planning.rounds r
			  ON r.satellite_id = b.satellite_id
			 AND lower(r.bucket) = b.bucket_start
			GROUP BY b.satellite_id, b.bucket_start
		)
		SELECT
			b.satellite_id,
			b.bucket_start,
			lr.triggered_at,
			MAX(b.created_at)                              AS newest_candidate_at,
			MIN(b.created_at) FILTER (
				WHERE lr.triggered_at IS NULL OR b.created_at > lr.triggered_at
			)                                              AS oldest_pending_at,
			count(*) FILTER (
				WHERE lr.triggered_at IS NULL OR b.created_at > lr.triggered_at
			)                                              AS pending,
			(array_agg(b.source_event_id::text ORDER BY b.created_at DESC))[1]
			                                               AS tipping_event_id,
			(
				SELECT p.plan_id::text
				FROM planning.collection_plans p
				WHERE p.satellite_id = b.satellite_id
				  AND lower(p.bucket) = b.bucket_start
				ORDER BY p.plan_version DESC
				LIMIT 1
			)                                              AS live_plan_id
		FROM bucketed b
		JOIN last_round lr
		  ON lr.satellite_id = b.satellite_id AND lr.bucket_start = b.bucket_start
		GROUP BY b.satellite_id, b.bucket_start, lr.triggered_at
		HAVING count(*) FILTER (
			WHERE lr.triggered_at IS NULL OR b.created_at > lr.triggered_at
		) > 0
		ORDER BY oldest_pending_at
		LIMIT $4
	`, seconds, q.HorizonStart, q.HorizonEnd, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("finding dirty buckets: %w", err)
	}
	defer rows.Close()

	var out []domain.BucketState
	for rows.Next() {
		var (
			state       domain.BucketState
			lastRound   *time.Time
			oldestPend  *time.Time
			livePlanID  *string
			bucketStart time.Time
			satellite   string
		)
		if err := rows.Scan(&satellite, &bucketStart, &lastRound,
			&state.NewestCandidateAt, &oldestPend, &state.PendingCandidates,
			&state.TippingEventID, &livePlanID); err != nil {
			return nil, fmt.Errorf("scanning a dirty bucket: %w", err)
		}
		state.Key = domain.RoundKey{SatelliteID: satellite, BucketStart: bucketStart.UTC()}
		state.LastRoundAt = lastRound
		state.LivePlanID = livePlanID
		if oldestPend != nil {
			state.OldestPendingAt = oldestPend.UTC()
		}
		state.NewestCandidateAt = state.NewestCandidateAt.UTC()
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading dirty buckets: %w", err)
	}
	return out, nil
}

// OpenRound serialises one round behind the advisory lock for its key.
//
// pg_advisory_xact_lock rather than SELECT ... FOR UPDATE, per M2-01: no row
// needs to exist for a bucket that has never been planned, and inventing a
// placeholder row to lock is a worse design than locking the concept directly.
//
// TRANSACTION-SCOPED, not session-scoped. A session lock has to be released by
// name, so any path that returns early — an error, a panic, a cancelled context
// — leaks it, and the leak is a satellite that can never be planned again until
// the connection is recycled. The xact variant releases at COMMIT or ROLLBACK,
// which the language guarantees happens.
func (r *Rounds) OpenRound(
	ctx context.Context,
	key domain.RoundKey,
	bucketEnd time.Time,
	open func(port.RoundInputs) (port.Round, []byte, error),
) (bool, error) {
	opened := false

	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		satelliteKey, bucketKey := domain.AdvisoryLockKey(key)

		// Blocking, not pg_try_advisory_xact_lock. A round that gave up because
		// another was in flight would silently skip this bucket for a whole
		// sweep, and the bucket stays dirty, so the next sweep tries again —
		// which turns contention into a livelock that looks like nothing
		// happening. Waiting is correct: the other holder is planning the same
		// bucket, and this round wants to see its result.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, satelliteKey, bucketKey); err != nil {
			return fmt.Errorf("taking the advisory lock for %s: %w", key, err)
		}

		inputs, err := r.readInputs(ctx, tx, key, bucketEnd)
		if err != nil {
			return err
		}

		round, payload, err := open(inputs)
		if err != nil {
			if errors.Is(err, port.ErrSkipRound) {
				// Roll back rather than commit an empty transaction. Same
				// effect on the data, but it releases the lock through the path
				// that is exercised on every error, instead of a second one
				// that only runs on this branch.
				return err
			}
			return err
		}

		if err := insertRound(ctx, tx, round); err != nil {
			return err
		}
		if err := enqueue(ctx, tx, round, payload); err != nil {
			return err
		}
		opened = true
		return nil
	})

	if errors.Is(err, port.ErrSkipRound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return opened, nil
}

// readInputs reads the candidate set UNDER the lock.
//
// Held candidates are excluded by the join to request_snapshots, which is what
// ADR-0015 means by holding them: they are stored, they are simply not
// allocatable, and per ADR-0014 they must not appear in candidate_request_ids
// because they can become neither an acquisition nor an unfulfilment.
//
// The COUNT, however, is of ALL candidates in the bucket including held ones.
// The contract calls candidate_opportunity_count "opportunities on the table
// when the round opened", which is a statement about what existed, while
// candidate_request_ids is a conservation ledger about what competed. Conflating
// them would either hide held candidates from the audit record or promise an
// outcome for requests that were never entered.
func (r *Rounds) readInputs(ctx context.Context, tx pgx.Tx, key domain.RoundKey, bucketEnd time.Time) (port.RoundInputs, error) {
	inputs := port.RoundInputs{Key: key, BucketEnd: bucketEnd}

	err := tx.QueryRow(ctx, `
		SELECT
			count(*),
			coalesce(
				array_agg(DISTINCT c.request_id) FILTER (WHERE s.request_id IS NOT NULL),
				'{}'
			)::text[]
		FROM planning.candidate_opportunities c
		LEFT JOIN planning.request_snapshots s ON s.request_id = c.request_id
		WHERE c.satellite_id = $1
		  AND lower(c.access_window) >= $2
		  AND lower(c.access_window) <  $3
	`, key.SatelliteID, key.BucketStart, bucketEnd).Scan(
		&inputs.CandidateOpportunityCount, &inputs.CandidateRequestIDs)
	if err != nil {
		return port.RoundInputs{}, fmt.Errorf("reading candidates for %s: %w", key, err)
	}

	// The budget the round allocates against, recorded rather than recomputed
	// later: a satellite's configured budget can change, and a round must stay
	// explicable against the number it actually used.
	if err := tx.QueryRow(ctx,
		`SELECT duty_cycle_budget_s FROM reference.satellites WHERE satellite_id = $1`,
		key.SatelliteID).Scan(&inputs.DutyCycleBudgetS); err != nil {
		return port.RoundInputs{}, fmt.Errorf("reading the duty-cycle budget for %s: %w", key.SatelliteID, err)
	}

	if err := tx.QueryRow(ctx, `
		SELECT plan_id::text FROM planning.collection_plans
		WHERE satellite_id = $1 AND lower(bucket) = $2
		ORDER BY plan_version DESC LIMIT 1
	`, key.SatelliteID, key.BucketStart).Scan(&inputs.LivePlanID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return port.RoundInputs{}, fmt.Errorf("reading the live plan for %s: %w", key, err)
	}

	return inputs, nil
}

func insertRound(ctx context.Context, tx pgx.Tx, r port.Round) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO planning.rounds (
			round_id, satellite_id, bucket, trigger, policy,
			candidate_opportunity_count, candidate_request_ids,
			duty_cycle_budget_s, supersedes_plan_id, triggered_at
		) VALUES (
			$1, $2, tstzrange($3, $4, '[)'), $5, $6,
			$7, $8::uuid[],
			$9, $10, $11
		)
	`,
		r.RoundID, r.Key.SatelliteID, r.Key.BucketStart, r.BucketEnd, r.Trigger, r.Policy,
		r.CandidateOpportunityCount, r.CandidateRequestIDs,
		r.DutyCycleBudgetS, r.SupersedesPlanID, r.TriggeredAt,
	)
	if err != nil {
		return fmt.Errorf("recording round %s: %w", r.RoundID, err)
	}
	return nil
}

// enqueue writes the outbox row in the SAME transaction as the round.
//
// ADR-0006: the business transaction writes the event, the relay publishes it.
// Never together and never the other way round — a publish inside the
// transaction would succeed and the transaction could still roll back,
// announcing a round that never opened, and nothing downstream could tell.
func enqueue(ctx context.Context, tx pgx.Tx, r port.Round, payload []byte) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO planning.outbox (
			event_id, event_type, schema_version, subject, payload, headers, occurred_at
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7)
	`,
		r.EventID,
		"planning.round.triggered.v1",
		"1.0.0",
		"planning.round.triggered.v1",
		string(payload),
		`{}`,
		r.TriggeredAt,
	)
	if err != nil {
		return fmt.Errorf("enqueueing the round event for %s: %w", r.RoundID, err)
	}
	return nil
}

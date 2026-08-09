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
	open func(port.RoundInputs) (port.RoundOutcome, error),
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

		outcome, err := open(inputs)
		if err != nil {
			// ErrSkipRound rolls back rather than committing an empty
			// transaction. Same effect on the data, but it releases the lock
			// through the path exercised on every error, instead of a second
			// one that only runs on this branch.
			return err
		}

		if err := insertRound(ctx, tx, outcome.Round); err != nil {
			return err
		}
		if err := enqueue(ctx, tx, outcome.Round.EventID, "planning.round.triggered.v1",
			"planning.round.triggered.v1", outcome.RoundPayload, outcome.Round.TriggeredAt); err != nil {
			return err
		}

		if outcome.Plan != nil {
			if err := commitPlan(ctx, tx, outcome.Round, *outcome.Plan); err != nil {
				return err
			}
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
				array_agg(DISTINCT c.request_id) FILTER (
					WHERE s.request_id IS NOT NULL
					  -- Must agree with the joinable query below: a request
					  -- served elsewhere (#163) is excluded from the ledger the
					  -- same way a held one is, and for the same reason — it
					  -- can become neither an acquisition nor an unfulfilment
					  -- in THIS round.
					  AND NOT EXISTS (
						SELECT 1
						FROM planning.acquisitions a
						JOIN planning.collection_plans p ON p.plan_id = a.plan_id
						WHERE a.request_id = c.request_id
						  AND a.status = 'ACTIVE'
						  AND NOT (p.satellite_id = $1 AND lower(p.bucket) = $2)
					  )
				),
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

	// The joinable rows themselves — the join is the same one, made at read
	// time exactly as ADR-0015 designed: a candidate with no snapshot is held,
	// which here means it simply does not appear.
	rows, err := tx.Query(ctx, `
		SELECT
			c.opportunity_id::text, c.request_id::text, c.satellite_id, c.mode,
			lower(c.access_window), upper(c.access_window),
			c.acquisition_duration_s, c.orbit_number,
			c.geometry::text, ST_AsGeoJSON(c.footprint),
			c.duty_cycle_cost_s, c.quality_score,
			s.customer_id, s.priority_tier, s.bid_credits,
			s.submitted_at, upper(s.request_window)
		FROM planning.candidate_opportunities c
		JOIN planning.request_snapshots s ON s.request_id = c.request_id
		WHERE c.satellite_id = $1
		  AND lower(c.access_window) >= $2
		  AND lower(c.access_window) <  $3
		  -- #163: a request already SERVED elsewhere does not compete here. The
		  -- per-round Schedule enforces one-acquisition-per-request within one
		  -- plan; this is what makes it hold ACROSS (satellite, bucket)
		  -- partitions, which the first full-stack demo showed it did not — one
		  -- request, five ACTIVE acquisitions on four satellites.
		  --
		  -- THIS bucket's own live plan is exempt, deliberately: ADR-0014's
		  -- whole-bucket recompute requires the bucket's holders to recompete
		  -- from scratch, and their ACTIVE rows are demoted in the same
		  -- transaction that commits the replacement. 00011's deferred
		  -- constraint is the backstop if two rounds race past this read.
		  AND NOT EXISTS (
			SELECT 1
			FROM planning.acquisitions a
			JOIN planning.collection_plans p ON p.plan_id = a.plan_id
			WHERE a.request_id = c.request_id
			  AND a.status = 'ACTIVE'
			  AND NOT (p.satellite_id = $1 AND lower(p.bucket) = $2)
		  )
		ORDER BY c.opportunity_id
	`, key.SatelliteID, key.BucketStart, bucketEnd)
	if err != nil {
		return port.RoundInputs{}, fmt.Errorf("reading joinable candidates for %s: %w", key, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			jc        port.JoinableCandidate
			geometry  string
			footprint string
		)
		if err := rows.Scan(
			&jc.OpportunityID, &jc.RequestID, &jc.SatelliteID, &jc.Mode,
			&jc.AccessStart, &jc.AccessEnd,
			&jc.AcquisitionDurationS, &jc.OrbitNumber,
			&geometry, &footprint,
			&jc.DutyCycleCostS, &jc.QualityScore,
			&jc.CustomerID, &jc.PriorityTier, &jc.BidCredits,
			&jc.SubmittedAt, &jc.Deadline,
		); err != nil {
			return port.RoundInputs{}, fmt.Errorf("scanning a joinable candidate: %w", err)
		}
		jc.GeometryJSON = []byte(geometry)
		jc.FootprintGeoJSON = []byte(footprint)
		inputs.Joinable = append(inputs.Joinable, jc)
	}
	if err := rows.Err(); err != nil {
		return port.RoundInputs{}, fmt.Errorf("reading joinable candidates: %w", err)
	}

	// How many rounds in this bucket already considered each request. Counted
	// from the round ledger via the array rather than a new table — 00009 said
	// this is what the GIN-able column is for. Prior rounds only: this round
	// has not been recorded yet, and the event's "has now lost" adds one at
	// build time.
	ageRows, ageErr := tx.Query(ctx, `
		SELECT u.request_id::text, count(*)
		FROM planning.rounds r
		CROSS JOIN LATERAL unnest(r.candidate_request_ids) AS u(request_id)
		WHERE r.satellite_id = $1 AND lower(r.bucket) = $2
		GROUP BY u.request_id
	`, key.SatelliteID, key.BucketStart)
	if ageErr != nil {
		return port.RoundInputs{}, fmt.Errorf("counting prior rounds for %s: %w", key, ageErr)
	}
	defer ageRows.Close()
	inputs.AgeRounds = map[string]int{}
	for ageRows.Next() {
		var requestID string
		var count int
		if scanErr := ageRows.Scan(&requestID, &count); scanErr != nil {
			return port.RoundInputs{}, fmt.Errorf("scanning a round count: %w", scanErr)
		}
		inputs.AgeRounds[requestID] = count
	}
	if rowsErr := ageRows.Err(); rowsErr != nil {
		return port.RoundInputs{}, fmt.Errorf("reading round counts: %w", rowsErr)
	}

	// The whole profile in the same transaction, so the plan is explicable
	// against the numbers it actually used — a satellite's configuration can
	// change between rounds.
	if err := tx.QueryRow(ctx, `
		SELECT duty_cycle_budget_s, slew_rate_deg_s, settle_time_s, mode_transition_s, max_roll_deg
		FROM reference.satellites WHERE satellite_id = $1
	`, key.SatelliteID).Scan(
		&inputs.Profile.DutyCycleBudgetS,
		&inputs.Profile.Agility.SlewRateDegS,
		&inputs.Profile.Agility.SettleTimeS,
		&inputs.Profile.Agility.ModeTransitionS,
		&inputs.Profile.Agility.MaxRollDeg,
	); err != nil {
		return port.RoundInputs{}, fmt.Errorf("reading the profile for %s: %w", key.SatelliteID, err)
	}
	inputs.DutyCycleBudgetS = inputs.Profile.DutyCycleBudgetS

	if err := tx.QueryRow(ctx, `
		SELECT plan_id::text, row_version FROM planning.collection_plans
		WHERE satellite_id = $1 AND lower(bucket) = $2
		ORDER BY plan_version DESC LIMIT 1
	`, key.SatelliteID, key.BucketStart).Scan(&inputs.LivePlanID, &inputs.LivePlanRowVersion); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return port.RoundInputs{}, fmt.Errorf("reading the live plan for %s: %w", key, err)
	}

	if inputs.LivePlanID != nil {
		// Who holds a slot the re-plan is about to contest. Read from the
		// acquisitions, not the candidate ledger: a holder whose candidates
		// have since expired is exactly the one most likely to be dropped, and
		// most in need of being told.
		holderRows, holdersErr := tx.Query(ctx, `
			SELECT DISTINCT request_id::text FROM planning.acquisitions
			WHERE plan_id = $1 AND status = 'ACTIVE'
		`, *inputs.LivePlanID)
		if holdersErr != nil {
			return port.RoundInputs{}, fmt.Errorf("reading the live plan's holders: %w", holdersErr)
		}
		defer holderRows.Close()
		for holderRows.Next() {
			var requestID string
			if scanErr := holderRows.Scan(&requestID); scanErr != nil {
				return port.RoundInputs{}, fmt.Errorf("scanning a holder: %w", scanErr)
			}
			inputs.LivePlanHolders = append(inputs.LivePlanHolders, requestID)
		}
		if rowsErr := holderRows.Err(); rowsErr != nil {
			return port.RoundInputs{}, fmt.Errorf("reading holders: %w", rowsErr)
		}
	}

	// The next version is dense per bucket. collection_plans_unique_version is
	// the backstop that says so out loud if two rounds ever race past the lock.
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(max(plan_version), 0) + 1 FROM planning.collection_plans
		WHERE satellite_id = $1 AND lower(bucket) = $2
	`, key.SatelliteID, key.BucketStart).Scan(&inputs.NextPlanVersion); err != nil {
		return port.RoundInputs{}, fmt.Errorf("reading the next plan version for %s: %w", key, err)
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
func enqueue(ctx context.Context, tx pgx.Tx, eventID, eventType, subject string, payload []byte, occurredAt time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO planning.outbox (
			event_id, event_type, schema_version, subject, payload, headers, occurred_at
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7)
	`, eventID, eventType, "1.0.0", subject, string(payload), `{}`, occurredAt)
	if err != nil {
		return fmt.Errorf("enqueueing %s (%s): %w", eventType, eventID, err)
	}
	return nil
}

// commitPlan writes the plan, its acquisitions, the demotion of anything it
// supersedes, and every outbox row — all inside the caller's locked
// transaction.
//
// Order matters and is NOT relied upon. ADR-0012 made the exclusion constraint
// DEFERRABLE INITIALLY DEFERRED precisely so that demoting the old plan and
// inserting the new one can happen in either order without a hidden statement
// contract; a genuinely conflicting plan is still rejected, at COMMIT.
func commitPlan(ctx context.Context, tx pgx.Tx, round port.Round, plan port.PlanCommit) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO planning.collection_plans (
			plan_id, round_id, satellite_id, bucket, plan_version,
			supersedes_plan_id, policy, metrics, committed_at
		) VALUES ($1, $2, $3, tstzrange($4, $5, '[)'), $6, $7, $8, $9::jsonb, $10)
	`,
		plan.PlanID, plan.RoundID, round.Key.SatelliteID,
		round.Key.BucketStart, round.BucketEnd, plan.PlanVersion,
		plan.SupersedesPlanID, plan.Policy, string(plan.MetricsJSON), plan.CommittedAt,
	); err != nil {
		return fmt.Errorf("writing plan %s: %w", plan.PlanID, err)
	}

	if plan.SupersedesPlanID != nil {
		// Retained with a status, not deleted (ADR-0012). The SUPERSEDED reason
		// code promises the customer an account of what replaced them, and
		// deleting the evidence in the same transaction that creates the need
		// for it is not an explanation.
		//
		// Guarded on row_version: optimistic concurrency on the plan being
		// replaced. If another writer touched it since this round read it, zero
		// rows update and the round aborts rather than superseding a plan it
		// never saw.
		tag, err := tx.Exec(ctx, `
			UPDATE planning.collection_plans
			SET row_version = row_version + 1
			WHERE plan_id = $1 AND row_version = $2
		`, *plan.SupersedesPlanID, plan.SupersededRowVersion)
		if err != nil {
			return fmt.Errorf("superseding plan %s: %w", *plan.SupersedesPlanID, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: plan %s changed under this round (row_version %d no longer current)",
				port.ErrConcurrentPlan, *plan.SupersedesPlanID, plan.SupersededRowVersion)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE planning.acquisitions
			SET status = 'SUPERSEDED', superseded_at = $2
			WHERE plan_id = $1 AND status = 'ACTIVE'
		`, *plan.SupersedesPlanID, plan.CommittedAt); err != nil {
			return fmt.Errorf("demoting the acquisitions of plan %s: %w", *plan.SupersedesPlanID, err)
		}
	}

	batch := &pgx.Batch{}
	for _, a := range plan.Acquisitions {
		batch.Queue(`
			INSERT INTO planning.acquisitions (
				acquisition_id, plan_id, request_id, opportunity_id, customer_id,
				satellite_id, mode, acq_window, geometry, footprint,
				slew_time_from_previous_s, gap_from_previous_s, duty_cycle_cost_s,
				awarded_value_credits, clearing_price_credits, status
			) VALUES (
				$16, $1, $2, $3, $4,
				$5, $6, tstzrange($7, $8, '[)'), $9, ST_GeomFromGeoJSON($10),
				$11, $12, $13,
				$14, $15, 'ACTIVE'
			)
		`,
			plan.PlanID, a.RequestID, a.OpportunityID, a.CustomerID,
			round.Key.SatelliteID, a.Mode, a.Start, a.End, a.GeometryJSON, string(a.FootprintGeoJSON),
			a.SlewFromPreviousS, a.GapFromPreviousS, a.DutyCycleCostS,
			a.AwardedValueCredits, a.ClearingPriceCredits,
			a.AcquisitionID,
		)
	}
	results := tx.SendBatch(ctx, batch)
	for i := range plan.Acquisitions {
		if _, err := results.Exec(); err != nil {
			_ = results.Close() //nolint:errcheck // the insert error is the one that matters
			return fmt.Errorf("writing acquisition %d of %d (%s): %w",
				i+1, len(plan.Acquisitions), plan.Acquisitions[i].OpportunityID, err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("closing the acquisition batch for plan %s: %w", plan.PlanID, err)
	}

	if err := enqueue(ctx, tx, plan.PlanEventID, "planning.plan.committed.v1",
		"planning.plan.committed.v1", plan.PlanPayload, plan.CommittedAt); err != nil {
		return err
	}
	for _, event := range plan.UnfulfilledEvents {
		if err := enqueue(ctx, tx, event.EventID, event.EventType, event.Subject,
			event.Payload, plan.CommittedAt); err != nil {
			return err
		}
	}
	return nil
}

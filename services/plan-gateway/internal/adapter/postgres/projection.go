// Package postgres implements the projection and the reads.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/plan-gateway/internal/domain"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// Projection folds events into readmodel.*.
//
// EVERY WRITE IS AN UPSERT GUARDED ON last_event_at. That is what makes a
// replay from sequence zero produce identical state: folding the same event
// twice is a no-op, and folding events out of order lands in the same place
// because the guard compares when they HAPPENED rather than when they arrived.
//
// The alternative — plain INSERTs and a "have I seen this?" table — needs a
// second lookup per event and gets the ordering wrong anyway.
type Projection struct {
	pool *pgxpool.Pool
}

// NewProjection wraps a pool.
func NewProjection(pool *pgxpool.Pool) *Projection { return &Projection{pool: pool} }

// ProjectRequestReceived materialises a request view.
func (p *Projection) ProjectRequestReceived(ctx context.Context, e port.RequestReceived) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO readmodel.request_views
			(request_id, customer_id, target_name, state, request_window, target, last_event_at)
		VALUES ($1, $2, $3, 'RECEIVED', tstzrange($4, $5, '[)'),
		        NULLIF($6, '')::geometry, $7)
		ON CONFLICT (request_id) DO UPDATE SET
			customer_id    = EXCLUDED.customer_id,
			target_name    = EXCLUDED.target_name,
			request_window = EXCLUDED.request_window,
			target         = EXCLUDED.target,
			last_event_at  = EXCLUDED.last_event_at,
			updated_at     = now()
		WHERE readmodel.request_views.last_event_at <= EXCLUDED.last_event_at
	`, e.RequestID, e.CustomerID, e.TargetName, e.WindowStart, e.WindowEnd,
		wktOrEmpty(e.TargetWKT), e.EventAt)
	if err != nil {
		return fmt.Errorf("projecting request %s: %w", e.RequestID, err)
	}
	return nil
}

// ProjectOpportunities materialises the candidates for a request.
func (p *Projection) ProjectOpportunities(ctx context.Context, e port.OpportunitiesComputed) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		for _, o := range e.Opportunities {
			if _, err := tx.Exec(ctx, `
				INSERT INTO readmodel.opportunity_views
					(opportunity_id, request_id, satellite_id, mode, access_window,
					 acquisition_duration_s, orbit_number, quality_score, footprint)
				VALUES ($1, $2, $3, $4, tstzrange($5, $6, '[)'), $7, $8, $9,
				        ST_GeomFromText($10, 4326))
				ON CONFLICT (opportunity_id) DO NOTHING
			`, o.OpportunityID, e.RequestID, o.SatelliteID, o.Mode,
				o.AccessStart, o.AccessEnd, o.AcquisitionDurationS, o.OrbitNumber,
				o.QualityScore, o.FootprintWKT); err != nil {
				return fmt.Errorf("projecting opportunity %s: %w", o.OpportunityID, err)
			}
		}

		// The count is recomputed from the table and is NOT guarded on
		// last_event_at.
		//
		// It is a derived aggregate over rows that are themselves idempotent,
		// so it is order-independent by construction and needs no guard. It
		// HAD one, and the convergence test caught what that cost: when a plan
		// event arrived first and pushed last_event_at forward, the guard
		// blocked the recount and the request reported zero opportunities
		// forever. Same events, two different answers, depending only on
		// arrival order.
		//
		// The guard belongs on the STATE, which is a decision, not on a count,
		// which is a fact about other rows.
		if _, err := tx.Exec(ctx, `
			UPDATE readmodel.request_views
			SET opportunity_count = (
			        SELECT count(*) FROM readmodel.opportunity_views WHERE request_id = $1
			    ),
			    updated_at = now()
			WHERE request_id = $1
		`, e.RequestID); err != nil {
			return fmt.Errorf("recounting opportunities for %s: %w", e.RequestID, err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE readmodel.request_views
			SET state         = CASE WHEN state = 'RECEIVED' THEN 'AWAITING_PLANNING' ELSE state END,
			    last_event_at = GREATEST(last_event_at, $2),
			    updated_at    = now()
			WHERE request_id = $1 AND last_event_at <= $2
		`, e.RequestID, e.EventAt); err != nil {
			return fmt.Errorf("updating request %s: %w", e.RequestID, err)
		}
		return nil
	})
}

// ProjectPlanCommitted materialises a plan and its acquisitions.
func (p *Projection) ProjectPlanCommitted(ctx context.Context, e port.PlanCommitted) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var currentVersion int
		err := tx.QueryRow(ctx, `
			SELECT coalesce(max(plan_version), 0) FROM readmodel.plan_views
			WHERE satellite_id = $1 AND bucket = tstzrange($2, $3, '[)')
		`, e.SatelliteID, e.BucketStart, e.BucketEnd).Scan(&currentVersion)
		if err != nil {
			return fmt.Errorf("reading current plan version: %w", err)
		}

		decision := domain.DecidePlanVersion(e.PlanVersion, currentVersion)
		if decision == domain.Ignore {
			return nil
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO readmodel.plan_views
				(plan_id, satellite_id, bucket, plan_version, supersedes_plan_id,
				 superseded, policy, metrics, committed_at, last_event_at)
			VALUES ($1, $2, tstzrange($3, $4, '[)'), $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (satellite_id, bucket, plan_version) DO NOTHING
		`, e.PlanID, e.SatelliteID, e.BucketStart, e.BucketEnd, e.PlanVersion,
			e.SupersedesPlanID, decision == domain.ApplyAsSuperseded,
			e.Policy, e.MetricsJSON, e.CommittedAt, e.EventAt); err != nil {
			return fmt.Errorf("projecting plan %s: %w", e.PlanID, err)
		}

		if decision == domain.ApplyAsCurrent {
			// Everything older in this bucket is now history. Set rather than
			// toggled, so a replay reaches the same answer whatever order the
			// versions arrived in.
			if _, err := tx.Exec(ctx, `
				UPDATE readmodel.plan_views SET superseded = (plan_version < $3)
				WHERE satellite_id = $1 AND bucket = tstzrange($2, $4, '[)')
			`, e.SatelliteID, e.BucketStart, e.PlanVersion, e.BucketEnd); err != nil {
				return fmt.Errorf("superseding older plans: %w", err)
			}
		}

		for _, a := range e.Acquisitions {
			if _, err := tx.Exec(ctx, `
				INSERT INTO readmodel.acquisition_views
					(acquisition_id, plan_id, request_id, opportunity_id, customer_id,
					 satellite_id, mode, acq_window, status, footprint,
					 slew_time_from_previous_s, gap_from_previous_s, awarded_value_credits)
				VALUES ($1, $2, $3, $4, $5, $6, $7, tstzrange($8, $9, '[)'), $10,
				        ST_GeomFromText($11, 4326), $12, $13, $14)
				ON CONFLICT (acquisition_id) DO UPDATE SET
					status = EXCLUDED.status
			`, a.AcquisitionID, e.PlanID, a.RequestID, a.OpportunityID, a.CustomerID,
				e.SatelliteID, a.Mode, a.WindowStart, a.WindowEnd,
				"ACTIVE", a.FootprintWKT,
				a.SlewTimeFromPreviousS, a.GapFromPreviousS, a.AwardedValueCredits,
			); err != nil {
				return fmt.Errorf("projecting acquisition %s: %w", a.AcquisitionID, err)
			}

			if a.OpportunityID != nil {
				if _, err := tx.Exec(ctx,
					`UPDATE readmodel.opportunity_views SET won = true WHERE opportunity_id = $1`,
					*a.OpportunityID); err != nil {
					return fmt.Errorf("marking opportunity won: %w", err)
				}
			}

			if decision == domain.ApplyAsCurrent {
				if _, err := tx.Exec(ctx, `
					UPDATE readmodel.request_views
					SET state = 'PLANNED', last_event_at = GREATEST(last_event_at, $2), updated_at = now()
					WHERE request_id = $1 AND last_event_at <= $2
				`, a.RequestID, e.EventAt); err != nil {
					return fmt.Errorf("updating request state: %w", err)
				}
			}
		}

		// An acquisition's status is DERIVED from its plan's, every time,
		// rather than set from the arrival decision.
		//
		// Setting it at insert made the projection order-dependent, and the
		// convergence test caught it on its first run: when v2 arrived after
		// v1, the plan_views UPDATE above marked v1 superseded but left its
		// acquisitions ACTIVE. Folding the same two events in the other order
		// produced SUPERSEDED. Same events, two different read models — which
		// is exactly the bug that makes a rebuild change what users see.
		if _, err := tx.Exec(ctx, `
			UPDATE readmodel.acquisition_views a
			SET status = CASE WHEN p.superseded THEN 'SUPERSEDED' ELSE 'ACTIVE' END
			FROM readmodel.plan_views p
			WHERE a.plan_id = p.plan_id
			  AND p.satellite_id = $1
			  AND p.bucket = tstzrange($2, $3, '[)')
		`, e.SatelliteID, e.BucketStart, e.BucketEnd); err != nil {
			return fmt.Errorf("deriving acquisition status: %w", err)
		}
		return nil
	})
}

// ProjectUnfulfilled records why a request lost.
func (p *Projection) ProjectUnfulfilled(ctx context.Context, e port.RequestUnfulfilled) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE readmodel.request_views
		SET unfulfilment  = $2,
		    state         = CASE WHEN state = 'PLANNED' THEN 'AWAITING_PLANNING' ELSE state END,
		    last_event_at = GREATEST(last_event_at, $3),
		    updated_at    = now()
		WHERE request_id = $1 AND last_event_at <= $3
	`, e.RequestID, e.ReasonJSON, e.EventAt)
	if err != nil {
		return fmt.Errorf("projecting unfulfilment for %s: %w", e.RequestID, err)
	}
	return nil
}

// Cursor reports how far a stream has been folded.
func (p *Projection) Cursor(ctx context.Context, stream string) (port.Cursor, error) {
	var c port.Cursor
	var lastAt *time.Time
	err := p.pool.QueryRow(ctx,
		`SELECT last_sequence, last_event_at FROM readmodel.stream_cursors WHERE stream = $1`,
		stream).Scan(&c.Sequence, &lastAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return port.Cursor{}, nil
	}
	if err != nil {
		return port.Cursor{}, fmt.Errorf("reading cursor for %s: %w", stream, err)
	}
	if lastAt != nil {
		c.LastEventAt = *lastAt
	}
	return c, nil
}

// Advance records progress.
//
// GREATEST rather than assignment, so a redelivery of an older message cannot
// rewind the cursor and cause everything after it to be folded again.
func (p *Projection) Advance(ctx context.Context, stream string, sequence uint64, eventAt time.Time) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO readmodel.stream_cursors (stream, last_sequence, last_event_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (stream) DO UPDATE SET
			last_sequence = GREATEST(readmodel.stream_cursors.last_sequence, EXCLUDED.last_sequence),
			last_event_at = GREATEST(
				coalesce(readmodel.stream_cursors.last_event_at, EXCLUDED.last_event_at),
				EXCLUDED.last_event_at),
			updated_at    = now()
	`, stream, int64(sequence), eventAt)
	if err != nil {
		return fmt.Errorf("advancing cursor for %s: %w", stream, err)
	}
	return nil
}

// Reset clears every projection.
//
// The operation the replay test exercises. A rebuild that cannot be triggered
// is a rebuild nobody will do, and a read model nobody can rebuild is one
// whose bugs are permanent.
func (p *Projection) Reset(ctx context.Context) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		for _, table := range []string{
			"readmodel.acquisition_views",
			"readmodel.plan_views",
			"readmodel.opportunity_views",
			"readmodel.request_views",
			"readmodel.stream_cursors",
		} {
			if _, err := tx.Exec(ctx, "DELETE FROM "+table); err != nil { //nolint:gosec // fixed list
				return fmt.Errorf("clearing %s: %w", table, err)
			}
		}
		return nil
	})
}

func wktOrEmpty(wkt string) any {
	if wkt == "" {
		return ""
	}
	return "SRID=4326;" + wkt
}

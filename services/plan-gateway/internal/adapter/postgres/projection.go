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
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		return p.projectRequestReceived(ctx, tx, e)
	})
}

func (p *Projection) projectRequestReceived(ctx context.Context, tx pgx.Tx, e port.RequestReceived) error {
	_, err := tx.Exec(ctx, `
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
		geometryOrNull(e.TargetGeoJSON), e.EventAt)
	if err != nil {
		return fmt.Errorf("projecting request %s: %w", e.RequestID, err)
	}

	// The request can arrive AFTER the events that depend on it. JetStream
	// orders per subject and nothing orders across streams, so a plan committed
	// for a request whose own event has not landed yet is ordinary, not
	// exceptional. Reconciling here is what makes the request event's late
	// arrival converge to the same state as its early one — the integration
	// test that reverses the whole sequence found this by reporting
	// state=RECEIVED opportunities=0 against state=PLANNED opportunities=2.
	return reconcileRequest(ctx, tx, e.RequestID, e.EventAt)
}

// reconcileRequest recomputes everything about a request that is DERIVED.
//
// From the rows that exist, never from the event that happens to be in hand.
// A count incremented on arrival and a state set by whichever event came last
// are both functions of the network; recomputing makes them functions of the
// facts, which is the only way the fold converges under reordering.
//
// last_event_at is a high-water mark. Guarding it would let an old event that
// legitimately updates identity fields drag staleness backwards.
// reconcileSQL recomputes the derived columns for whichever requests the
// caller selects. The selection is the only thing that varies, so the
// derivation itself is written once — two copies of this drifting apart is
// exactly how the count and the state ended up with different guards.
//
// State precedence:
//
//	a live acquisition exists  -> PLANNED
//	any opportunity exists     -> AWAITING_PLANNING
//	otherwise                  -> RECEIVED
//
// This subsumes unfulfilment rather than special-casing it. There is no
// UNFULFILLED state and there must not be: a request that lost ages, gains
// fairness weight, and competes again. Note the consequence — an unfulfilment
// alongside a still-live acquisition does NOT demote the request, and should
// not, because until the displacing plan lands the request genuinely is still
// planned.
//
// The terminal states (EXPIRED, CANCELLED, INFEASIBLE, ACQUIRED) are not
// derivable from the four events this gateway folds and are not projected yet.
// When their events land they must win over this derivation, because they are
// decisions rather than consequences.
//
// last_event_at is a high-water mark. Guarding it would let an old event that
// legitimately updates identity fields drag staleness backwards.
const reconcileSQL = `
	UPDATE readmodel.request_views r
	SET opportunity_count = (
	        SELECT count(*) FROM readmodel.opportunity_views o
	        WHERE o.request_id = r.request_id
	    ),
	    state = CASE
	        WHEN EXISTS (
	            SELECT 1 FROM readmodel.acquisition_views a
	            WHERE a.request_id = r.request_id AND a.status <> 'SUPERSEDED'
	        ) THEN 'PLANNED'
	        WHEN EXISTS (
	            SELECT 1 FROM readmodel.opportunity_views o
	            WHERE o.request_id = r.request_id
	        ) THEN 'AWAITING_PLANNING'
	        ELSE 'RECEIVED'
	    END,
	    last_event_at = GREATEST(r.last_event_at, $2),
	    updated_at    = now()
`

// reconcileRequest recomputes everything DERIVED about one request.
//
// From the rows that exist, never from the event in hand. A count incremented
// on arrival and a state set by whichever event came last are both functions of
// the network; recomputing makes them functions of the facts, which is the only
// way the fold converges under reordering.
func reconcileRequest(ctx context.Context, tx pgx.Tx, requestID string, eventAt time.Time) error {
	if _, err := tx.Exec(ctx, reconcileSQL+` WHERE r.request_id = $1`, requestID, eventAt); err != nil {
		return fmt.Errorf("reconciling request %s: %w", requestID, err)
	}
	return nil
}

// reconcileBucket recomputes every request with an acquisition in a bucket.
//
// Every request, not just the ones the arriving plan names. A new plan version
// DISPLACES requests, and a displaced request appears nowhere in the event that
// displaced it — its acquisition is quietly marked SUPERSEDED and, if only the
// arriving plan's requests were reconciled, its state would stay PLANNED
// forever with no acquisition to back it up. Found by an integration test that
// awarded version 2 to a rival.
func reconcileBucket(ctx context.Context, tx pgx.Tx, e port.PlanCommitted) error {
	if _, err := tx.Exec(ctx, reconcileSQL+`
		WHERE r.request_id IN (
		    SELECT a.request_id
		    FROM readmodel.acquisition_views a
		    JOIN readmodel.plan_views p ON p.plan_id = a.plan_id
		    WHERE p.satellite_id = $1 AND p.bucket = tstzrange($3, $4, '[)')
		)
	`, e.SatelliteID, e.EventAt, e.BucketStart, e.BucketEnd); err != nil {
		return fmt.Errorf("reconciling bucket for plan %s: %w", e.PlanID, err)
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
				        ST_SetSRID(ST_GeomFromGeoJSON($10), 4326))
				ON CONFLICT (opportunity_id) DO NOTHING
			`, o.OpportunityID, e.RequestID, o.SatelliteID, o.Mode,
				o.AccessStart, o.AccessEnd, o.AcquisitionDurationS, o.OrbitNumber,
				o.QualityScore, geometryOrNull(o.FootprintGeoJSON)); err != nil {
				return fmt.Errorf("projecting opportunity %s: %w", o.OpportunityID, err)
			}
		}

		// Everything derived goes through one function. The count used to be
		// recomputed here and the state set with a CASE, and keeping the two
		// in step across four folds was never going to hold — the count lost
		// its last_event_at guard for good reasons the state still had.
		return reconcileRequest(ctx, tx, e.RequestID, e.EventAt)
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
				        ST_SetSRID(ST_GeomFromGeoJSON($11), 4326), $12, $13, $14)
				ON CONFLICT (acquisition_id) DO UPDATE SET
					status = EXCLUDED.status
			`, a.AcquisitionID, e.PlanID, a.RequestID, a.OpportunityID, a.CustomerID,
				e.SatelliteID, a.Mode, a.WindowStart, a.WindowEnd,
				"ACTIVE", geometryOrNull(a.FootprintGeoJSON),
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
		return reconcileBucket(ctx, tx, e)
	})
}

// ProjectEphemeris materialises one satellite's sampled track.
//
// One statement for the whole bucket, via unnest. A thousand samples is a
// thousand rows, and a thousand round trips to insert them would make the fold
// slower than the propagation that produced them.
//
// The guard is on tle_epoch, not on last_event_at, and that is the one
// interesting decision here. A satellite gets a new element set roughly daily,
// and the next sweep republishes the same buckets from it — same satellite, the
// same instants, slightly different positions. Newest ELEMENT SET wins, not
// newest event: two events for one bucket are not a correction and a stale one,
// they are two different answers whose quality is ordered by the TLE behind
// them. Comparing epochs makes the fold converge on the same track whatever
// order they arrive in.
func (p *Projection) ProjectEphemeris(ctx context.Context, e port.EphemerisComputed) error {
	if len(e.Samples) == 0 {
		return nil
	}

	at := make([]time.Time, len(e.Samples))
	longitudes := make([]float64, len(e.Samples))
	latitudes := make([]float64, len(e.Samples))
	altitudes := make([]float64, len(e.Samples))
	for i, s := range e.Samples {
		at[i] = s.At
		longitudes[i] = s.LongitudeDeg
		latitudes[i] = s.LatitudeDeg
		altitudes[i] = s.AltitudeM
	}

	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO readmodel.ephemeris
				(satellite_id, sample_at, longitude_deg, latitude_deg, altitude_m,
				 tle_epoch, last_event_at)
			SELECT $1, s.at, s.lon, s.lat, s.alt, $6, $7
			FROM unnest($2::timestamptz[], $3::float8[], $4::float8[], $5::float8[])
			     AS s(at, lon, lat, alt)
			ON CONFLICT (satellite_id, sample_at) DO UPDATE SET
				longitude_deg = EXCLUDED.longitude_deg,
				latitude_deg  = EXCLUDED.latitude_deg,
				altitude_m    = EXCLUDED.altitude_m,
				tle_epoch     = EXCLUDED.tle_epoch,
				last_event_at = GREATEST(readmodel.ephemeris.last_event_at, EXCLUDED.last_event_at)
			WHERE readmodel.ephemeris.tle_epoch <= EXCLUDED.tle_epoch
		`, e.SatelliteID, at, longitudes, latitudes, altitudes, e.TleEpoch, e.EventAt)
		if err != nil {
			return fmt.Errorf("projecting ephemeris for %s: %w", e.SatelliteID, err)
		}
		return nil
	})
}

// ProjectUnfulfilled records why a request lost.
func (p *Projection) ProjectUnfulfilled(ctx context.Context, e port.RequestUnfulfilled) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// The explanation is stored; the state is not touched here. Losing a
		// round supersedes the acquisition, and the derivation in
		// reconcileRequest reads that and drops the request back to
		// AWAITING_PLANNING on its own. Writing the state here as well would
		// be a second opinion on the same question.
		//
		// The guard stays on unfulfilment because it is a DECISION — the newest
		// explanation is the true one, and an old one redelivered must not
		// overwrite it.
		if _, err := tx.Exec(ctx, `
			UPDATE readmodel.request_views
			SET unfulfilment = $2, updated_at = now()
			WHERE request_id = $1 AND last_event_at <= $3
		`, e.RequestID, e.ReasonJSON, e.EventAt); err != nil {
			return fmt.Errorf("projecting unfulfilment for %s: %w", e.RequestID, err)
		}
		return reconcileRequest(ctx, tx, e.RequestID, e.EventAt)
	})
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
			"readmodel.ephemeris",
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

// geometryOrNull hands raw GeoJSON to PostGIS, or NULL when there is none.
//
// A nil slice must become a SQL NULL rather than an empty string: an empty
// string is not valid GeoJSON and ST_GeomFromGeoJSON errors on it, which would
// turn "this acquisition has no footprint" — a legitimate state — into a failed
// fold and an endlessly redelivered message.
func geometryOrNull(geojson []byte) any {
	if len(geojson) == 0 {
		return nil
	}
	return string(geojson)
}

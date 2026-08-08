package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// Coordinate precision is the literal 6 in every ST_AsGeoJSON call below, and
// deliberately not a Go constant.
//
// It was a constant first, written as ST_AsGeoJSON(footprint, geoJSONPrecision)
// — inside a backtick string, where Go does not interpolate anything. Postgres
// would have read it as a column name and every geo query would have failed at
// runtime. `unused` caught the orphaned constant; nothing else would have, until
// the first request. A value that has to be pasted into SQL by hand is better
// written where it is read.
//
// Why 6: PostGIS defaults to 9 decimal places, roughly 0.1 mm, which is about a
// third of the payload for no information — no SAR footprint edge is known to a
// millimetre, and the number comes from a propagator whose own error is metres.
// Six places is ~0.1 m at the equator and takes about 30% off every response.
//
// Measured, not assumed:
//
//	ST_AsGeoJSON(POINT(4.123456789012 51.987654321098))  -> [4.123456789,51.987654321]
//	ST_AsGeoJSON(..., 6)                                 -> [4.123457,51.987654]

// Reads serves the REST endpoints from readmodel.*.
type Reads struct {
	pool *pgxpool.Pool
}

// NewReads wraps a pool.
func NewReads(pool *pgxpool.Pool) *Reads { return &Reads{pool: pool} }

// Plans lists plans, newest bucket first.
func (r *Reads) Plans(ctx context.Context, q port.PlanQuery) ([]port.PlanView, port.Cursor, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT plan_id::text, satellite_id, lower(bucket), upper(bucket), plan_version,
		       supersedes_plan_id::text, superseded, policy, metrics::text, committed_at
		FROM readmodel.plan_views
		WHERE ($1 = '' OR satellite_id = $1)
		  AND ($2::timestamptz IS NULL OR lower(bucket) >= $2)
		  AND ($3::timestamptz IS NULL OR lower(bucket) <= $3)
		  AND ($4 OR NOT superseded)
		ORDER BY lower(bucket) DESC, satellite_id, plan_version DESC
		LIMIT $5
	`, q.SatelliteID, q.BucketStartAfter, q.BucketStartBefore, q.IncludeSuperseded, limitOr(q.Limit))
	if err != nil {
		return nil, port.Cursor{}, fmt.Errorf("listing plans: %w", err)
	}
	defer rows.Close()

	var out []port.PlanView
	for rows.Next() {
		var p port.PlanView
		var supersedes *string
		var metrics string
		if scanErr := rows.Scan(&p.PlanID, &p.SatelliteID, &p.BucketStart, &p.BucketEnd,
			&p.PlanVersion, &supersedes, &p.Superseded, &p.Policy, &metrics,
			&p.CommittedAt); scanErr != nil {
			return nil, port.Cursor{}, fmt.Errorf("scanning plan: %w", scanErr)
		}
		p.SupersedesPlanID = supersedes
		p.MetricsJSON = []byte(metrics)
		out = append(out, p)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, port.Cursor{}, fmt.Errorf("reading plans: %w", rowsErr)
	}

	cursor, err := r.cursor(ctx)
	return out, cursor, err
}

// Plan reads one bucket's plan, current or named.
func (r *Reads) Plan(
	ctx context.Context, satelliteID string, bucketStart time.Time, version *int,
) (port.PlanView, error) {
	var p port.PlanView
	var supersedes *string
	var metrics string

	err := r.pool.QueryRow(ctx, `
		SELECT plan_id::text, satellite_id, lower(bucket), upper(bucket), plan_version,
		       supersedes_plan_id::text, superseded, policy, metrics::text, committed_at
		FROM readmodel.plan_views
		WHERE satellite_id = $1 AND lower(bucket) = $2
		  AND ($3::int IS NULL OR plan_version = $3)
		  AND ($3::int IS NOT NULL OR NOT superseded)
		ORDER BY plan_version DESC
		LIMIT 1
	`, satelliteID, bucketStart, version).Scan(&p.PlanID, &p.SatelliteID,
		&p.BucketStart, &p.BucketEnd, &p.PlanVersion, &supersedes, &p.Superseded,
		&p.Policy, &metrics, &p.CommittedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return port.PlanView{}, port.ErrNotFound
	}
	if err != nil {
		return port.PlanView{}, fmt.Errorf("reading plan: %w", err)
	}
	p.SupersedesPlanID = supersedes
	p.MetricsJSON = []byte(metrics)

	acqs, err := r.acquisitionsForPlan(ctx, p.PlanID)
	if err != nil {
		return port.PlanView{}, err
	}
	p.Acquisitions = acqs
	return p, nil
}

func (r *Reads) acquisitionsForPlan(ctx context.Context, planID string) ([]port.AcquisitionView, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT acquisition_id::text, plan_id::text, request_id::text, customer_id,
		       satellite_id, mode, lower(acq_window), upper(acq_window), status,
		       ST_AsGeoJSON(footprint, 6), slew_time_from_previous_s,
		       gap_from_previous_s, awarded_value_credits
		FROM readmodel.acquisition_views
		WHERE plan_id = $1
		ORDER BY lower(acq_window)
	`, planID)
	if err != nil {
		return nil, fmt.Errorf("reading acquisitions: %w", err)
	}
	defer rows.Close()
	return scanAcquisitions(rows)
}

// Acquisitions lists by time range.
func (r *Reads) Acquisitions(ctx context.Context, q port.AcquisitionQuery) ([]port.AcquisitionView, port.Cursor, error) {
	statuses := q.Statuses
	if len(statuses) == 0 {
		// The live set. A timeline showing superseded acquisitions alongside
		// live ones is a timeline nobody can read.
		statuses = []string{"ACTIVE", "EXECUTED"}
	}

	rows, err := r.pool.Query(ctx, `
		SELECT acquisition_id::text, plan_id::text, request_id::text, customer_id,
		       satellite_id, mode, lower(acq_window), upper(acq_window), status,
		       ST_AsGeoJSON(footprint, 6), slew_time_from_previous_s,
		       gap_from_previous_s, awarded_value_credits
		FROM readmodel.acquisition_views
		WHERE ($1 = '' OR satellite_id = $1)
		  AND acq_window && tstzrange($2, $3, '[)')
		  AND ($4 = '' OR request_id::text = $4)
		  AND status = ANY($5)
		ORDER BY lower(acq_window)
		LIMIT $6
	`, q.SatelliteID, q.WindowStart, q.WindowEnd, q.RequestID, statuses, limitOr(q.Limit))
	if err != nil {
		return nil, port.Cursor{}, fmt.Errorf("listing acquisitions: %w", err)
	}
	defer rows.Close()

	out, err := scanAcquisitions(rows)
	if err != nil {
		return nil, port.Cursor{}, err
	}
	cursor, err := r.cursor(ctx)
	return out, cursor, err
}

func scanAcquisitions(rows pgx.Rows) ([]port.AcquisitionView, error) {
	var out []port.AcquisitionView
	for rows.Next() {
		var a port.AcquisitionView
		var geojson string
		if err := rows.Scan(&a.AcquisitionID, &a.PlanID, &a.RequestID, &a.CustomerID,
			&a.SatelliteID, &a.Mode, &a.WindowStart, &a.WindowEnd, &a.Status,
			&geojson, &a.SlewTimeFromPreviousS, &a.GapFromPreviousS,
			&a.AwardedValueCredits); err != nil {
			return nil, fmt.Errorf("scanning acquisition: %w", err)
		}
		a.FootprintGeoJSON = []byte(geojson)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading acquisitions: %w", err)
	}
	return out, nil
}

// Request reads one request's projected view.
func (r *Reads) Request(ctx context.Context, requestID string) (port.RequestView, error) {
	var v port.RequestView
	var unfulfilment *string
	err := r.pool.QueryRow(ctx, `
		SELECT request_id::text, customer_id, target_name, state,
		       lower(request_window), upper(request_window), opportunity_count,
		       unfulfilment::text, last_event_at
		FROM readmodel.request_views WHERE request_id = $1
	`, requestID).Scan(&v.RequestID, &v.CustomerID, &v.TargetName, &v.State,
		&v.WindowStart, &v.WindowEnd, &v.OpportunityCount, &unfulfilment, &v.LastEventAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return port.RequestView{}, port.ErrNotFound
	}
	if err != nil {
		return port.RequestView{}, fmt.Errorf("reading request: %w", err)
	}
	if unfulfilment != nil {
		v.UnfulfilmentJSON = []byte(*unfulfilment)
	}
	return v, nil
}

// RequestOpportunities lists every candidate, won or not.
//
// The losers are the point: showing only the winner explains nothing about why
// it won, and the ghost rendering in M4-05 needs them.
func (r *Reads) RequestOpportunities(
	ctx context.Context, requestID string,
) ([]port.OpportunityView, port.Cursor, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT opportunity_id::text, satellite_id, mode,
		       lower(access_window), upper(access_window), acquisition_duration_s,
		       orbit_number, quality_score, ST_AsGeoJSON(footprint, 6), won
		FROM readmodel.opportunity_views
		WHERE request_id = $1
		ORDER BY lower(access_window)
	`, requestID)
	if err != nil {
		return nil, port.Cursor{}, fmt.Errorf("listing opportunities: %w", err)
	}
	defer rows.Close()

	var out []port.OpportunityView
	for rows.Next() {
		var o port.OpportunityView
		var geojson string
		if scanErr := rows.Scan(&o.OpportunityID, &o.SatelliteID, &o.Mode,
			&o.AccessStart, &o.AccessEnd, &o.AcquisitionDurationS, &o.OrbitNumber,
			&o.QualityScore, &geojson, &o.Won); scanErr != nil {
			return nil, port.Cursor{}, fmt.Errorf("scanning opportunity: %w", scanErr)
		}
		o.FootprintGeoJSON = []byte(geojson)
		out = append(out, o)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, port.Cursor{}, fmt.Errorf("reading opportunities: %w", rowsErr)
	}

	cursor, err := r.cursor(ctx)
	return out, cursor, err
}

// cursor reports the newest event folded across all streams.
//
// The MAXIMUM rather than the minimum. Staleness is "how old is the newest
// thing I know", and taking the oldest cursor would report a stream that is
// simply quiet as though the whole projection were behind.
func (r *Reads) cursor(ctx context.Context) (port.Cursor, error) {
	var lastAt *time.Time
	if err := r.pool.QueryRow(ctx,
		`SELECT max(last_event_at) FROM readmodel.stream_cursors`).Scan(&lastAt); err != nil {
		return port.Cursor{}, fmt.Errorf("reading staleness: %w", err)
	}
	if lastAt == nil {
		return port.Cursor{}, nil
	}
	return port.Cursor{LastEventAt: *lastAt}, nil
}

func limitOr(n int) int {
	if n <= 0 || n > 200 {
		return 50
	}
	return n
}

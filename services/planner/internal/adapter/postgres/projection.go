// Package postgres writes the planner's input projections.
//
// Every method here is one transaction that records the event in
// planning.processed_events AND applies the projection. They commit together or
// not at all. Recording the event outside the transaction that applied it turns
// a crash between the two into a permanently skipped event — and a skipped
// event is invisible: nothing retries, nothing alerts, a customer's request
// simply never competes in any round.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// Projections is the pgx-backed implementation of port.Projections.
type Projections struct {
	pool *pgxpool.Pool
}

// NewProjections wraps a pool.
func NewProjections(pool *pgxpool.Pool) *Projections {
	return &Projections{pool: pool}
}

// ProjectSnapshot folds tasking.request.received.v1 into
// planning.request_snapshots.
func (p *Projections) ProjectSnapshot(ctx context.Context, consumer string, e port.RequestReceived) (bool, error) {
	var applied bool
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		first, err := claimEvent(ctx, tx, consumer, e.EventID)
		if err != nil {
			return err
		}
		if !first {
			return nil // a redelivery; the ledger already has it
		}
		applied = true

		s := e.Snapshot
		// ON CONFLICT DO NOTHING rather than an upsert, and ADR-0015 says why:
		// occurred_at is the ordering key a future update would compare
		// against, but today the only concurrency this row faces is
		// redelivery, which the primary key absorbs. An upsert would invent a
		// last-writer-wins rule nobody has asked for, over two events that the
		// contract says cannot both exist.
		_, err = tx.Exec(ctx, `
			INSERT INTO planning.request_snapshots (
				request_id, customer_id, priority_tier, bid_credits,
				request_window, submitted_at, source_event_id, occurred_at
			) VALUES ($1, $2, $3, $4, tstzrange($5, $6, '[)'), $7, $8, $9)
			ON CONFLICT (request_id) DO NOTHING
		`,
			s.RequestID, s.CustomerID, s.PriorityTier, s.BidCredits,
			s.WindowStart, s.WindowEnd, s.SubmittedAt, s.SourceEventID, s.OccurredAt,
		)
		if err != nil {
			return fmt.Errorf("inserting request snapshot %s: %w", s.RequestID, err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// ProjectCandidates folds feasibility.opportunities.computed.v1 into
// planning.candidate_opportunities.
//
// One transaction for the whole batch. A partially projected event would let a
// round allocate over some of a request's options while the rest were never
// offered, and then report the difference as unfulfilment — telling a customer
// they lost a competition that was never run.
func (p *Projections) ProjectCandidates(ctx context.Context, consumer string, e port.OpportunitiesComputed) (bool, error) {
	var applied bool
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		first, err := claimEvent(ctx, tx, consumer, e.EventID)
		if err != nil {
			return err
		}
		if !first {
			return nil
		}
		applied = true

		// A batch rather than a loop of round trips: a 5 000-candidate event is
		// contract-legal, and 5 000 sequential round trips inside one
		// transaction is a long-held write lock for no reason.
		batch := &pgx.Batch{}
		for _, c := range e.Candidates {
			batch.Queue(`
				INSERT INTO planning.candidate_opportunities (
					opportunity_id, request_id, satellite_id, mode,
					access_window, acquisition_duration_s, orbit_number,
					geometry, footprint, duty_cycle_cost_s, quality_score,
					computed_at, source_event_id
				) VALUES (
					$1, $2, $3, $4,
					tstzrange($5, $6, '[)'), $7, $8,
					$9, ST_GeomFromGeoJSON($10), $11, $12,
					$13, $14
				)
				ON CONFLICT (opportunity_id) DO NOTHING
			`,
				c.OpportunityID, c.RequestID, c.SatelliteID, c.Mode,
				c.AccessStart, c.AccessEnd, c.AcquisitionDurationS, c.OrbitNumber,
				c.GeometryJSON, string(c.FootprintGeoJSON), c.DutyCycleCostS, c.QualityScore,
				c.ComputedAt, c.SourceEventID,
			)
		}

		results := p.send(ctx, tx, batch)
		defer func() { _ = results.Close() }() //nolint:errcheck // the exec errors below are the ones that matter

		for i := range e.Candidates {
			if _, err := results.Exec(); err != nil {
				return fmt.Errorf("inserting candidate %d of %d (%s): %w",
					i+1, len(e.Candidates), e.Candidates[i].OpportunityID, err)
			}
		}
		// Closed explicitly as well as deferred: the deferred Close cannot
		// report, and a batch whose results are abandoned unread leaves the
		// connection in a state pgx will complain about on the next use.
		if err := results.Close(); err != nil {
			return fmt.Errorf("closing candidate batch for %s: %w", e.RequestID, err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

func (p *Projections) send(ctx context.Context, tx pgx.Tx, batch *pgx.Batch) pgx.BatchResults {
	return tx.SendBatch(ctx, batch)
}

// claimEvent records the event in the dedup ledger, reporting whether this is
// the first time it has been seen.
//
// The INSERT is the claim. Checking with a SELECT and then inserting would be a
// read-then-write race between two projector replicas, and the whole point of
// the ledger is that it is the thing being raced on. ON CONFLICT DO NOTHING
// makes the database arbitrate: exactly one caller sees a row affected.
//
// Keyed (consumer, event_id) per ADR-0008, so the two streams cannot mask each
// other's redeliveries — the same event replayed onto a second consumer is a
// separate fact.
func claimEvent(ctx context.Context, tx pgx.Tx, consumer, eventID string) (bool, error) {
	if eventID == "" {
		// Would become an empty dedup key, so every such message would look
		// like a redelivery of the same one and all but the first would be
		// silently dropped. The decoder already refuses these; this is the
		// second line, because the consequence is silent data loss.
		return false, fmt.Errorf("%w: empty event_id cannot be deduplicated", domain.ErrInvalid)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO planning.processed_events (consumer, event_id)
		VALUES ($1, $2)
		ON CONFLICT (consumer, event_id) DO NOTHING
	`, consumer, eventID)
	if err != nil {
		return false, fmt.Errorf("claiming event %s for %s: %w", eventID, consumer, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Package consume is effectively-once processing on top of at-least-once
// delivery, extracted from the pattern the planner and the feasibility service
// each built separately — because four subtly different implementations of the
// same pattern is how one of them ends up wrong.
//
// JetStream gives at-least-once. Nothing here claims exactly-once. What this
// package builds is *effectively-once processing*: the work may be attempted
// more than once, but it lands exactly once, because the dedup claim and the
// state change commit in ONE transaction and the ack follows the commit:
//
//	BEGIN;
//	  INSERT INTO <schema>.processed_events (consumer, event_id) VALUES (...);
//	  -- the actual state change
//	COMMIT;
//	-- then ACK
//
// The two halves must commit or fail together. As separate transactions, a
// crash between them either loses the work or marks it permanently done without
// having happened — and both are silent.
package consume

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Executor is the one method Cleanup needs, satisfied by both *pgxpool.Pool
// and pgx.Tx — so a service can clean inside or outside a transaction without
// this module choosing for it.
type Executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// tableName guards the one piece of SQL this package interpolates. The ledger
// table is schema-qualified per service (planning.processed_events,
// feasibility.processed_events), so it arrives as a string — and a string that
// reaches SQL unvalidated is an injection waiting for a config file to go
// wrong.
var tableName = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)

// Ledger claims events for one schema's processed_events table.
type Ledger struct {
	table string
}

// NewLedger validates the table once, so every later call site stays a
// one-liner.
func NewLedger(table string) (Ledger, error) {
	if !tableName.MatchString(table) {
		return Ledger{}, fmt.Errorf("ledger table %q is not schema.table", table)
	}
	return Ledger{table: table}, nil
}

// Claim records the event inside the caller's transaction, reporting whether
// this is the first time it has been seen.
//
// The INSERT is the claim. Checking with a SELECT and then inserting would be a
// read-then-write race between two replicas, and the whole point of the ledger
// is that it is the thing being raced on: ON CONFLICT DO NOTHING makes the
// database arbitrate, and exactly one caller sees a row affected.
//
// Keyed (consumer, event_id), so two consumers of one stream cannot mask each
// other's redeliveries — the same event replayed onto a second consumer is a
// separate fact.
func (l Ledger) Claim(ctx context.Context, tx pgx.Tx, consumer, eventID string) (bool, error) {
	if eventID == "" {
		// An empty id would become an empty dedup key, making every such
		// message look like a redelivery of the same one — all but the first
		// silently dropped. Silent data loss earns a refusal, not a shrug.
		return false, fmt.Errorf("empty event_id cannot be deduplicated")
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO `+l.table+` (consumer, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		consumer, eventID)
	if err != nil {
		return false, fmt.Errorf("claiming event %s for %s: %w", eventID, consumer, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Cleanup deletes ledger rows older than retention, refusing any retention
// that could outrun redelivery.
//
// The guard is the point of the function. A ledger row younger than the
// broker's longest possible redelivery horizon is still doing its job: delete
// it and a late redelivery is reprocessed as NEW — a silent correctness bug
// that only appears under load, when redeliveries are slowest and cleanup
// pressure is highest. The margin multiplies max_deliver by ack_wait because
// that is the worst case the broker is configured to produce, and doubles it
// because clocks and queues are real.
func (l Ledger) Cleanup(
	ctx context.Context,
	db Executor,
	retention time.Duration,
	maxDeliver int,
	ackWait time.Duration,
) (int64, error) {
	redeliveryHorizon := time.Duration(maxDeliver) * ackWait * 2
	if retention < redeliveryHorizon {
		return 0, fmt.Errorf(
			"retention %s is inside the redelivery horizon %s (max_deliver %d × ack_wait %s × 2); "+
				"a late redelivery would be reprocessed as new",
			retention, redeliveryHorizon, maxDeliver, ackWait)
	}
	tag, err := db.Exec(ctx,
		`DELETE FROM `+l.table+` WHERE processed_at < now() - $1::interval`,
		retention.String())
	if err != nil {
		return 0, fmt.Errorf("cleaning %s: %w", l.table, err)
	}
	return tag.RowsAffected(), nil
}

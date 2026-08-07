// Package postgres holds the database adapter.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// querier is the one method Probe needs, declared here so the check can be
// tested without a database.
//
// Narrow on purpose: an interface with one method has one thing to fake, and
// the alternative — an integration test for a SELECT 1 — buys nothing the
// readiness endpoint's own test does not already cover.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Probe reports whether Postgres is reachable, for /readyz.
//
// Readiness checks Postgres and deliberately NOT NATS. The outbox decouples
// publication from acceptance: with the broker down, requests are still
// persisted and drain when the relay reconnects. Failing readiness on a broker
// outage would turn a recoverable delay into refused customer traffic and throw
// away the entire benefit of the outbox pattern.
type Probe struct {
	db querier
}

// NewProbe wraps a pool.
func NewProbe(pool *pgxpool.Pool) *Probe { return &Probe{db: pool} }

// newProbeWith exists for tests, which supply their own querier.
func newProbeWith(db querier) *Probe { return &Probe{db: db} }

// Name identifies this dependency in the readiness response.
func (p *Probe) Name() string { return "postgres" }

// Check runs the cheapest query that proves a usable connection.
//
// `SELECT 1` and not `Ping`: pgxpool's Ping can be satisfied by a connection
// that is established but whose backend is wedged, and a readiness probe that
// passes against a wedged backend is worse than none.
func (p *Probe) Check(ctx context.Context) error {
	var one int
	if err := p.db.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("postgres query failed: %w", err)
	}
	if one != 1 {
		return fmt.Errorf("postgres returned %d for SELECT 1", one)
	}
	return nil
}

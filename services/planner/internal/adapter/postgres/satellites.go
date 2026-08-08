package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// Satellites reads reference.satellites.
type Satellites struct {
	pool *pgxpool.Pool
}

// NewSatellites wraps a pool.
func NewSatellites(pool *pgxpool.Pool) *Satellites { return &Satellites{pool: pool} }

// Agility returns one satellite's transition parameters.
//
// Validated on the way out, not trusted. The columns carry CHECK constraints, so
// a violating row cannot exist — but this service will also be pointed at
// databases restored from elsewhere, and a slew rate of zero reaching the
// transition function divides by zero rather than saying what is wrong.
func (s *Satellites) Agility(ctx context.Context, satelliteID string) (domain.Agility, error) {
	var a domain.Agility
	err := s.pool.QueryRow(ctx, `
		SELECT slew_rate_deg_s, settle_time_s, mode_transition_s, max_roll_deg
		FROM reference.satellites
		WHERE satellite_id = $1
	`, satelliteID).Scan(&a.SlewRateDegS, &a.SettleTimeS, &a.ModeTransitionS, &a.MaxRollDeg)

	if errors.Is(err, pgx.ErrNoRows) {
		// Distinct from a satellite with default parameters. "I have never
		// heard of this satellite" and "this satellite is unremarkable" are
		// different answers, and a planner that conflated them would schedule
		// against a spacecraft that does not exist.
		return domain.Agility{}, fmt.Errorf("%w: satellite %s", port.ErrNotFound, satelliteID)
	}
	if err != nil {
		return domain.Agility{}, fmt.Errorf("reading agility for %s: %w", satelliteID, err)
	}
	if err := a.Validate(); err != nil {
		return domain.Agility{}, fmt.Errorf("satellite %s has unusable agility: %w", satelliteID, err)
	}
	return a, nil
}

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

// Profile returns one satellite's agility and power budget.
//
// Validated on the way out, not trusted. The columns carry CHECK constraints, so
// a violating row cannot exist — but this service will also be pointed at
// databases restored from elsewhere, and a slew rate of zero reaching the
// transition function divides by zero rather than saying what is wrong, while a
// zero power budget makes every candidate unaffordable and produces an empty
// plan nobody can explain.
func (s *Satellites) Profile(ctx context.Context, satelliteID string) (domain.SatelliteProfile, error) {
	var p domain.SatelliteProfile
	err := s.pool.QueryRow(ctx, `
		SELECT slew_rate_deg_s, settle_time_s, mode_transition_s, max_roll_deg, duty_cycle_budget_s
		FROM reference.satellites
		WHERE satellite_id = $1
	`, satelliteID).Scan(&p.Agility.SlewRateDegS, &p.Agility.SettleTimeS,
		&p.Agility.ModeTransitionS, &p.Agility.MaxRollDeg, &p.DutyCycleBudgetS)

	if errors.Is(err, pgx.ErrNoRows) {
		// Distinct from a satellite with default parameters. "I have never
		// heard of this satellite" and "this satellite is unremarkable" are
		// different answers, and a planner that conflated them would schedule
		// against a spacecraft that does not exist.
		return domain.SatelliteProfile{}, fmt.Errorf("%w: satellite %s", port.ErrNotFound, satelliteID)
	}
	if err != nil {
		return domain.SatelliteProfile{}, fmt.Errorf("reading the profile for %s: %w", satelliteID, err)
	}
	if err := p.Validate(); err != nil {
		return domain.SatelliteProfile{}, fmt.Errorf("satellite %s has an unusable profile: %w", satelliteID, err)
	}
	return p, nil
}

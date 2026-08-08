package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

// States applies lifecycle transitions to tasking requests.
type States struct {
	pool *pgxpool.Pool
}

// NewStates wraps a pool.
func NewStates(pool *pgxpool.Pool) *States { return &States{pool: pool} }

// Apply moves one request, with the decision made under a row lock.
//
// SELECT ... FOR UPDATE, decide, UPDATE — all in one transaction. Three
// consumers drive this machine and two of them can be handling different events
// for the same request at the same instant. Reading the state outside a lock,
// deciding in Go, and writing the answer back is a race that silently loses one
// of the two transitions, and the evidence is gone by the time anyone looks.
func (s *States) Apply(
	ctx context.Context, requestID, trigger string, eventAt time.Time,
) (port.Applied, error) {
	var out port.Applied

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			current string
			updated time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT state, updated_at FROM tasking.tasking_requests
			WHERE request_id = $1
			FOR UPDATE
		`, requestID).Scan(&current, &updated)
		if errors.Is(err, pgx.ErrNoRows) {
			return port.ErrRequestNotFound
		}
		if err != nil {
			return fmt.Errorf("reading request state: %w", err)
		}

		result, applyErr := domain.Apply(
			domain.State(current), updated, domain.Trigger(trigger), eventAt)
		if applyErr != nil {
			// Refused, and the caller logs it. Never silently ignored: an
			// ignored illegal transition is a bug that already happened and
			// left no evidence.
			return applyErr
		}

		out = port.Applied{From: current, To: string(result.To), Changed: result.Applied}
		if !result.Applied {
			return nil
		}

		if _, err := tx.Exec(ctx, `
			UPDATE tasking.tasking_requests
			SET state = $2, updated_at = $3
			WHERE request_id = $1
		`, requestID, string(result.To), eventAt); err != nil {
			return fmt.Errorf("writing request state: %w", err)
		}
		return nil
	})
	if err != nil {
		return port.Applied{}, err
	}
	return out, nil
}

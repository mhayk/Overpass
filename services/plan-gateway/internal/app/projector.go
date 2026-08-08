// Package app drives the projection loop. It knows the order of operations;
// it does not know Postgres or NATS.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// Projector folds a stream of events into the read models.
type Projector struct {
	source     port.MessageSource
	projection port.Projection
	log        *slog.Logger
}

// NewProjector wires the loop.
func NewProjector(source port.MessageSource, projection port.Projection, log *slog.Logger) *Projector {
	return &Projector{source: source, projection: projection, log: log}
}

// Run folds until the context is cancelled.
func (p *Projector) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil //nolint:nilerr // cancellation is a clean stop, not a failure
		}
		batch, err := p.source.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // a cancelled fetch is a shutdown, not a failure
			}
			// Keep going. A fetch failure is almost always the broker
			// reconnecting, and exiting the loop would turn a blip into a
			// projection that silently stops advancing.
			p.log.Warn("fetch failed", slog.Any("error", err))
			continue
		}
		for _, m := range batch {
			p.handle(ctx, m)
		}
	}
}

func (p *Projector) handle(ctx context.Context, m port.Message) {
	// Fold first, ack second, always in that order. Acking on receipt would
	// make a crash mid-fold lose the event permanently; this way it redelivers
	// and the fold's own idempotency absorbs the duplicate.
	if err := p.Apply(ctx, m); err != nil {
		p.log.Error("fold failed; will redeliver",
			slog.String("subject", m.Subject),
			slog.String("event_id", m.EventID),
			slog.Uint64("sequence", m.Sequence),
			slog.Any("error", err))
		if nakErr := p.source.Nak(ctx, m); nakErr != nil {
			p.log.Warn("nak failed", slog.Any("error", nakErr))
		}
		return
	}
	if err := p.source.Ack(ctx, m); err != nil {
		// Already folded. The redelivery this causes is harmless because the
		// cursor guard will Ignore it, which is exactly why the guard exists.
		p.log.Warn("ack failed; the event will redeliver and be ignored",
			slog.String("event_id", m.EventID), slog.Any("error", err))
	}
}

// Apply routes one message to the matching fold and advances the cursor.
//
// Exported so the integration tests can drive a replay without a broker.
func (p *Projector) Apply(ctx context.Context, m port.Message) error {
	cursor, err := p.projection.Cursor(ctx, m.Stream)
	if err != nil {
		return fmt.Errorf("reading cursor for %s: %w", m.Stream, err)
	}
	// Sequence, not timestamp. Within one stream JetStream's sequence is the
	// only total order there is, and a publisher's clock is not.
	if m.Sequence != 0 && m.Sequence <= cursor.Sequence {
		return nil
	}

	if err := p.route(ctx, m); err != nil {
		return err
	}
	return p.projection.Advance(ctx, m.Stream, m.Sequence, m.EventAt)
}

// route decodes and dispatches.
//
// EventAt is taken from the envelope in every case, overwriting whatever the
// payload carried. The envelope's occurred_at is the one the outbox stamped and
// the one the cursor is compared against; letting a payload field disagree with
// it would make staleness a function of two different clocks.
func (p *Projector) route(ctx context.Context, m port.Message) error {
	switch m.Subject {
	case SubjectRequestReceived:
		var e port.RequestReceived
		if err := decode(m, &e); err != nil {
			return err
		}
		e.EventAt = m.EventAt
		return p.projection.ProjectRequestReceived(ctx, e)
	case SubjectOpportunitiesComputed:
		var e port.OpportunitiesComputed
		if err := decode(m, &e); err != nil {
			return err
		}
		e.EventAt = m.EventAt
		return p.projection.ProjectOpportunities(ctx, e)
	case SubjectPlanCommitted:
		var e port.PlanCommitted
		if err := decode(m, &e); err != nil {
			return err
		}
		e.EventAt = m.EventAt
		return p.projection.ProjectPlanCommitted(ctx, e)
	case SubjectRequestUnfulfilled:
		var e port.RequestUnfulfilled
		if err := decode(m, &e); err != nil {
			return err
		}
		e.EventAt = m.EventAt
		return p.projection.ProjectUnfulfilled(ctx, e)
	default:
		// Not an error. A gateway that fails on a subject it was not built for
		// would block the whole stream the day another service starts
		// publishing something new.
		p.log.Debug("ignoring unrecognised subject", slog.String("subject", m.Subject))
		return nil
	}
}

// Subjects this projector folds.
const (
	SubjectRequestReceived       = "tasking.request.received.v1"
	SubjectOpportunitiesComputed = "feasibility.opportunities.computed.v1"
	SubjectPlanCommitted         = "planning.plan.committed.v1"
	SubjectRequestUnfulfilled    = "planning.request.unfulfilled.v1"
)

// ErrMalformed marks a payload that will never parse.
var ErrMalformed = errors.New("malformed payload")

func decode(m port.Message, into any) error {
	if err := json.Unmarshal(m.Payload, into); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMalformed, m.Subject, err)
	}
	return nil
}

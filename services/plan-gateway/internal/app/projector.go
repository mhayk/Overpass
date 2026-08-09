// Package app drives the projection loop. It knows the order of operations;
// it does not know Postgres or NATS.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mhayk/overpass/lib/go/consume"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// maxDeliver mirrors deploy/nats/init.sh for the gateway's three consumers.
// Higher than the workers' (10, not 5), because a pure projector with no side
// effects beyond its own tables can afford more retries — and the poison
// decision is what keeps that budget for genuinely transient failures instead
// of letting malformed payloads burn it, which is precisely what the demo's
// "hello" messages spent an hour doing at this consumer.
const maxDeliver = 10

// Projector folds a stream of events into the read models.
type Projector struct {
	source     port.MessageSource
	projection port.Projection
	decode     port.Decoder
	log        *slog.Logger

	// Metrics is exported so main can serve it and M3-06 can scrape it.
	Metrics consume.Metrics
}

// NewProjector wires the loop.
func NewProjector(
	source port.MessageSource,
	projection port.Projection,
	decoder port.Decoder,
	log *slog.Logger,
) *Projector {
	return &Projector{source: source, projection: projection, decode: decoder, log: log}
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
	received := time.Now()
	if m.Delivered > 1 {
		p.Metrics.Redelivered()
	}

	// Fold first, ack second, always in that order. Acking on receipt would
	// make a crash mid-fold lose the event permanently; this way it redelivers
	// and the fold's own idempotency absorbs the duplicate.
	if err := p.Apply(ctx, m); err != nil {
		// A malformed payload will be malformed on redelivery too: permanent,
		// terminated on the first delivery. Everything else retries until the
		// LAST delivery, then terminates deliberately — a lapsed max_deliver
		// is a silent drop, a Term has this log line and a metric.
		permanent := errors.Is(err, port.ErrMalformed)
		decision := consume.OnFailure(permanent, m.Delivered, maxDeliver)
		p.log.Error("fold failed",
			slog.String("subject", m.Subject),
			slog.String("event_id", m.EventID),
			slog.Uint64("sequence", m.Sequence),
			slog.Uint64("delivered", m.Delivered),
			slog.Bool("permanent", permanent),
			slog.String("decision", decision.String()),
			slog.Any("error", err))
		if decision == consume.Terminate {
			p.Metrics.Terminated()
			if termErr := p.source.Term(ctx, m); termErr != nil {
				p.log.Warn("term failed", slog.Any("error", termErr))
			}
			return
		}
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
		return
	}
	p.Metrics.Processed()
	p.Metrics.AckAfter(time.Since(received))
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

	// The cursor is stamped with the EVENT's occurred_at, returned by route,
	// not with m.EventAt. The broker's store time is when the message was
	// persisted, which is later than when the thing happened and drifts further
	// the longer a producer's outbox backs up — so staleness computed from it
	// would report the projection as fresher than it is.
	eventAt, err := p.route(ctx, m)
	if err != nil {
		return err
	}
	return p.projection.Advance(ctx, m.Stream, m.Sequence, eventAt)
}

// route decodes and dispatches.
//
// EventAt comes from the DECODED event's occurred_at, which is the envelope
// field the outbox stamped, not from the broker's store time. The message's own
// EventAt is only a fallback for a payload that somehow carries no timestamp.
func (p *Projector) route(ctx context.Context, m port.Message) (time.Time, error) {
	switch m.Subject {
	case SubjectRequestReceived:
		e, err := p.decode.RequestReceived(m.Payload)
		if err != nil {
			return time.Time{}, err
		}
		e.EventAt = pick(e.EventAt, m.EventAt)
		return e.EventAt, p.projection.ProjectRequestReceived(ctx, e)
	case SubjectOpportunitiesComputed:
		e, err := p.decode.Opportunities(m.Payload)
		if err != nil {
			return time.Time{}, err
		}
		e.EventAt = pick(e.EventAt, m.EventAt)
		return e.EventAt, p.projection.ProjectOpportunities(ctx, e)
	case SubjectPlanCommitted:
		e, err := p.decode.PlanCommitted(m.Payload)
		if err != nil {
			return time.Time{}, err
		}
		e.EventAt = pick(e.EventAt, m.EventAt)
		return e.EventAt, p.projection.ProjectPlanCommitted(ctx, e)
	case SubjectEphemerisComputed:
		e, err := p.decode.Ephemeris(m.Payload)
		if err != nil {
			return time.Time{}, err
		}
		e.EventAt = pick(e.EventAt, m.EventAt)
		return e.EventAt, p.projection.ProjectEphemeris(ctx, e)
	case SubjectRequestUnfulfilled:
		e, err := p.decode.Unfulfilled(m.Payload)
		if err != nil {
			return time.Time{}, err
		}
		e.EventAt = pick(e.EventAt, m.EventAt)
		return e.EventAt, p.projection.ProjectUnfulfilled(ctx, e)
	default:
		// Not an error. A gateway that fails on a subject it was not built for
		// would block the whole stream the day another service starts
		// publishing something new.
		p.log.Debug("ignoring unrecognised subject", slog.String("subject", m.Subject))
		// The broker's timestamp, because there is no decoded event to take one
		// from. The cursor still advances: leaving it behind on a subject this
		// service will never handle would stall the stream forever.
		return m.EventAt, nil
	}
}

// pick prefers the envelope's occurred_at and falls back to the broker's.
//
// A zero timestamp would make staleness report decades and would sort ahead of
// every real event, so it is never allowed through.
func pick(fromPayload, fromBroker time.Time) time.Time {
	if fromPayload.IsZero() {
		return fromBroker
	}
	return fromPayload
}

// Subjects this projector folds.
const (
	SubjectRequestReceived       = "tasking.request.received.v1"
	SubjectOpportunitiesComputed = "feasibility.opportunities.computed.v1"
	SubjectEphemerisComputed     = "feasibility.ephemeris.computed.v1"
	SubjectPlanCommitted         = "planning.plan.committed.v1"
	SubjectRequestUnfulfilled    = "planning.request.unfulfilled.v1"
)

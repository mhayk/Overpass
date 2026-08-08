// Package app orchestrates the planner's use cases.
//
// This file holds one: keep planning.request_snapshots and
// planning.candidate_opportunities fed, so that when a round fires it reads its
// own schema and nothing else. ADR-0015 has the argument for why the planner
// projects rather than reads across schemas.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// Subjects this projector acts on.
//
// The planner-lifecycle consumer filters on tasking.request.> — broader than
// what is projected — so the router must ignore siblings such as
// tasking.request.rejected.v1 rather than treat them as an error. Widening the
// filter later is then a topology change with no code change, which is the
// point of binding to a declared consumer rather than creating one.
const (
	SubjectRequestReceived = "tasking.request.received.v1"
	SubjectOpportunities   = "feasibility.opportunities.computed.v1"
)

// Streams the projector drains, and the consumer name each is bound through.
//
// Tasking first, so on a cold start a request snapshot more often lands before
// the candidates that reference it. That is an optimisation and NOT a
// guarantee: the broker gives no cross-stream ordering, the two streams race by
// design, and ADR-0015 chose to hold an early candidate rather than reject it.
// If this ordering were load-bearing, the held-candidate path would never be
// exercised — which is exactly how that path would rot unnoticed.
var Streams = []struct {
	Stream   string
	Consumer string
}{
	{Stream: "TASKING", Consumer: port.ConsumerLifecycle},
	{Stream: "FEASIBILITY", Consumer: port.ConsumerOpportunities},
}

// Projector folds broker messages into the planner's input tables.
type Projector struct {
	source      port.MessageSource
	decoder     port.Decoder
	projections port.Projections
	log         *slog.Logger

	// consumerFor maps a stream to the dedup ledger partition it writes.
	consumerFor map[string]string
}

// NewProjector wires the projector.
func NewProjector(
	source port.MessageSource,
	decoder port.Decoder,
	projections port.Projections,
	log *slog.Logger,
) *Projector {
	consumerFor := make(map[string]string, len(Streams))
	for _, s := range Streams {
		consumerFor[s.Stream] = s.Consumer
	}
	return &Projector{
		source:      source,
		decoder:     decoder,
		projections: projections,
		log:         log,
		consumerFor: consumerFor,
	}
}

// Stats is what an operator needs to know the projector is healthy.
type Stats struct {
	Applied   int
	Duplicate int
	Ignored   int
	Failed    int
}

// DrainOnce folds at most one batch. Returns what happened to it.
func (p *Projector) DrainOnce(ctx context.Context) (Stats, error) {
	var stats Stats

	messages, err := p.source.Next(ctx)
	if err != nil {
		return stats, fmt.Errorf("fetching: %w", err)
	}

	for _, m := range messages {
		outcome, err := p.fold(ctx, m)
		switch {
		case err != nil:
			stats.Failed++
			// Nak, not Ack. The message goes back for redelivery and, past the
			// consumer's max-deliver, into the DLQ that M0-06 provisioned for
			// exactly this. Acking a failure would drop the event silently, and
			// a silently dropped opportunity is a customer whose request never
			// competes in any round.
			//
			// This is deliberately the same response for a payload that can
			// never succeed (a malformed event) and one that may (an unknown
			// customer, whose reference row might still arrive). Distinguishing
			// them would mean the projector deciding which failures are
			// permanent, and the max-deliver limit already bounds the cost of
			// being wrong about that. M3-02 owns the DLQ and its replay.
			p.log.Error("fold failed",
				slog.String("stream", m.Stream),
				slog.String("subject", m.Subject),
				slog.String("event_id", m.EventID),
				slog.Any("error", err),
			)
			if nakErr := p.source.Nak(ctx, m); nakErr != nil {
				return stats, fmt.Errorf("naking %s: %w", m.EventID, nakErr)
			}
			continue
		case outcome == outcomeApplied:
			stats.Applied++
		case outcome == outcomeDuplicate:
			stats.Duplicate++
			p.log.Debug("already processed",
				slog.String("consumer", p.consumerFor[m.Stream]),
				slog.String("event_id", m.EventID),
			)
		default:
			stats.Ignored++
		}

		// Ack only after the transaction committed. A crash between commit and
		// ack redelivers, and the ledger absorbs it — which is the trade the
		// idempotent-consumer pattern exists to make.
		if ackErr := p.source.Ack(ctx, m); ackErr != nil {
			return stats, fmt.Errorf("acking %s: %w", m.EventID, ackErr)
		}
	}
	return stats, nil
}

type outcome int

const (
	outcomeIgnored outcome = iota
	outcomeApplied
	outcomeDuplicate
)

func (p *Projector) fold(ctx context.Context, m port.Message) (outcome, error) {
	consumer, known := p.consumerFor[m.Stream]
	if !known {
		// A message from a stream this projector never bound cannot be routed
		// to a ledger partition, so it cannot be deduplicated either. Ignoring
		// it is the only safe answer; failing would poison a consumer over a
		// topology change that does not concern it.
		return outcomeIgnored, nil
	}

	switch m.Subject {
	case SubjectRequestReceived:
		event, err := p.decoder.RequestReceived(m.Payload)
		if err != nil {
			return outcomeIgnored, fmt.Errorf("decoding %s: %w", m.Subject, err)
		}
		if invalid := event.Snapshot.Validate(); invalid != nil {
			return outcomeIgnored, invalid
		}
		applied, err := p.projections.ProjectSnapshot(ctx, consumer, event)
		if err != nil {
			return outcomeIgnored, fmt.Errorf("projecting snapshot %s: %w", event.Snapshot.RequestID, err)
		}
		return appliedOutcome(applied), nil

	case SubjectOpportunities:
		event, err := p.decoder.Opportunities(m.Payload)
		if err != nil {
			return outcomeIgnored, fmt.Errorf("decoding %s: %w", m.Subject, err)
		}
		for i, candidate := range event.Candidates {
			if invalid := candidate.Validate(); invalid != nil {
				// The whole batch fails, not just the bad candidate. A
				// partially projected event would let a round allocate over
				// some of a request's options while the rest were never
				// offered, and report the difference as unfulfilment — a
				// customer told they lost a competition that was never run.
				return outcomeIgnored, fmt.Errorf("candidate %d of %d in %s: %w",
					i+1, len(event.Candidates), event.RequestID, invalid)
			}
		}
		applied, err := p.projections.ProjectCandidates(ctx, consumer, event)
		if err != nil {
			return outcomeIgnored, fmt.Errorf("projecting candidates for %s: %w", event.RequestID, err)
		}
		return appliedOutcome(applied), nil

	default:
		// A sibling subject the consumer's filter admits and this projector has
		// no use for. Acked and forgotten, deliberately.
		return outcomeIgnored, nil
	}
}

func appliedOutcome(applied bool) outcome {
	if applied {
		return outcomeApplied
	}
	return outcomeDuplicate
}

// Run drains until the context is cancelled.
//
// maxIterations exists so a test can run the real loop to completion rather
// than cancelling it. A loop only ever stopped by cancellation is a loop whose
// shutdown path is never exercised — the same reasoning as the outbox relay's.
func (p *Projector) Run(ctx context.Context, maxIterations int, idle time.Duration) error {
	for i := 0; maxIterations <= 0 || i < maxIterations; i++ {
		stats, err := p.DrainOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// A cancelled context is a clean stop, not a failure.
				return nil //nolint:nilerr // cancellation is not an error here
			}
			p.log.Error("drain failed", slog.Any("error", err))
		}

		if stats.Applied+stats.Duplicate+stats.Ignored+stats.Failed > 0 {
			// There was work; look again immediately rather than sleeping
			// through a backlog.
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(idle):
		}
	}
	return nil
}

// IsInvalid reports whether an error came from a payload the planner refuses.
//
// Exported so main and the tests can tell a bad event from a broken database
// without importing domain's sentinel through a second path.
func IsInvalid(err error) bool { return errors.Is(err, domain.ErrInvalid) }

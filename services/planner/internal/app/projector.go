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

	"github.com/mhayk/overpass/lib/go/consume"

	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// maxDeliver mirrors deploy/nats/init.sh for the planner's two consumers. The
// poison decision needs it to terminate the LAST attempt deliberately instead
// of letting the broker's counter lapse into a silent drop.
const maxDeliver = 5

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

	// Metrics is exported so main can serve it and M3-06 can scrape it.
	Metrics consume.Metrics

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
		received := time.Now()
		if m.Delivered > 1 {
			// The early-warning line: climbing redeliveries with flat
			// throughput is poison or a dying dependency, visible before
			// max_deliver makes it a loss.
			p.Metrics.Redelivered()
		}

		outcome, err := p.fold(ctx, m)
		switch {
		case err != nil:
			stats.Failed++
			// The M2 position — Nak everything, let max_deliver bound the cost
			// of being wrong — is replaced by the #48 decision the lib makes
			// explicit: a PERMANENT failure (malformed payload, contract
			// violation — anything wrapping domain.ErrInvalid) terminates on
			// its first delivery, because rerunning a deterministic failure
			// buys nothing but latency for the messages behind it. Everything
			// else retries until the LAST delivery, then terminates
			// DELIBERATELY: a lapsed max_deliver is a silent drop, a Term has
			// this log line and a metric.
			permanent := IsInvalid(err)
			decision := consume.OnFailure(permanent, m.Delivered, maxDeliver)
			p.log.Error("fold failed",
				slog.String("stream", m.Stream),
				slog.String("subject", m.Subject),
				slog.String("event_id", m.EventID),
				slog.Uint64("delivered", m.Delivered),
				slog.Bool("permanent", permanent),
				slog.String("decision", decision.String()),
				slog.Any("error", err),
			)
			if decision == consume.Terminate {
				// Publish, then Term. If the publish fails, Nak (ADR-0017):
				// the whole delivery retries, handling and dead-lettering
				// both, and both are idempotent. Terminating anyway would be
				// the drop this mechanism exists to prevent, with a metric
				// attached to make it look handled.
				if dlqErr := p.source.Deadletter(ctx, m, terminalReason(permanent)); dlqErr != nil {
					p.log.Error("dead-lettering failed; naking rather than dropping the payload",
						slog.String("event_id", m.EventID),
						slog.Any("error", dlqErr),
					)
					if nakErr := p.source.Nak(ctx, m); nakErr != nil {
						return stats, fmt.Errorf("naking %s after a failed dead letter: %w", m.EventID, nakErr)
					}
					continue
				}
				p.Metrics.Deadlettered()
				p.Metrics.Terminated()
				if termErr := p.source.Term(ctx, m); termErr != nil {
					return stats, fmt.Errorf("terminating %s: %w", m.EventID, termErr)
				}
				continue
			}
			if nakErr := p.source.Nak(ctx, m); nakErr != nil {
				return stats, fmt.Errorf("naking %s: %w", m.EventID, nakErr)
			}
			continue
		case outcome == outcomeApplied:
			stats.Applied++
			p.Metrics.Processed()
		case outcome == outcomeDuplicate:
			stats.Duplicate++
			p.Metrics.Duplicate()
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
		p.Metrics.AckAfter(time.Since(received))
	}
	return stats, nil
}

// terminalReason names the error class for the DLQ header an operator triages
// on. The two cases are genuinely different incidents: a refused payload is a
// bug or a bad producer, an exhausted retry is a dependency that was down for
// five deliveries.
//
// Permanent maps to "contract" rather than "decode" because the planner cannot
// tell the two apart: a decode failure and a failed domain guard both arrive
// wrapped in domain.ErrInvalid, and inventing the distinction from the error
// string would be a lie with a constant's authority.
func terminalReason(permanent bool) string {
	if permanent {
		return consume.ReasonContract
	}
	return consume.ReasonExhausted
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
			// A payload that does not decode will not decode on redelivery
			// either: permanent, by construction.
			return outcomeIgnored, fmt.Errorf("decoding %s: %w: %w", m.Subject, domain.ErrInvalid, err)
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
			return outcomeIgnored, fmt.Errorf("decoding %s: %w: %w", m.Subject, domain.ErrInvalid, err)
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

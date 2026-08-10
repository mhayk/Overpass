// Package outboxmetrics is the transactional outbox's instruments, defined
// once for every relay that has one.
//
// A subpackage of lib/go/telemetry rather than a copy per service, because the
// names are load-bearing in a way the code is not:
// deploy/prometheus/rules/alerts.yml has committed to
// overpass_outbox_pending_seconds, and three independent copies of a string
// literal is exactly where that name drifts. The relays themselves stay
// duplicated per service — they own different schemas and different
// transactions — but what they REPORT is one vocabulary.
package outboxmetrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The instrument names. Units are in the names and the OTel unit field is left
// empty — measured against the running collector, that is what produces
// overpass_outbox_pending_seconds rather than a renamed series. Declaring
// unit "s" would export overpass_outbox_pending_seconds too, by coincidence,
// but the same is not true of the millisecond instruments elsewhere, and one
// rule applied everywhere beats a rule with an exception nobody remembers.
const (
	InstrumentPendingSeconds = "overpass.outbox.pending_seconds"
	InstrumentPublished      = "overpass.outbox.published"
)

// Outcome values for the published counter.
const (
	OutcomePublished = "published"
	OutcomeFailed    = "failed"
)

// Instruments reports what a relay knows about its own backlog.
//
// Lag is the one that matters. Batch size and failure rate describe how the
// relay is WORKING; lag describes whether anyone downstream has heard about
// what already happened. It is the number OutboxRelayLagging alerts on, and
// the number that distinguishes "the outbox is doing its job" — ingress
// unaffected, events queued — from "the pipeline is starved".
type Instruments struct {
	published metric.Int64Counter

	// Pending is served through an observable gauge from a stored value. A
	// synchronous gauge would report whichever batch happened to land inside
	// the export window; the relay drains far more often than it exports, so
	// most of its measurements would simply be lost.
	mu      sync.Mutex
	pending time.Duration
	// measured guards the initial state. Zero lag and "no batch has been
	// drained yet" are different facts, and reporting the second as the first
	// would show a perfectly healthy outbox for a relay that has never run.
	measured bool
}

// New builds the outbox instruments.
func New(meter metric.Meter) (*Instruments, error) {
	i := &Instruments{}

	published, err := meter.Int64Counter(InstrumentPublished,
		metric.WithDescription("Outbox rows the relay attempted, by outcome."),
	)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", InstrumentPublished, err)
	}
	i.published = published

	if _, err := meter.Float64ObservableGauge(InstrumentPendingSeconds,
		metric.WithDescription(
			"Age of the oldest unpublished outbox row, in seconds. Ingress is "+
				"unaffected by design; this says whether anything downstream has heard."),
		metric.WithFloat64Callback(i.observePending),
	); err != nil {
		return nil, fmt.Errorf("creating %s: %w", InstrumentPendingSeconds, err)
	}

	return i, nil
}

func (i *Instruments) observePending(_ context.Context, observer metric.Float64Observer) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.measured {
		return nil
	}
	observer.Observe(i.pending.Seconds())
	return nil
}

// RecordBatch reports one drained batch.
//
// lag is the age of the oldest row the batch claimed, or zero when the batch
// was empty — an empty batch means nothing is waiting, which IS zero lag and
// is the healthy steady state.
//
// A nil receiver is a no-op, so a relay built without a meter publishes
// exactly as it otherwise would.
func (i *Instruments) RecordBatch(ctx context.Context, published, failed int, lag time.Duration) {
	if i == nil {
		return
	}

	if published > 0 {
		i.published.Add(ctx, int64(published),
			metric.WithAttributes(attribute.String("outcome", OutcomePublished)))
	}
	if failed > 0 {
		i.published.Add(ctx, int64(failed),
			metric.WithAttributes(attribute.String("outcome", OutcomeFailed)))
	}

	i.mu.Lock()
	i.pending = lag
	i.measured = true
	i.mu.Unlock()
}

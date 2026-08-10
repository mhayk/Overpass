package consume

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The outcome of one delivery. Exactly one of these describes any delivery a
// consumer finished with, and every exit path must report one — a path that
// returns without an outcome is a message the dashboard cannot see.
const (
	// OutcomeProcessed is a first-time delivery whose transaction committed.
	OutcomeProcessed = "processed"
	// OutcomeDuplicate is a redelivery the ledger absorbed.
	OutcomeDuplicate = "duplicate"
	// OutcomeTerminated is a deliberate Term whose payload was NOT preserved.
	OutcomeTerminated = "terminated"
	// OutcomeDeadlettered is a Term whose payload reached a DLQ stream.
	OutcomeDeadlettered = "deadlettered"
	// OutcomeFailed is a delivery that will be redelivered — a Nak.
	OutcomeFailed = "failed"
)

// InstrumentDurationMs is the histogram every consumer's RED comes from.
//
// Its _count is the rate, its outcome label is the errors, its buckets are the
// duration. Named as a constant because the dashboards query it and a rename
// is otherwise a silent "No data" panel.
const InstrumentDurationMs = "overpass.consume.duration_ms"

// InstrumentRedeliveries counts deliveries the broker had tried before.
const InstrumentRedeliveries = "overpass.consume.redeliveries"

// Metrics is what an operator needs to know a consumer is healthy.
//
// Duplicates suppressed is the one that proves idempotency is WORKING rather
// than untested: at-least-once delivery means duplicates arrive by design, and
// a counter stuck at zero over weeks means the dedup path has never actually
// run in anger. Redeliveries seen is the early-warning line — climbing
// redeliveries with flat throughput is a poison message or a dying dependency,
// visible before max_deliver makes it a loss.
//
// The in-process counters and the OTel instruments are both fed from Observe,
// so there is one call per delivery rather than two that can drift apart. The
// counters are not redundant: Snapshot is what the tests and the DLQ tooling
// read, and it works with no meter installed at all.
type Metrics struct {
	mu sync.Mutex

	processed    int64
	duplicates   int64
	redeliveries int64
	terminated   int64
	deadlettered int64
	ackTotal     time.Duration
	ackCount     int64
	ackMax       time.Duration

	// nil until Bind. Every record is guarded, because most callers never
	// bind: telemetry must not become a correctness dependency.
	duration    metric.Float64Histogram
	redelivered metric.Int64Counter
}

// Snapshot is a consistent copy, safe to serve from another goroutine.
type Snapshot struct {
	Processed    int64
	Duplicates   int64
	Redeliveries int64
	Terminated   int64
	// Deadlettered counts terminal failures whose payload reached a DLQ
	// stream. Terminated minus Deadlettered is the number of messages this
	// consumer dropped without keeping a copy — which should be zero, and is
	// the reason the two are separate counters rather than one.
	Deadlettered int64
	// AckLatencyMean and AckLatencyMax cover receive-to-outcome, which bounds
	// how long a crash window can silently redeliver.
	AckLatencyMean time.Duration
	AckLatencyMax  time.Duration
}

// Bind installs OTel instruments. Optional: an unbound Metrics still
// accumulates its Snapshot and must never panic.
//
// The duration instrument declares NO unit, deliberately. Measured against the
// running collector: overpass.consume.duration_ms with an empty unit exports
// as overpass_consume_duration_ms_bucket, which is what the dashboards query,
// while declaring unit "ms" exports it as
// overpass_consume_duration_milliseconds_bucket instead. The unit is baked
// into the instrument name for that reason and must stay there.
func (m *Metrics) Bind(meter metric.Meter) error {
	duration, err := meter.Float64Histogram(InstrumentDurationMs,
		metric.WithDescription(
			"Receive-to-outcome latency for one delivery, in milliseconds, by subject and outcome."),
		// Buckets in milliseconds, spanning a fast in-memory fold to a
		// delivery slow enough that the broker is about to redeliver it. The
		// SDK's default boundaries top out at 10s in SECONDS, which would put
		// every observation here in the first bucket.
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000),
	)
	if err != nil {
		return fmt.Errorf("creating %s: %w", InstrumentDurationMs, err)
	}

	redelivered, err := meter.Int64Counter(InstrumentRedeliveries,
		metric.WithDescription(
			"Deliveries the broker had already attempted. Climbing against flat "+
				"throughput is a poison message or a dying dependency."),
	)
	if err != nil {
		return fmt.Errorf("creating %s: %w", InstrumentRedeliveries, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.duration = duration
	m.redelivered = redelivered
	return nil
}

// Observe records one finished delivery.
//
// Called on EVERY exit path, including the ones that Nak or Term. Its
// predecessor, AckAfter, was called only after a successful ack — the Term and
// Nak paths returned before reaching it — so the latency it reported described
// successes exclusively, which is the half that is never the problem.
func (m *Metrics) Observe(ctx context.Context, subject, outcome string, d time.Duration) {
	m.mu.Lock()
	switch outcome {
	case OutcomeProcessed:
		m.processed++
	case OutcomeDuplicate:
		m.duplicates++
	case OutcomeTerminated:
		m.terminated++
	case OutcomeDeadlettered:
		// Both. A dead letter IS a termination that kept the payload, and
		// Terminated minus Deadlettered is the invariant an operator reads to
		// find messages dropped without a copy.
		m.terminated++
		m.deadlettered++
	}
	m.ackTotal += d
	m.ackCount++
	if d > m.ackMax {
		m.ackMax = d
	}
	duration := m.duration
	m.mu.Unlock()

	if duration != nil {
		duration.Record(ctx, float64(d)/float64(time.Millisecond), metric.WithAttributes(
			attribute.String("subject", subject),
			attribute.String("outcome", outcome),
		))
	}
}

// Redelivered records a delivery whose broker counter was above one, whatever
// its outcome.
//
// Orthogonal to Observe rather than an outcome of its own: a redelivered
// message still ends up processed, duplicate or terminated, and folding it
// into the outcome label would hide which.
func (m *Metrics) Redelivered(ctx context.Context, subject string) {
	m.mu.Lock()
	m.redeliveries++
	counter := m.redelivered
	m.mu.Unlock()

	if counter != nil {
		counter.Add(ctx, 1, metric.WithAttributes(attribute.String("subject", subject)))
	}
}

// Snapshot returns a consistent copy.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{
		Processed:     m.processed,
		Duplicates:    m.duplicates,
		Redeliveries:  m.redeliveries,
		Terminated:    m.terminated,
		Deadlettered:  m.deadlettered,
		AckLatencyMax: m.ackMax,
	}
	if m.ackCount > 0 {
		s.AckLatencyMean = m.ackTotal / time.Duration(m.ackCount)
	}
	return s
}

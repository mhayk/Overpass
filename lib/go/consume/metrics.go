package consume

import (
	"sync"
	"time"
)

// Metrics is what an operator needs to know a consumer is healthy, in the
// shape M3-06 will scrape.
//
// Duplicates suppressed is the one that proves idempotency is WORKING rather
// than untested: at-least-once delivery means duplicates arrive by design, and
// a counter stuck at zero over weeks means the dedup path has never actually
// run in anger. Redeliveries seen is the early-warning line — climbing
// redeliveries with flat throughput is a poison message or a dying dependency,
// visible before max_deliver makes it a loss.
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
	// AckLatencyMean and AckLatencyMax cover receive-to-ack, which bounds how
	// long a crash window can silently redeliver.
	AckLatencyMean time.Duration
	AckLatencyMax  time.Duration
}

// Processed records a first-time delivery that committed.
func (m *Metrics) Processed() { m.count(&m.processed) }

// Duplicate records a redelivery the ledger absorbed.
func (m *Metrics) Duplicate() { m.count(&m.duplicates) }

// Redelivered records a delivery whose broker counter was above one, whatever
// its outcome — the load-bearing early-warning signal.
func (m *Metrics) Redelivered() { m.count(&m.redeliveries) }

// Terminated records a deliberate Term.
func (m *Metrics) Terminated() { m.count(&m.terminated) }

// Deadlettered records a terminal failure whose payload landed in a DLQ stream.
func (m *Metrics) Deadlettered() { m.count(&m.deadlettered) }

// AckAfter records receive-to-ack latency for one delivery.
func (m *Metrics) AckAfter(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ackTotal += d
	m.ackCount++
	if d > m.ackMax {
		m.ackMax = d
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

func (m *Metrics) count(field *int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	*field++
}

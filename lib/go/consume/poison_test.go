package consume_test

import (
	"context"
	"testing"
	"time"

	"github.com/mhayk/overpass/lib/go/consume"
)

func TestPermanentFailuresTerminateOnTheFirstDelivery(t *testing.T) {
	// A decode error is deterministic: rerunning it max_deliver times adds
	// nothing but log noise and latency for the messages behind it.
	if got := consume.OnFailure(true, 1, 5); got != consume.Terminate {
		t.Errorf("a permanent failure on delivery 1 decided %s, want terminate", got)
	}
}

func TestTransientFailuresRetryUntilTheLastDelivery(t *testing.T) {
	for delivered := uint64(1); delivered <= 4; delivered++ {
		if got := consume.OnFailure(false, delivered, 5); got != consume.Retry {
			t.Errorf("delivery %d of 5 decided %s, want retry", delivered, got)
		}
	}
}

// The last delivery terminates DELIBERATELY instead of letting max_deliver
// lapse. The broker's counter reaching zero pages nobody; a Term has a log
// line and a metric.
func TestTheLastDeliveryTerminatesInsteadOfLapsing(t *testing.T) {
	if got := consume.OnFailure(false, 5, 5); got != consume.Terminate {
		t.Errorf("the final delivery decided %s, want terminate — the drop must be chosen, not suffered", got)
	}
}

func TestAnUnboundedConsumerNeverTerminatesTransients(t *testing.T) {
	// max_deliver <= 0 means the topology did not bound delivery; the helper
	// must not invent a bound the operator did not configure.
	if got := consume.OnFailure(false, 10_000, 0); got != consume.Retry {
		t.Errorf("an unbounded consumer decided %s on a transient, want retry", got)
	}
}

func TestMetricsAggregateHonestly(t *testing.T) {
	var m consume.Metrics
	ctx := context.Background()
	m.Observe(ctx, "s", consume.OutcomeProcessed, 10*time.Millisecond)
	m.Observe(ctx, "s", consume.OutcomeProcessed, 30*time.Millisecond)
	m.Observe(ctx, "s", consume.OutcomeDuplicate, 20*time.Millisecond)
	m.Observe(ctx, "s", consume.OutcomeTerminated, 20*time.Millisecond)
	m.Redelivered(ctx, "s")

	s := m.Snapshot()
	if s.Processed != 2 || s.Duplicates != 1 || s.Redeliveries != 1 || s.Terminated != 1 {
		t.Errorf("snapshot = %+v", s)
	}
	if s.AckLatencyMean != 20*time.Millisecond {
		t.Errorf("mean ack latency = %s, want 20ms", s.AckLatencyMean)
	}
	if s.AckLatencyMax != 30*time.Millisecond {
		t.Errorf("max ack latency = %s, want 30ms", s.AckLatencyMax)
	}
}

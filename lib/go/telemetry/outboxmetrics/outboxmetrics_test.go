package outboxmetrics_test

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/mhayk/overpass/lib/go/telemetry/outboxmetrics"
)

// The name the committed alert rule depends on.
//
// deploy/prometheus/rules/alerts.yml has queried
// overpass_outbox_pending_seconds since before anything published it. This
// asserts the instrument that finally does is the one it was waiting for.
func TestPendingInstrumentMatchesTheCommittedAlertRule(t *testing.T) {
	if outboxmetrics.InstrumentPendingSeconds != "overpass.outbox.pending_seconds" {
		t.Fatalf("instrument = %q; alerts.yml queries overpass_outbox_pending_seconds",
			outboxmetrics.InstrumentPendingSeconds)
	}
}

// Lag is reported in SECONDS, because the name says seconds.
//
// Recording a time.Duration's native nanoseconds would make a 30-second
// backlog read as 3e10, and OutboxRelayLagging's "> 30" threshold would fire
// on a 30-nanosecond lag — permanently, from the first batch.
func TestPendingIsReportedInSeconds(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	instruments := build(t, reader)

	instruments.RecordBatch(context.Background(), 5, 0, 90*time.Second)

	got := gaugePoints(t, collect(t, reader), outboxmetrics.InstrumentPendingSeconds)
	if len(got) != 1 || got[0].Value != 90 {
		t.Errorf("pending = %v, want a single point of 90 seconds", got)
	}
}

// Nothing is reported before the first batch.
//
// Zero lag and "this relay has never drained" are different facts. Reporting
// the second as the first shows a perfectly healthy outbox for a relay that
// has not run at all — which is the failure the metric exists to catch.
func TestNothingIsReportedBeforeTheFirstBatch(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	build(t, reader)

	if got := gaugePoints(t, collect(t, reader), outboxmetrics.InstrumentPendingSeconds); len(got) != 0 {
		t.Errorf("pending reported %v before any batch drained; want no series", got)
	}
}

// Published and failed are the same counter under different outcomes, so the
// success ratio is one query rather than a join.
func TestOutcomesShareOneCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	instruments := build(t, reader)

	instruments.RecordBatch(context.Background(), 7, 2, time.Second)

	byOutcome := map[string]int64{}
	for _, dp := range sumPoints(t, collect(t, reader), outboxmetrics.InstrumentPublished) {
		v, ok := dp.Attributes.Value("outcome")
		if !ok {
			t.Fatal("a published data point carries no outcome attribute")
		}
		byOutcome[v.AsString()] = dp.Value
	}
	if byOutcome[outboxmetrics.OutcomePublished] != 7 || byOutcome[outboxmetrics.OutcomeFailed] != 2 {
		t.Errorf("outcomes = %v, want published 7 and failed 2", byOutcome)
	}
}

// A nil Instruments is a no-op. Every relay unit test builds one without a
// meter, and a relay that refused to publish without telemetry would have
// inverted the dependency this whole module is careful about.
func TestNilInstrumentsIsANoOp(t *testing.T) {
	var instruments *outboxmetrics.Instruments
	instruments.RecordBatch(context.Background(), 1, 1, time.Second)
}

func build(t *testing.T, reader sdkmetric.Reader) *outboxmetrics.Instruments {
	t.Helper()
	instruments, err := outboxmetrics.New(
		sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return instruments
}

func collect(t *testing.T, reader sdkmetric.Reader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rm
}

func gaugePoints(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.DataPoint[float64] {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 gauge", name, m.Data)
			}
			return g.DataPoints
		}
	}
	return nil
}

func sumPoints(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.DataPoint[int64] {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
			}
			return s.DataPoints
		}
	}
	t.Fatalf("no instrument named %q", name)
	return nil
}

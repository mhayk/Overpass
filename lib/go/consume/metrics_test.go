package consume_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/mhayk/overpass/lib/go/consume"
)

// Every outcome must produce a series.
//
// An outcome that is recorded nowhere looks exactly like one that never
// happens. This is the whole reason the histogram carries an outcome label
// rather than counting only successes: a consumer whose failures are invisible
// reports itself healthy while dropping everything.
func TestObserveRecordsEveryOutcome(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	var m consume.Metrics
	if err := m.Bind(provider.Meter("test")); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	outcomes := []string{
		consume.OutcomeProcessed,
		consume.OutcomeDuplicate,
		consume.OutcomeTerminated,
		consume.OutcomeDeadlettered,
		consume.OutcomeFailed,
		consume.OutcomeIgnored,
	}
	for _, outcome := range outcomes {
		m.Observe(context.Background(), "tasking.request.received.v1", outcome, 5*time.Millisecond)
	}

	h := histogram(t, collect(t, reader), "overpass.consume.duration_ms")
	if len(h.DataPoints) != len(outcomes) {
		t.Fatalf("data points = %d, want %d (one per outcome)", len(h.DataPoints), len(outcomes))
	}

	seen := map[string]bool{}
	for _, dp := range h.DataPoints {
		value, ok := dp.Attributes.Value("outcome")
		if !ok {
			t.Fatal("a data point carries no outcome attribute")
		}
		seen[value.AsString()] = true
	}
	for _, outcome := range outcomes {
		if !seen[outcome] {
			t.Errorf("no series for outcome %q", outcome)
		}
	}
}

// The instrument names are what the dashboards query.
//
// Renaming one is a silent dashboard break — the panel renders "No data" and
// nothing else notices. This test is the tripwire, and it is the reason the
// names are asserted as literals rather than read from a constant that would
// move with the rename.
func TestInstrumentNamesAreTheOnesTheDashboardsQuery(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	var m consume.Metrics
	if err := m.Bind(provider.Meter("test")); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	m.Observe(context.Background(), "s", consume.OutcomeProcessed, time.Millisecond)
	m.Redelivered(context.Background(), "s")

	got := map[string]bool{}
	for _, scope := range collect(t, reader).ScopeMetrics {
		for _, metric := range scope.Metrics {
			got[metric.Name] = true
		}
	}

	for _, want := range []string{
		"overpass.consume.duration_ms",
		"overpass.consume.redeliveries",
	} {
		if !got[want] {
			t.Errorf("instrument %q missing; have %v", want, got)
		}
	}
}

// The unit field must stay EMPTY.
//
// Measured against the running collector: an instrument named
// overpass.consume.duration_ms with no unit exports as
// overpass_consume_duration_ms_bucket, which is what the dashboards query.
// Declaring unit:"ms" — the more correct-looking thing — exports as
// overpass_consume_duration_milliseconds_bucket instead. The unit is baked
// into the name deliberately, and this asserts nobody "fixes" that later.
func TestDurationInstrumentDeclaresNoUnit(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	var m consume.Metrics
	if err := m.Bind(provider.Meter("test")); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	m.Observe(context.Background(), "s", consume.OutcomeProcessed, time.Millisecond)

	for _, scope := range collect(t, reader).ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == "overpass.consume.duration_ms" && metric.Unit != "" {
				t.Errorf("unit = %q, want empty: a unit makes the exporter rename the series", metric.Unit)
			}
		}
	}
}

// Observe records the duration in MILLISECONDS.
//
// The name says ms and the histogram buckets are chosen for ms. Recording
// time.Duration's native nanoseconds would put every real observation in the
// overflow bucket, and the p95 panel would read a constant.
func TestObserveRecordsMilliseconds(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	var m consume.Metrics
	if err := m.Bind(provider.Meter("test")); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	m.Observe(context.Background(), "s", consume.OutcomeProcessed, 250*time.Millisecond)

	h := histogram(t, collect(t, reader), "overpass.consume.duration_ms")
	if got := h.DataPoints[0].Sum; got != 250 {
		t.Errorf("sum = %v, want 250 (milliseconds, not %v nanoseconds)", got, 250*time.Millisecond)
	}
}

// Snapshot keeps working for callers that never Bind.
//
// Every existing unit test and the DLQ tooling use Metrics with no meter at
// all. An unbound Metrics must accumulate its counters and must not panic —
// telemetry is not allowed to become a correctness dependency.
func TestSnapshotWorksWithoutBind(t *testing.T) {
	var m consume.Metrics

	m.Observe(context.Background(), "s", consume.OutcomeProcessed, time.Second)
	m.Observe(context.Background(), "s", consume.OutcomeDuplicate, time.Second)
	m.Observe(context.Background(), "s", consume.OutcomeTerminated, time.Second)
	m.Observe(context.Background(), "s", consume.OutcomeDeadlettered, time.Second)
	m.Redelivered(context.Background(), "s")

	got := m.Snapshot()
	if got.Processed != 1 || got.Duplicates != 1 || got.Redeliveries != 1 {
		t.Errorf("Snapshot() = %+v; processed, duplicates and redeliveries should each be 1", got)
	}
	// A dead letter IS a termination that kept the payload, so it counts as
	// both. Terminated minus Deadlettered is the invariant an operator reads
	// to find messages dropped without a copy — here, the one bare Term.
	if got.Terminated != 2 || got.Deadlettered != 1 {
		t.Errorf("Terminated = %d, Deadlettered = %d; want 2 and 1",
			got.Terminated, got.Deadlettered)
	}
	if got.Terminated-got.Deadlettered != 1 {
		t.Errorf("dropped-without-a-copy = %d, want 1", got.Terminated-got.Deadlettered)
	}
	if got.AckLatencyMax != time.Second {
		t.Errorf("AckLatencyMax = %v, want 1s", got.AckLatencyMax)
	}
}

// A failed delivery still contributes to ack latency accounting.
//
// Observe is called on every exit path, including the ones that Nak or Term.
// The previous AckAfter was reached only after a successful ack, so the
// latency figure described successes exclusively — which is the half that is
// never the problem.
func TestFailedDeliveriesAreObservedToo(t *testing.T) {
	var m consume.Metrics
	m.Observe(context.Background(), "s", consume.OutcomeFailed, 2*time.Second)

	if got := m.Snapshot().AckLatencyMax; got != 2*time.Second {
		t.Errorf("AckLatencyMax = %v, want 2s from the failed delivery", got)
	}
}

func collect(t *testing.T, reader sdkmetric.Reader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rm
}

func histogram(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Histogram[float64] {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}
			h, ok := metric.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", name, metric.Data)
			}
			return h
		}
	}
	t.Fatalf("no instrument named %q", name)
	return metricdata.Histogram[float64]{}
}

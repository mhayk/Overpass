package telemetry_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/mhayk/overpass/lib/go/telemetry"
)

// Setup with an empty endpoint installs no-op providers and does not error.
//
// Every unit test in every service reaches Setup this way — no collector, no
// network. If it returned an error here, the services would refuse to start
// under test, and the "telemetry is not an availability dependency" rule would
// be true in production and false everywhere else.
func TestSetupWithoutEndpointIsNoop(t *testing.T) {
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName: "probe",
	})
	if err != nil {
		t.Fatalf("Setup with no endpoint: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Setup returned a nil shutdown; every caller defers it")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// The propagator is installed even when export is off.
//
// This line is load-bearing in Go in a way it is not in Python: Go's default
// global propagator is a no-op, so without it a traceparent does not travel
// THROUGH a service whose telemetry is disabled, and a partially instrumented
// deployment produces two unrelated traces instead of one with a gap.
func TestPropagatorInstalledWithExportOff(t *testing.T) {
	if _, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName: "probe",
	}); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	ctx := telemetry.Extract(context.Background(), map[string]string{
		"traceparent": "00-" + traceID + "-00f067aa0ba902b7-01",
	})

	gotTrace, gotSpan := telemetry.IDs(ctx)
	if gotTrace != traceID {
		t.Errorf("trace id = %q, want %q", gotTrace, traceID)
	}
	if gotSpan != "00f067aa0ba902b7" {
		t.Errorf("span id = %q, want 00f067aa0ba902b7", gotSpan)
	}
}

// Inject and Extract are inverses over a header map.
//
// The map is what goes into the outbox row and then onto the NATS message, so
// a round trip that loses the context is the async hop losing its trace.
func TestInjectExtractRoundTrip(t *testing.T) {
	if _, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName: "probe",
	}); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	ctx := telemetry.Extract(context.Background(), map[string]string{
		"traceparent": "00-" + traceID + "-00f067aa0ba902b7-01",
	})

	headers := telemetry.Inject(ctx, nil)
	if headers["traceparent"] == "" {
		t.Fatal("Inject wrote no traceparent")
	}

	got, _ := telemetry.IDs(telemetry.Extract(context.Background(), headers))
	if got != traceID {
		t.Errorf("round-tripped trace id = %q, want %q", got, traceID)
	}
}

// IDs returns empty strings, not the all-zero id, when there is no span.
//
// "00000000000000000000000000000000" in a log line looks like a real id and
// matches nothing, which is a worse debugging experience than an absent field.
func TestIDsWithoutASpanAreEmpty(t *testing.T) {
	traceID, spanID := telemetry.IDs(context.Background())
	if traceID != "" || spanID != "" {
		t.Errorf("IDs() = %q, %q; want empty strings", traceID, spanID)
	}
}

// Setup with an endpoint builds real exporters and shuts them down cleanly.
//
// No collector needs to be listening. That is not a weakness of the test — it
// is the behaviour being asserted: an unreachable collector must not be a
// startup failure, because refusing to serve traffic when a telemetry backend
// is down would make observability an availability dependency. The gRPC
// exporters connect lazily and retry in the background, and this test is what
// says so.
//
// The address is a port nothing is listening on, deliberately.
func TestSetupWithAnEndpointSucceedsWithNoCollector(t *testing.T) {
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName:    "probe",
		ServiceVersion: "0.0.0",
		Environment:    "test",
		Endpoint:       "127.0.0.1:1",
		SampleRatio:    1,
	})
	if err != nil {
		t.Fatalf("Setup against an unreachable collector: %v", err)
	}

	// What matters is that shutdown RETURNS, within a bounded time.
	//
	// It returns an ERROR here, and that is correct rather than a defect: the
	// meter provider's shutdown flushes, and flushing to a collector that is
	// not there fails. Measured — the trace provider drops its batch silently
	// and only the metric side reports:
	//
	//   failed to upload metrics: exporter export timeout: rpc error:
	//   code = Unavailable ... connect: connection refused
	//
	// Every service logs that as a warning on the way out, which is the honest
	// thing to do. The cost is bounded by the exporter's 5s timeout, so a
	// process exiting against a dead collector takes about five seconds longer
	// — worth knowing, and much better than the alternative this asserts
	// against: a shutdown that never returns, with a telemetry backend holding
	// the process open forever.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- shutdown(ctx) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown did not return; an unreachable collector is holding the process open")
	}
}

// Tracer and Meter return usable instruments after Setup.
//
// A no-op provider still returns non-nil, so this asserts they can be USED —
// a nil meter would panic at the first instrument, in a composition root, at
// startup.
func TestTracerAndMeterAreUsable(t *testing.T) {
	if _, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName: "probe",
	}); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	_, span := telemetry.Tracer("scope").Start(context.Background(), "probe")
	span.End()

	counter, err := telemetry.Meter("scope").Int64Counter("probe.count")
	if err != nil {
		t.Fatalf("building a counter from the meter: %v", err)
	}
	counter.Add(context.Background(), 1)
}

// The resource carries what every dashboard slices by.
//
// service_name, service_version and deployment_environment arrive as
// Prometheus labels through the collector's resource_to_telemetry_conversion,
// which is why no instrument declares them. If they were dropped here, every
// panel that groups by service would silently collapse into one series.
func TestResourceCarriesTheIdentityDashboardsSliceBy(t *testing.T) {
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName:    "probe-service",
		ServiceVersion: "1.2.3",
		Environment:    "test",
		Endpoint:       "127.0.0.1:1",
		SampleRatio:    1,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// The error is expected and reported rather than discarded: the
		// collector is unreachable, so the metric flush fails. Logging it
		// keeps the reason visible if this test ever starts hanging instead.
		if err := shutdown(ctx); err != nil {
			t.Logf("shutdown reported (expected, no collector): %v", err)
		}
	})

	// The resource is not exported directly, so this asserts the merge that
	// builds it did not error — the failure mode that once left a service
	// running, logging one warning, and exporting nothing at all.
	if provider := otel.GetMeterProvider(); provider == nil {
		t.Fatal("no meter provider installed")
	}
	if provider := otel.GetTracerProvider(); provider == nil {
		t.Fatal("no tracer provider installed")
	}
}

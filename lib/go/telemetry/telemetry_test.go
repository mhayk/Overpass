package telemetry_test

import (
	"context"
	"testing"

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

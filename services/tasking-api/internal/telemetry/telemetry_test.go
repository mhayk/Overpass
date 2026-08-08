package telemetry_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/mhayk/overpass/services/tasking-api/internal/telemetry"
)

// The one property everything else rests on: a span's identity survives being
// written into a header map, stored, retrieved, and read back.
//
// If it does not, every service still produces spans and every trace is a
// singleton — which looks like working tracing right up to the moment somebody
// asks what happened to a request.

// recording installs an in-memory provider and returns the collected spans.
func recording(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	// Setup with no endpoint installs the propagator and skips the exporter,
	// which is exactly what a test wants: real propagation, no network.
	if _, err := telemetry.Setup(t.Context(), telemetry.Config{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Setup replaced the provider with a noop; put the recorder back.
	otel.SetTracerProvider(provider)
	return recorder
}

func TestATraceparentSurvivesTheOutboxRoundTrip(t *testing.T) {
	recording(t)

	// The ingress span, as the HTTP middleware would create it.
	ingressCtx, ingress := telemetry.Tracer().Start(t.Context(), "POST /v1/tasking-requests")
	defer ingress.End()

	// Enqueue: the headers written into the outbox row.
	stored := telemetry.Inject(ingressCtx, map[string]string{})
	if stored["traceparent"] == "" {
		t.Fatal("no traceparent was written; the outbox row would carry nothing")
	}

	// Relay, later, on another goroutine, with no memory of the request.
	relayCtx := telemetry.Extract(context.Background(), stored)
	resumed := trace.SpanContextFromContext(relayCtx)
	if !resumed.IsValid() {
		t.Fatal("the stored traceparent did not resume; the publish would be a new root trace")
	}
	if resumed.TraceID() != ingress.SpanContext().TraceID() {
		t.Fatalf("resumed trace %s, want the ingress trace %s",
			resumed.TraceID(), ingress.SpanContext().TraceID())
	}
	if resumed.SpanID() != ingress.SpanContext().SpanID() {
		t.Fatalf("resumed parent span %s, want the ingress span %s",
			resumed.SpanID(), ingress.SpanContext().SpanID())
	}
}

// TestThePublishSpanIsAChildOfTheIngressSpan is the shape the relay produces.
//
// Not a sibling and not a root. The publish must appear UNDER the request that
// caused it, which is the entire reason the traceparent is captured inside the
// enqueue transaction rather than generated when the relay happens to run.
func TestThePublishSpanIsAChildOfTheIngressSpan(t *testing.T) {
	recorder := recording(t)

	ingressCtx, ingress := telemetry.Tracer().Start(t.Context(), "POST /v1/tasking-requests")
	stored := telemetry.Inject(ingressCtx, map[string]string{})
	ingress.End()

	// The relay resumes from the row and starts its own span.
	_, publish := telemetry.Tracer().Start(
		telemetry.Extract(context.Background(), stored),
		"tasking.outbox publish",
		trace.WithSpanKind(trace.SpanKindProducer),
	)
	publish.End()

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(spans))
	}
	var published sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() == "tasking.outbox publish" {
			published = span
		}
	}
	if published == nil {
		t.Fatal("no publish span recorded")
	}
	if published.Parent().SpanID() != ingress.SpanContext().SpanID() {
		t.Errorf("publish parent = %s, want the ingress span %s",
			published.Parent().SpanID(), ingress.SpanContext().SpanID())
	}
	if published.SpanKind() != trace.SpanKindProducer {
		t.Errorf("publish kind = %v, want Producer", published.SpanKind())
	}
}

// TestTheWireCarriesThePublishSpanNotTheIngressSpan pins the overwrite the
// relay performs on purpose.
//
// A consumer parented directly to the ingress span would show the broker hop as
// taking no time at all — the message would appear to be consumed before it was
// published. Parenting to the publish is what makes queue latency visible.
func TestTheWireCarriesThePublishSpanNotTheIngressSpan(t *testing.T) {
	recording(t)

	ingressCtx, ingress := telemetry.Tracer().Start(t.Context(), "ingress")
	stored := telemetry.Inject(ingressCtx, map[string]string{})
	ingress.End()

	publishCtx, publish := telemetry.Tracer().Start(
		telemetry.Extract(context.Background(), stored), "publish")
	publish.End()

	// What actually goes onto the NATS message.
	onTheWire := map[string]string{"Nats-Msg-Id": "evt-1"}
	for k, v := range stored {
		onTheWire[k] = v
	}
	telemetry.Inject(publishCtx, onTheWire)

	consumerParent := trace.SpanContextFromContext(
		telemetry.Extract(context.Background(), onTheWire))
	if consumerParent.SpanID() != publish.SpanContext().SpanID() {
		t.Fatalf("the wire names %s as parent, want the publish span %s",
			consumerParent.SpanID(), publish.SpanContext().SpanID())
	}
	// Same trace throughout. This is the assertion the whole issue is about.
	if consumerParent.TraceID() != ingress.SpanContext().TraceID() {
		t.Fatalf("the consumer would join trace %s, not the request's %s",
			consumerParent.TraceID(), ingress.SpanContext().TraceID())
	}
	// And the id the relay needs is untouched by the injection.
	if onTheWire["Nats-Msg-Id"] != "evt-1" {
		t.Error("injecting the trace context clobbered the dedup id")
	}
}

// TestPropagationWorksWithTracingDisabled is why Setup installs the propagator
// even with no endpoint.
//
// A service with no exporter should still pass a traceparent THROUGH, so a
// partially-instrumented deployment yields one trace with a gap rather than two
// unrelated traces.
func TestPropagationWorksWithTracingDisabled(t *testing.T) {
	if _, err := telemetry.Setup(t.Context(), telemetry.Config{}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	upstream := map[string]string{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
	ctx := telemetry.Extract(t.Context(), upstream)

	forwarded := telemetry.Inject(ctx, map[string]string{})
	if !strings.Contains(forwarded["traceparent"], "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("the trace was not forwarded: %q", forwarded["traceparent"])
	}
}

// TestIDsReportsNothingRatherThanZeroes.
//
// "00000000000000000000000000000000" in a log line looks like a real id and
// matches nothing, which sends whoever is reading it to search Tempo for a
// trace that cannot exist.
func TestIDsReportsNothingRatherThanZeroes(t *testing.T) {
	traceID, spanID := telemetry.IDs(context.Background())
	if traceID != "" || spanID != "" {
		t.Fatalf("got %q/%q from a context with no span, want empty", traceID, spanID)
	}

	recording(t)
	ctx, span := telemetry.Tracer().Start(t.Context(), "work")
	defer span.End()

	traceID, spanID = telemetry.IDs(ctx)
	if len(traceID) != 32 || len(spanID) != 16 {
		t.Fatalf("got %q/%q, want a 32-hex trace id and a 16-hex span id", traceID, spanID)
	}
}

func TestSetupRejectsNothingAndDisablesCleanly(t *testing.T) {
	shutdown, err := telemetry.Setup(t.Context(), telemetry.Config{})
	if err != nil {
		t.Fatalf("setup with no endpoint failed: %v", err)
	}
	if err := shutdown(t.Context()); err != nil {
		t.Errorf("shutdown of a disabled provider failed: %v", err)
	}
}

// TestSetupSucceedsWithAnEndpoint is the test that was missing.
//
// Every other test here calls Setup with no endpoint, which returns before the
// resource is built — so a genuine failure in that path went unnoticed:
//
//	building the resource: conflicting Schema URL:
//	https://opentelemetry.io/schemas/1.43.0 and .../1.26.0
//
// resource.Merge refuses to merge two resources whose schema URLs differ, and
// resource.Default() carries whichever version the SDK ships. Setup treats the
// error as non-fatal — correctly — so the service started, logged one warning,
// and exported nothing at all. It compiled, it linted, and every unit test
// passed.
//
// No collector is needed: the OTLP gRPC exporter connects lazily, so this
// exercises the whole construction path without a network.
func TestSetupSucceedsWithAnEndpoint(t *testing.T) {
	shutdown, err := telemetry.Setup(t.Context(), telemetry.Config{
		ServiceName:    "tasking-api",
		ServiceVersion: "test",
		Environment:    "test",
		Endpoint:       "127.0.0.1:4317",
		SampleRatio:    1,
	})
	if err != nil {
		t.Fatalf("tracing would have been silently disabled in production: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if shutdownErr := shutdown(ctx); shutdownErr != nil {
			t.Logf("shutdown: %v", shutdownErr)
		}
	})

	// And it really installed a provider, rather than leaving the noop in place.
	ctx, span := telemetry.Tracer().Start(t.Context(), "probe")
	defer span.End()
	if traceID, _ := telemetry.IDs(ctx); traceID == "" {
		t.Fatal("no span context after a successful Setup; the provider is a noop")
	}
}

package telemetry_test

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otel"

	"github.com/mhayk/overpass/lib/go/telemetry"
)

const (
	probeTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	probeSpanID  = "00f067aa0ba902b7"
)

func recording(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	if _, err := telemetry.Setup(context.Background(), telemetry.Config{ServiceName: "probe"}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// Setup installs a NOOP provider when export is off, so the recorder goes
	// in AFTER it. Otherwise this test records nothing and passes for the
	// wrong reason — which is the failure mode it exists to catch elsewhere.
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	return recorder
}

// The consumer span is a CHILD of the producer, so the causal chain survives
// the async hop.
//
// Without the parent relationship the consumer becomes a root: it shares the
// trace id and nothing else, and "what happened to this request" returns two
// traces a human has to join by hand.
func TestConsumerSpanIsAChildOfTheProducer(t *testing.T) {
	recorder := recording(t)

	_, span := telemetry.ConsumerSpan(context.Background(), "scope", "probe process",
		map[string]string{"traceparent": "00-" + probeTraceID + "-" + probeSpanID + "-01"})
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	if got := ended[0].Parent().SpanID().String(); got != probeSpanID {
		t.Errorf("parent span = %s, want the producer %s", got, probeSpanID)
	}
	if got := ended[0].SpanContext().TraceID().String(); got != probeTraceID {
		t.Errorf("trace id = %s, want %s", got, probeTraceID)
	}
}

// And a LINK as well as a child.
//
// The link states, in the model rather than in a comment, that this is a
// message-driven continuation and not a nested call. A pure parent-child
// relationship reads as "the consumer ran inside the producer", which is how a
// synchronous call looks and would make the publish appear to contain however
// long the message sat in the queue.
func TestConsumerSpanAlsoLinksTheProducer(t *testing.T) {
	recorder := recording(t)

	_, span := telemetry.ConsumerSpan(context.Background(), "scope", "probe process",
		map[string]string{"traceparent": "00-" + probeTraceID + "-" + probeSpanID + "-01"})
	span.End()

	links := recorder.Ended()[0].Links()
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1", len(links))
	}
	if got := links[0].SpanContext.SpanID().String(); got != probeSpanID {
		t.Errorf("link points at %s, want the producer %s", got, probeSpanID)
	}
}

// A message with NO traceparent yields a root span rather than an error.
//
// That is the correct answer for an event published before this
// instrumentation existed, or by a producer that has none. A gap in a trace
// beats a dropped message, and it must not carry a phantom link either.
func TestAMessageWithoutATraceparentStartsARoot(t *testing.T) {
	recorder := recording(t)

	_, span := telemetry.ConsumerSpan(context.Background(), "scope", "probe process", nil)
	span.End()

	ended := recorder.Ended()[0]
	if ended.Parent().IsValid() {
		t.Errorf("span has parent %s; an untraced message must start a root", ended.Parent().SpanID())
	}
	if len(ended.Links()) != 0 {
		t.Errorf("links = %d, want 0: there is no producer to link to", len(ended.Links()))
	}
}

// The span kind is CONSUMER, which is what makes Tempo's service graph and
// messaging views recognise the hop as a queue read rather than a function
// call.
func TestConsumerSpanIsKindConsumer(t *testing.T) {
	recorder := recording(t)

	_, span := telemetry.ConsumerSpan(context.Background(), "scope", "probe process", nil)
	span.End()

	if got := recorder.Ended()[0].SpanKind(); got != trace.SpanKindConsumer {
		t.Errorf("kind = %v, want %v", got, trace.SpanKindConsumer)
	}
}

// Caller attributes survive alongside the ones ConsumerSpan adds.
//
// The messaging semconv keys and the caller's domain attributes have to
// coexist: one makes the span recognisable to tooling, the other makes it
// answer a question.
func TestCallerAttributesAreKept(t *testing.T) {
	recorder := recording(t)

	_, span := telemetry.ConsumerSpan(context.Background(), "scope", "probe process", nil,
		telemetry.MessagingAttributes("planning.plan.committed.v1", "event-1"))
	span.End()

	found := map[string]string{}
	for _, attr := range recorder.Ended()[0].Attributes() {
		found[string(attr.Key)] = attr.Value.AsString()
	}
	if found["messaging.destination.name"] != "planning.plan.committed.v1" {
		t.Errorf("destination = %q, want the subject", found["messaging.destination.name"])
	}
	if found["messaging.message.id"] != "event-1" {
		t.Errorf("message id = %q, want event-1", found["messaging.message.id"])
	}
	if found["messaging.system"] != "nats" {
		t.Errorf("system = %q, want nats", found["messaging.system"])
	}
}

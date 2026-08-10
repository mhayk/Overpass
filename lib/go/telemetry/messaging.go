package telemetry

import (
	"context"

	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// ConsumerSpan starts a span for one consumed message, as both a CHILD and a
// LINK of the producer.
//
// Both, deliberately, and the reason is the shape of an async hop.
//
// A pure parent-child relationship says the consumer ran INSIDE the producer,
// which is how a synchronous call looks: the publish would appear to contain
// the consume, and the producer's duration would swallow however long the
// message sat in the queue. A pure link says the two are related and loses the
// causal chain — the consumer becomes a root, and "what happened to this
// request" returns two traces a human has to join by hand.
//
// Child gives the chain. Link states, in the model rather than in a comment,
// that this is a message-driven continuation and not a nested call. Tempo
// renders both, and a reader can see at a glance that the gap between the
// spans is queue time rather than work.
//
// A message with NO traceparent yields a root span rather than an error. That
// is the correct answer for an event published before this instrumentation
// existed, or by a producer that has none — a gap in a trace beats a dropped
// message.
//
// This is a deliberate port of feasibility's telemetry.consumer_span. The two
// languages consume from the same streams, and a trace whose shape depends on
// which service happened to handle a message is not one anybody can read.
func ConsumerSpan(
	ctx context.Context,
	scope string,
	name string,
	headers map[string]string,
	attrs ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	parent := Extract(ctx, headers)

	options := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindConsumer)}
	if producer := trace.SpanContextFromContext(parent); producer.IsValid() {
		options = append(options, trace.WithLinks(trace.Link{SpanContext: producer}))
	}
	options = append(options, attrs...)

	return Tracer(scope).Start(parent, name, options...)
}

// MessagingAttributes describes one consumed or published message.
//
// semconv rather than hand-rolled keys, so Tempo's messaging views and the
// service graph recognise these spans without a mapping nobody would remember
// to update.
func MessagingAttributes(subject, eventID string) trace.SpanStartEventOption {
	return trace.WithAttributes(
		semconv.MessagingSystemKey.String("nats"),
		semconv.MessagingDestinationName(subject),
		semconv.MessagingMessageIDKey.String(eventID),
	)
}

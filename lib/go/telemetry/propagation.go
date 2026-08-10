package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Inject writes the current span's context into a header map.
//
// The map carries W3C traceparent and tracestate, which is what goes into the
// outbox row and then onto the NATS message. Returns the map it was given so it
// can be used inline.
func Inject(ctx context.Context, headers map[string]string) map[string]string {
	if headers == nil {
		headers = map[string]string{}
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(headers))
	return headers
}

// Extract reads a header map back into a context.
func Extract(ctx context.Context, headers map[string]string) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(headers))
}

// IDs returns the trace and span ids for logging, or empty strings.
//
// Empty rather than the all-zero id. "00000000000000000000000000000000" in a log
// line looks like a real id and matches nothing, which is a worse experience
// than an absent field.
func IDs(ctx context.Context) (traceID, spanID string) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}

// Package telemetry wires OpenTelemetry tracing.
//
// One export target: the collector. Not Tempo directly — services should know
// one endpoint, and sampling, redaction or a second backend then have one place
// to live rather than N.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ScopeName identifies spans this service creates by hand.
const ScopeName = "github.com/mhayk/overpass/services/tasking-api"

// Config is what tracing needs from the environment.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
	// Endpoint is the collector's OTLP gRPC address. Empty disables tracing.
	Endpoint string
	// SampleRatio is the head sampling ratio. 1 means everything.
	SampleRatio float64
}

// Setup installs a global tracer provider and returns its shutdown.
//
// The shutdown must be called and must be given time. Spans are batched, so a
// process that exits without flushing loses the last few seconds of trace —
// which is reliably the interesting few seconds, because it is the ones around
// whatever made you look.
//
// An unreachable collector is NOT a startup failure. Tracing is observability,
// not function; refusing to serve traffic because a telemetry backend is down
// would make the observability stack an availability dependency, which is
// exactly backwards. The exporter retries in the background and spans are
// dropped in the meantime.
func Setup(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	// The propagator is installed even when export is off. It costs nothing and
	// it means a traceparent still travels through this service to the next
	// one, so a partially-instrumented deployment produces a trace with a gap
	// rather than two unrelated traces.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if cfg.Endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		// Plaintext. The collector is on the compose network; TLS between two
		// containers on the same bridge buys nothing and adds certificates to
		// a stack whose whole point is starting in one command.
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("creating the OTLP exporter: %w", err)
	}

	// NewSchemaless, not NewWithAttributes(semconv.SchemaURL, ...).
	//
	// resource.Merge REFUSES to merge two resources with different schema URLs,
	// and resource.Default() carries whichever version the SDK ships. Pinning
	// our own semconv version here produced:
	//
	//   building the resource: conflicting Schema URL:
	//   https://opentelemetry.io/schemas/1.43.0 and .../1.26.0
	//
	// Setup treats that as non-fatal — correctly, tracing must not stop the
	// service — so the service started, logged one warning, and exported
	// nothing. It compiled, linted, and passed every unit test, because those
	// call Setup with no endpoint and return before this line. Only running it
	// against a real collector showed the feature was off.
	//
	// Schemaless attributes merge with anything. The alternative is chasing the
	// SDK's semconv version on every upgrade, and getting that wrong silently
	// disables tracing again.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
		attribute.String("deployment.environment", cfg.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("building the resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			// Half a second. The default is five, which is longer than the
			// end-to-end test is willing to wait and longer than a demo's
			// patience for a trace to appear.
			sdktrace.WithBatchTimeout(500*time.Millisecond),
		),
		sdktrace.WithResource(res),
		// ParentBased, so a sampling decision made upstream is honoured rather
		// than re-rolled. Re-rolling per service is how a trace ends up with
		// holes in the middle of it.
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SampleRatio),
		)),
	)
	otel.SetTracerProvider(provider)

	return func(shutdownCtx context.Context) error {
		return errors.Join(provider.Shutdown(shutdownCtx))
	}, nil
}

// Tracer returns this service's tracer.
func Tracer() trace.Tracer { return otel.Tracer(ScopeName) }

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

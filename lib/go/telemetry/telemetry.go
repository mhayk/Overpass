// Package telemetry wires OpenTelemetry tracing and metrics.
//
// One export target: the collector. Not Tempo or Prometheus directly —
// services should know one endpoint, and sampling, redaction or a second
// backend then have one place to live rather than N.
//
// Metrics take the same path as traces, pushed over OTLP rather than scraped
// from a per-service /metrics endpoint. ADR-0018 has the argument; the short
// version is that one of the four services is a worker with no HTTP listener,
// deliberately, and giving it one purely to be scraped is the thing
// docker-compose.yml already refused to do for a readiness probe.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// ExportInterval is how often the meter provider pushes to the collector.
//
// Ten seconds, matching prometheus.yml's scrape_interval. Faster exports
// samples Prometheus never reads; slower means one scrape sees the same
// cumulative value twice, which flattens a rate into a staircase.
const ExportInterval = 10 * time.Second

// Config is what telemetry needs from the environment.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
	// Endpoint is the collector's OTLP gRPC address. Empty disables export.
	Endpoint string
	// SampleRatio is the head sampling ratio for traces. 1 means everything.
	SampleRatio float64
}

// Setup installs global tracer and meter providers and returns one shutdown.
//
// The shutdown must be called and must be given time. Spans are batched and
// metrics are exported on an interval, so a process that exits without
// flushing loses the last few seconds of both — which is reliably the
// interesting few seconds, because it is the ones around whatever made you
// look.
//
// An unreachable collector is NOT a startup failure. Observability is not
// function; refusing to serve traffic because a telemetry backend is down
// would make the observability stack an availability dependency, which is
// exactly backwards. The exporters retry in the background and drop data in
// the meantime.
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
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		return func(context.Context) error { return nil }, nil
	}

	res, err := buildResource(cfg)
	if err != nil {
		return nil, err
	}

	traceProvider, err := setupTraces(ctx, cfg, res)
	if err != nil {
		return nil, err
	}
	otel.SetTracerProvider(traceProvider)

	meterProvider, err := setupMetrics(ctx, cfg, res)
	if err != nil {
		// Shut the trace provider down rather than leaking its exporter
		// goroutine. Returning an error here aborts startup, so nothing else
		// will ever call the shutdown we would otherwise have returned.
		return nil, errors.Join(err, traceProvider.Shutdown(ctx))
	}
	otel.SetMeterProvider(meterProvider)

	return func(shutdownCtx context.Context) error {
		return errors.Join(
			traceProvider.Shutdown(shutdownCtx),
			meterProvider.Shutdown(shutdownCtx),
		)
	}, nil
}

// buildResource describes this process to both signals.
//
// NewSchemaless, not NewWithAttributes(semconv.SchemaURL, ...).
//
// resource.Merge REFUSES to merge two resources with different schema URLs,
// and resource.Default() carries whichever version the SDK ships. Pinning our
// own semconv version here produced:
//
//	building the resource: conflicting Schema URL:
//	https://opentelemetry.io/schemas/1.43.0 and .../1.26.0
//
// Setup treats that as non-fatal — correctly, telemetry must not stop the
// service — so the service started, logged one warning, and exported nothing.
// It compiled, linted, and passed every unit test, because those call Setup
// with no endpoint and return before this line. Only running it against a real
// collector showed the feature was off.
//
// Schemaless attributes merge with anything. The alternative is chasing the
// SDK's semconv version on every upgrade, and getting that wrong silently
// disables telemetry again.
func buildResource(cfg Config) (*resource.Resource, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
		attribute.String("deployment.environment", cfg.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("building the resource: %w", err)
	}
	return res, nil
}

func setupTraces(ctx context.Context, cfg Config, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		// Plaintext. The collector is on the compose network; TLS between two
		// containers on the same bridge buys nothing and adds certificates to
		// a stack whose whole point is starting in one command.
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("creating the OTLP trace exporter: %w", err)
	}

	return sdktrace.NewTracerProvider(
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
	), nil
}

func setupMetrics(ctx context.Context, cfg Config, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
		otlpmetricgrpc.WithInsecure(),
		otlpmetricgrpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("creating the OTLP metric exporter: %w", err)
	}

	// Cumulative temporality, which is the SDK's default for OTLP and what the
	// collector's prometheus exporter expects. Delta would be re-accumulated
	// by the exporter and any collector restart would silently reset every
	// counter on the dashboards.
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter,
			sdkmetric.WithInterval(ExportInterval),
		)),
		sdkmetric.WithResource(res),
	), nil
}

// Tracer returns a tracer for the given instrumentation scope.
func Tracer(scope string) trace.Tracer { return otel.Tracer(scope) }

// Meter returns a meter for the given instrumentation scope.
func Meter(scope string) metric.Meter { return otel.Meter(scope) }

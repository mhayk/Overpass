package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The route PATTERN reaches the metrics, not the raw path.
//
// This service is the one where it matters most: /v1/plans/{satellite_id}/
// {bucket_start} instantiates to a different path for every satellite and
// every three-hour bucket. Labelling by raw path would create an unbounded
// label set — a Prometheus instance killed by its own instrumentation — and
// labelling by nothing, which is what otelhttp does unaided, makes "which
// endpoint is slow" unanswerable.
func TestRoutePatternReachesTheMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	handler := serve(t, &fakeReads{}, nil)
	for _, path := range []string{
		"/v1/plans/SENTINEL-1A/2026-08-10T00:00:00Z",
		"/v1/plans/ICEYE-X2/2026-08-10T03:00:00Z",
	} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, http.NoBody))
	}

	routes := routeLabels(t, reader)
	if len(routes) != 1 || !routes["/v1/plans/{satellite_id}/{bucket_start}"] {
		t.Errorf("http.route labels = %v; want exactly the pattern, not one series per satellite", routes)
	}
}

// Health probes stay out of RED.
//
// A liveness check every five seconds is the highest-volume operation this
// service performs. Leaving it in makes the error ratio meaningless: a 100%
// failure rate on a real route disappears into a denominator of probes.
func TestProbesAreExcludedFromMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	handler := serve(t, &fakeReads{}, nil)
	for _, path := range []string{"/healthz", "/readyz"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, http.NoBody))
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "http.server.request.duration" {
				t.Error("probes produced HTTP RED; the otelhttp filter is not excluding them")
			}
		}
	}
}

func routeLabels(t *testing.T, reader sdkmetric.Reader) map[string]bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	routes := map[string]bool{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
			}
			for _, dp := range h.DataPoints {
				if v, found := dp.Attributes.Value("http.route"); found {
					routes[v.AsString()] = true
				}
			}
		}
	}
	return routes
}

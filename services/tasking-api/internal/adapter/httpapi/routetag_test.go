package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The route pattern must reach the HTTP server metrics.
//
// Without it every request in the service collapses into a single series and
// "which endpoint is slow" is unanswerable — which is how the metric shipped
// on the first attempt, because otelhttp wraps the router from the outside and
// never sees chi's routing decision.
func TestRoutePatternReachesTheMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	handler := submitServer(t, &fakeStore{})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/tasking-requests", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	routes := routeLabels(t, reader)
	if !routes["/v1/tasking-requests"] {
		t.Errorf("http.route labels = %v, want /v1/tasking-requests", routes)
	}
}

// Health probes stay out of RED entirely.
//
// A liveness check every five seconds is the highest-volume and least
// interesting operation this service performs. Leaving it in makes the error
// ratio meaningless: a 100% failure rate on the one route that matters
// disappears into a denominator of probes.
func TestProbesAreExcludedFromMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	handler := submitServer(t, &fakeStore{})
	for _, path := range []string{"/healthz", "/readyz"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody))
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "http.server.request.duration" {
				t.Errorf("probes produced %s; the otelhttp filter is not excluding them", m.Name)
			}
		}
	}
}

// An unrouted path is left unlabelled rather than tagged "unknown".
//
// A 404 for a path nobody routes is not an endpoint. Naming it would put
// scanner traffic on the dashboard beside real routes.
func TestUnmatchedPathCarriesNoRouteLabel(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	handler := submitServer(t, &fakeStore{})
	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/no-such-path", http.NoBody))

	if routes := routeLabels(t, reader); len(routes) != 0 {
		t.Errorf("http.route labels = %v, want none for an unmatched path", routes)
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

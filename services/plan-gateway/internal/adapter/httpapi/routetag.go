package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// RouteTag puts chi's route pattern onto the HTTP server metrics.
//
// otelhttp wraps the router from the OUTSIDE, which is right for the span — it
// must enclose routing and every middleware — but means otelhttp never learns
// which route was matched. Measured rather than assumed: with only the wrapper
// in place, http_server_request_duration_seconds carried method, status,
// scheme and host, and no http_route at all. Every request in the service
// collapsed into one series, so "which endpoint is slow" had no answer and the
// dashboard's rate-by-route panel could not be written.
//
// The Labeler is otelhttp's supported way to add attributes from inside the
// handler chain. It is read AFTER the handler returns, which is exactly when
// chi has finished filling the pattern in, so the tag is applied on the way
// back out rather than on the way in.
//
// The route PATTERN, never the raw path: "/v1/requests/{request_id}" is one
// series, "/v1/requests/<uuid>" is one series per customer request, and the
// second kind is how an unbounded label kills a Prometheus instance.
func RouteTag(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		route := chi.RouteContext(r.Context())
		if route == nil || route.RoutePattern() == "" {
			// An unmatched path. Deliberately left unlabelled rather than
			// tagged "unknown" — a 404 for a path nobody routes is not an
			// endpoint, and giving it a name puts scanner traffic on the
			// dashboard next to real routes.
			return
		}
		if labeler, ok := otelhttp.LabelerFromContext(r.Context()); ok {
			labeler.Add(semconv.HTTPRoute(route.RoutePattern()))
		}
	})
}

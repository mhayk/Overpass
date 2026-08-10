package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/mhayk/overpass/gen/go/taskingapi"
	"github.com/mhayk/overpass/services/tasking-api/internal/app"
	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
)

// Server owns the routing table and nothing else.
type Server struct {
	health    *app.HealthService
	submitter *app.SubmitService
	log       *slog.Logger

	// submitTimeout bounds how long one submission may spend reaching the
	// database. Without it, an exhausted connection pool holds every request
	// open indefinitely: the service looks alive, answers nothing, and the
	// request count simply stops — the failure mode hardest to attribute,
	// because every dashboard stays green.
	//
	// A refusal is recoverable and a hang is not, which is the same argument
	// `unavailable` already makes about a request that could not be stored.
	submitTimeout time.Duration
}

// New builds the router.
func New(
	health *app.HealthService,
	submitter *app.SubmitService,
	submitTimeout time.Duration,
	log *slog.Logger,
) *Server {
	return &Server{health: health, submitter: submitter, submitTimeout: submitTimeout, log: log}
}

// Routes returns the fully wired handler.
//
// otelhttp wraps the whole router rather than being a chi middleware, so the
// server span is the OUTERMOST thing in the request: it extracts the caller's
// traceparent, starts a child, and every middleware inside it — including the
// correlation logger — runs within that span's context. A span started inside
// chi's chain would exclude the routing and the middleware from its own
// duration, which is exactly where a slow request often is.
//
// Health probes are excluded. A liveness check every five seconds is the
// highest-volume operation this service performs and the least interesting, and
// leaving it in buries real traffic in the trace list.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(RouteTag)
	r.Use(Correlate(s.log))

	r.Get("/healthz", s.liveness)
	r.Get("/readyz", s.readiness)
	r.Post("/v1/tasking-requests", s.submit)

	return otelhttp.NewHandler(r, "tasking-api",
		otelhttp.WithFilter(func(req *http.Request) bool {
			return req.URL.Path != "/healthz" && req.URL.Path != "/readyz"
		}),
		// The chi route pattern, not the raw path. Without this every request
		// is its own span name and the trace list is unaggregatable — though
		// this service has no path parameters today, so it costs nothing now
		// and prevents a surprise the first time one is added.
		otelhttp.WithSpanNameFormatter(func(_ string, req *http.Request) string {
			if route := chi.RouteContext(req.Context()); route != nil && route.RoutePattern() != "" {
				return req.Method + " " + route.RoutePattern()
			}
			return req.Method + " " + req.URL.Path
		}),
	)
}

// liveness answers /healthz. Never consults a dependency — see app.Live.
func (s *Server) liveness(w http.ResponseWriter, r *http.Request) {
	if err := writeJSON(w, http.StatusOK, taskingapi.HealthStatus{
		Status:  taskingapi.HealthStatusStatus(s.health.Live()),
		Version: strptr(s.health.Version()),
	}); err != nil {
		LoggerFrom(r.Context(), s.log).Error("liveness response failed", slog.Any("error", err))
	}
}

// readiness answers /readyz.
//
// 503 when a dependency is down, which is what makes an orchestrator stop
// sending traffic. Returning 200 with a "degraded" body would be read as ready,
// and this service accepting a request it cannot persist is the worst thing it
// can do.
func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	result := s.health.Ready(r.Context())

	checks := make(map[string]struct {
		Detail    *string                              `json:"detail,omitempty"`
		LatencyMs *float32                             `json:"latency_ms,omitempty"`
		Status    *taskingapi.HealthStatusChecksStatus `json:"status,omitempty"`
	}, len(result.Checks))

	for _, c := range result.Checks {
		status := taskingapi.HealthStatusChecksStatusOk
		if !c.Healthy {
			status = taskingapi.HealthStatusChecksStatusFailed
		}
		latency := float32(c.LatencyMS)
		entry := struct {
			Detail    *string                              `json:"detail,omitempty"`
			LatencyMs *float32                             `json:"latency_ms,omitempty"`
			Status    *taskingapi.HealthStatusChecksStatus `json:"status,omitempty"`
		}{LatencyMs: &latency, Status: &status}
		if c.Detail != "" {
			entry.Detail = strptr(c.Detail)
		}
		checks[c.Name] = entry
	}

	code := http.StatusOK
	if !result.Ready() {
		code = http.StatusServiceUnavailable
		LoggerFrom(r.Context(), s.log).Warn("not ready", slog.Any("checks", result.Checks))
	}

	if err := writeJSON(w, code, taskingapi.HealthStatus{
		Status:  taskingapi.HealthStatusStatus(result.Status()),
		Checks:  &checks,
		Version: strptr(s.health.Version()),
	}); err != nil {
		LoggerFrom(r.Context(), s.log).Error("readiness response failed", slog.Any("error", err))
	}
}

// writeJSON writes the response, reporting an encode failure to the caller.
//
// The error is returned rather than dropped: by the time encoding fails the
// status line is already sent, so it cannot be turned into a 500 — but a
// handler that silently truncates a response is a handler nobody can debug.
func writeJSON(w http.ResponseWriter, code int, body any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	return encode(w, body)
}

func encode(w http.ResponseWriter, body any) error {
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return fmt.Errorf("encoding response: %w", err)
	}
	return nil
}

func strptr(s string) *string { return &s }

// compile-time assurance that the domain statuses and the generated enum agree.
var (
	_ = taskingapi.HealthStatusStatus(domain.StatusOK)
	_ = taskingapi.HealthStatusStatus(domain.StatusUnavailable)
)

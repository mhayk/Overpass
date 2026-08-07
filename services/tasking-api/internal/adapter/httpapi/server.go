package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mhayk/overpass/gen/go/taskingapi"
	"github.com/mhayk/overpass/services/tasking-api/internal/app"
	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
)

// Server owns the routing table and nothing else.
type Server struct {
	health *app.HealthService
	log    *slog.Logger
}

// New builds the router.
func New(health *app.HealthService, log *slog.Logger) *Server {
	return &Server{health: health, log: log}
}

// Routes returns the fully wired handler.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(Correlate(s.log))

	r.Get("/healthz", s.liveness)
	r.Get("/readyz", s.readiness)
	return r
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

// Package httpapi serves the planner's liveness and readiness probes.
//
// The planner is a worker, not an API. This exists so compose and, later,
// anything scraping the stack can tell "the process is up" from "the process
// can do its job" — a distinction that matters here more than for a REST
// service, because a planner that cannot reach Postgres fails silently: it
// simply never fires a round, and nothing external notices the absence of a
// plan that was never promised.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Probe reports whether a dependency is usable.
type Probe func(context.Context) error

// Handler answers the two probes.
type Handler struct {
	ready   Probe
	version string
	log     *slog.Logger
}

// New builds the probe handler.
func New(ready Probe, version string, log *slog.Logger) *Handler {
	return &Handler{ready: ready, version: version, log: log}
}

// Routes wires the probes onto a mux.
//
// net/http's own mux rather than chi. tasking-api and plan-gateway both take
// chi because they route real APIs with middleware; two endpoints that take no
// parameters do not, and a dependency added for two endpoints is a dependency
// whose cost outlives its reason.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Liveness: the process is running. Deliberately checks nothing else — a
	// liveness probe that fails when the database is down asks the orchestrator
	// to restart a process whose problem a restart cannot fix.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		h.write(w, http.StatusOK, map[string]string{"status": "ok", "version": h.version})
	})

	// Readiness: the process can do its job.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := h.ready(r.Context()); err != nil {
			h.log.Warn("not ready", slog.Any("error", err))
			h.write(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable",
				"reason": err.Error(),
			})
			return
		}
		h.write(w, http.StatusOK, map[string]string{"status": "ready", "version": h.version})
	})

	return mux
}

func (h *Handler) write(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.Warn("writing probe response", slog.Any("error", err))
	}
}

// Serve runs the probe server until the context is cancelled, then shuts it
// down within timeout.
func Serve(ctx context.Context, server *http.Server, timeout time.Duration, log *slog.Logger) error {
	errs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		// context.WithoutCancel, NOT context.Background(). The shutdown budget
		// must survive the cancellation that triggered it — a plain child of
		// ctx would already be done — but it should still carry ctx's values,
		// so a trace spanning the shutdown stays joined to the run that caused
		// it. Background() severs that silently.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Warn("probe server shutdown", slog.Any("error", err))
		}
		return <-errs
	}
}

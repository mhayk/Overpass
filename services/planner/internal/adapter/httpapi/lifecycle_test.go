package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/adapter/httpapi"
)

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func get(t *testing.T, h http.Handler, path string) (int, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding %s response: %v", path, err)
	}
	return rec.Code, body
}

// Liveness must NOT depend on Postgres.
//
// A liveness probe that fails when the database is down asks the orchestrator
// to restart a process whose problem a restart cannot fix — turning a database
// outage into a crash loop on top of a database outage.
func TestLivenessIgnoresDependencies(t *testing.T) {
	handler := httpapi.New(
		func(context.Context) error { return errors.New("database is on fire") },
		"v1.2.3", discard()).Routes()

	code, body := get(t, handler, "/healthz")
	if code != http.StatusOK {
		t.Errorf("healthz = %d with a failing dependency, want 200", code)
	}
	if body["version"] != "v1.2.3" {
		t.Errorf("version = %q", body["version"])
	}
}

func TestReadinessReportsAWorkingDependency(t *testing.T) {
	handler := httpapi.New(func(context.Context) error { return nil }, "v1.2.3", discard()).Routes()

	code, body := get(t, handler, "/readyz")
	if code != http.StatusOK {
		t.Errorf("readyz = %d, want 200", code)
	}
	if body["status"] != "ready" {
		t.Errorf("status = %q, want ready", body["status"])
	}
}

// The planner fails SILENTLY when it cannot reach Postgres: it simply never
// fires a round, and nothing external notices the absence of a plan that was
// never promised. Readiness is the only thing that makes that visible.
func TestReadinessReportsABrokenDependency(t *testing.T) {
	handler := httpapi.New(
		func(context.Context) error { return errors.New("connection refused") },
		"v1.2.3", discard()).Routes()

	code, body := get(t, handler, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d with an unreachable database, want 503", code)
	}
	if body["reason"] != "connection refused" {
		t.Errorf("reason = %q, want the underlying error", body["reason"])
	}
}

func TestProbesAreJSON(t *testing.T) {
	handler := httpapi.New(func(context.Context) error { return nil }, "dev", discard()).Routes()

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s Content-Type = %q", path, ct)
		}
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	handler := httpapi.New(func(context.Context) error { return nil }, "dev", discard()).Routes()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/plans", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /plans = %d, want 404 — the planner serves probes, not an API", rec.Code)
	}
}

// Shutdown must return rather than hang, and it must be exercised by running
// the real server rather than by calling Shutdown directly.
func TestServeStopsOnCancellation(t *testing.T) {
	// Port 0 so the test cannot collide with anything else on the machine, and
	// so a leaked server from an earlier run cannot make this one pass.
	server := &http.Server{
		Addr:              "127.0.0.1:0",
		Handler:           httpapi.New(func(context.Context) error { return nil }, "dev", discard()).Routes(),
		ReadHeaderTimeout: time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- httpapi.Serve(ctx, server, 5*time.Second, discard()) }()

	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve ignored cancellation")
	}
}

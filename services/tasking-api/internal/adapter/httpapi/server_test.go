package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/tasking-api/internal/app"
	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

// stubProbe is why the layering matters: the whole HTTP surface is testable
// without Postgres, because app depends on an interface it owns.
type stubProbe struct {
	name string
	err  error
}

func (s stubProbe) Name() string                { return s.name }
func (s stubProbe) Check(context.Context) error { return s.err }

func serve(t *testing.T, probes ...stubProbe) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc := app.NewHealthService("test-version", time.Second, toPorts(probes)...)
	return httpapi.New(svc, logger).Routes()
}

func TestLivenessIsAlwaysOKEvenWhenPostgresIsDown(t *testing.T) {
	// A liveness probe that fails on a dependency outage causes a restart loop,
	// and a restart loop during an outage makes the outage worse.
	rec := httptest.NewRecorder()
	serve(t, stubProbe{"postgres", errors.New("connection refused")}).
		ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("liveness returned %d during a dependency outage", rec.Code)
	}
	if body := decode(t, rec.Body.Bytes()); body["status"] != "ok" {
		t.Fatalf("got status %v", body["status"])
	}
}

func TestReadinessIs503WhenPostgresIsDown(t *testing.T) {
	rec := httptest.NewRecorder()
	serve(t, stubProbe{"postgres", errors.New("connection refused")}).
		ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 — an orchestrator reads anything else as ready", rec.Code)
	}
	body := decode(t, rec.Body.Bytes())
	if body["status"] != "unavailable" {
		t.Fatalf("got status %v", body["status"])
	}
	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("no checks object in the response: %v", body)
	}
	pg, ok := checks["postgres"].(map[string]any)
	if !ok {
		t.Fatalf("the failing dependency was not named: %v", checks)
	}
	if pg["status"] != "failed" {
		t.Fatalf("the failing dependency was not marked failed: %v", pg)
	}
	detail, ok := pg["detail"].(string)
	if !ok || !strings.Contains(detail, "refused") {
		t.Fatalf("the reason was not reported: %v", pg)
	}
}

func TestReadinessIs200WhenEverythingIsUp(t *testing.T) {
	rec := httptest.NewRecorder()
	serve(t, stubProbe{"postgres", nil}).
		ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
}

func TestACorrelationIdIsAssignedAndEchoed(t *testing.T) {
	rec := httptest.NewRecorder()
	serve(t, stubProbe{"postgres", nil}).
		ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))

	if got := rec.Header().Get(httpapi.CorrelationHeader); got == "" {
		t.Fatal("no correlation id was assigned")
	}
}

func TestAnInboundCorrelationIdIsAdopted(t *testing.T) {
	// A caller that supplied an id must see the same one back, or its support
	// ticket cannot be tied to our logs.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	req.Header.Set(httpapi.CorrelationHeader, "caller-supplied-id")

	rec := httptest.NewRecorder()
	serve(t, stubProbe{"postgres", nil}).ServeHTTP(rec, req)

	if got := rec.Header().Get(httpapi.CorrelationHeader); got != "caller-supplied-id" {
		t.Fatalf("got %q, want the id the caller supplied", got)
	}
}

func TestEveryLogLineCarriesTheCorrelationId(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	svc := app.NewHealthService("v", time.Second, stubProbe{"postgres", nil})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
	req.Header.Set(httpapi.CorrelationHeader, "trace-me")
	httpapi.New(svc, logger).Routes().ServeHTTP(httptest.NewRecorder(), req)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("nothing was logged")
	}
	for _, line := range lines {
		entry := decode(t, []byte(line))
		if entry["correlation_id"] != "trace-me" {
			t.Fatalf("a log line has no correlation_id: %s", line)
		}
	}
}

func toPorts(probes []stubProbe) []port.DependencyProbe {
	out := make([]port.DependencyProbe, 0, len(probes))
	for _, p := range probes {
		out = append(out, p)
	}
	return out
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response was not JSON: %v\n%s", err, raw)
	}
	return out
}

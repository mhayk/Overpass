package httpapi_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/tasking-api/internal/app"
	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
)

// unroutedServer is submitServer's server before Routes, so these tests can
// attach an allow-list to it.
func unroutedServer() *httpapi.Server {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	submitter := app.NewSubmitService(
		&fakeStore{}, fixedClock{submitNow}, domain.ConfiguredSensors(), domain.DefaultValidationPolicy(),
	)
	return httpapi.New(app.NewHealthService("test", time.Second), submitter, 5*time.Second, logger)
}

// SUBMITTING IS THE ONE WRITE THE UI MAKES, AND IT IS CROSS-ORIGIN.
//
// The web app is on :3000, this service on :8080. A POST carrying
// Idempotency-Key and Content-Type is not a "simple" request, so the browser
// sends a preflight first and never sends the POST at all unless it is
// answered. The middleware is covered in lib/go/httpx; this covers the WIRING,
// which is what was missing when the whole frontend shipped unable to reach
// either service.
func corsServer(t *testing.T) http.Handler {
	t.Helper()
	return unroutedServer().WithCORS([]string{"http://localhost:3000"}).Routes()
}

func TestThePreflightForASubmissionIsAnswered(t *testing.T) {
	r := httptest.NewRequest(http.MethodOptions, "/v1/tasking-requests", nil)
	r.Header.Set("Origin", "http://localhost:3000")
	r.Header.Set("Access-Control-Request-Method", http.MethodPost)
	r.Header.Set("Access-Control-Request-Headers", "content-type,idempotency-key")

	recorder := httptest.NewRecorder()
	corsServer(t).ServeHTTP(recorder, r)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 — the browser will not send the POST", recorder.Code)
	}
	allowed := recorder.Header().Get("Access-Control-Allow-Headers")
	for _, header := range []string{"Content-Type", "Idempotency-Key"} {
		if !strings.Contains(strings.ToLower(allowed), strings.ToLower(header)) {
			t.Errorf("%q absent from Access-Control-Allow-Headers %q", header, allowed)
		}
	}
	if !strings.Contains(recorder.Header().Get("Access-Control-Allow-Methods"), http.MethodPost) {
		t.Error("POST is not in Access-Control-Allow-Methods")
	}
}

// THE REPLAY HEADER MUST BE EXPOSED OR THE UI MISREPORTS EVERY DE-DUPLICATION.
//
// Cross-origin, a page reads only the CORS-safelisted response headers.
// Idempotency-Replayed comes back as null otherwise — and the client turns it
// into a boolean, where null and "false" are the same thing. A resubmission
// that the server correctly de-duplicated would be shown to the user as a new
// request, which is the exact confusion idempotency exists to prevent.
func TestTheReplayHeaderIsExposedToThePage(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("Origin", "http://localhost:3000")

	recorder := httptest.NewRecorder()
	corsServer(t).ServeHTTP(recorder, r)

	exposed := recorder.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(exposed, "Idempotency-Replayed") {
		t.Errorf("Access-Control-Expose-Headers = %q; the page reads the replay flag as null", exposed)
	}
}

func TestWithoutAnAllowListNoOriginIsPermitted(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("Origin", "http://localhost:3000")

	recorder := httptest.NewRecorder()
	unroutedServer().Routes().ServeHTTP(recorder, r)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("permitted %q with no allow-list configured", got)
	}
}

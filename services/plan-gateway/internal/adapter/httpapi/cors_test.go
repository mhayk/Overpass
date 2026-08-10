package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/plan-gateway/internal/app"
)

// THE READ API IS CALLED FROM A BROWSER, AND THAT IS PART OF ITS CONTRACT.
//
// The web app runs on :3000 and this service on :8083, so every read the UI
// makes is cross-origin. Without Access-Control-Allow-Origin the browser
// fetches, receives the response, and then refuses to hand it to the page —
// leaving a blank UI, a green readiness probe, and an error visible only in a
// console.
//
// That is exactly what shipped. The unit tests passed, curl worked, and the
// whole M4 frontend rendered nothing in a real browser. The middleware is
// covered in lib/go/httpx; this test covers the WIRING, which is the half that
// was actually missing.
func corsServer(t *testing.T) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return httpapi.New(&fakeReads{}, func() error { return nil },
		func() time.Time { return pinnedNow }, time.Second, log).
		WithCORS([]string{"http://localhost:3000"}).
		WithHub(app.NewHub()).
		Routes()
}

func TestTheReadApiIsReadableFromTheWebOrigin(t *testing.T) {
	// Every route the UI actually calls, including the ones added late. A
	// header set on some routes and not others is worse than none: the page
	// half-renders and the missing half looks like missing data.
	for _, path := range []string{
		"/v1/plans",
		"/v1/acquisitions",
		"/v1/geo/footprints",
		"/v1/geo/targets",
		"/v1/geo/opportunities",
	} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
		r.Header.Set("Origin", "http://localhost:3000")

		recorder := httptest.NewRecorder()
		corsServer(t).ServeHTTP(recorder, r)

		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q — the browser will discard this response",
				path, got)
		}
	}
}

// THE STREAM IS OUTSIDE THE DEADLINE GROUP, SO IT IS ALSO OUTSIDE ANYTHING
// ELSE REGISTERED THERE.
//
// /v1/events is registered on the outer router to escape the read deadline.
// Any middleware added to the inner group therefore misses it — and a live
// view that cannot connect is precisely the failure the SSE work existed to
// prevent. This asserts the CORS middleware sits above that split.
func TestTheEventStreamIsAlsoReadableFromTheWebOrigin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/events", http.NoBody)
	r.Header.Set("Origin", "http://localhost:3000")

	recorder := httptest.NewRecorder()
	corsServer(t).ServeHTTP(recorder, r)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q — EventSource will fail to connect", got)
	}
}

// A server built without an allow-list adds nothing, so the middleware cannot
// become an accidental default in some future test harness.
func TestWithoutAnAllowListNoOriginIsPermitted(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := httpapi.New(&fakeReads{}, func() error { return nil },
		func() time.Time { return pinnedNow }, time.Second, log).Routes()

	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/plans", http.NoBody)
	r.Header.Set("Origin", "http://localhost:3000")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, r)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("permitted %q with no allow-list configured", got)
	}
}

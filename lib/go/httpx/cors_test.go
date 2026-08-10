package httpx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/lib/go/httpx"
)

func middleware() func(http.Handler) http.Handler {
	return httpx.CORS(httpx.CORSConfig{
		AllowedOrigins: []string{"http://localhost:3000"},
		AllowedMethods: httpx.DefaultCORSMethods,
		AllowedHeaders: httpx.DefaultCORSHeaders,
		ExposedHeaders: []string{"Idempotency-Replayed"},
		MaxAge:         10 * time.Minute,
	})
}

// served runs a request through the middleware and a handler that always
// answers, so a missing header is distinguishable from a missing response.
func served(t *testing.T, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler := middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Idempotency-Replayed", "true")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}))
	handler.ServeHTTP(rec, r)
	return rec
}

// THE TEST THAT WOULD HAVE CAUGHT IT.
//
// The whole M4 frontend shipped without this header. Every unit test passed,
// every curl worked, and the browser refused every single read — a blank page
// with the failure visible only in a console nobody was reading.
func TestAnAllowedOriginMayReadTheResponse(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/acquisitions", http.NoBody)
	r.Header.Set("Origin", "http://localhost:3000")

	got := served(t, r).Header().Get("Access-Control-Allow-Origin")
	if got != "http://localhost:3000" {
		t.Fatalf("Access-Control-Allow-Origin = %q; a browser cannot read this response", got)
	}
}

// The origin is echoed, never "*".
func TestTheOriginIsEchoedRatherThanWildcarded(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/acquisitions", http.NoBody)
	r.Header.Set("Origin", "http://localhost:3000")

	if got := served(t, r).Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error(`answered "*"; that is a standing invitation to every page on the internet`)
	}
}

func TestAnUnlistedOriginGetsNoPermission(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/acquisitions", http.NoBody)
	r.Header.Set("Origin", "https://evil.example")

	rec := served(t, r)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allowed %q; the allow-list is not being consulted", got)
	}
	// Still served. This middleware is not authentication, and pretending it is
	// would break every non-browser client the moment someone sets an Origin.
	if rec.Code != http.StatusOK {
		t.Errorf("status %d; a non-browser client was refused service", rec.Code)
	}
}

// VARY: ORIGIN, OR A SHARED CACHE HANDS ONE ORIGIN'S PERMISSION TO ANOTHER.
//
// The response body is identical for every origin; only this header differs.
// A cache that does not know that will serve the allowed origin's
// Access-Control-Allow-Origin to an unlisted one — a security hole that
// appears only behind a proxy and never in a test that skips it.
func TestVaryOriginIsSetForAllowedAndRefusedAlike(t *testing.T) {
	for _, origin := range []string{"http://localhost:3000", "https://evil.example"} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/acquisitions", http.NoBody)
		r.Header.Set("Origin", origin)

		if vary := served(t, r).Header().Get("Vary"); vary != "Origin" {
			t.Errorf("origin %s: Vary = %q, want Origin", origin, vary)
		}
	}
}

// A REQUEST WITH NO ORIGIN IS LEFT ALONE.
func TestARequestWithoutAnOriginIsUntouched(t *testing.T) {
	rec := served(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", http.NoBody))

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("added a CORS header to a request that never asked")
	}
	if rec.Header().Get("Vary") != "" {
		t.Error("varied a response that does not vary; this suppresses caching for nothing")
	}
}

func TestAPreflightIsAnsweredWithoutReachingTheHandler(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/v1/requests", http.NoBody)
	r.Header.Set("Origin", "http://localhost:3000")
	r.Header.Set("Access-Control-Request-Method", http.MethodPost)
	r.Header.Set("Access-Control-Request-Headers", "content-type,idempotency-key")

	rec := httptest.NewRecorder()
	reached := false
	middleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	})).ServeHTTP(rec, r)

	if reached {
		t.Error("the preflight reached the router; a handler with no OPTIONS case would 405 it")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("no Access-Control-Allow-Headers; the POST that follows would be refused")
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("Access-Control-Max-Age = %q, want 600", got)
	}
}

// THE PREFLIGHT MUST PERMIT IDEMPOTENCY-KEY.
//
// It is the header that forces the preflight to exist at all: without it a
// JSON POST is not a simple request either, but with it the browser will not
// send the POST unless this list names it. Submitting a tasking request is the
// one write the UI makes.
func TestThePreflightPermitsTheHeadersTheUISends(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/v1/requests", http.NoBody)
	r.Header.Set("Origin", "http://localhost:3000")
	r.Header.Set("Access-Control-Request-Method", http.MethodPost)

	allowed := served(t, r).Header().Get("Access-Control-Allow-Headers")
	for _, header := range []string{"Content-Type", "Idempotency-Key"} {
		if !containsHeader(allowed, header) {
			t.Errorf("%q absent from %q; the browser will not send the request", header, allowed)
		}
	}
}

// A PREFLIGHT FROM AN UNLISTED ORIGIN IS REFUSED VISIBLY.
func TestAnUnlistedPreflightIsRefusedWithAStatusSomeoneCanSee(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/v1/requests", http.NoBody)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Access-Control-Request-Method", http.MethodPost)

	if code := served(t, r).Code; code != http.StatusForbidden {
		t.Errorf("status %d, want 403 — a 204 that does not work is unreadable in a network tab", code)
	}
}

// A HEADER THE PAGE CANNOT READ IS A HEADER THAT DOES NOT EXIST.
//
// Cross-origin, JavaScript sees only the CORS-safelisted response headers.
// Idempotency-Replayed reads back as null unless it is exposed — and null is
// indistinguishable from "false", so the UI would report a replayed submission
// as a fresh one.
func TestTheReplayHeaderIsExposedToThePage(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/requests", http.NoBody)
	r.Header.Set("Origin", "http://localhost:3000")

	exposed := served(t, r).Header().Get("Access-Control-Expose-Headers")
	if !containsHeader(exposed, "Idempotency-Replayed") {
		t.Errorf("Access-Control-Expose-Headers = %q; the page reads Idempotency-Replayed as null", exposed)
	}
}

// CREDENTIALS ARE NOT ALLOWED, and that is a decision rather than an omission.
func TestCredentialsAreNotAllowed(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/acquisitions", http.NoBody)
	r.Header.Set("Origin", "http://localhost:3000")

	if got := served(t, r).Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q; these APIs authenticate nothing", got)
	}
}

// An empty allow-list disables CORS rather than allowing everything.
func TestAnEmptyAllowListPermitsNoOrigin(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/acquisitions", http.NoBody)
	r.Header.Set("Origin", "http://localhost:3000")

	rec := httptest.NewRecorder()
	httpx.CORS(httpx.CORSConfig{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, r)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an empty allow-list permitted %q", got)
	}
}

// Scheme and host are case-insensitive; the allow-list must not be stricter
// than the URL grammar.
func TestOriginMatchingIgnoresCase(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/acquisitions", http.NoBody)
	r.Header.Set("Origin", "HTTP://LocalHost:3000")

	if got := served(t, r).Header().Get("Access-Control-Allow-Origin"); got == "" {
		t.Error("refused an origin that differs only in case")
	}
}

// Port and scheme are part of the origin and must not be ignored.
func TestOriginMatchingIsExactOnSchemeAndPort(t *testing.T) {
	for _, origin := range []string{
		"http://localhost:3001",
		"https://localhost:3000",
		"http://localhost",
		"http://localhost:3000.evil.example",
	} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/acquisitions", http.NoBody)
		r.Header.Set("Origin", origin)

		if got := served(t, r).Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %s was allowed as %q; matching is not exact", origin, got)
		}
	}
}

// THE TRAILING SLASH IS THE ONE THAT COSTS AN AFTERNOON.
//
// "http://localhost:3000/" is what a person copies out of an address bar. It
// is not an origin, no browser ever sends one, and it therefore matches
// nothing — producing a service that starts cleanly, logs nothing, and blocks
// every request from the origin its operator believes they allowed.
func TestParseOriginsRefusesWhatCouldNeverMatch(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:3000/",
		"http://localhost:3000/app",
		"localhost:3000",
		"//localhost:3000",
		"ftp://localhost:3000",
		"http://",
		"http://localhost:3000?x=1",
		"http://localhost:3000#top",
	} {
		if got, err := httpx.ParseOrigins(raw); err == nil {
			t.Errorf("ParseOrigins(%q) = %v, want an error at startup", raw, got)
		}
	}
}

func TestParseOriginsAcceptsRealOrigins(t *testing.T) {
	got, err := httpx.ParseOrigins(" http://localhost:3000 , https://overpass.example ,")
	if err != nil {
		t.Fatalf("ParseOrigins: %v", err)
	}
	want := []string{"http://localhost:3000", "https://overpass.example"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// An unset variable is not an error. A service with no browser client is a
// normal thing to run, and refusing to start over it would be worse.
func TestParseOriginsAcceptsEmpty(t *testing.T) {
	got, err := httpx.ParseOrigins("")
	if err != nil || got != nil {
		t.Errorf("ParseOrigins(\"\") = %v, %v; want nil, nil", got, err)
	}
}

// THE PARSER AND THE MATCHER MUST AGREE.
//
// A value one accepts and the other rejects is the silent outage this pairing
// exists to prevent, so the check is that anything ParseOrigins returns is
// actually honoured by CORS.
func TestEveryParsedOriginIsOneTheMatcherHonours(t *testing.T) {
	origins, err := httpx.ParseOrigins("http://localhost:3000,https://overpass.example")
	if err != nil {
		t.Fatalf("ParseOrigins: %v", err)
	}

	handler := httpx.CORS(httpx.CORSConfig{AllowedOrigins: origins})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	for _, origin := range origins {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
		r.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("parser accepted %q but the matcher answered %q", origin, got)
		}
	}
}

// containsHeader compares case-insensitively, because header names are.
func containsHeader(list, want string) bool {
	for _, part := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(part), want) {
			return true
		}
	}
	return false
}

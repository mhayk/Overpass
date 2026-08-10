// Package httpx holds HTTP middleware shared by the services a browser calls
// directly.
package httpx

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORSConfig is the allow-list and what it permits.
//
// Every field is explicit. There is no wildcard and no "allow everything in
// development" escape hatch, because the one place such a switch gets set is
// the place it matters, and an allow-list nobody can misconfigure is worth the
// one extra line in compose.
type CORSConfig struct {
	// AllowedOrigins are matched exactly against the request's Origin, after
	// lowercasing (scheme and host are case-insensitive; the rest of an origin
	// is empty by definition). An empty list disables CORS entirely, which is
	// the correct posture for a service no browser talks to.
	AllowedOrigins []string
	// AllowedMethods and AllowedHeaders answer the preflight. They are stated
	// rather than echoed from the request: echoing means the middleware
	// approves whatever it is asked about, which is an allow-list in name only.
	AllowedMethods []string
	AllowedHeaders []string
	// ExposedHeaders are the response headers JavaScript may read.
	//
	// This one is easy to forget and fails silently. Cross-origin, a browser
	// hands the page only the CORS-safelisted response headers; everything else
	// reads back as null with no error anywhere. tasking-api's
	// Idempotency-Replayed is exactly that shape — the client would conclude
	// "not a replay" from a header it was never allowed to see.
	ExposedHeaders []string
	// MaxAge is how long a browser may cache the preflight answer.
	MaxAge time.Duration
}

// CORS answers preflights and marks responses for the allowed origins.
//
// Three deliberate positions:
//
// The Origin is ECHOED, never answered with "*". A wildcard is a standing
// statement that any page on the internet may read this API, and it also
// cannot be combined with credentials should this service ever grow them —
// which turns a future auth change into a mysterious browser-only outage.
//
// Vary: Origin is set on every request that carries one. The response differs
// by origin, so a cache that ignores that will serve one origin's
// Access-Control-Allow-Origin to another, and the failure appears only behind
// a shared cache — the least reproducible place there is.
//
// Access-Control-Allow-Credentials is NOT set. These APIs authenticate
// nothing, and a browser that is not sending cookies does not need permission
// to. Adding it "just in case" would widen what a hostile page can do with a
// user's session the moment one exists.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, origin := range cfg.AllowedOrigins {
		if trimmed := strings.ToLower(strings.TrimSpace(origin)); trimmed != "" {
			allowed[trimmed] = struct{}{}
		}
	}

	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")
	exposed := strings.Join(cfg.ExposedHeaders, ", ")
	maxAge := strconv.Itoa(int(cfg.MaxAge.Seconds()))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// No Origin at all: a same-origin fetch, curl, a probe, another
				// service. CORS has nothing to say about it, and adding headers
				// would only mislead whoever reads them.
				next.ServeHTTP(w, r)
				return
			}

			// Set before the allow-list check, not after. The answer varies by
			// origin whether or not this particular origin is allowed, and a
			// cache needs to know that for the refusals too.
			w.Header().Add("Vary", "Origin")

			_, ok := allowed[strings.ToLower(origin)]
			preflight := r.Method == http.MethodOptions &&
				r.Header.Get("Access-Control-Request-Method") != ""

			if !ok {
				if preflight {
					// 403 rather than a bare 204 without the headers. Both make
					// the browser refuse, but only one is legible in a network
					// tab: an operator who added an origin to the UI and forgot
					// the server sees a refusal instead of a success that
					// somehow does not work.
					http.Error(w, "origin not allowed", http.StatusForbidden)
					return
				}
				// A simple request from an unlisted origin is served normally
				// and simply carries no Access-Control-Allow-Origin. The
				// browser withholds the body from the page; anything that is
				// not a browser is unaffected, which is what keeps this
				// middleware from becoming an authentication mechanism it is
				// not.
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Access-Control-Allow-Origin", origin)
			if exposed != "" {
				w.Header().Set("Access-Control-Expose-Headers", exposed)
			}

			if preflight {
				w.Header().Set("Access-Control-Allow-Methods", methods)
				w.Header().Set("Access-Control-Allow-Headers", headers)
				w.Header().Set("Access-Control-Max-Age", maxAge)
				// The preflight is answered here and never reaches the router.
				// Routing it would mean every handler grows an OPTIONS case,
				// and the ones that forgot would 405 a request the browser
				// needs a 2xx for.
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ParseOrigins reads a comma-separated allow-list and refuses what could never
// match.
//
// It lives beside the matcher rather than in each service's config, because a
// value this parser accepts and that matcher rejects is a silent outage: the
// service starts, the header is absent, and the browser reports a CORS error
// that names no cause.
//
// The trailing slash is the failure worth naming. "http://localhost:3000/" is
// what a person copies out of a browser address bar, it is not an origin, and
// it will never equal the Origin header a browser sends. Refusing it at
// startup costs one restart; accepting it costs an afternoon.
func ParseOrigins(raw string) ([]string, error) {
	var origins []string
	for _, part := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(part)
		if origin == "" {
			continue
		}
		if err := validOrigin(origin); err != nil {
			return nil, err
		}
		origins = append(origins, origin)
	}
	return origins, nil
}

func validOrigin(origin string) error {
	scheme, rest, found := strings.Cut(origin, "://")
	if !found || (scheme != "http" && scheme != "https") {
		return fmt.Errorf("origin %q must start with http:// or https://", origin)
	}
	if rest == "" {
		return fmt.Errorf("origin %q has no host", origin)
	}
	// An origin is scheme, host and port — nothing else. A path, a query or a
	// fragment means whoever wrote it pasted a URL, and a URL never matches.
	if strings.ContainsAny(rest, "/?#") {
		return fmt.Errorf(
			"origin %q must be scheme://host[:port] with no trailing slash or path; "+
				"a browser's Origin header never carries one", origin)
	}
	return nil
}

// DefaultCORSMethods and DefaultCORSHeaders are what both services allow.
//
// Shared so the two cannot drift. A browser that may POST to tasking-api but
// finds plan-gateway rejecting its Content-Type preflight is a bug that looks
// like a network problem.
var (
	DefaultCORSMethods = []string{
		http.MethodGet, http.MethodPost, http.MethodOptions,
	}
	DefaultCORSHeaders = []string{
		"Content-Type",
		"Accept",
		// The submit path's de-duplication key. Its presence is what forces a
		// preflight on POST at all — without it the request would be "simple"
		// and this list would never be consulted.
		"Idempotency-Key",
		// Sent by a resuming EventSource. Browsers set it themselves, but a
		// test client or a proxy replaying a stream sets it by hand.
		"Last-Event-ID",
	}
)

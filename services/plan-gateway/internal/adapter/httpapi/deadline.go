package httpapi

import (
	"context"
	"net/http"
	"time"
)

// Deadline bounds how long any one request may spend in the read models.
//
// AUDITED, not assumed (#51). Every handler in this service passed
// r.Context() straight to the query layer, and an inbound request context has
// no deadline of its own — so a slow query held its request open indefinitely.
// The service looks alive, answers nothing, and the request count simply stops.
// That is the failure mode hardest to attribute, because every dashboard stays
// green while nothing is being served.
//
// It is the same argument tasking-api's submitTimeout already makes about the
// write path, and it applies at least as strongly here: this is the service
// whose queries touch PostGIS geometry and span whole buckets, so it is the one
// where a query can genuinely take minutes. A refusal is recoverable and a hang
// is not.
//
// MIDDLEWARE RATHER THAN EIGHT EDITS. Applying it per handler would work today
// and be forgotten by the ninth endpoint, and a bound that covers most requests
// is not a bound. Wrapping the router means a new route is covered by existing
// it.
//
// Health probes are wrapped too, deliberately. They are already the fastest
// thing here, so the timeout never fires — but a readiness probe that could
// hang is a readiness probe that turns one slow dependency into an
// unschedulable service, and that is worth closing off by construction rather
// than by argument.
func Deadline(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			// Cancel on the way out regardless of outcome. Without this every
			// completed request leaks its timer until the deadline passes,
			// which under load is a slow, quiet growth that looks like a
			// memory leak somewhere else entirely.
			defer cancel()

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

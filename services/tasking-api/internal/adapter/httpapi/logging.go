// Package httpapi is the REST adapter: chi routing, JSON responses, and the
// middleware that makes a request traceable.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type contextKey string

const correlationKey contextKey = "correlation_id"

// CorrelationHeader is the inbound header a caller can use to supply its own
// id. Accepting one is what lets a customer's support ticket be tied to our
// logs without either side guessing.
const CorrelationHeader = "X-Correlation-Id"

// CorrelationID returns the id for this request, or "" outside one.
func CorrelationID(ctx context.Context) string {
	if v, ok := ctx.Value(correlationKey).(string); ok {
		return v
	}
	return ""
}

// WithCorrelationID puts an id on the context. Exported for tests and for the
// outbox relay, which continues a trace rather than starting one.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey, id)
}

// LoggerFrom returns a logger that already carries this request's
// correlation_id, so no handler has to remember to add it.
//
// That is the entire reason this exists. "Log the correlation id" as a
// convention is a convention that holds until the first tired afternoon, and
// the line that gets missed is invariably the one someone needs at 3am.
func LoggerFrom(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := CorrelationID(ctx); id != "" {
		return base.With(slog.String("correlation_id", id))
	}
	return base
}

// Correlate assigns or adopts a correlation id and logs the request.
func Correlate(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(CorrelationHeader)
			if id == "" {
				id = uuid.NewString()
			}
			ctx := WithCorrelationID(r.Context(), id)

			// Echo it back. A caller that cannot see the id we used cannot
			// quote it to us later, which defeats the point of having one.
			w.Header().Set(CorrelationHeader, id)

			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			started := time.Now()
			next.ServeHTTP(recorder, r.WithContext(ctx))

			LoggerFrom(ctx, base).Info("http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", recorder.status),
				slog.Float64("duration_ms", float64(time.Since(started).Microseconds())/1000.0),
			)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

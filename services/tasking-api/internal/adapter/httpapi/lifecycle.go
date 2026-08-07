package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Serve runs an HTTP server until the context is cancelled, then drains.
//
// Extracted from main so the shutdown path is testable. A graceful shutdown
// that has never been exercised is a graceful shutdown that hangs, or that
// cuts in-flight requests, and either one is discovered during a deploy.
func Serve(ctx context.Context, server *http.Server, grace time.Duration, log *slog.Logger) error {
	errs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
		close(errs)
	}()

	select {
	case err, ok := <-errs:
		if ok && err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down", slog.Duration("grace", grace))
	}

	// A FRESH context, deliberately. The one above is already cancelled by the
	// signal, and passing it to Shutdown aborts in-flight requests immediately
	// — the exact opposite of draining them.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	//nolint:contextcheck // Not inheriting is the entire point: ctx is already
	// cancelled, and Shutdown with a cancelled context kills in-flight requests
	// instead of draining them.
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

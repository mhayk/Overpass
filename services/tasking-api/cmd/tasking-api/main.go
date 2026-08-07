// Command tasking-api is the REST ingress for tasking requests.
//
// main() is the only place in this service that panics or calls os.Exit. Every
// other package returns errors, which is what makes them testable.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/config"
	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/tasking-api/internal/app"
)

func main() {
	if err := run(); err != nil {
		// Log through the plain logger: a configuration failure can happen
		// before the configured one exists.
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("startup failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	log.Info("starting",
		slog.String("service", "tasking-api"),
		slog.String("version", cfg.Version),
		slog.String("env", cfg.Environment),
		slog.String("addr", cfg.HTTPAddr),
	)

	// Signals are trapped BEFORE anything long-running starts, so a Ctrl-C
	// during pool construction is honoured rather than ignored.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	health := app.NewHealthService(
		cfg.Version,
		cfg.ReadinessTimeout,
		postgres.NewProbe(pool),
	)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.New(health, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down", slog.Duration("grace", cfg.ShutdownTimeout))
	}

	// Graceful shutdown, with its own context: the one above is already
	// cancelled by the signal, and passing it here would abort in-flight
	// requests immediately — which is the opposite of draining them.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown did not drain cleanly", slog.Any("error", err))
		return err
	}
	log.Info("stopped")
	return nil
}

// newLogger builds the structured JSON logger.
//
// JSON always, including in development. A format that differs between
// environments is a format whose parsing bugs are found in production.
func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).With(
		slog.String("service", "tasking-api"),
		slog.String("version", cfg.Version),
	)
}

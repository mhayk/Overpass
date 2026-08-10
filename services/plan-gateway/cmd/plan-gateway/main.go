// Command plan-gateway projects events into read models and serves them.
//
// A composition root and nothing else. Two concerns run side by side: the
// projector folding the streams, and the REST server answering from what has
// been folded. They share a pool and nothing else — the reader never waits on
// the writer, which is the whole reason for a read model.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/mhayk/overpass/lib/go/telemetry"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/config"
	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/logging"
	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/natsmsg"
	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/wire"
	"github.com/mhayk/overpass/services/plan-gateway/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("startup failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Telemetry before anything reports anything. An unreachable collector is
	// a warning, never a startup failure: refusing to serve read models
	// because a metrics backend is down would make observability an
	// availability dependency, which is exactly backwards.
	shutdownTelemetry, telemetryErr := telemetry.Setup(ctx, telemetry.Config{
		ServiceName:    "plan-gateway",
		ServiceVersion: cfg.Version,
		Environment:    cfg.Environment,
		Endpoint:       cfg.OTLPEndpoint,
		SampleRatio:    cfg.TraceSampleRatio,
	})
	if telemetryErr != nil {
		shutdownTelemetry = func(context.Context) error { return nil }
	}
	defer func() {
		// WithoutCancel, not Background: the shutdown must outlive the
		// cancelled run context — that is the whole point of flushing — but it
		// should still carry the run's values rather than starting from a
		// context unrelated to this process.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		if shutdownErr := shutdownTelemetry(shutdownCtx); shutdownErr != nil {
			slog.Warn("telemetry shutdown", slog.Any("error", shutdownErr))
		}
	}()

	log := logging.New(os.Stdout, "plan-gateway", cfg.Version, cfg.LogLevel)
	if telemetryErr != nil {
		log.Warn("telemetry not started; metrics and spans will be dropped",
			slog.String("endpoint", cfg.OTLPEndpoint), slog.Any("error", telemetryErr))
	}
	log.Info("starting",
		slog.String("version", cfg.Version),
		slog.String("env", cfg.Environment),
		slog.String("addr", cfg.HTTPAddr),
	)

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	reads := postgres.NewReads(pool)
	probe := func() error { return pool.Ping(ctx) }
	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.New(reads, probe, func() time.Time { return time.Now().UTC() },
			cfg.ReadTimeout, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The projector is started as a warning-on-failure, same as tasking-api's
	// relay. A gateway that cannot reach the broker still serves what it has
	// already folded, with the staleness on every response saying exactly how
	// old that is. Refusing to start would replace stale answers with none.
	var wg sync.WaitGroup
	if closeProjector, projErr := startProjector(ctx, cfg, pool, log, &wg); projErr != nil {
		log.Warn("projector not started; read models will not advance",
			slog.Any("error", projErr))
	} else {
		defer closeProjector()
	}

	serveErr := httpapi.Serve(ctx, server, cfg.ShutdownTimeout, log)
	wg.Wait()
	return serveErr
}

func startProjector(
	ctx context.Context,
	cfg config.Config,
	pool *pgxpool.Pool,
	log *slog.Logger,
	wg *sync.WaitGroup,
) (func(), error) {
	conn, err := nats.Connect(cfg.NATSURL)
	if err != nil {
		return nil, err
	}
	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, err
	}
	source, err := natsmsg.Bind(js, cfg.DurablePrefix, cfg.FetchBatch, cfg.FetchWait)
	if err != nil {
		conn.Close()
		return nil, err
	}

	projector := app.NewProjector(source, postgres.NewProjection(pool), wire.New(),
		log.With(slog.String("component", "projector")))
	if bindErr := projector.Metrics.Bind(telemetry.Meter(app.TelemetryScope)); bindErr != nil {
		// A warning, matching how the projector itself is started: one that
		// folds correctly while reporting nothing is still better than one
		// that does not run.
		log.Warn("consumer metrics not bound", slog.Any("error", bindErr))
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if runErr := projector.Run(ctx); runErr != nil {
			log.Error("projector stopped", slog.Any("error", runErr))
		}
	}()

	log.Info("projector started",
		slog.String("nats", cfg.NATSURL),
		slog.String("durable_prefix", cfg.DurablePrefix),
		slog.Any("streams", natsmsg.Streams))

	return func() {
		// Drain before closing, so acks already issued reach the broker. A bare
		// Close here would leave folded events unacked and redeliver them on
		// the next start — harmless, because the fold is idempotent, but it
		// makes every restart look like a burst of duplicate work.
		if drainErr := source.Drain(); drainErr != nil {
			log.Warn("drain failed", slog.Any("error", drainErr))
		}
		conn.Close()
	}, nil
}

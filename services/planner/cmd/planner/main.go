// Command planner keeps the planner's input projections fed.
//
// A composition root and nothing else. Today it runs one concern — the
// projector folding tasking.request.received.v1 into planning.request_snapshots
// and feasibility.opportunities.computed.v1 into
// planning.candidate_opportunities, so that when a round fires it reads its own
// schema and nothing else (ADR-0015).
//
// The round trigger, the advisory lock and the allocation policies arrive with
// M2-01 onward and will start beside the projector here.
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

	"github.com/mhayk/overpass/services/planner/internal/adapter/config"
	"github.com/mhayk/overpass/services/planner/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/planner/internal/adapter/logging"
	"github.com/mhayk/overpass/services/planner/internal/adapter/natsmsg"
	"github.com/mhayk/overpass/services/planner/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/planner/internal/adapter/wire"
	"github.com/mhayk/overpass/services/planner/internal/app"
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

	log := logging.New(os.Stdout, "planner", cfg.Version, cfg.LogLevel)
	log.Info("starting",
		slog.String("version", cfg.Version),
		slog.String("env", cfg.Environment),
	)

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.New(pool.Ping, cfg.Version, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The projector is started warning-on-failure, matching tasking-api's relay
	// and plan-gateway's projector.
	//
	// The reasoning differs from the gateway's, though, and is worth stating.
	// A gateway that cannot reach the broker still serves what it folded. A
	// planner that cannot reach the broker has nothing to serve — but it also
	// has nothing to corrupt, and refusing to start would take the probe
	// endpoint down with it, replacing a service that reports itself unready
	// with one that is simply absent. Unready and observable beats gone.
	var wg sync.WaitGroup
	if closeProjector, projErr := startProjector(ctx, cfg, pool, log, &wg); projErr != nil {
		log.Warn("projector not started; the planner's inputs will not advance",
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
	source, err := natsmsg.Bind(js, cfg.FetchBatch, cfg.FetchWait)
	if err != nil {
		conn.Close()
		return nil, err
	}

	projector := app.NewProjector(source, wire.New(), postgres.NewProjections(pool),
		log.With(slog.String("component", "projector")))

	wg.Add(1)
	go func() {
		defer wg.Done()
		if runErr := projector.Run(ctx, 0, cfg.IdleWait); runErr != nil {
			log.Error("projector stopped", slog.Any("error", runErr))
		}
	}()

	log.Info("projector started",
		slog.String("nats", cfg.NATSURL),
		slog.Any("bindings", app.Streams))

	return func() {
		// Drain before closing, so acks already issued reach the broker. A bare
		// Close would leave folded events unacked and redeliver them on the
		// next start — harmless, because the fold is idempotent, but it makes
		// every restart look like a burst of duplicate work.
		if drainErr := source.Drain(); drainErr != nil {
			log.Warn("drain failed", slog.Any("error", drainErr))
		}
		conn.Close()
	}, nil
}

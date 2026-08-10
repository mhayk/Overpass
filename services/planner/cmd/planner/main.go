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
	"fmt"
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
	"github.com/mhayk/overpass/lib/go/telemetry/outboxmetrics"

	"github.com/mhayk/overpass/services/planner/internal/adapter/config"
	"github.com/mhayk/overpass/services/planner/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/planner/internal/adapter/logging"
	"github.com/mhayk/overpass/services/planner/internal/adapter/natsmsg"
	"github.com/mhayk/overpass/services/planner/internal/adapter/outbox"
	"github.com/mhayk/overpass/services/planner/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/planner/internal/adapter/wire"
	"github.com/mhayk/overpass/services/planner/internal/allocation"
	"github.com/mhayk/overpass/services/planner/internal/app"
	"github.com/mhayk/overpass/services/planner/internal/domain"
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

	// Telemetry before anything else reports anything. An unreachable
	// collector is a warning, never a startup failure: refusing to plan
	// because a metrics backend is down would make observability an
	// availability dependency, which is exactly backwards.
	shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName:    "planner",
		ServiceVersion: cfg.Version,
		Environment:    cfg.Environment,
		Endpoint:       cfg.OTLPEndpoint,
		SampleRatio:    cfg.TraceSampleRatio,
	})
	if err != nil {
		shutdownTelemetry = func(context.Context) error { return nil }
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		if shutdownErr := shutdownTelemetry(shutdownCtx); shutdownErr != nil {
			slog.Warn("telemetry shutdown", slog.Any("error", shutdownErr))
		}
	}()

	log := logging.New(os.Stdout, "planner", cfg.Version, cfg.LogLevel)
	if err != nil {
		log.Warn("telemetry not started; metrics and spans will be dropped",
			slog.String("endpoint", cfg.OTLPEndpoint), slog.Any("error", err))
	}

	meter := telemetry.Meter(app.TelemetryScope)
	instruments, err := app.NewInstruments(meter)
	if err != nil {
		// A refusal, not a warning. Instrument construction fails on a
		// duplicate or malformed name, which is a programming error the
		// dashboards depend on — and a planner that starts having silently
		// failed to build its own metrics is the state #53 exists to end.
		return fmt.Errorf("building planner instruments: %w", err)
	}
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
	// The round trigger starts REGARDLESS of the broker, unlike the projector.
	//
	// It reads Postgres and writes Postgres — the outbox is what defers the
	// publish — so a broker outage does not stop rounds being opened and
	// recorded. They queue in planning.outbox and go out when the relay
	// reconnects, which is the entire point of ADR-0006. A trigger that refused
	// to run without a broker would turn a publishing problem into a planning
	// problem.
	allocator, err := allocatorFor(cfg.AllocationPolicy)
	if err != nil {
		return err
	}
	trigger, err := app.NewTrigger(postgres.NewRounds(pool), app.TriggerConfig{
		Policy: domain.TriggerPolicy{
			QuietPeriod:      cfg.QuietPeriod,
			StalenessCeiling: cfg.StalenessCeiling,
		},
		BucketDuration: cfg.BucketDuration,
		HorizonAhead:   cfg.HorizonAhead,
		SweepLimit:     cfg.SweepLimit,
		Allocator:      allocator,
		Fairness:       cfg.Fairness,
		Instruments:    instruments,
	}, log.With(slog.String("component", "trigger")))
	if err != nil {
		// A misconfigured firing rule is a startup failure, not a warning. A
		// ceiling below the quiet period still plans — it just silently never
		// debounces — and a system that has quietly lost half its rule is worse
		// than one that refused to start.
		return err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if runErr := trigger.Run(ctx, 0, cfg.SweepInterval); runErr != nil {
			log.Error("trigger stopped", slog.Any("error", runErr))
		}
	}()

	// Ledger cleanup, hourly. The retention guard lives in the lib: a value
	// inside the redelivery horizon is refused loudly here rather than
	// silently reprocessing a late redelivery as new.
	projections := postgres.NewProjections(pool)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				deleted, cleanErr := projections.CleanupLedger(ctx, cfg.LedgerRetention)
				if cleanErr != nil {
					log.Error("ledger cleanup failed", slog.Any("error", cleanErr))
					continue
				}
				if deleted > 0 {
					log.Info("ledger cleaned", slog.Int64("rows", deleted))
				}
			}
		}
	}()

	if closeProjector, projErr := startProjector(ctx, cfg, pool, log, &wg); projErr != nil {
		log.Warn("projector and relay not started; inputs will not advance and rounds will not publish",
			slog.Any("error", projErr))
	} else {
		defer closeProjector()
	}

	serveErr := httpapi.Serve(ctx, server, cfg.ShutdownTimeout, log)
	wg.Wait()
	return serveErr
}

// allocatorFor maps the configured name onto a policy. The names are the
// contract enum, which config.Load has already validated against.
func allocatorFor(name string) (domain.AllocationPolicy, error) {
	switch name {
	case "GREEDY_BY_BID":
		return allocation.GreedyByBid{}, nil
	case "GREEDY_BY_VALUE_DENSITY":
		return allocation.GreedyByValueDensity{}, nil
	case "VICKREY_SEALED_BID":
		return allocation.VickreySealedBid{}, nil
	case "EXACT_DP":
		// Legal but loud: the exact solver refuses rounds beyond its size
		// limit, so running it in production means large rounds allocate
		// nothing. It exists as a configured option for the benchmark and the
		// demo, not for real traffic.
		return allocation.NewExactDP(), nil
	default:
		return nil, fmt.Errorf("unknown allocation policy %q", name)
	}
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
	if bindErr := projector.Metrics.Bind(telemetry.Meter(app.TelemetryScope)); bindErr != nil {
		// A warning rather than a refusal, unlike the domain instruments
		// above: the projector is started warning-on-failure already, and a
		// consumer that folds correctly while reporting nothing is strictly
		// better than one that does not run.
		log.Warn("consumer metrics not bound", slog.Any("error", bindErr))
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if runErr := projector.Run(ctx, 0, cfg.IdleWait); runErr != nil {
			log.Error("projector stopped", slog.Any("error", runErr))
		}
	}()

	relay := outbox.New(pool, outbox.NewNATSPublisher(js), outbox.DefaultConfig(),
		log.With(slog.String("component", "relay")))
	if outboxInstruments, obErr := outboxmetrics.New(telemetry.Meter(app.TelemetryScope)); obErr != nil {
		log.Warn("outbox metrics not bound", slog.Any("error", obErr))
	} else {
		relay.Instruments = outboxInstruments
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if runErr := relay.Run(ctx, 0); runErr != nil {
			log.Error("relay stopped", slog.Any("error", runErr))
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

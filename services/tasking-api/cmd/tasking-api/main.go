// Command tasking-api is the REST ingress for tasking requests.
//
// A composition root and nothing else: it reads configuration, wires the parts
// together, and hands off. Every piece of logic it used to hold now lives in a
// package that can be tested — main() is the one place in this service that
// calls os.Exit, and code that exits is code no test can reach.
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

	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/config"
	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/logging"
	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/outbox"
	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/tasking-api/internal/app"
	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
	"github.com/mhayk/overpass/services/tasking-api/internal/telemetry"
)

// systemClock is the real clock. Everything that needs the time takes a
// port.Clock instead of calling time.Now, so a test can pin it.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func main() {
	// Signals are trapped BEFORE anything long-running starts, so a Ctrl-C
	// during pool construction is honoured rather than ignored.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		// Through a plain logger: a configuration failure happens before the
		// configured one exists.
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("startup failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// run does the wiring and blocks until the context is cancelled.
//
// Takes the context rather than installing the signal handler itself, so the
// whole composition can be exercised by a test that cancels it. A wiring
// function that can only be stopped by a real SIGTERM is a wiring function
// nobody ever runs outside production.
func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logging.New(os.Stdout, "tasking-api", cfg.Version, cfg.LogLevel)
	log.Info("starting",
		slog.String("version", cfg.Version),
		slog.String("env", cfg.Environment),
		slog.String("addr", cfg.HTTPAddr),
	)

	// Tracing before anything that might produce a span, and shut down last.
	//
	// A failure here does NOT stop the service. Tracing is observability, not
	// function; refusing to serve traffic because a telemetry backend is
	// unreachable would make the observability stack an availability
	// dependency, which is exactly backwards.
	shutdownTracing, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName:    "tasking-api",
		ServiceVersion: cfg.Version,
		Environment:    cfg.Environment,
		Endpoint:       cfg.OTLPEndpoint,
		SampleRatio:    cfg.TraceSampleRatio,
	})
	if err != nil {
		log.Warn("tracing not started; spans will be dropped",
			slog.String("endpoint", cfg.OTLPEndpoint), slog.Any("error", err))
		shutdownTracing = func(context.Context) error { return nil }
	} else {
		log.Info("tracing started",
			slog.String("endpoint", cfg.OTLPEndpoint),
			slog.Float64("sample_ratio", cfg.TraceSampleRatio))
	}
	//nolint:contextcheck // Not inheriting is the point; see the comment below.
	defer func() {
		// A FRESH context with its own deadline. ctx is already cancelled by
		// the time this runs, and Shutdown on a cancelled context drops the
		// batch instead of flushing it — losing precisely the spans around
		// whatever made the process stop.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if flushErr := shutdownTracing(flushCtx); flushErr != nil {
			log.Warn("flushing traces failed", slog.Any("error", flushErr))
		}
	}()

	// TWO POOLS, not one — the bulkhead (#51).
	//
	// Ingress and the background work are separated so that neither can consume
	// the other's capacity. The relay drains batches and the readiness probe
	// runs on a timer; both are throughput-shaped and can wait. A submission
	// cannot: refusing one is business damage, and ADR-0003 makes ingress
	// availability the property this architecture exists to protect.
	//
	// A shared pool makes that property depend on whatever else is querying at
	// the time, which is precisely the coupling a bulkhead removes.
	ingressPool, err := openPool(ctx, cfg.DatabaseURL, cfg.IngressMaxConns, "overpass-tasking-api-ingress")
	if err != nil {
		return fmt.Errorf("ingress pool: %w", err)
	}
	defer ingressPool.Close()

	backgroundPool, err := openPool(ctx, cfg.DatabaseURL, cfg.BackgroundMaxConns, "overpass-tasking-api-background")
	if err != nil {
		return fmt.Errorf("background pool: %w", err)
	}
	defer backgroundPool.Close()

	health := app.NewHealthService(cfg.Version, cfg.ReadinessTimeout, postgres.NewProbe(backgroundPool))
	submitter := app.NewSubmitService(
		postgres.NewSubmissions(ingressPool),
		systemClock{},
		domain.ConfiguredSensors(),
		domain.DefaultValidationPolicy(),
	)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.New(health, submitter, cfg.SubmitTimeout, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The relay runs in-process for now. It is a separate concern and will want
	// its own deployable eventually — but one process is one thing to start on
	// a reviewer's laptop, and the definition of done is about that laptop.
	// Splitting it is a packaging change, not a code change.
	var wg sync.WaitGroup
	if closeRelay, relayErr := startRelay(ctx, cfg, backgroundPool, log, &wg); relayErr != nil {
		log.Warn("outbox relay not started; events will accumulate unpublished",
			slog.Any("error", relayErr))
	} else {
		defer closeRelay()
	}

	serveErr := httpapi.Serve(ctx, server, cfg.ShutdownTimeout, log)
	wg.Wait()
	return serveErr
}

// openPool builds one pool with its own size and its own application_name.
//
// The name is not decoration. It is what makes the bulkhead visible in
// pg_stat_activity during an incident — "which half is holding the
// connections" is otherwise a question nobody can answer — and it is what the
// integration test asserts on, because two pools that are separate in the code
// and identical on the wire would be a bulkhead in name only.
func openPool(ctx context.Context, dsn string, maxConns int32, name string) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// Set, not appended to a DSN string: a DSN that already carries
	// pool_max_conns would otherwise win silently, and the bulkhead would be
	// whatever the environment happened to say.
	poolCfg.MaxConns = maxConns
	poolCfg.ConnConfig.RuntimeParams["application_name"] = name
	return pgxpool.NewWithConfig(ctx, poolCfg)
}

// startRelay connects to NATS and drains the outbox in the background.
//
// A failure to reach the broker is a WARNING, not a fatal error. The outbox
// exists precisely so that acceptance survives a broker outage: requests are
// still persisted and drain when the relay reconnects. Refusing to start would
// turn a recoverable delay into refused customer traffic and discard the whole
// point of the pattern.
func startRelay(
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

	relay := outbox.New(pool, outbox.NewNATSPublisher(js), outbox.DefaultConfig(),
		log.With(slog.String("component", "outbox-relay")))

	wg.Add(1)
	go func() {
		defer wg.Done()
		if runErr := relay.Run(ctx, 0); runErr != nil {
			log.Error("outbox relay stopped", slog.Any("error", runErr))
		}
	}()

	log.Info("outbox relay started", slog.String("nats", cfg.NATSURL))
	return conn.Close, nil
}

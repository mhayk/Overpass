package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, for goose
)

// Containers are started ONCE for the whole package, not per test.
//
// Per-test isolation is correct in principle and takes minutes rather than
// seconds, and a suite nobody waits for is a suite nobody runs. Each test
// instead isolates itself by id — every row it writes carries a UUID it
// generated — so tests can share a database without sharing state.

type stack struct {
	pool     *pgxpool.Pool
	dsn      string
	natsConn *nats.Conn
	js       nats.JetStreamContext
	natsURL  string

	taskingAPIBin  string
	planGatewayBin string
}

var (
	env    *stack
	binDir string
)

func TestMain(m *testing.M) {
	if os.Getenv("OVERPASS_SKIP_INTEGRATION") != "" {
		fmt.Fprintln(os.Stderr, "OVERPASS_SKIP_INTEGRATION set; skipping")
		os.Exit(0)
	}

	ctx := context.Background()
	built, teardown, err := boot(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration stack failed to start: %v\n", err)
		if teardown != nil {
			teardown()
		}
		os.Exit(1)
	}
	env = built

	code := m.Run()
	teardown()
	os.Exit(code)
}

func boot(ctx context.Context) (*stack, func(), error) {
	var cleanups []func()
	teardown := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	// postgis/postgis, not plain postgres. The read models store footprints as
	// geography and the migrations create GiST indexes over them; a plain
	// postgres image fails at migration time with a message about an unknown
	// type, which is a confusing way to learn the image is wrong.
	pg, err := tcpostgres.Run(ctx, "postgis/postgis:16-3.4",
		tcpostgres.WithDatabase("overpass"),
		tcpostgres.WithUsername("overpass"),
		tcpostgres.WithPassword("overpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		return nil, teardown, fmt.Errorf("starting postgres: %w", err)
	}
	// A fresh context on purpose: ctx is cancelled by the time teardown runs,
	// and Terminate with a cancelled context leaves the container behind.
	cleanups = append(cleanups, func() { //nolint:contextcheck // see above
		_ = pg.Terminate(context.Background()) //nolint:errcheck // teardown
	})

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, teardown, fmt.Errorf("postgres dsn: %w", err)
	}
	if prepErr := prepareDatabase(dsn); prepErr != nil {
		return nil, teardown, prepErr
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, teardown, fmt.Errorf("pool: %w", err)
	}
	cleanups = append(cleanups, pool.Close)

	// No JetStream flag here: the testcontainers nats module already starts the
	// server with `-DV -js`. Passing tcnats.WithArgument("jetstream", "") looks
	// right and is not — it renders as `--jetstream ""` and the server exits
	// with "unrecognized command". Verified by running it.
	//
	// The default is worth knowing because JetStream is NOT on by default in
	// the bare image, which is what broke the CI workflow in #109.
	nc, err := tcnats.Run(ctx, "nats:2.10-alpine",
		testcontainers.WithWaitStrategy(
			wait.ForLog("Server is ready").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, teardown, fmt.Errorf("starting nats: %w", err)
	}
	cleanups = append(cleanups, func() { //nolint:contextcheck // as above
		_ = nc.Terminate(context.Background()) //nolint:errcheck // teardown
	})

	natsURL, err := nc.ConnectionString(ctx)
	if err != nil {
		return nil, teardown, fmt.Errorf("nats url: %w", err)
	}
	conn, err := nats.Connect(natsURL, nats.Timeout(10*time.Second))
	if err != nil {
		return nil, teardown, fmt.Errorf("nats connect: %w", err)
	}
	cleanups = append(cleanups, conn.Close)

	js, err := conn.JetStream()
	if err != nil {
		return nil, teardown, fmt.Errorf("jetstream: %w", err)
	}
	if topologyErr := applyTopology(js); topologyErr != nil {
		return nil, teardown, topologyErr
	}

	built := &stack{
		pool: pool, dsn: dsn,
		natsConn: conn, js: js, natsURL: natsURL,
	}

	// Compiled once for the package. Building per test would dominate the
	// runtime and the five-minute CI budget in #30 is not generous.
	root, err := repoRoot()
	if err != nil {
		return nil, teardown, err
	}
	binDir, err = os.MkdirTemp("", "overpass-bin-")
	if err != nil {
		return nil, teardown, fmt.Errorf("temp dir: %w", err)
	}
	cleanups = append(cleanups, func() { _ = os.RemoveAll(binDir) }) //nolint:errcheck // teardown

	if built.taskingAPIBin, err = build(root, "tasking-api", "tasking-api"); err != nil {
		return nil, teardown, err
	}
	if built.planGatewayBin, err = build(root, "plan-gateway", "plan-gateway"); err != nil {
		return nil, teardown, err
	}

	return built, teardown, nil
}

// prepareDatabase runs the same two sources the compose stack and CI use.
//
// Extensions first, then goose. Not a hand-written schema for tests: a test
// schema that drifts from db/migrations tests a database nobody deploys.
func prepareDatabase(dsn string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}

	extensions, err := os.ReadFile(filepath.Join(root, "deploy", "postgres", "init", "01-extensions.sql"))
	if err != nil {
		return fmt.Errorf("reading extensions: %w", err)
	}

	db, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		return fmt.Errorf("opening for migrations: %w", err)
	}
	defer db.Close() //nolint:errcheck // teardown

	if _, err := db.Exec(string(extensions)); err != nil {
		return fmt.Errorf("creating extensions: %w", err)
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.Up(db, filepath.Join(root, "db", "migrations")); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	return nil
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// tests/integration -> repo root
	return filepath.Abs(filepath.Join(wd, "..", ".."))
}

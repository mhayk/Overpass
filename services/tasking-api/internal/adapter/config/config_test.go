package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/config"
)

func TestRefusesToStartWithoutADatabaseURL(t *testing.T) {
	// No default, deliberately. A wrong guess that happens to connect to a
	// developer's local Postgres is worse than a refusal to start.
	t.Setenv("DATABASE_URL", "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("started with no DATABASE_URL")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("the error does not name the missing variable: %v", err)
	}
}

func TestReportsEveryProblemAtOnce(t *testing.T) {
	// One restart per misconfiguration is a bad way to spend an afternoon.
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LOG_LEVEL", "verbose")
	t.Setenv("SHUTDOWN_TIMEOUT", "soon")

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"DATABASE_URL", "LOG_LEVEL", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s is missing from the combined error:\n%v", want, err)
		}
	}
}

func TestLoadsWithDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/x")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.LogLevel != "info" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Fatalf("shutdown default is %v", cfg.ShutdownTimeout)
	}
}

func TestAcceptsBareSecondsAsWellAsADuration(t *testing.T) {
	// Operators reach for "30" far more often than "30s", and rejecting it
	// teaches them nothing useful.
	t.Setenv("DATABASE_URL", "postgres://localhost/x")
	t.Setenv("SHUTDOWN_TIMEOUT", "30")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Fatalf("got %v, want 30s", cfg.ShutdownTimeout)
	}
}

func TestRejectsANonPositiveTimeout(t *testing.T) {
	// A zero shutdown grace does not drain; it kills in-flight requests and
	// looks like an intermittent client error.
	t.Setenv("DATABASE_URL", "postgres://localhost/x")
	t.Setenv("SHUTDOWN_TIMEOUT", "0")

	if _, err := config.Load(); err == nil {
		t.Fatal("accepted a zero shutdown timeout")
	}
}

func TestRejectsAnUnknownLogLevel(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/x")
	t.Setenv("LOG_LEVEL", "chatty")

	if _, err := config.Load(); err == nil {
		t.Fatal("accepted an unknown log level")
	}
}

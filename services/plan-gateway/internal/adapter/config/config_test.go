package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/config"
)

// valid sets the one variable with no default, so each test can vary one thing.
func valid(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://overpass:overpass@localhost:5432/overpass")
}

func TestDefaultsAreUsable(t *testing.T) {
	valid(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTPAddr != ":8083" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	// Must match deploy/nats/init.sh, which creates gateway-projector-tasking
	// and friends. A prefix that drifts from the topology binds to nothing and
	// the projector fails at start with a consumer-not-found.
	if cfg.DurablePrefix != "gateway-projector" {
		t.Errorf("DurablePrefix = %q; deploy/nats/init.sh creates gateway-projector-*", cfg.DurablePrefix)
	}
	if cfg.FetchBatch != 32 || cfg.FetchWait != 5*time.Second {
		t.Errorf("fetch defaults = %d/%s", cfg.FetchBatch, cfg.FetchWait)
	}
}

// TestDatabaseURLHasNoDefault is the fail-fast rule.
//
// A wrong guess that happens to connect to a developer's local Postgres is
// worse than a refusal to start.
func TestDatabaseURLHasNoDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	_, err := config.Load()
	if err == nil {
		t.Fatal("loaded with no DATABASE_URL")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("the error does not name the missing variable: %v", err)
	}
}

// TestEveryProblemIsReportedAtOnce means a misconfigured deployment takes one
// restart to diagnose rather than four.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LOG_LEVEL", "shout")
	t.Setenv("FETCH_BATCH", "0")

	_, err := config.Load()
	if err == nil {
		t.Fatal("loaded with three broken settings")
	}
	for _, want := range []string{"DATABASE_URL", "LOG_LEVEL", "FETCH_BATCH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s missing from the report:\n%v", want, err)
		}
	}
}

func TestDurationsAcceptBareSecondsAndGoSyntax(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"30", 30 * time.Second},
		{"30s", 30 * time.Second},
		{"1m30s", 90 * time.Second},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			valid(t)
			t.Setenv("SHUTDOWN_TIMEOUT", tc.raw)
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.ShutdownTimeout != tc.want {
				t.Errorf("ShutdownTimeout = %s, want %s", cfg.ShutdownTimeout, tc.want)
			}
		})
	}
}

// TestNonsensicalValuesAreRejected covers the ones that would otherwise turn
// into a silent misbehaviour: a zero timeout means shut down instantly, and a
// zero batch means fetch nothing forever.
func TestNonsensicalValuesAreRejected(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"SHUTDOWN_TIMEOUT", "0"},
		{"SHUTDOWN_TIMEOUT", "-5"},
		{"SHUTDOWN_TIMEOUT", "soon"},
		{"FETCH_WAIT", "0"},
		{"FETCH_WAIT", "eventually"},
		{"FETCH_BATCH", "-1"},
		{"FETCH_BATCH", "lots"},
		{"LOG_LEVEL", "verbose"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			valid(t)
			t.Setenv(tc.key, tc.value)
			if _, err := config.Load(); err == nil {
				t.Fatalf("accepted %s=%s", tc.key, tc.value)
			}
		})
	}
}

// TestAnEmptyOverrideFallsBackRatherThanFailing matters because compose passes
// an empty string for every variable that is unset in the environment file.
func TestAnEmptyOverrideFallsBackRatherThanFailing(t *testing.T) {
	valid(t)
	t.Setenv("SHUTDOWN_TIMEOUT", "")
	t.Setenv("PLAN_GATEWAY_ADDR", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("an empty override was treated as invalid: %v", err)
	}
	if cfg.ShutdownTimeout != 15*time.Second || cfg.HTTPAddr != ":8083" {
		t.Errorf("empty overrides did not fall back: %s / %q", cfg.ShutdownTimeout, cfg.HTTPAddr)
	}
}

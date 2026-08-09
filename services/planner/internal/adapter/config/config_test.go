package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/adapter/config"
)

// A config that starts on nonsense is a service that fails later, somewhere
// less obvious. These tests are about refusing at startup.

func TestDefaultsApplyWhenOnlyTheRequiredValueIsSet(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://overpass@localhost/overpass")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	if cfg.HTTPAddr != ":8084" {
		t.Errorf("HTTPAddr = %q, want :8084 — after tasking-api and plan-gateway", cfg.HTTPAddr)
	}
	if cfg.NATSURL != "nats://localhost:4222" {
		t.Errorf("NATSURL = %q", cfg.NATSURL)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.FetchBatch != 32 {
		t.Errorf("FetchBatch = %d", cfg.FetchBatch)
	}
	// The default ADR-0007 accepted on the benchmark's numbers. Changing it is
	// a decision that needs new evidence, not a config tweak.
	if cfg.AllocationPolicy != "GREEDY_BY_VALUE_DENSITY" {
		t.Errorf("AllocationPolicy = %q, want the ADR-0007 default", cfg.AllocationPolicy)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %s", cfg.ShutdownTimeout)
	}
}

// DATABASE_URL has no default ON PURPOSE. A default would let the planner start
// against a database nobody meant to point it at, which for the one
// strongly-consistent component in the system is the worst possible default.
func TestDatabaseURLIsRequired(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("loaded with no DATABASE_URL")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error does not name the missing variable: %v", err)
	}
}

// Operators reach for "30" far more often than "30s", and the other two
// services already accept both. A planner that disagreed would be an incident
// nobody debugs quickly.
func TestDurationsAcceptBareSecondsAndGoDurations(t *testing.T) {
	tests := []struct {
		raw  string
		want time.Duration
	}{
		{"30", 30 * time.Second},
		{"30s", 30 * time.Second},
		{"2m", 2 * time.Minute},
		{"1500ms", 1500 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://overpass@localhost/overpass")
			t.Setenv("SHUTDOWN_TIMEOUT", tt.raw)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			if cfg.ShutdownTimeout != tt.want {
				t.Errorf("SHUTDOWN_TIMEOUT %q = %s, want %s", tt.raw, cfg.ShutdownTimeout, tt.want)
			}
		})
	}
}

func TestNonsenseIsRefused(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		names string
	}{
		{"unknown log level", "LOG_LEVEL", "verbose", "LOG_LEVEL"},
		{"negative fetch batch", "FETCH_BATCH", "-1", "FETCH_BATCH"},
		{"zero fetch batch", "FETCH_BATCH", "0", "FETCH_BATCH"},
		{"fetch batch is not a number", "FETCH_BATCH", "many", "FETCH_BATCH"},
		{"zero timeout", "SHUTDOWN_TIMEOUT", "0", "SHUTDOWN_TIMEOUT"},
		{"negative timeout", "SHUTDOWN_TIMEOUT", "-5", "SHUTDOWN_TIMEOUT"},
		{"timeout is not a duration", "FETCH_WAIT", "soon", "FETCH_WAIT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://overpass@localhost/overpass")
			t.Setenv(tt.key, tt.value)

			_, err := config.Load()
			if err == nil {
				t.Fatalf("accepted %s=%q", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.names) {
				t.Errorf("error %q does not name %s", err, tt.names)
			}
		})
	}
}

// Every problem is reported, not just the first. An operator fixing a
// misconfigured deployment one restart at a time is an operator wasting an
// afternoon.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LOG_LEVEL", "verbose")
	t.Setenv("FETCH_BATCH", "-3")

	_, err := config.Load()
	if err == nil {
		t.Fatal("accepted three problems at once")
	}
	for _, want := range []string{"DATABASE_URL", "LOG_LEVEL", "FETCH_BATCH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error stops short of %s: %v", want, err)
		}
	}
}

func TestEveryValidLogLevelIsAccepted(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://overpass@localhost/overpass")
			t.Setenv("LOG_LEVEL", level)
			if _, err := config.Load(); err != nil {
				t.Errorf("refused %s: %v", level, err)
			}
		})
	}
}

func TestFairnessDefaultsAreLoaded(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://overpass@localhost/overpass")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if got := cfg.Fairness.TierMultipliers["CIVIL_PROTECTION"]; got != 3.0 {
		t.Errorf("CIVIL_PROTECTION multiplier = %v, want 3.0", got)
	}
	if cfg.Fairness.MaxAgeingFactor != 3.0 {
		t.Errorf("max ageing factor = %v", cfg.Fairness.MaxAgeingFactor)
	}
	if err := cfg.Fairness.Validate(); err != nil {
		t.Errorf("the loaded default fairness is invalid: %v", err)
	}
}

func TestTierMultipliersCanBeOverridden(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://overpass@localhost/overpass")
	t.Setenv("TIER_MULTIPLIERS", "GOVERNMENT=10,CIVIL_PROTECTION=8,COMMERCIAL=2,BEST_EFFORT=1")
	t.Setenv("MAX_AGEING_FACTOR", "4")
	t.Setenv("AGEING_TIME_CONSTANT", "2h")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if got := cfg.Fairness.TierMultipliers["GOVERNMENT"]; got != 10 {
		t.Errorf("GOVERNMENT multiplier = %v, want 10", got)
	}
	if cfg.Fairness.MaxAgeingFactor != 4 {
		t.Errorf("max ageing factor = %v, want 4", cfg.Fairness.MaxAgeingFactor)
	}
	if cfg.Fairness.AgeingTimeConstant != 2*time.Hour {
		t.Errorf("ageing time constant = %v, want 2h", cfg.Fairness.AgeingTimeConstant)
	}
}

// A partial override is REPLACED wholesale, not merged, so Validate catches the
// missing tier at startup. Merging would let it inherit a multiplier from the
// policy it was written to replace — a fairness policy nobody chose.
func TestAPartialTierOverrideIsRefused(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://overpass@localhost/overpass")
	t.Setenv("TIER_MULTIPLIERS", "GOVERNMENT=10,COMMERCIAL=2")

	_, err := config.Load()
	if err == nil {
		t.Fatal("a tier list missing two tiers was accepted; those requests would be valued at zero and lose in silence")
	}
	if !strings.Contains(err.Error(), "CIVIL_PROTECTION") && !strings.Contains(err.Error(), "BEST_EFFORT") {
		t.Errorf("the error does not name a missing tier: %v", err)
	}
}

func TestMalformedFairnessIsRefused(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"a multiplier entry with no equals", "TIER_MULTIPLIERS", "GOVERNMENT:4"},
		{"a non-numeric multiplier", "TIER_MULTIPLIERS", "GOVERNMENT=high,CIVIL_PROTECTION=3,COMMERCIAL=1,BEST_EFFORT=0.5"},
		{"a non-numeric ageing cap", "MAX_AGEING_FACTOR", "lots"},
		// The bound: a cap at or above the tier spread lets an aged bottom-tier
		// request outrank a fresh top-tier one.
		{"an ageing cap that can invert the tiers", "MAX_AGEING_FACTOR", "8"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://overpass@localhost/overpass")
			t.Setenv(tt.key, tt.value)
			if _, err := config.Load(); err == nil {
				t.Fatalf("accepted %s=%q", tt.key, tt.value)
			}
		})
	}
}

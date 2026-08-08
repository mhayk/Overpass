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

// TestTheSamplingRatioIsValidated covers a knob whose wrong values are silent.
//
// Out of range is rejected rather than clamped: "2.0" means the author believed
// something about this setting that is not true, and quietly treating it as 1.0
// leaves the belief in place until it matters.
func TestTheSamplingRatioIsValidated(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  float64
		valid bool
	}{
		{"", 1.0, true}, // compose passes an empty string for anything unset
		{"1", 1.0, true},
		{"0", 0.0, true}, // a legitimate way to turn sampling off
		{"0.05", 0.05, true},
		{"2", 0, false},
		{"-0.1", 0, false},
		{"most", 0, false},
	} {
		t.Run("TRACE_SAMPLE_RATIO="+tc.raw, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://localhost/x")
			t.Setenv("TRACE_SAMPLE_RATIO", tc.raw)

			cfg, err := config.Load()
			if tc.valid {
				if err != nil {
					t.Fatalf("rejected a valid ratio: %v", err)
				}
				if cfg.TraceSampleRatio != tc.want {
					t.Errorf("TraceSampleRatio = %v, want %v", cfg.TraceSampleRatio, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %q", tc.raw)
			}
		})
	}
}

// TestTheCollectorEndpointHasADefault, unlike DATABASE_URL.
//
// Guessing wrong here loses telemetry; guessing wrong there corrupts data. An
// empty value disables tracing outright, which is what a unit test wants and
// what a deployment with no collector needs.
func TestTheCollectorEndpointHasADefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/x")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.OTLPEndpoint != "otel-collector:4317" {
		t.Errorf("OTLPEndpoint = %q", cfg.OTLPEndpoint)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("an empty endpoint was treated as invalid: %v", err)
	}
	if cfg.OTLPEndpoint != "otel-collector:4317" {
		t.Errorf("an empty override did not fall back: %q", cfg.OTLPEndpoint)
	}
}

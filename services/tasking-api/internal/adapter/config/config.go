// Package config reads the environment and refuses to start on anything
// missing or nonsensical.
//
// Fail-fast, and loudly. A service that starts with a blank database URL and
// discovers it on the first request has turned a configuration error into an
// outage, and the log line that explains it is buried under the traffic that
// followed. Every error found is reported together, so a misconfigured
// deployment takes one restart to diagnose rather than four.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is everything this service reads from its environment.
type Config struct {
	Environment      string
	Version          string
	HTTPAddr         string
	DatabaseURL      string
	NATSURL          string
	LogLevel         string
	ShutdownTimeout  time.Duration
	ReadinessTimeout time.Duration

	OTLPEndpoint     string
	TraceSampleRatio float64
}

// Load reads and validates the environment.
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		Environment: env("OVERPASS_ENV", "development"),
		Version:     env("OVERPASS_VERSION", "dev"),
		HTTPAddr:    env("TASKING_API_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		NATSURL:     env("NATS_URL", "nats://localhost:4222"),
		LogLevel:    env("LOG_LEVEL", "info"),
		// Empty disables tracing entirely, which is what a unit test wants and
		// what a deployment without a collector needs. There IS a default,
		// unlike DATABASE_URL, because guessing wrong here loses telemetry
		// rather than corrupting data.
		OTLPEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "otel-collector:4317"),
	}

	// No default. A database URL is not something to guess at: a wrong guess
	// that happens to connect to a developer's local Postgres is worse than a
	// refusal to start.
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		problems = append(problems, "DATABASE_URL is required and has no default")
	}

	var err error
	if cfg.ShutdownTimeout, err = duration("SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.ReadinessTimeout, err = duration("READINESS_TIMEOUT", 2*time.Second); err != nil {
		problems = append(problems, err.Error())
	}

	// Everything, by default. This is a demo stack where a missing trace is a
	// broken demo, and the traffic is a handful of requests. M3 lowers it when
	// there is load worth sampling.
	if cfg.TraceSampleRatio, err = ratio("TRACE_SAMPLE_RATIO", 1.0); err != nil {
		problems = append(problems, err.Error())
	}

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("LOG_LEVEL %q is not one of debug, info, warn, error", cfg.LogLevel))
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// ratio parses a sampling fraction and refuses anything outside [0,1].
//
// Out of range is rejected rather than clamped. "2.0" means the author believed
// something about this knob that is not true, and silently treating it as 1.0
// leaves that belief in place.
func ratio(key string, fallback float64) (float64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is not a number: %q", key, raw)
	}
	if v < 0 || v > 1 {
		return 0, fmt.Errorf("%s must be between 0 and 1, got %q", key, raw)
	}
	return v, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	// Accept a bare number of seconds as well as a Go duration. Operators reach
	// for "30" far more often than "30s", and rejecting it teaches them nothing
	// useful.
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 {
			return 0, fmt.Errorf("%s must be positive, got %q", key, raw)
		}
		return time.Duration(secs) * time.Second, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a duration: %q", key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", key, raw)
	}
	return d, nil
}

// Package config reads the environment and refuses to start on anything
// missing or nonsensical.
//
// Same shape and same fail-fast rule as tasking-api's and plan-gateway's,
// deliberately. Two services that disagree about how to read DATABASE_URL is a
// class of incident nobody debugs quickly.
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
	Environment     string
	Version         string
	HTTPAddr        string
	DatabaseURL     string
	NATSURL         string
	LogLevel        string
	ShutdownTimeout time.Duration
	FetchBatch      int
	FetchWait       time.Duration
	IdleWait        time.Duration

	// The ADR-0014 firing rule. Values, not constants in an ADR: naming
	// specific numbers before M3 has measured anything would be the
	// prediction-dressed-as-decision that ADR-0007 avoids. The RELATION between
	// them is the invariant, and domain.TriggerPolicy.Validate enforces it.
	QuietPeriod      time.Duration
	StalenessCeiling time.Duration

	// BucketDuration must divide 24h evenly or "UTC-aligned" is a lie — see
	// domain.ValidBucketDuration. Three hours matches ADR-0016's ephemeris
	// buckets, so a plan and the track it is flown against tile identically.
	BucketDuration time.Duration
	HorizonAhead   time.Duration
	SweepInterval  time.Duration
	SweepLimit     int

	// AllocationPolicy is which strategy a round announces. ADR-0007 defers the
	// default to the M2-13 benchmark, so it is configuration and the round
	// records it — which is what lets a committed plan be attributed to a
	// strategy after the fact.
	AllocationPolicy string
}

// Load reads and validates the environment.
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		Environment: env("OVERPASS_ENV", "development"),
		Version:     env("OVERPASS_VERSION", "dev"),
		// 8084, after tasking-api and plan-gateway. Probes only — see httpapi.
		HTTPAddr:    env("PLANNER_ADDR", ":8084"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		NATSURL:     env("NATS_URL", "nats://localhost:4222"),
		LogLevel:    env("LOG_LEVEL", "info"),
		// GREEDY_BY_BID is the naive baseline and is deliberately NOT a claim
		// about what the default should be. ADR-0007 names the default on
		// benchmark evidence; until then the planner runs the policy whose
		// behaviour is easiest to reason about when something looks wrong.
		AllocationPolicy: env("ALLOCATION_POLICY", "GREEDY_BY_BID"),
	}

	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		problems = append(problems, "DATABASE_URL is required and has no default")
	}

	var err error
	if cfg.ShutdownTimeout, err = duration("SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.FetchWait, err = duration("FETCH_WAIT", 5*time.Second); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.IdleWait, err = duration("IDLE_WAIT", time.Second); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.FetchBatch, err = positiveInt("FETCH_BATCH", 32); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.QuietPeriod, err = duration("ROUND_QUIET_PERIOD", 5*time.Second); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.StalenessCeiling, err = duration("ROUND_STALENESS_CEILING", 60*time.Second); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.BucketDuration, err = duration("BUCKET_DURATION", 3*time.Hour); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.HorizonAhead, err = duration("PLANNING_HORIZON", 24*time.Hour); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.SweepInterval, err = duration("SWEEP_INTERVAL", time.Second); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.SweepLimit, err = positiveInt("SWEEP_LIMIT", 64); err != nil {
		problems = append(problems, err.Error())
	}

	switch cfg.AllocationPolicy {
	case "GREEDY_BY_BID", "GREEDY_BY_VALUE_DENSITY", "VICKREY_SEALED_BID", "EXACT_DP":
	default:
		// The four the contract enumerates. A policy name the schema rejects
		// would produce an invalid round event at publish time, which is much
		// further from the mistake than startup is.
		problems = append(problems, fmt.Sprintf(
			"ALLOCATION_POLICY %q is not one of GREEDY_BY_BID, GREEDY_BY_VALUE_DENSITY, VICKREY_SEALED_BID, EXACT_DP",
			cfg.AllocationPolicy))
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

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func positiveInt(key string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a number: %q", key, raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", key, raw)
	}
	return n, nil
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	// Accept a bare number of seconds as well as a Go duration, matching the
	// other two services. Operators reach for "30" far more often than "30s".
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

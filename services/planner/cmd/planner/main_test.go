package main

import (
	"context"
	"strings"
	"testing"
)

// The composition root, exercised. Wiring that only ever runs in production is
// wiring whose first failure is in production, and the failures it hides are
// the dull ones: a probe never mounted, a projector never started, a shutdown
// that never returns.

func TestRunRefusesToStartOnBadConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	err := run(t.Context())
	if err == nil {
		t.Fatal("started with no DATABASE_URL")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("the error does not name the problem: %v", err)
	}
}

func TestRunRejectsAnUnparseableDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "not-a-url://%%%")

	if err := run(t.Context()); err == nil {
		t.Fatal("started against an unparseable DATABASE_URL")
	}
}

// A planner that cannot reach the broker must still come up.
//
// It has nothing to serve, but it also has nothing to corrupt — and refusing to
// start would take the probe endpoint down with it, replacing a service that
// reports itself unready with one that is simply absent. Unready and observable
// beats gone.
//
// The context is cancelled immediately, so this asserts the startup path and
// the shutdown path in one go: run must return cleanly rather than block on a
// broker it never reached.
func TestRunStartsWithoutTheBroker(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://overpass:overpass@127.0.0.1:1/overpass")
	// Port 1 is reserved and nothing listens there, so the connection fails
	// fast rather than hanging on a firewall drop.
	t.Setenv("NATS_URL", "nats://127.0.0.1:1")
	t.Setenv("PLANNER_ADDR", "127.0.0.1:0")
	t.Setenv("SHUTDOWN_TIMEOUT", "1s")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := run(ctx); err != nil {
		t.Fatalf("run did not survive an unreachable broker: %v", err)
	}
}

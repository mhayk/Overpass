package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The composition root, exercised. Wiring that only ever runs in production is
// wiring whose first failure is in production, and the failures it hides are
// the dull ones: a route never mounted, a projector never started, a shutdown
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
		t.Fatal("accepted a malformed DATABASE_URL")
	}
}

// TestTheGatewayServesWithoutABroker is the deliberate behaviour, not an
// accident of the test setup.
//
// NATS is unreachable here. The gateway must still come up and serve, because
// a read model that refuses to answer when the broker is down replaces stale
// answers — which every response labels with its own staleness — with no
// answers at all.
func TestTheGatewayServesWithoutABroker(t *testing.T) {
	// Neither the pool nor the broker is ever contacted successfully: pgxpool
	// connects lazily and nothing here issues a query, and port 1 refuses.
	t.Setenv("DATABASE_URL", "postgres://overpass:overpass@127.0.0.1:1/overpass")
	t.Setenv("NATS_URL", "nats://127.0.0.1:1")
	t.Setenv("PLAN_GATEWAY_ADDR", "127.0.0.1:0")
	t.Setenv("SHUTDOWN_TIMEOUT", "2")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

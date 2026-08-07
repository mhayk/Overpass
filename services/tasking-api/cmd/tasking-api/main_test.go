package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// These exercise the composition root itself. Wiring that is only ever run in
// production is wiring whose first failure is in production — and the failures
// it hides are the dull ones: a probe never registered, a handler never
// mounted, a shutdown that never returns.

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
	// pgxpool.New parses eagerly and connects lazily, so a malformed URL is
	// caught at startup rather than on the first request. Worth pinning: the
	// opposite behaviour would turn a typo into a runtime outage.
	t.Setenv("DATABASE_URL", "not-a-url://%%%")

	if err := run(t.Context()); err == nil {
		t.Fatal("accepted a malformed DATABASE_URL")
	}
}

func TestRunWiresEverythingAndShutsDownCleanly(t *testing.T) {
	// The pool is never contacted — pgxpool connects lazily and nothing here
	// issues a query — so this runs the whole composition without a database.
	t.Setenv("DATABASE_URL", "postgres://overpass:overpass@127.0.0.1:1/overpass")
	t.Setenv("TASKING_API_ADDR", "127.0.0.1:0")
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

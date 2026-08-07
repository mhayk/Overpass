// Package port declares the interfaces the application layer depends on.
//
// Declared HERE, next to the code that uses them, rather than next to the
// implementations. That direction is the whole point of the layout: app depends
// on an interface it owns, and adapter satisfies it, so the dependency arrow
// points inward and Postgres can be swapped for a fake in a test without the
// application layer knowing.
package port

import (
	"context"
	"time"
)

// DependencyProbe reports whether one external dependency is usable.
type DependencyProbe interface {
	// Name identifies the dependency in the readiness response.
	Name() string
	// Check returns nil when the dependency is usable. The context carries the
	// timeout — a readiness probe that can hang is a readiness probe that turns
	// a slow dependency into an unresponsive service.
	Check(ctx context.Context) error
}

// Clock exists so that anything time-dependent can be tested without sleeping.
type Clock interface {
	Now() time.Time
}

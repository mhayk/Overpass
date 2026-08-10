// Package telemetry binds this service's instrumentation scope to the shared
// providers in lib/go/telemetry.
//
// The provider setup itself used to live here. It moved to the library when
// #53 needed a MeterProvider in three services rather than one — the reasoning,
// and the resource.NewSchemaless defect the move preserves, are documented
// there.
//
// What stays here is the one thing that is genuinely per-service: the scope
// name spans and metrics are attributed to. Tracer() and Meter() take no
// argument on purpose, so no call site can attribute this service's work to
// another service's scope by passing the wrong string.
package telemetry

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	lib "github.com/mhayk/overpass/lib/go/telemetry"
)

// ScopeName identifies spans and metrics this service creates by hand.
const ScopeName = "github.com/mhayk/overpass/services/tasking-api"

// Config is what telemetry needs from the environment.
//
// An alias rather than a wrapper struct: callers build it as a literal, and a
// distinct type would mean a conversion at every call site that adds nothing.
type Config = lib.Config

// Setup installs the global tracer and meter providers and returns their
// shutdown. See lib/go/telemetry for why an unreachable collector is not a
// startup failure.
var Setup = lib.Setup

// Inject writes the current span's context into a header map.
var Inject = lib.Inject

// Extract reads a header map back into a context.
var Extract = lib.Extract

// IDs returns the trace and span ids for logging, or empty strings.
var IDs = lib.IDs

// Tracer returns this service's tracer.
func Tracer() trace.Tracer { return lib.Tracer(ScopeName) }

// Meter returns this service's meter.
func Meter() metric.Meter { return lib.Meter(ScopeName) }

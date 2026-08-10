// lib/go/consume: effectively-once processing on top of at-least-once delivery.
//
// Its own module for the same reason gen/go is one: it is shared by services
// that deploy separately, and it arrives through a replace directive so there
// is no publishing step and no version skew. It depends on pgx only — no NATS
// import, deliberately, so the transport stays in each service's adapter and
// this module stays testable against a database alone.
module github.com/mhayk/overpass/lib/go/consume

go 1.25.0

require (
	github.com/jackc/pgx/v5 v5.10.0
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/metric v1.45.0
	go.opentelemetry.io/otel/sdk/metric v1.45.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/sdk v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.35.0 // indirect
)

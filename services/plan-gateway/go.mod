// plan-gateway: read models, CZML and GeoJSON serialisation, SSE.
//
// Its own module for the same reason tasking-api has one: the services deploy
// separately and a shared module makes every service's dependency graph the
// union of all of them. Generated contract types come in through a replace
// directive, so there is no publishing step and no version skew.
module github.com/mhayk/overpass/services/plan-gateway

go 1.25.0

replace github.com/mhayk/overpass/gen/go => ../../gen/go

require (
	github.com/go-chi/chi/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/mhayk/overpass/gen/go v0.0.0-20260808081418-1c8e3ce25b01
	github.com/nats-io/nats.go v1.52.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.35.0 // indirect
)

# tasking-api — the Go service pattern (#166). planner and plan-gateway carry
# the same file with only the service name changed; a difference between them
# beyond that name is a bug.
#
# Built from the REPO ROOT (`context: .` in docker-compose.yml), not from this
# directory: the module pulls gen/go and lib/go through replace directives two
# levels up, and a build context that cannot see them fails at `go build` with
# a message about a missing module rather than anything naming the real cause.
#
# Multi-stage for the same reason as deploy/migrate/Dockerfile: the shipped
# image is one static binary on Alpine, not a Go toolchain. No layer-caching
# choreography with a separate `go mod download` stage — the replace
# directives point at local directories, so the download needs the sources
# anyway, and the clean-machine budget already prices in one full build.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY gen/go gen/go
COPY lib/go lib/go
COPY services/tasking-api services/tasking-api
WORKDIR /src/services/tasking-api
# Static on purpose: the runtime image has no libc worth linking against, and
# a binary that quietly picked up cgo would die at exec with "not found".
RUN CGO_ENABLED=0 go build -o /out/tasking-api ./cmd/tasking-api

FROM alpine:3.21
# ca-certificates so an outbound TLS call fails on policy, never on a missing
# trust store; busybox wget is already present for the compose healthcheck.
RUN apk add --no-cache ca-certificates
COPY --from=build /out/tasking-api /usr/local/bin/tasking-api
USER nobody
ENTRYPOINT ["tasking-api"]

# Migration runner.
#
# The definition of done is `git clone && docker compose up` on a clean machine.
# That machine has Docker and nothing else — no Go toolchain — so the migration
# tool cannot come from `make tools`. It comes from here, and `docker compose
# up` brings up a database with its schema already applied rather than an empty
# one waiting for a second command nobody ran.
#
# Multi-stage so the shipped image is goose on Alpine rather than a full Go
# toolchain. The build stage is cached after the first run; a genuinely clean
# machine pays for it once.
#
# GOOSE_VERSION is duplicated from the Makefile on purpose rather than shared
# through some mechanism: two places, both explicit, both greppable. A drift
# between them shows up as two different versions in `goose --version`, which is
# a better failure than an indirection nobody can follow.
FROM golang:1.25-alpine AS build

# goose v3.27.3 requires Go >= 1.25.7. On an older toolchain this fails with a
# module resolution error rather than anything that names the real problem.
ARG GOOSE_VERSION=v3.27.3
RUN go install github.com/pressly/goose/v3/cmd/goose@${GOOSE_VERSION}

FROM alpine:3.21
COPY --from=build /go/bin/goose /usr/local/bin/goose
ENTRYPOINT ["goose"]
CMD ["up"]

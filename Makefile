# Overpass — task runner
#
# Every target is self-documenting: a target followed by `## description` on the
# same line appears in `make help`. That keeps this file and its documentation
# from drifting apart, which is the usual failure mode of a README section
# listing commands.
#
# `help` is the default target on purpose. Running bare `make` on an unfamiliar
# repo should tell you what you can do, not start building something.

.DEFAULT_GOAL := help
SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

# Pinned tool versions. Pinned exactly, not with ranges: a generator minor bump
# that reformats output would fail the codegen drift check for no semantic
# reason, and debugging that is a bad afternoon.
OAPI_CODEGEN_VERSION  := v2.4.1
GO_JSONSCHEMA_VERSION := v0.24.1
DATAMODEL_CG_VERSION  := 0.28.5
GOLANGCI_LINT_VERSION := v2.12.2
# goose v3.27.3 requires Go >= 1.25.7 — it will not build on an older toolchain,
# and the failure is a module resolution error rather than anything obvious.
# go.mod and the CI setup-go step are both on 1.25, which resolves to a patch
# above that floor.
GOOSE_VERSION         := v3.27.3

ROOT        := $(shell git rev-parse --show-toplevel 2>/dev/null || pwd)
# Read from docker-compose.yml rather than restated, so promtool and the
# running server can never disagree about rule syntax.
PROMETHEUS_IMAGE := $(shell grep -oE 'prom/prometheus:[^ ]+' $(ROOT)/docker-compose.yml | head -1)
# k6 in a container: --network host so localhost reaches the compose ports, and
# the scripts mounted read-only. No host install to keep in step with CI.
K6 := docker run --rm --network host -v $(ROOT)/loadtest/k6:/scripts:ro \
	-e TASKING_API_URL -e PLAN_GATEWAY_URL -e RATE -e TIME_UNIT -e DURATION \
	grafana/k6:1.4.0

# Read .env the way docker compose does, so the two cannot disagree about where
# Postgres is listening.
#
# This is not convenience. compose publishes "$${POSTGRES_PORT:-5433}:5432" and
# loads .env automatically; make loads nothing. So a developer who set
# POSTGRES_PORT — the knob .env.example documents, usually because 5433 was
# already taken — got a stack on one port and a DATABASE_URL pointing at
# another. `make migrate` then connected to whatever else was on 5433, which on
# the machine where this was found was a different project's Postgres. Only a
# password mismatch stopped goose applying this repo's migrations to it.
#
# CI cannot catch this class of bug: it has no .env, so POSTGRES_PORT is unset,
# compose falls back to 5433, and everything agrees. It is invisible until it
# happens on exactly the machine the 5433 default was chosen to help.
#
# `-include`, not `include`: a fresh clone has no .env and must still build.
# This treats .env as makefile syntax, which is fine for the KEY=VALUE lines
# .env.example specifies and would not be for values carrying spaces or `#`.
#
# One asymmetry this does NOT fix, measured rather than assumed. A value in .env
# arrives as a makefile assignment, and makefile assignments beat the
# environment while losing to the command line:
#
#   make migrate POSTGRES_PORT=6000     uses 6000
#   POSTGRES_PORT=6000 make migrate     uses .env, ignoring you, silently
#
# compose gives the environment precedence over .env, so the two still disagree
# about overrides even though they now agree about defaults. DATABASE_URL is the
# escape hatch that always works, because it is `?=` and never set in .env.
-include $(ROOT)/.env
TOOLS_BIN   := $(ROOT)/.tools/bin
CONTRACTS   := $(ROOT)/contracts
GEN         := $(ROOT)/gen
GO_SERVICES := tasking-api planner plan-gateway
# The shared Go modules. They were absent from test-go and lint-go until #64,
# so consume/ and telemetry/ shipped with tests that `make test` never ran —
# a suite nobody runs is a suite nobody can trust.
GO_LIBS     := consume telemetry httpx
MIGRATIONS  := $(ROOT)/db/migrations

# One migration sequence for the whole database, not one per schema. The
# schemas are namespaces inside a single Postgres and there are foreign keys
# across them — planning.acquisitions references reference.satellites — so the
# ordering is global whether or not it is tracked that way.
#
# DATABASE_URL is overridable so this points at a test database as easily as at
# the compose stack.
#
# The port is derived, never written twice. 5433 is the default for the reason
# docker-compose.yml gives — a developer machine very often already has a local
# Postgres on 5432 — but the default lives in exactly one place per consumer and
# .env overrides all of them together.
POSTGRES_PORT ?= 5433
DATABASE_URL  ?= postgres://overpass:overpass@localhost:$(POSTGRES_PORT)/overpass?sslmode=disable
GOOSE        := $(TOOLS_BIN)/goose -dir $(MIGRATIONS) postgres "$(DATABASE_URL)"

export PATH := $(TOOLS_BIN):$(PATH)

# ---------------------------------------------------------------------------
# Help
# ---------------------------------------------------------------------------

.PHONY: help
help: ## Show this help
	@printf '\nOverpass — satellite tasking and collection planning\n\n'
	@awk 'BEGIN {FS = ":.*?## "} \
		/^# =+$$/ { next } \
		/^## / { printf "\n\033[1m%s\033[0m\n", substr($$0, 4); next } \
		/^[a-zA-Z0-9_-]+:.*?## / { printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)
	@printf '\n'

## Contracts

.PHONY: contracts-validate
contracts-validate: ## Validate every event schema and its examples
	@$(ROOT)/scripts/contracts-validate.sh

.PHONY: contracts-generate
contracts-generate: ## Regenerate Go and Python types from the contracts
	@$(ROOT)/scripts/contracts-generate.sh

.PHONY: contracts-verify
contracts-verify: ## Fail if regeneration would change anything (CI drift gate)
	@$(ROOT)/scripts/contracts-verify.sh

.PHONY: contracts-smoke
contracts-smoke: ## Round-trip fixtures through the generated Go and Python types
	@cd $(ROOT)/gen/go && go test ./contracttest/...
	@$(ROOT)/scripts/contracts-smoke.sh

.PHONY: contracts
contracts: contracts-validate contracts-generate contracts-smoke ## Validate, regenerate, round-trip

## Development

.PHONY: up
up: ## Bring the infrastructure up and wait for it to be genuinely ready
	@$(ROOT)/scripts/stack-up.sh

.PHONY: up-all
up-all: ## Everything `up` brings, plus the application services (#166)
	@COMPOSE_PROFILES=app $(ROOT)/scripts/stack-up.sh

.PHONY: down
down: ## Stop the stack, keep volumes
	docker compose down

.PHONY: clean
clean: ## Stop the stack and destroy volumes and generated tool binaries
	docker compose down -v --remove-orphans
	rm -rf $(ROOT)/.tools

.PHONY: logs
logs: ## Tail logs from all services
	docker compose logs -f --tail=100

## Database

.PHONY: migrate
migrate: $(TOOLS_BIN)/goose ## Apply every pending migration
	@$(GOOSE) up

.PHONY: migrate-down
migrate-down: $(TOOLS_BIN)/goose ## Roll back exactly one migration
	@$(GOOSE) down

.PHONY: migrate-status
migrate-status: $(TOOLS_BIN)/goose ## Show which migrations have been applied
	@$(GOOSE) status

.PHONY: migrate-reset
migrate-reset: $(TOOLS_BIN)/goose ## Roll every migration back, then re-apply — proves down works
	@$(GOOSE) down-to 0
	@$(GOOSE) up

.PHONY: db-test
db-test: ## Assert the planning schema's structural claims against the running database
	@$(ROOT)/scripts/db-invariants.sh

.PHONY: seed
seed: ## Seed the database with the constellation and sample customers
	@$(ROOT)/scripts/seed.sh

.PHONY: demo
demo: ## Submit a scripted set of contested requests and watch the plan change
	@$(ROOT)/scripts/demo.sh

## Quality

.PHONY: lint
lint: lint-go lint-python lint-web ## Run every linter

.PHONY: lint-go
lint-go: $(TOOLS_BIN)/golangci-lint ## Lint the Go services
	@for s in $(GO_SERVICES); do \
		if [ -f services/$$s/go.mod ]; then \
			echo "==> $$s"; (cd services/$$s && golangci-lint run ./...); \
		fi; \
	done
	@for l in $(GO_LIBS); do \
		if [ -f lib/go/$$l/go.mod ]; then \
			echo "==> lib/go/$$l"; (cd lib/go/$$l && golangci-lint run ./...); \
		fi; \
	done

.PHONY: lint-python
lint-python: ## Lint and type-check feasibility-service
	@if [ -f services/feasibility/pyproject.toml ]; then \
		cd services/feasibility && uv run ruff check . && uv run ruff format --check . && uv run mypy .; \
	else echo "skip: services/feasibility not yet initialised"; fi

.PHONY: lint-web
lint-web: ## Lint and type-check the frontend
	@if [ -f web/package.json ]; then cd web && npm run lint && npx tsc --noEmit; \
	else echo "skip: web not yet initialised"; fi

.PHONY: test
test: test-go test-python test-web ## Run every unit test suite

.PHONY: prometheus-config
prometheus-config: ## Validate prometheus.yml the way the server loads it
	@# MOUNTED AT /etc/prometheus, the runtime path, and that is load-bearing.
	@# rule_files names an ABSOLUTE path — /etc/prometheus/rules/*.yml — so
	@# mounted anywhere else the glob matches nothing and this check passes
	@# vacuously. It did exactly that on the first attempt: green against a
	@# rules directory containing a file that takes the server down.
	@#
	@# `check config` FOLLOWS rule_files, which is why it catches what
	@# validating the rules by name cannot: promtool was happy with the rule
	@# file and the test file individually, and Prometheus still refused to
	@# start because the glob swept up both.
	@test -n "$(PROMETHEUS_IMAGE)" || { \
		echo "error: could not read prom/prometheus tag from docker-compose.yml" >&2; exit 1; }
	@docker run --rm -v $(ROOT)/deploy/prometheus:/etc/prometheus:ro \
		--entrypoint promtool $(PROMETHEUS_IMAGE) \
		check config /etc/prometheus/prometheus.yml

.PHONY: alerts-test
alerts-test: prometheus-config ## Unit-test the Prometheus alert rules (needs Docker)
	@# promtool comes from the Prometheus image, pinned to the SAME tag
	@# docker-compose.yml runs. A different promtool could accept rule syntax
	@# the running server rejects, which would make this gate agree with
	@# nothing.
	@# Every rule is tested BOTH ways: firing on the series it exists to catch,
	@# and silent on the series it must not. Without the negative case, `> 0`
	@# and `> 72` both pass.
	@# Refuse loudly on an empty image. An unset variable would expand the
	@# command to `--entrypoint promtool  test rules ...`, and docker would
	@# read `test` as the image name — a confusing failure that looks like a
	@# registry problem rather than a Makefile one.
	@test -n "$(PROMETHEUS_IMAGE)" || { \
		echo "error: could not read prom/prometheus tag from docker-compose.yml" >&2; exit 1; }
	@# The whole deploy/prometheus tree is mounted, not just rules/: the tests
	@# live OUTSIDE the directory prometheus.yml globs for rule files, because
	@# a test file inside it is matched by that glob and Prometheus refuses to
	@# start on it.
	@docker run --rm -v $(ROOT)/deploy/prometheus:/etc/prometheus:ro \
		--entrypoint promtool $(PROMETHEUS_IMAGE) \
		test rules /etc/prometheus/tests/alerts_test.yml

.PHONY: loadtest
loadtest: loadtest-ingress loadtest-pipeline ## Run every k6 scenario as a gate (needs a running stack)

.PHONY: loadtest-ingress
loadtest-ingress: ## Ingress latency curve at 10/100/1000 rps, thresholds as gates
	@$(K6) run /scripts/ingress.js

.PHONY: loadtest-pipeline
loadtest-pipeline: ## Submit-to-visible latency end to end, thresholds as gates
	@# RATE and TIME_UNIT default to 0.2 rps inside the script. The sustainable
	@# rate is below one per second — docs/performance.md has the measurement
	@# and the reason — and above capacity this measures queue depth rather
	@# than pipeline latency.
	@$(K6) run /scripts/pipeline.js

.PHONY: loadtest-breakpoint
loadtest-breakpoint: ## Ramp until something gives, then watch it recover (not a gate)
	@# Deliberately NOT part of `make loadtest`. It has no thresholds — its
	@# output is a number and a failure mode, and a threshold would turn
	@# "find the limit" into "assert the limit has not moved".
	@$(K6) run /scripts/breakpoint.js

.PHONY: dashboards-check
dashboards-check: ## Assert every Grafana panel renders against the running stack
	@# The queries are read from the committed dashboard JSON, never restated.
	@# A check carrying its own copy of the query proves only that the check's
	@# query works — which is how overpass_dlq_depth survived review.
	@$(ROOT)/scripts/dashboards-check.sh

.PHONY: test-integration
test-integration: ## Integration tests against real Postgres and real NATS (needs Docker)
	@# Not part of `make test`. These start containers and build both service
	@# binaries, so they take a minute rather than a second — and a suite that
	@# slow in the inner loop is a suite people start skipping.
	@cd tests/integration && go test -timeout 20m ./...

.PHONY: test-go
test-go: ## Go unit tests, always with -race
	@for s in $(GO_SERVICES); do \
		if [ -f services/$$s/go.mod ]; then \
			echo "==> $$s"; (cd services/$$s && go test -race -coverprofile=coverage.out ./...); \
		fi; \
	done
	@for l in $(GO_LIBS); do \
		if [ -f lib/go/$$l/go.mod ]; then \
			echo "==> lib/go/$$l"; (cd lib/go/$$l && go test -race -coverprofile=coverage.out ./...); \
		fi; \
	done

.PHONY: test-python
test-python: ## Python unit tests
	@if [ -f services/feasibility/pyproject.toml ]; then \
		cd services/feasibility && uv run pytest --cov --cov-report=term-missing; \
	else echo "skip: services/feasibility not yet initialised"; fi

.PHONY: test-web
test-web: ## Frontend unit tests
	@if [ -f web/package.json ]; then cd web && npm test; \
	else echo "skip: web not yet initialised"; fi

.PHONY: coverage
coverage: ## Enforce coverage thresholds (80% overall, 95% planner and geometry)
	@$(ROOT)/scripts/coverage-gate.sh

.PHONY: test-e2e
test-e2e: ## Playwright end-to-end tests against the full stack
	@$(ROOT)/scripts/e2e.sh $(ARGS)

## Performance

.PHONY: frontend-perf
frontend-perf: ## Measure the frontend: frame time, TTI, memory over a session
	@$(ROOT)/scripts/frontend-perf.sh $(ARGS)

.PHONY: benchmark
benchmark: ## Run the allocation policy benchmark and regenerate the report
	cd services/planner && go run ./cmd/benchmark -out ../../docs/policy-benchmark.md
	@echo "wrote docs/policy-benchmark.md"

## Operations

.PHONY: dlq-inspect
dlq-inspect: ## List dead-lettered messages (STREAM=<name>, or none for depth)
	@$(ROOT)/scripts/dlq-inspect.sh $(STREAM)

.PHONY: dlq-replay
dlq-replay: ## Replay a dead-lettered message (STREAM=<name> EVENT_ID=<uuid> | SEQ=<n>)
	@if [ -z "$(STREAM)" ]; then echo "usage: make dlq-replay STREAM=DLQ_TASKING EVENT_ID=<uuid>"; exit 2; fi
	@if [ -n "$(EVENT_ID)" ]; then $(ROOT)/scripts/dlq-replay.sh $(STREAM) --event-id $(EVENT_ID); \
	elif [ -n "$(SEQ)" ]; then $(ROOT)/scripts/dlq-replay.sh $(STREAM) --seq $(SEQ); \
	else echo "give EVENT_ID=<uuid> or, for a dead letter with no id, SEQ=<n>"; exit 2; fi

## Tooling

.PHONY: tools
tools: $(TOOLS_BIN)/golangci-lint $(TOOLS_BIN)/oapi-codegen $(TOOLS_BIN)/go-jsonschema $(TOOLS_BIN)/goose ## Install pinned dev tools into .tools/bin

$(TOOLS_BIN)/goose:
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VERSION)

$(TOOLS_BIN)/golangci-lint:
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(TOOLS_BIN)/oapi-codegen:
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION)

$(TOOLS_BIN)/go-jsonschema:
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install github.com/atombender/go-jsonschema@$(GO_JSONSCHEMA_VERSION)

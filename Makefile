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
GO_JSONSCHEMA_VERSION := v0.17.0
DATAMODEL_CG_VERSION  := 0.28.5
GOLANGCI_LINT_VERSION := v1.62.2

ROOT        := $(shell git rev-parse --show-toplevel 2>/dev/null || pwd)
TOOLS_BIN   := $(ROOT)/.tools/bin
CONTRACTS   := $(ROOT)/contracts
GEN         := $(ROOT)/gen
GO_SERVICES := tasking-api planner plan-gateway

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
up: ## Bring the whole stack up (docker compose up -d, waits for healthy)
	docker compose up -d --wait

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

.PHONY: seed
seed: ## Seed the database with the constellation and sample customers
	@echo "not yet implemented — issue #31 (M1-19)"

.PHONY: demo
demo: ## Submit a scripted set of contested requests and watch the plan change
	@echo "not yet implemented — issue #31 (M1-19)"

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

.PHONY: test-go
test-go: ## Go unit tests, always with -race
	@for s in $(GO_SERVICES); do \
		if [ -f services/$$s/go.mod ]; then \
			echo "==> $$s"; (cd services/$$s && go test -race -coverprofile=coverage.out ./...); \
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

.PHONY: test-integration
test-integration: ## Integration tests against real Postgres and NATS (Testcontainers)
	@echo "not yet implemented — issue #30 (M1-18)"

.PHONY: test-e2e
test-e2e: ## Playwright end-to-end tests against the full stack
	@echo "not yet implemented — issue #58 (M4-08)"

## Performance

.PHONY: benchmark
benchmark: ## Run the allocation policy benchmark and regenerate the report
	@echo "not yet implemented — issue #45 (M2-13)"

.PHONY: loadtest
loadtest: ## Run the k6 suite with thresholds as gates
	@echo "not yet implemented — issue #52 (M3-07)"

## Operations

.PHONY: dlq-inspect
dlq-inspect: ## List dead-lettered messages (STREAM=<name>)
	@echo "not yet implemented — issue #47 (M3-02)"

.PHONY: dlq-replay
dlq-replay: ## Replay a dead-lettered message (STREAM=<name> EVENT_ID=<uuid>)
	@echo "not yet implemented — issue #47 (M3-02)"

## Tooling

.PHONY: tools
tools: $(TOOLS_BIN)/golangci-lint $(TOOLS_BIN)/oapi-codegen $(TOOLS_BIN)/go-jsonschema ## Install pinned dev tools into .tools/bin

$(TOOLS_BIN)/golangci-lint:
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(TOOLS_BIN)/oapi-codegen:
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION)

$(TOOLS_BIN)/go-jsonschema:
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install github.com/atombender/go-jsonschema@$(GO_JSONSCHEMA_VERSION)

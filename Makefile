# webhook-relay
#
# A fresh clone should need exactly one command: `make up`.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# Load .env from the repo root if present, so port overrides live in one place
# that both compose and these targets agree on.
ENV_FILE      := $(wildcard .env)
COMPOSE       := docker compose -f deploy/compose/docker-compose.yml $(if $(ENV_FILE),--env-file $(ENV_FILE),)

# Host ports. Overridable in .env or on the command line:
#   make up API_PORT=8088
API_PORT      ?= $(shell [ -f .env ] && grep -E '^API_PORT=' .env | cut -d= -f2 || true)
API_PORT      := $(if $(API_PORT),$(API_PORT),8080)
POSTGRES_PORT ?= $(shell [ -f .env ] && grep -E '^POSTGRES_PORT=' .env | cut -d= -f2 || true)
POSTGRES_PORT := $(if $(POSTGRES_PORT),$(POSTGRES_PORT),5432)
VALKEY_PORT   ?= $(shell [ -f .env ] && grep -E '^VALKEY_PORT=' .env | cut -d= -f2 || true)
VALKEY_PORT   := $(if $(VALKEY_PORT),$(VALKEY_PORT),6379)

export API_PORT POSTGRES_PORT VALKEY_PORT

API_URL       ?= http://localhost:$(API_PORT)
GO            ?= go
GOLANGCI      := $(shell command -v golangci-lint 2>/dev/null)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# -----------------------------------------------------------------------------
# Stack
# -----------------------------------------------------------------------------

.PHONY: preflight
preflight: ## Fail early and clearly if a required host port is taken
	@fail=0; \
	for spec in "API_PORT:$(API_PORT)" "POSTGRES_PORT:$(POSTGRES_PORT)" "VALKEY_PORT:$(VALKEY_PORT)"; do \
		var=$${spec%%:*}; port=$${spec##*:}; \
		if docker ps --format '{{.Ports}}' 2>/dev/null | grep -q ":$$port->"; then continue; fi; \
		if ! ./scripts/port-in-use.sh "$$port"; then continue; fi; \
		owner=$$(lsof -nP -iTCP:$$port -sTCP:LISTEN 2>/dev/null | awk 'NR==2 {print $$1}'); \
		[ -n "$$owner" ] || owner="another process"; \
		echo "  port $$port is already in use by '$$owner'"; \
		echo "     override it:  echo '$$var=<free-port>' >> .env  &&  make up"; \
		fail=1; \
	done; \
	if [ $$fail -ne 0 ]; then \
		echo ""; \
		echo "  refusing to start — see .env.example for every overridable port."; \
		echo "  find a free port:  make free-port"; \
		exit 1; \
	fi

.PHONY: free-port
free-port: ## Print the first free host port at or above 8100
	@for p in $$(seq 8100 8200); do \
		if ! ./scripts/port-in-use.sh "$$p"; then echo "$$p"; exit 0; fi; \
	done; \
	echo "no free port found in 8100-8200" >&2; exit 1

# -----------------------------------------------------------------------------
# Stack
# -----------------------------------------------------------------------------

.PHONY: up
up: hooks preflight ## Build and start the full stack, then wait until it is serving
	$(COMPOSE) up -d --build
	@$(MAKE) --no-print-directory wait
	@echo ""
	@echo "  stack is up.  api: $(API_URL)"
	@echo "  next: make migrate-up && make seed"

.PHONY: wait
wait: ## Block until the API answers /readyz
	@echo -n "waiting for the api to become ready"
	@for i in $$(seq 1 60); do \
		if curl -fsS $(API_URL)/readyz >/dev/null 2>&1; then echo " ok"; exit 0; fi; \
		echo -n "."; sleep 1; \
	done; \
	echo " timed out after 60s"; \
	echo "--- api logs ---"; $(COMPOSE) logs --tail=40 api; \
	exit 1

.PHONY: down
down: ## Stop the stack and delete its volumes
	$(COMPOSE) down -v --remove-orphans

.PHONY: logs
logs: ## Follow the API logs
	$(COMPOSE) logs -f api

.PHONY: ps
ps: ## Show stack status
	$(COMPOSE) ps

# -----------------------------------------------------------------------------
# Migrations
# -----------------------------------------------------------------------------

.PHONY: migrate-up
migrate-up: ## Apply all migrations
	$(COMPOSE) run --rm migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	$(COMPOSE) run --rm migrate down 1

.PHONY: migrate-version
migrate-version: ## Print the current schema version
	$(COMPOSE) run --rm migrate version

# -----------------------------------------------------------------------------
# Development
# -----------------------------------------------------------------------------

.PHONY: seed
seed: ## Register a demo endpoint and post a few events
	@./scripts/seed.sh

.PHONY: test
test: ## Run every test (integration tests need Docker)
	$(GO) test ./... -race -count=1

.PHONY: test-short
test-short: ## Run only unit tests, skipping anything that needs Docker
	$(GO) test ./... -short -count=1

.PHONY: cover
cover: ## Run tests and open an HTML coverage report
	$(GO) test ./... -coverprofile=coverage.out -covermode=atomic
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

.PHONY: lint
lint: ## Run golangci-lint
ifeq ($(GOLANGCI),)
	@echo "golangci-lint is not installed."
	@echo "  brew install golangci-lint"
	@exit 1
else
	$(GOLANGCI) run ./...
endif

.PHONY: fmt
fmt: ## Format all Go code
	$(GO) fmt ./...
	gofmt -s -w .

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

.PHONY: build
build: ## Build both binaries into ./bin
	$(GO) build -o bin/api ./cmd/api
	$(GO) build -o bin/worker ./cmd/worker

# -----------------------------------------------------------------------------
# Repo hygiene
# -----------------------------------------------------------------------------

.PHONY: hooks
hooks: ## Install the gitleaks pre-commit hook
	@git config core.hooksPath .githooks
	@echo "git hooks installed from .githooks/"

.PHONY: scan
scan: ## Scan the whole history for secrets
	gitleaks git --redact --no-banner --verbose

.PHONY: verify
verify: fmt vet lint test ## Everything CI runs, locally

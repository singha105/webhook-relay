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
SINK_PORT     ?= $(shell [ -f .env ] && grep -E '^SINK_PORT=' .env | cut -d= -f2 || true)
SINK_PORT     := $(if $(SINK_PORT),$(SINK_PORT),9090)
GRAFANA_PORT  ?= $(shell [ -f .env ] && grep -E '^GRAFANA_PORT=' .env | cut -d= -f2 || true)
GRAFANA_PORT  := $(if $(GRAFANA_PORT),$(GRAFANA_PORT),3000)
PROMETHEUS_PORT ?= $(shell [ -f .env ] && grep -E '^PROMETHEUS_PORT=' .env | cut -d= -f2 || true)
PROMETHEUS_PORT := $(if $(PROMETHEUS_PORT),$(PROMETHEUS_PORT),9190)

export API_PORT POSTGRES_PORT VALKEY_PORT SINK_PORT GRAFANA_PORT PROMETHEUS_PORT

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
	for spec in "API_PORT:$(API_PORT)" "POSTGRES_PORT:$(POSTGRES_PORT)" "VALKEY_PORT:$(VALKEY_PORT)" "GRAFANA_PORT:$(GRAFANA_PORT)" "PROMETHEUS_PORT:$(PROMETHEUS_PORT)"; do \
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
	@echo "  stack is up."
	@echo "    api:        $(API_URL)"
	@echo "    grafana:    http://localhost:$(GRAFANA_PORT)   (dashboard already loaded, no login)"
	@echo "    prometheus: http://localhost:$(PROMETHEUS_PORT)"
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

.PHONY: dashboard
dashboard: ## Print the Grafana dashboard URL
	@echo "http://localhost:$(GRAFANA_PORT)/d/webhook-relay/webhook-relay"

.PHONY: worker-logs
worker-logs: ## Follow the delivery worker logs
	$(COMPOSE) logs -f worker

.PHONY: demo-up
demo-up: ## Start the stack plus the controllable test sink
	$(COMPOSE) --profile demo up -d --build
	@$(MAKE) --no-print-directory wait
	@echo ""
	@echo "  stack + sink are up."
	@echo "    api:  $(API_URL)"
	@echo "    sink: http://localhost:$(SINK_PORT)   (control: /_control/behavior, /_control/stats)"

.PHONY: demo
demo: ## ONE COMMAND: bring the stack up and prove it works end to end
	@./scripts/demo.sh

.PHONY: demo-delivery
demo-delivery: ## The Day 2 retry/DLQ/replay walkthrough (needs `make demo-up`)
	@./scripts/demo-delivery.sh

.PHONY: chaos-list
chaos-list: ## List the chaos experiments and how to run them
	@echo ""
	@echo "  Runnable now, on the compose stack:"
	@for f in chaos/compose/*.sh; do printf '    %s\n' "./$$f"; done
	@echo ""
	@echo "  Committed as Chaos Mesh manifests (need the k3d cluster, ~6 GiB Docker):"
	@for f in chaos/*.yaml; do printf '    %s\n' "$$f"; done
	@echo ""
	@echo "  Results and predictions: docs/chaos-results.md"
	@echo ""

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
cover: ## Run tests, refresh the committed coverage report and badge
	$(GO) test ./... -coverprofile=coverage.out -covermode=atomic
	$(GO) tool cover -html=coverage.out -o docs/coverage/coverage.html
	$(GO) tool cover -func=coverage.out > docs/coverage/coverage.txt
	@./scripts/coverage-badge.sh
	@echo "wrote docs/coverage/{coverage.html,coverage.txt,badge.svg}"

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
# Kubernetes
#
# The whole day-4 path: cluster-up creates the cluster, bootstrap provisions
# everything into it. Both are reproducible from committed files.
# -----------------------------------------------------------------------------

PROFILE      ?= full
K3D_CONFIG   := $(if $(filter lowmem,$(PROFILE)),deploy/k3d/cluster-lowmem.yaml,deploy/k3d/cluster.yaml)
CLUSTER_NAME := webhook-relay
KUBE_CONTEXT := k3d-$(CLUSTER_NAME)
TF           := terraform -chdir=terraform

.PHONY: cluster-up
cluster-up: ## Create the k3d cluster from the committed config
	@echo "creating cluster from $(K3D_CONFIG) (profile: $(PROFILE))"
	k3d cluster create --config $(K3D_CONFIG)
	@kubectl config use-context $(KUBE_CONTEXT) >/dev/null
	@echo ""
	@kubectl get nodes
	@echo ""
	@echo "  next: make bootstrap"

.PHONY: cluster-down
cluster-down: ## Destroy the k3d cluster and its registry
	k3d cluster delete $(CLUSTER_NAME) || true
	k3d registry delete webhook-relay-registry 2>/dev/null || true

.PHONY: cluster-status
cluster-status: ## Show what is running in the cluster
	@kubectl get nodes -o wide
	@echo ""
	@kubectl get pods -A --sort-by=.metadata.namespace

.PHONY: bootstrap
bootstrap: ## Provision everything into the cluster with Terraform
	$(TF) init -input=false -upgrade
	$(TF) apply -auto-approve -input=false \
		-var 'profile=$(PROFILE)' \
		-var 'kube_context=$(KUBE_CONTEXT)'
	@$(MAKE) --no-print-directory endpoints

.PHONY: plan
plan: ## Show what bootstrap would change
	$(TF) init -input=false
	$(TF) plan -input=false -var 'profile=$(PROFILE)' -var 'kube_context=$(KUBE_CONTEXT)'

.PHONY: teardown
teardown: ## Destroy everything Terraform created, keeping the cluster
	$(TF) destroy -auto-approve -input=false \
		-var 'profile=$(PROFILE)' -var 'kube_context=$(KUBE_CONTEXT)'

.PHONY: endpoints
endpoints: ## Print the cluster's URLs and credentials
	@echo ""
	@echo "  api:      http://webhook-relay.localhost:8081"
	@echo "  grafana:  http://grafana.localhost:8081   (admin / see below)"
	@echo "  argocd:   http://argocd.localhost:8081"
	@echo "  chaos:    http://chaos.localhost:8081"
	@echo ""
	@echo "  These hostnames need to resolve to 127.0.0.1. On macOS/Linux:"
	@echo "    echo '127.0.0.1 webhook-relay.localhost grafana.localhost argocd.localhost chaos.localhost' | sudo tee -a /etc/hosts"
	@echo ""
	@printf "  grafana password: "; $(TF) output -raw grafana_admin_password 2>/dev/null || echo "(run make bootstrap first)"
	@echo ""
	@printf "  argocd password:  "; kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' 2>/dev/null | base64 -d || echo "(not ready yet)"
	@echo ""

.PHONY: chart-sync
chart-sync: ## Copy /migrations into the chart so the two cannot drift
	@rm -f deploy/charts/webhook-relay/migrations/*.sql
	@cp migrations/*.sql deploy/charts/webhook-relay/migrations/
	@echo "synced $$(ls migrations/*.sql | wc -l | tr -d ' ') migration files into the chart"

.PHONY: chart-lint
chart-lint: chart-sync ## Lint and render the chart, and validate the output
	helm lint deploy/charts/webhook-relay --set image.tag=lint-test
	helm lint deploy/charts/webhook-relay --set image.tag=lint-test -f deploy/charts/webhook-relay/values-dev.yaml
	@helm template webhook-relay deploy/charts/webhook-relay --set image.tag=lint-test -n webhook-relay \
		| kubeconform -strict -summary -kubernetes-version 1.31.0 \
			-schema-location default \
			-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

.PHONY: tf-check
tf-check: ## terraform fmt and validate
	$(TF) fmt -check -recursive -diff
	$(TF) init -backend=false -input=false >/dev/null
	$(TF) validate

.PHONY: image-dev
image-dev: ## Build the image and push it to the cluster's local registry
	docker build -t localhost:5111/webhook-relay:dev --build-arg TARGET=api .
	docker push localhost:5111/webhook-relay:dev
	@echo "pushed localhost:5111/webhook-relay:dev"

.PHONY: seal-backup
seal-backup: ## Export the Sealed Secrets private key (gitignored; back this up)
	@kubectl -n kube-system get secret \
		-l sealedsecrets.bitnami.com/sealed-secrets-key -o yaml > sealed-secrets-key.yaml
	@echo "wrote sealed-secrets-key.yaml"
	@echo "WITHOUT THIS, a recreated cluster cannot decrypt any committed SealedSecret."

.PHONY: argocd-sync
argocd-sync: ## Force ArgoCD to reconcile now
	kubectl -n argocd patch application webhook-relay --type merge \
		-p '{"operation":{"initiatedBy":{"username":"make"},"sync":{"revision":"HEAD"}}}'

.PHONY: argocd-status
argocd-status: ## Show ArgoCD application health and sync state
	@kubectl -n argocd get applications -o custom-columns=\
NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status,REVISION:.status.sync.revision

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

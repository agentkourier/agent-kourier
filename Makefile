VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION)
LINT    := go tool golangci-lint

.DEFAULT_GOAL := help
.PHONY: help fmt lint test build tidy stub-oracle e2e-fast-up e2e-fast e2e-fast-down e2e-full-up e2e-full e2e-full-down e2e-down

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-z0-9-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format code (gofmt and goimports rules from .golangci.yml)
	$(LINT) fmt ./...

lint: ## Run golangci-lint (also over the e2e-tagged tests)
	$(LINT) run --build-tags e2e ./...

test: ## Run unit tests with the race detector
	go test -race ./...

build: ## Build bin/courier
	go build -ldflags "$(LDFLAGS)" -o bin/courier ./cmd/courier

tidy: ## Tidy go.mod and go.sum
	go mod tidy

stub-oracle: ## Run the WP-02 spike against the stub agent, with a token and with -no-auth (about 2 minutes)
	hack/stub-oracle.sh

e2e-fast-up: ## Bring up the fast E2E tier (kind, Alertmanager, fake Slack, stub agent, sink)
	hack/kind/up-fast.sh

e2e-fast: ## Run the E2E tests in the fast tier, as a Job (the tier must be up)
	hack/kind/e2e-run.sh fast

e2e-fast-down: ## Tear down the fast E2E tier only
	hack/kind/down.sh fast

e2e-full-up: ## Bring up the full E2E tier (kind, Substrate, kagent, Dex, mockllm)
	hack/kind/up-full.sh

e2e-full: ## Run the full E2E tier (stub)
	@echo "not implemented yet (see WP-14)"

e2e-full-down: ## Tear down the full E2E tier only
	hack/kind/down.sh full

e2e-down: ## Tear down both E2E tiers
	hack/kind/down.sh

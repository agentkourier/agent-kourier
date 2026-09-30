VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION)
LINT    := go tool golangci-lint

.DEFAULT_GOAL := help
.PHONY: help fmt lint test build tidy e2e-fast-up e2e-fast e2e-full-up e2e-full e2e-down

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-z0-9-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format code (gofmt and goimports rules from .golangci.yml)
	$(LINT) fmt ./...

lint: ## Run golangci-lint
	$(LINT) run ./...

test: ## Run unit tests with the race detector
	go test -race ./...

build: ## Build bin/courier
	go build -ldflags "$(LDFLAGS)" -o bin/courier ./cmd/courier

tidy: ## Tidy go.mod and go.sum
	go mod tidy

e2e-fast-up: ## Bring up the fast E2E tier (stub)
	@echo "not implemented yet (see WP-13)"

e2e-fast: ## Run the fast E2E tier (stub)
	@echo "not implemented yet (see WP-14)"

e2e-full-up: ## Bring up the full E2E tier (stub)
	@echo "not implemented yet (see WP-01)"

e2e-full: ## Run the full E2E tier (stub)
	@echo "not implemented yet (see WP-14)"

e2e-down: ## Tear down E2E environments (stub)
	@echo "not implemented yet (see WP-13 / WP-01)"

VERSION ?= dev
STATE   ?= firing
LDFLAGS := -X main.version=$(VERSION)
LINT    := go tool golangci-lint

.DEFAULT_GOAL := help
.PHONY: help fmt lint test build tidy image image-testhooks image-check stub-oracle e2e-fast-up e2e-fast e2e-fast-down e2e-full-up e2e-full e2e-full-down e2e-down sandbox-up sandbox-down sandbox-heal sandbox-alert

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-z0-9-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format code (gofmt and goimports rules from .golangci.yml)
	$(LINT) fmt ./...

lint: ## Run golangci-lint (also over the e2e, image and testhooks tagged code)
	$(LINT) run --build-tags e2e,image ./...
	$(LINT) run --build-tags testhooks ./...

test: ## Run unit tests with the race detector, then the testhooks-tagged ones
	go test -race ./...
	go test -race -tags testhooks ./internal/...

build: ## Build bin/courier
	go build -ldflags "$(LDFLAGS)" -o bin/courier ./cmd/courier

tidy: ## Tidy go.mod and go.sum
	go mod tidy

image: ## Build the production courier image, courier:dev (hack/kind/build-courier.sh; Docker)
	hack/kind/build-courier.sh

image-testhooks: ## Build the test image, courier-e2e/courier-testhooks:dev, which the kind tiers run and nothing pushes
	hack/kind/build-courier.sh testhooks

image-check: image image-testhooks ## Prove the production image's binary lacks the test hooks, and the test image's has them
	go test -tags image -count=1 -run TestImageHasNeitherHook ./internal/testhooks

stub-oracle: ## Run the WP-02 spike against the stub agent, with a token and with -no-auth (about 2 minutes)
	hack/stub-oracle.sh

e2e-fast-up: ## Bring up the fast E2E tier (kind, Alertmanager, fake Slack, stub agent, sink)
	hack/kind/up-fast.sh

e2e-fast: ## Run the E2E tests in the fast tier, as a Job (the tier must be up)
	hack/kind/e2e-run.sh fast

e2e-fast-down: ## Tear down the fast E2E tier only
	hack/kind/down.sh fast

e2e-full-up: ## Bring up the full E2E tier (kind, Substrate, kagent, Dex, mockllm, and the fast tier's doubles)
	hack/kind/up-full.sh

e2e-full: ## Run the E2E tests in the full tier, as a Job (the tier must be up)
	hack/kind/e2e-run.sh full

e2e-full-down: ## Tear down the full E2E tier only
	hack/kind/down.sh full

e2e-down: ## Tear down both E2E tiers
	hack/kind/down.sh

sandbox-up: ## Dogfood tier on the full tier: courier on a real Slack, a read-only SRE agent on a real model, seeded faults (secrets file: SANDBOX_ENV)
	hack/kind/sandbox/up.sh

sandbox-down: ## Remove the sandbox and put the full tier's own courier back, so that e2e-full works again
	hack/kind/sandbox/down.sh

sandbox-heal: ## Restart the full tier's egress gateway if its certificate is stale (about a day old); for a long session, sandbox-up does it too
	hack/kind/sandbox/heal.sh

sandbox-alert: ## Fire (STATE=firing, the default) or resolve (STATE=resolved) a seeded fault's alert, e.g. ALERT=KubePodCrashLooping: it posts to the real alert channel as [kind]
	hack/kind/sandbox/alert.sh "$(ALERT)" "$(STATE)"

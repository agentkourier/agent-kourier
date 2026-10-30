VERSION ?= dev
STATE   ?= firing
LDFLAGS := -X main.version=$(VERSION)
LINT    := go tool golangci-lint

.DEFAULT_GOAL := help
.PHONY: help fmt lint test test-race test-testhooks test-scripts test-postgres lint-workflows security security-govulncheck security-gitleaks security-image-scan build tidy image image-testhooks image-check release-image e2e-fast-up e2e-fast e2e-fast-down e2e-full-up e2e-full-down e2e-full e2e-down sandbox-up sandbox-down sandbox-heal sandbox-alert

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-z0-9-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format code (gofmt and goimports rules from .golangci.yml)
	$(LINT) fmt ./...

lint: ## Run golangci-lint (also over the e2e, image and testhooks tagged code)
	$(LINT) run --build-tags e2e,image ./...
	$(LINT) run --build-tags testhooks ./...

test: test-race test-testhooks test-scripts ## Run all the unit tests: with the race detector, the testhooks-tagged ones, the kind scripts' own (CI runs the three apart)

test-race: ## Run unit tests with the race detector (the chart tests need helm; AGENTKOURIER_REQUIRE_HELM=1 fails without it)
	go test -race -timeout 20m ./...

test-testhooks: ## Run the testhooks-tagged unit tests with the race detector
	go test -race -timeout 20m -tags testhooks ./internal/...

test-scripts: ## Run kind, security policy, release gate, PR label and CI tool installer tests (no cluster)
	hack/kind/netpol_test.sh
	hack/security/test.sh
	hack/release/test.sh
	hack/github/test.sh
	hack/ci/test.sh

lint-workflows: ## Lint .github/workflows with zizmor after proving its policy fails each unsafe fixture (installs the pinned zizmor unless ZIZMOR_BIN is set; needs jq)
	hack/security/lint-workflows.sh

security: security-govulncheck security-gitleaks ## Run source vulnerability and secret scans (the image scan needs IMAGE_REF)

security-govulncheck: ## Run govulncheck against the root, AX and docs-agent modules
	hack/security/govulncheck.sh .
	hack/security/govulncheck.sh examples/ax
	hack/security/govulncheck.sh examples/recipes/docs-agent/agent

security-gitleaks: ## Scan repository history for leaked credentials (GITLEAKS_BIN may select a pinned binary)
	hack/security/scan-secrets.sh .

security-image-scan: ## Scan IMAGE_REF for PLATFORM (linux/amd64 or linux/arm64) into OUTFILE
	test -n "$(IMAGE_REF)" || (printf '%s\n' 'IMAGE_REF is required' >&2; exit 1)
	test -n "$(PLATFORM)" || (printf '%s\n' 'PLATFORM is required' >&2; exit 1)
	test -n "$(OUTFILE)" || (printf '%s\n' 'OUTFILE is required' >&2; exit 1)
	hack/security/scan-image.sh "$(IMAGE_REF)" "$(PLATFORM)" "$(OUTFILE)"

test-postgres: ## Run the Postgres store's tests, the store conformance suite and the broker's Postgres test against a throwaway Postgres container (Docker; hack/postgres/test.sh)
	hack/postgres/test.sh

build: ## Build bin/agent-kourier
	go build -ldflags "$(LDFLAGS)" -o bin/agent-kourier ./cmd/agent-kourier

tidy: ## Tidy go.mod and go.sum
	go mod tidy

image: ## Build the production Agent Kourier image, agent-kourier:dev (hack/kind/build-agent-kourier.sh; Docker)
	hack/kind/build-agent-kourier.sh

image-testhooks: ## Build the test image, agent-kourier-e2e/agent-kourier-testhooks:dev, which the kind tiers run and nothing pushes
	hack/kind/build-agent-kourier.sh testhooks

image-check: image image-testhooks ## Prove the production image's binary lacks the test hooks, and the test image's has them
	go test -tags image -count=1 -run TestImageHasNeitherHook ./internal/testhooks

release-image: ## Build the multi-arch release image to bin/release (VERSION=vX.Y.Z); PUSH=1 REGISTRY=<repo> CHART_REGISTRY=oci://<repo> also pushes; see RELEASING.md
	VERSION="$(VERSION)" REGISTRY="$(REGISTRY)" CHART_REGISTRY="$(CHART_REGISTRY)" PUSH="$(PUSH)" hack/release/release.sh

e2e-fast-up: ## Bring up the fast E2E tier (kind, Alertmanager, fake Slack, stub agent, sink); E2E_STORE=postgres runs Agent Kourier on Postgres, with E2E_REPLICAS=2 on two replicas (S16, failover)
	hack/kind/up-fast.sh

e2e-fast: ## Run the E2E tests in the fast tier, as a Job (the tier must be up)
	hack/kind/e2e-run.sh fast

e2e-fast-down: ## Tear down the fast E2E tier only
	hack/kind/down.sh fast

e2e-full-up: ## Bring up the full E2E tier (kind, Substrate, kagent, Dex, mockllm, and the fast tier's doubles)
	hack/kind/up-full.sh

e2e-full: ## Run the E2E tests in the full tier, as a Job (the tier must be up, with its network policy live)
	hack/kind/e2e-run.sh full

e2e-full-down: ## Tear down the full E2E tier only
	hack/kind/down.sh full

e2e-down: ## Tear down all E2E tiers
	hack/kind/down.sh

sandbox-up: ## Dogfood tier on the full tier: Agent Kourier on a real Slack, a read-only SRE agent on a real model, seeded faults (secrets file: SANDBOX_ENV)
	hack/kind/sandbox/up.sh

sandbox-down: ## Remove the sandbox and put the full tier's own Agent Kourier back, so that e2e-full works again
	hack/kind/sandbox/down.sh

sandbox-heal: ## Restart the full tier's kindnet if NetworkPolicy is not applied, and its egress gateway if its certificate is stale (about a day old)
	hack/kind/sandbox/heal.sh

sandbox-alert: ## Fire (STATE=firing, the default) or resolve (STATE=resolved) a seeded fault's alert, e.g. ALERT=KubePodCrashLooping: it posts to the real alert channel as [kind]
	hack/kind/sandbox/alert.sh "$(ALERT)" "$(STATE)"

.PHONY: ax-test ax-image ax-smoke
ax-test: ## Test the separate AX/ADK/Bedrock reference module with the race detector
	cd examples/ax && go test -race -timeout 20m ./...
	cd examples/ax && go vet ./...

ax-image: ## Build the AX reference agent and runner image
	docker build -t agent-kourier-ax:dev examples/ax

ax-smoke: ## Run the real Bedrock A2A smoke using the existing sandbox credentials file
	bash examples/ax/smoke.sh

.PHONY: docs-agent-test docs-agent-lint docs-agent-image docs-agent-smoke docs-agent-smoke-live
docs-agent-test: ## Test the docs-agent recipe's separate module with the race detector
	cd examples/recipes/docs-agent/agent && go test -race -timeout 20m ./...
	cd examples/recipes/docs-agent/agent && go vet ./...

docs-agent-lint: ## Lint the docs-agent recipe's separate module with the repository's golangci-lint and config
	cd examples/recipes/docs-agent/agent && "$$(go tool -C ../../../.. -n golangci-lint)" run --config ../../../../.golangci.yml ./...

docs-agent-image: ## Build the docs agent image, agent-kourier-docs-agent:dev, with this repository's docs as its corpus (Docker with BuildKit)
	docker build --build-context docs=docs -t agent-kourier-docs-agent:dev examples/recipes/docs-agent/agent

docs-agent-smoke: ## Ask the docs agent fixed questions over A2A on loopback, with a scripted model and this repository's docs: no network, no credentials
	cd examples/recipes/docs-agent/agent && go run ./cmd/smoke

docs-agent-smoke-live: ## Owner-run: the same questions with the real model named in the sandbox credentials file (SANDBOX_ENV overrides its path); incurs model usage
	bash examples/recipes/docs-agent/agent/smoke-live.sh

# The echo-agent recipe (examples/recipes/echo-agent/agent) is Python, not Go: a virtualenv under bin/ with the
# versions and hashes pinned in requirements-dev.txt, on CPython 3.14 (requirements.txt is compiled for it).
.PHONY: echo-agent-test echo-agent-lint echo-agent-image
ECHO_AGENT  := examples/recipes/echo-agent/agent
ECHO_PYTHON ?= python3.14
ECHO_VENV   := bin/echo-agent-venv

$(ECHO_VENV)/.installed: $(ECHO_AGENT)/requirements-dev.txt
	$(ECHO_PYTHON) -m venv --clear $(ECHO_VENV)
	$(ECHO_VENV)/bin/pip install --quiet --disable-pip-version-check --require-hashes -r $(ECHO_AGENT)/requirements-dev.txt
	touch $@

echo-agent-test: $(ECHO_VENV)/.installed ## Test the echo-agent recipe's Python agent with pytest (python3.14; ECHO_PYTHON overrides)
	cd $(ECHO_AGENT) && $(CURDIR)/$(ECHO_VENV)/bin/python -m pytest -q

echo-agent-lint: $(ECHO_VENV)/.installed ## Lint the echo-agent recipe with ruff (check and format check) and audit its pinned runtime packages with pip-audit (needs network)
	cd $(ECHO_AGENT) && $(CURDIR)/$(ECHO_VENV)/bin/ruff check . && $(CURDIR)/$(ECHO_VENV)/bin/ruff format --check .
	$(ECHO_VENV)/bin/pip-audit --disable-pip --require-hashes -r $(ECHO_AGENT)/requirements.txt

echo-agent-image: ## Build the echo agent image, agent-kourier-echo-agent:dev (Docker)
	docker build -t agent-kourier-echo-agent:dev $(ECHO_AGENT)

# The prompt-agent recipe (examples/recipes/prompt-agent/agent) is Python too, set up like the echo agent's. Its image
# build proves the Dockerfile and requirements.txt (about 90 hashed packages, for all three provider families) install together.
.PHONY: prompt-agent-test prompt-agent-lint prompt-agent-image
PROMPT_AGENT  := examples/recipes/prompt-agent/agent
PROMPT_PYTHON ?= python3.14
PROMPT_VENV   := bin/prompt-agent-venv

$(PROMPT_VENV)/.installed: $(PROMPT_AGENT)/requirements-dev.txt
	$(PROMPT_PYTHON) -m venv --clear $(PROMPT_VENV)
	$(PROMPT_VENV)/bin/pip install --quiet --disable-pip-version-check --require-hashes -r $(PROMPT_AGENT)/requirements-dev.txt
	touch $@

prompt-agent-test: $(PROMPT_VENV)/.installed ## Test the prompt-agent recipe's Python agent with pytest (python3.14; PROMPT_PYTHON overrides)
	cd $(PROMPT_AGENT) && $(CURDIR)/$(PROMPT_VENV)/bin/python -m pytest -q

prompt-agent-lint: $(PROMPT_VENV)/.installed ## Lint the prompt-agent recipe with ruff (check and format check) and audit its pinned runtime packages with pip-audit (needs network)
	cd $(PROMPT_AGENT) && $(CURDIR)/$(PROMPT_VENV)/bin/ruff check . && $(CURDIR)/$(PROMPT_VENV)/bin/ruff format --check .
	$(PROMPT_VENV)/bin/pip-audit --disable-pip --require-hashes -r $(PROMPT_AGENT)/requirements.txt

prompt-agent-image: ## Build the prompt agent image, agent-kourier-prompt-agent:dev (Docker)
	docker build -t agent-kourier-prompt-agent:dev $(PROMPT_AGENT)

.PHONY: e2e-ax-up e2e-ax e2e-ax-down
e2e-ax-up: ## Bring up AX/Substrate with deterministic ADK scenarios and fake Slack
	hack/kind/up-ax.sh

e2e-ax: ## Run the shared E2E scenarios and AX text-question/lifecycle scenarios
	hack/kind/e2e-run.sh ax

e2e-ax-down: ## Tear down only the AX E2E tier
	hack/kind/down.sh ax

# The documentation site (mkdocs.yml, docs/): MkDocs with Material, in a virtualenv under bin/ with the versions
# pinned in hack/docs/requirements.txt. Needs Python 3.
.PHONY: docs docs-build docs-site docs-ref docs-check docs-diagrams
DOCS_VENV := bin/docs-venv

$(DOCS_VENV)/.installed: hack/docs/requirements.txt
	python3 -m venv --clear $(DOCS_VENV)
	$(DOCS_VENV)/bin/pip install --quiet --disable-pip-version-check -r hack/docs/requirements.txt
	touch $@

docs-ref: ## Generate the reference pages from api/v1alpha1 and the chart's values (hack/docs/refgen)
	go run ./hack/docs/refgen

docs-check: ## Fail on a misspelt product name, and on internal IDs, removed names, workspace identifiers or dead README links in the public docs
	go run ./hack/docs/namecheck
	go run ./hack/docs/contentcheck

docs-diagrams: ## Render the full architecture map to docs/assets/diagrams (Node; mermaid-cli through npx)
	npx -y @mermaid-js/mermaid-cli@11.15.0 -i hack/docs/diagrams/architecture-map.mmd \
	  -o docs/assets/diagrams/architecture-map.svg -c hack/docs/diagrams/mermaid.json -b white
	@# mermaid writes width="100%", which an <img> reads as 300 px; give the SVG its real size so it opens full size.
	perl -0pi -e 's/<svg id="my-svg" width="100%"(.*?)viewBox="0 0 ([\d.]+) ([\d.]+)"/sprintf(q{<svg id="my-svg" width="%d" height="%d"%sviewBox="0 0 %s %s"}, $$2+0.5, $$3+0.5, $$1, $$2, $$3)/e' \
	  docs/assets/diagrams/architecture-map.svg

docs: $(DOCS_VENV)/.installed docs-ref ## Serve the documentation site at http://127.0.0.1:8000, reloading on change
	$(DOCS_VENV)/bin/mkdocs serve

docs-build: $(DOCS_VENV)/.installed docs-ref docs-check ## Build the documentation site into bin/site; any warning fails the build
	$(DOCS_VENV)/bin/mkdocs build --strict --site-dir bin/site

docs-site: $(DOCS_VENV)/.installed docs-ref docs-check ## Build the versioned site into bin/versioned-site: this tree as dev, and every final tag of the release lines in hack/docs/versions.yaml
	go run ./hack/docs/sitebuild -venv $(DOCS_VENV) -out bin/versioned-site

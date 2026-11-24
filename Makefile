VERSION ?= dev
STATE   ?= firing
LDFLAGS := -X main.version=$(VERSION)
LINT    := go tool golangci-lint

.DEFAULT_GOAL := help
.PHONY: help fmt lint test test-race test-testhooks test-scripts test-postgres lint-workflows security security-govulncheck security-gitleaks security-image-scan build tidy crds crd-corpus image image-testhooks image-check release-image e2e-fast-up e2e-fast e2e-fast-down e2e-full-up e2e-full-down e2e-full e2e-down e2e-crs-scenarios e2e-crs-migration e2e-two-apps e2e-teams sandbox-up sandbox-down sandbox-heal sandbox-alert

# A rule line `target: ... ## text` gives the target a line. An add-on alias's rule line, which starts with
# `$(addsuffix -<kind>,$(ADDONS)):`, gives one to <name>-<kind> for each add-on, with <name> in its text replaced.
help: ## Show this help
	@awk -v addons='$(ADDONS)' 'BEGIN {FS = ":.*## "; n = split(addons, a, " ")} \
		/^[a-z0-9-]+:.*## / {printf "  %-14s %s\n", $$1, $$2} \
		/^\$$\(addsuffix -[a-z]+,\$$\(ADDONS\)\):.*## / {k = $$1; sub(/^\$$\(addsuffix /, "", k); sub(/,.*/, "", k); \
			for (i = 1; i <= n; i++) {t = $$2; gsub(/<name>/, a[i], t); printf "  %-14s %s\n", a[i] k, t}}' $(MAKEFILE_LIST)

fmt: ## Format code (gofmt and goimports rules from .golangci.yml)
	$(LINT) fmt ./...

lint: ## Run golangci-lint (also over the e2e, image, crdcorpus and testhooks tagged code)
	$(LINT) run --build-tags e2e,image,crdcorpus ./...
	$(LINT) run --build-tags testhooks ./...

test: test-race test-testhooks test-scripts ## Run all the unit tests: with the race detector, again with the testhooks tag where it changes the build, the scripts' own (CI runs the three apart)

# TEST_SHARD=I/N runs only leg I of N of a pass, test-race or test-testhooks: hack/ci/test-shard.sh splits the pass's
# packages over N legs by the seconds in hack/ci/test-weights, so that the N legs together run each package once. CI's
# matrix legs set it. Unset, as make test leaves it, a pass runs all its packages.
TEST_SHARD ?=

test-race: ## Run unit tests with the race detector (the chart tests need helm; AGENTKOURIER_REQUIRE_HELM=1 fails without it); TEST_SHARD=I/N runs one leg
ifeq ($(TEST_SHARD),)
	go test -race -timeout 20m ./...
else
	pkgs=$$(go list -race ./...) && \
		pkgs=$$(hack/ci/test-shard.sh pick hack/ci/test-weights/race.txt '$(TEST_SHARD)' $$pkgs) && \
		go test -race -timeout 20m $$pkgs
endif

# The testhooks pass's build flags besides -tags. The selector lists packages under the same ones: they select files too.
TESTHOOKS_BUILD_FLAGS := -race

test-testhooks: ## Run with the race detector and the testhooks tag the internal packages whose test build the tag changes (hack/ci/testhooks-packages.sh); the others compile as in test-race, which ran them; TEST_SHARD=I/N runs one leg
ifeq ($(TEST_SHARD),)
	pkgs=$$(TESTHOOKS_BUILD_FLAGS='$(TESTHOOKS_BUILD_FLAGS)' hack/ci/testhooks-packages.sh ./internal/...) && \
		go test $(TESTHOOKS_BUILD_FLAGS) -timeout 20m -tags testhooks $$pkgs
else
	pkgs=$$(TESTHOOKS_BUILD_FLAGS='$(TESTHOOKS_BUILD_FLAGS)' hack/ci/testhooks-packages.sh ./internal/...) && \
		pkgs=$$(hack/ci/test-shard.sh pick hack/ci/test-weights/testhooks.txt '$(TEST_SHARD)' $$pkgs) && \
		go test $(TESTHOOKS_BUILD_FLAGS) -timeout 20m -tags testhooks $$pkgs
endif

test-scripts: ## Run add-on list, kind, security policy, release gate, PR label, Slack HTTP-mode probe, CI tool installers, testhooks package selection, test leg split, Actions cache upkeep and docs-only classification tests (no cluster)
	hack/addons_test.sh
	hack/kind/netpol_test.sh
	hack/security/test.sh
	hack/release/test.sh
	hack/github/test.sh
	hack/slack/test.sh
	hack/ci/test.sh
	hack/ci/install-github-mcp-server_test.sh
	hack/ci/testhooks-packages_test.sh
	hack/ci/test-shard_test.sh
	hack/ci/actions-cache_test.sh
	hack/ci/docs-only_test.sh

lint-workflows: ## Lint .github/workflows with zizmor after proving its policy fails each unsafe fixture (installs the pinned zizmor unless ZIZMOR_BIN is set; needs jq)
	hack/security/lint-workflows.sh

security: security-govulncheck security-gitleaks ## Run source vulnerability and secret scans (the image scan needs IMAGE_REF)

security-govulncheck: ## Run govulncheck against the root, docs-agent, each add-on's (hack/addons.txt) and add-on kit modules
	hack/security/govulncheck.sh .
	hack/security/govulncheck.sh examples/recipes/docs-agent/agent
	$(foreach a,$(ADDONS),hack/security/govulncheck.sh addons/$(a)$(newline))
	hack/security/govulncheck.sh addons/kit

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

crds: ## Generate the CRDs from api/v1alpha1 into charts/agent-kourier/files/crds (controller-gen, pinned in go.mod) and their hashes into api/v1alpha1/crd_schema_hashes.go; tests fail when either drifts
	rm -f charts/agent-kourier/files/crds/*.yaml
	go tool controller-gen crd paths=./api/v1alpha1 output:crd:dir=charts/agent-kourier/files/crds
	go run ./hack/crdhashes

crd-corpus: ## Hold the CRD schemas to the broker's validation, and the leader label's admission policy to its rules, on a throwaway kind cluster (hack/kind/crd-corpus.sh; kind, kubectl, helm and Docker; CRD_CORPUS_KUBECONFIG uses a local cluster you made instead)
	hack/kind/crd-corpus.sh

image: ## Build the production Agent Kourier image, agent-kourier:dev (hack/kind/build-agent-kourier.sh; Docker)
	hack/kind/build-agent-kourier.sh

image-testhooks: ## Build the test image, agent-kourier-e2e/agent-kourier-testhooks:dev, which the kind tiers run and nothing pushes
	hack/kind/build-agent-kourier.sh testhooks

image-check: image image-testhooks ## Prove the production image's binary lacks the test hooks, and the test image's has them
	go test -tags image -count=1 -run TestImageHasNeitherHook ./internal/testhooks

release-image: ## Build the multi-arch release images (Agent Kourier, the prompt and echo agents, the CMA and AX add-ons) and the chart to bin/release (VERSION=vX.Y.Z); PUSH=1 REGISTRY=<repo> CHART_REGISTRY=oci://<repo> also pushes; see RELEASING.md
	VERSION="$(VERSION)" REGISTRY="$(REGISTRY)" CHART_REGISTRY="$(CHART_REGISTRY)" PUSH="$(PUSH)" hack/release/release.sh

e2e-fast-up: ## Bring up the fast E2E tier (kind, Alertmanager, fake Slack, stub agent, sink); E2E_STORE=postgres runs Agent Kourier on Postgres, with E2E_REPLICAS=2 on two replicas (S16, failover); E2E_CONFIG_SOURCE=crds reads the config from custom resources, not the ConfigMap, and with E2E_TWO_APPS=1 serves a team's Slack app beside the platform's (make e2e-two-apps runs the MC_ tests), or with E2E_TEAMS=1 a team's Microsoft Teams connections on a fake Teams service (make e2e-teams runs the TEAMS_ tests)
	hack/kind/up-fast.sh

e2e-fast: ## Run the E2E tests in the fast tier, as a Job (the tier must be up)
	hack/kind/e2e-run.sh fast

e2e-fast-down: ## Tear down the fast E2E tier only
	hack/kind/down.sh fast

e2e-full-up: ## Bring up the full E2E tier (kind, Substrate, kagent, Dex, mockllm, and the fast tier's doubles; E2E_CONFIG_SOURCE=crds reads the config from custom resources)
	hack/kind/up-full.sh

e2e-full: ## Run the E2E tests in the full tier, as a Job (the tier must be up, with its network policy live)
	hack/kind/e2e-run.sh full

e2e-full-down: ## Tear down the full E2E tier only
	hack/kind/down.sh full

e2e-down: ## Tear down all E2E tiers
	hack/kind/down.sh

e2e-crs-scenarios: ## The custom resource scenarios that need kubectl or Helm: unknown fields, kubectl get, a stored-invalid Binding, kubectl wait (the fast tier up on E2E_CONFIG_SOURCE=crds)
	hack/kind/crs-scenarios.sh

e2e-crs-migration: ## Move the fast tier between the ConfigMap and the custom resources with Helm, and back, with a session in flight (the tier up on Postgres, two replicas, files)
	hack/kind/crs-migration.sh

e2e-two-apps: ## Run the MC_ tests (a broker with two Slack apps) as a Job of their own, then take a team off the platform's admission list with Helm and put it back, then run with no ChatConnection of its own, the platform's namespace off the list and then on it (with a leader takeover on Postgres with two replicas), then admit a team by a namespace label with no restart, and serve every namespace with nothing set and with the selector {}: the fast tier up on E2E_TWO_APPS=1 E2E_CONFIG_SOURCE=crds
	hack/kind/two-apps.sh

e2e-teams: ## Run the TEAMS_ tests (Microsoft Teams connections on the fake Teams service: a mention, live edits, a tool approval, alerts through Graph's notifications and through polling, feedback, and a leader takeover on Postgres with two replicas) as a Job of their own: the fast tier up on E2E_TEAMS=1 E2E_CONFIG_SOURCE=crds
	E2E_RUN='^TestTEAMS_' hack/kind/e2e-run.sh fast

sandbox-up: ## Dogfood tier on the full tier: Agent Kourier on a real Slack, a read-only SRE agent on a real model, seeded faults (secrets file: SANDBOX_ENV)
	hack/kind/sandbox/up.sh

sandbox-down: ## Remove the sandbox and put the full tier's own Agent Kourier back, so that e2e-full works again
	hack/kind/sandbox/down.sh

sandbox-heal: ## Restart the full tier's kindnet if NetworkPolicy is not applied, and its egress gateway if its certificate is stale (about a day old)
	hack/kind/sandbox/heal.sh

sandbox-alert: ## Fire (STATE=firing, the default) or resolve (STATE=resolved) a seeded fault's alert, e.g. ALERT=KubePodCrashLooping: it posts to the real alert channel as [kind]
	hack/kind/sandbox/alert.sh "$(ALERT)" "$(STATE)"

# The add-ons' shared kit (addons/kit) is a separate Go module, which each add-on requires through `replace ../kit` and
# which is released with them. kit-test runs its tests at the kit's own versions. An add-on's go.mod may require a newer
# version of a dependency than the kit's floor, so each add-on's test target runs again, under the add-on's module graph,
# the kit's packages that the add-on links, its tests included (KIT_PACKAGES, run in the add-on's directory). A kit
# package it does not link may need a module its graph lacks: kit/auth/oidc, which only AX links, needs go-oidc.
# -count=1: the kit's tests read docs/reference, which go test's result cache never checks.
KIT_PACKAGES = $$(go list -deps -test -f '{{if not .ForTest}}{{.ImportPath}}{{end}}' ./... | grep '^github.com/agentkourier/agent-kourier/addons/kit/' | sort -u)

# The add-ons are the names in hack/addons.txt, which hack/addons.sh reads (it fails on a malformed list, and then so does
# every make). Add-on <name> is the Go module addons/<name>, and has the targets addon-test-<name>, addon-lint-<name> and
# addon-image-<name>, which CI runs, and their aliases <name>-test, <name>-lint and <name>-image below. A module nested
# in an add-on's directory (addons/<name>/<dir>/go.mod: the CMA add-on's livecheck, which imports the root module through
# a replace) is a module of its own: its tests run without the race detector, every test on every run, offline (a live
# check skips without its credentials), and addons-tidy tidies it.
ADDONS := $(shell hack/addons.sh)
ifeq ($(strip $(ADDONS)),)
$(error hack/addons.sh read no add-on from hack/addons.txt; it says why above)
endif
addon_nested = $(patsubst %/go.mod,%,$(wildcard addons/$(1)/*/go.mod))
ADDON_TEST  := $(addprefix addon-test-,$(ADDONS))
ADDON_LINT  := $(addprefix addon-lint-,$(ADDONS))
ADDON_IMAGE := $(addprefix addon-image-,$(ADDONS))
ADDON_ALIASES := $(foreach k,test lint image,$(addsuffix -$(k),$(ADDONS)))

# A recipe line that expands to several lines (a $(foreach) ending each item with $(newline)) runs as that many lines.
define newline


endef

.PHONY: kit-test kit-lint addons-tidy $(ADDON_TEST) $(ADDON_LINT) $(ADDON_IMAGE) $(ADDON_ALIASES)
# ADDON_TEST_TIMEOUT: go test's limit for the kit's, an add-on's and the docs-agent's tests, which take about a minute.
# It stays under their CI jobs' timeout-minutes (15 and 20), so a test that hangs makes go test panic and name it
# before GitHub cancels the job, which prints nothing about it.
ADDON_TEST_TIMEOUT ?= 10m
kit-test: ## Test the add-ons' shared kit module with the race detector, every test on every run
	cd addons/kit && go test -race -count=1 -timeout $(ADDON_TEST_TIMEOUT) ./...
	cd addons/kit && go vet ./...

kit-lint: ## Lint the add-ons' shared kit module with the repository's golangci-lint and config
	cd addons/kit && "$$(go tool -C ../.. -n golangci-lint)" run --config ../../.golangci.yml ./...

addons-tidy: ## Run go mod tidy in the kit and in each add-on's modules (hack/addons.txt), which share dependencies through the kit
	cd addons/kit && go mod tidy
	$(foreach d,$(foreach a,$(ADDONS),addons/$(a) $(call addon_nested,$(a))),cd $(d) && go mod tidy$(newline))

# -count=1: the Postgres tests reach a database that go test's result cache knows nothing of, so a reused result would
# say nothing about it. They read AGENTKOURIER_POSTGRES_DSN and skip when it is unset; hack/postgres/run.sh starts a
# throwaway Postgres and sets it for one command (hack/postgres/run.sh make addon-test-cma).
$(ADDON_TEST): addon-test-%:
	cd addons/$* && go test -race -count=1 -timeout $(ADDON_TEST_TIMEOUT) ./...
	cd addons/$* && go test -race -count=1 -timeout $(ADDON_TEST_TIMEOUT) $(KIT_PACKAGES)
	cd addons/$* && go vet ./...
	$(foreach m,$(call addon_nested,$*),cd $(m) && go test -count=1 ./...$(newline))

$(ADDON_LINT): addon-lint-%:
	cd addons/$* && "$$(go tool -C ../.. -n golangci-lint)" run --config ../../.golangci.yml ./...

# The image agent-kourier-<name>:dev, from the context addons/ (which holds the kit the add-on requires through
# `replace ../kit`) with addons/<name>/Dockerfile, on Agent Kourier's pinned bases (hack/kind/lib.sh).
$(ADDON_IMAGE): addon-image-%:
	bash -c '. hack/kind/lib.sh && docker build --build-arg "GO_IMAGE=$${GO_IMAGE}" --build-arg "RUNTIME_IMAGE=$${RUNTIME_IMAGE}" -f addons/$*/Dockerfile -t agent-kourier-$*:dev addons'

# The aliases: <name>-test runs addon-test-<name>, and so on, for each add-on in the list (make cma-test, make ax-lint).
# make help prints each line's text once per add-on (the help target says how).
$(addsuffix -test,$(ADDONS)): %-test: addon-test-% ## Test add-on <name>'s module (addons/<name>) with the race detector, every test on every run, the kit's packages under its module graph, and the offline tests of a module nested in its directory; its Postgres tests run with AGENTKOURIER_POSTGRES_DSN set (hack/postgres/run.sh make <name>-test)
$(addsuffix -lint,$(ADDONS)): %-lint: addon-lint-% ## Lint add-on <name>'s module (addons/<name>) with the repository's golangci-lint and config
$(addsuffix -image,$(ADDONS)): %-image: addon-image-% ## Build add-on <name>'s image, agent-kourier-<name>:dev, on Agent Kourier's pinned bases (hack/kind/lib.sh; Docker)

# The AX add-on's own targets, beside its aliases.
.PHONY: ax-runner-image ax-smoke
ax-runner-image: ## Build the image of the route inside Google AX, agent-kourier-ax-runner:dev: AX's task runner, git and the agent on Debian (addons/ax/Dockerfile.ax; Docker)
	docker build -f addons/ax/Dockerfile.ax -t agent-kourier-ax-runner:dev addons

ax-smoke: ## Run the real A2A smoke on Bedrock or the Anthropic API (the sandbox file's MODEL_PROVIDER), using the existing sandbox credentials file
	bash addons/ax/smoke.sh

.PHONY: docs-agent-test docs-agent-lint docs-agent-image docs-agent-smoke docs-agent-smoke-live
# -count=1: the smoke test reads docs/, outside the module, which go test's result cache never checks, so a reused result
# would not see a change to the docs.
docs-agent-test: ## Test the docs-agent recipe's separate module with the race detector, every test on every run
	cd examples/recipes/docs-agent/agent && go test -race -count=1 -timeout $(ADDON_TEST_TIMEOUT) ./...
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

prompt-agent-test: $(PROMPT_VENV)/.installed ## Test the prompt-agent recipe's Python agent with pytest (python3.14; PROMPT_PYTHON overrides; GITHUB_MCP_SERVER_BIN, from hack/ci/install-github-mcp-server.sh, runs the webhook-github agent's tests)
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

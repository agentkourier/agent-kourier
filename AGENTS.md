# Agent Kourier

`CLAUDE.md` is a symlink to this file; edit `AGENTS.md`.

Agent Kourier is a Go broker between Slack and A2A agents (kagent 1.0 or any A2A backend). It
turns Slack messages, including the alerts Alertmanager posts to Slack, into agent sessions and
renders the agent's output back into Slack threads. It has no Alertmanager webhook: alerts enter
only through `chat.triggers` (spec D39). `spec.md` is the source of truth (decisions in section 5,
architecture in section 6); the build plan is `tasks/plans/agent-courier-pilot.md`. `tasks/` (plans, todo,
lessons) and `spike/` are gitignored and live only in the maintainer's main checkout. A new worktree does
not have them; link them in from the main checkout (`ln -s <main checkout>/tasks tasks`).

## Layout

Directories appear when the work package that owns them lands; do not create empty ones.

```
cmd/agent-kourier/      entrypoint: the broker, the dry-run subcommand, --version
api/v1alpha1/           resource types (Binding, AgentBackend, ChatConnection) and their validation
internal/config         the config sources' shared core: the file loader, Binding resolution (all or nothing, or one object at a time), reload and its report
internal/config/crsource   the custom resource source (AGENTKOURIER_CONFIG_SOURCE=crds): informers of the three kinds, the last good generation of each object
internal/config/k8ssecrets per-Secret informers behind the Secret resolver
internal/config/sourcetest conformance suite that the file source and the custom resource source both run
internal/session        session manager (turns, reply queue, controls, recovery)
internal/agent          A2A client: streaming, reconnect, typed outcomes, client pool
internal/dialect        agent-dialect adapters (kagent, generic a2a)
internal/identity       Binding token source and credentials for the agent pool
internal/secret         redacting Secret value type
internal/chat           chat-neutral model and chat.Platform; imports no session, config or store (chattest: conformance suite, fake platform, the Alertmanager trigger the tests share)
internal/chat/slack     Slack adapter (Block Kit, Socket Mode); the only code, with the broker, that may import Slack or slack-go
internal/trigger        message intake: chat.triggers matched (spec 8.2), trigger threads and limits, bot mentions, Submit to the session manager, the dry run's Replay
internal/store          SQLite and Postgres persistence, leader fencing
internal/outbox         durable outbound posting (the versioned neutral message)
internal/queue          durable work queue worker
internal/retention      store retention loop: undelivered replies, stale tasks, expired sessions, old audit entries
internal/render         agent output to neutral messages
internal/feedback       useful / not-useful votes on investigations
internal/audit          audit log
internal/telemetry      Prometheus metrics and OpenTelemetry traces
internal/bindingcheck   per-Binding conditions and the prober that checks them
internal/broker         composition: Bindings, chat platform factory, lifecycle, health, leader election, shutdown order
internal/interact       interactions: pause intake, button presses, timeout sweep
internal/redact         shared secret redaction for display and audit text
internal/backoff        doubling retry wait shared by the retry loops (leaf package, imports nothing of the module)
internal/limitsdoc      test support that holds docs/reference/limits.md to the constants of the code
internal/testhooks      test-only hooks, compiled in under the testhooks build tag
internal/testutil       helpers only tests import: Eventually (poll until a condition holds), DiscardLogger; standard library only (a migrated SQLite store for a test: internal/store/sqlite/sqlitetest)
charts/agent-kourier    Helm chart
examples/ax             reference A2A agent for Google AX (separate module)
docs/                   the documentation site (mkdocs; docs/brand is the brand kit and is not published)
test/{fakeslack,stubagent,e2e,fixtures,kindmanifests,chart,...}
test/crdcorpus          valid and invalid objects that the CRD schemas and the broker's Validate must treat alike (`make crd-corpus` sends them to a real API server)
hack/kind/              kind tier scripts (fast, full, ax, sandbox)
hack/{docs,release,postgres,crdhashes}  doc generators, release scripts, Postgres test helpers, the CRD schema hashes
```

## Make targets

Run `make` for help. Only Go and the tools pinned in `go.mod` (`tool` directive) are needed;
golangci-lint runs as `go tool golangci-lint`, never a global install.

- `make fmt lint test build tidy`: `test` runs `go test -race ./...` and then `go test -race -tags testhooks ./internal/...`; `build` writes `bin/agent-kourier`.
- `make crds` regenerates the CustomResourceDefinitions in `charts/agent-kourier/files/crds/` from the markers in `api/v1alpha1` with
  `controller-gen` (pinned as a `tool` in `go.mod`, run as `go tool controller-gen`). Run it after changing an API type or marker and commit the
  result: `TestCommittedCRDsMatchGeneration` in `make test` fails when the committed files differ from what generation produces. The
  production binary does not link controller-tools. It then runs `hack/crdhashes`, which adds to each CRD file the annotation `agentkourier.dev/schema-sha256` (the SHA-256 of the file
  without that line) and writes the same hashes into `api/v1alpha1/crd_schema_hashes.go`: the broker compares them, in CR mode, with the
  annotation of each CRD in its cluster, and `TestSchemaHashesMatchTheCommittedCRDs` fails when the files and the constants drift
  (`TestCommittedCRDsMatchGeneration` compares the files without that line).
- `make crd-corpus` holds the CRD schemas to the broker's validation: it applies only the CRDs to a throwaway kind cluster
  (`hack/kind/crd-corpus.sh`; `CRD_CORPUS_KUBECONFIG` uses a local cluster you made instead) and `test/crdcorpus` sends each corpus entry to it
  as a server-side dry-run create. A new API field or validation rule gets a marker where the schema can copy it and an entry in
  `test/crdcorpus/testdata/invalid` with its `# schema: refuses` or `# schema: accepts` flag. The CI job `crd-corpus` runs it on the pinned node and on the chart's minimum Kubernetes (`CRD_CORPUS_NODE=min`, `NODE_IMAGE_MIN` in `hack/kind/lib.sh`). A bound in the
  schema is a cap in `Validate` too (`maxTriggers`, `maxExtract`, `maxHeaders`, `maxHeaderNameLen`), so lowering one can refuse a config that loads today.
- `make e2e-fast-up` / `e2e-full-up` / `e2e-ax-up` bring up the kind tiers (`hack/kind/`); `e2e-fast-down`,
  `e2e-full-down`, `e2e-ax-down` and `e2e-down` (all) tear them down. `e2e-fast` / `e2e-full` / `e2e-ax` run the E2E tests in that tier
  as an in-cluster Job (`hack/kind/e2e-run.sh`).
- `E2E_CONFIG_SOURCE=crds make e2e-fast-up` (or `e2e-full-up`) runs the tier on custom resources: the CRDs first, then the same config as CRs and the
  release upgraded with `hack/kind/crds-values.yaml`, as the chart README says to move. The default, `files`, is the ConfigMap route, which each tier's values name (`config.source: files`) because the chart's own default is `crds`. The bring-up moves a tier
  between the two in place; `e2e-run.sh` reads the source from the Deployment, and the `CR_` tests skip on files. Run `make e2e-fast` on each source before
  a change to `internal/config`, the chart or `hack/kind`.
- `E2E_TWO_APPS=1` (with `E2E_CONFIG_SOURCE=crds`) adds a team's Slack app to the fast tier, which the `MC_` tests drive. `make e2e-fast` does not run them (their failover cases would take its Job near its time limit); `make e2e-two-apps` (`hack/kind/two-apps.sh`) runs them as a Job of their own (`E2E_RUN=^TestMC_`), then the one that changes the admission list with Helm.
- `make e2e-crs-scenarios` runs what the Job cannot, because its image has no kubectl or Helm (`hack/kind/crs-scenarios.sh`; the fast tier up on
  `E2E_CONFIG_SOURCE=crds`): what each client does with an unknown field, what `kubectl get` shows, a
  Binding stored before a tightening of the schema, and `kubectl wait`. `make e2e-crs-migration` (`hack/kind/crs-migration.sh`) moves a release between the ConfigMap
  and the custom resources and back with Helm while a session is in flight, on a fresh Postgres tier with two replicas, and puts the tier back on the ConfigMap route after (the CRDs stay under the release, so a second run needs a fresh tier).
- AX fixtures use the separate `examples/ax` module and unmodified pinned AX/Substrate APIs; see
  `hack/kind/ax/README.md` for shared scenarios and capability differences. `make ax-smoke` exercises Bedrock/Haiku.
- `make image` builds `agent-kourier:dev`, the production image, from the `Dockerfile` (pinned Go and distroless
  images, in `hack/kind/lib.sh`); `make image-testhooks` builds `agent-kourier-e2e/agent-kourier-testhooks:dev`, the
  labelled test image the kind tiers run and nothing pushes; `make image-check` proves the production
  binary lacks the test hooks. All three need Docker.
- `make docs-ref` regenerates `docs/reference/` from the API types and the chart values; `make docs-build` builds the site (strict) and runs `docs-check`.
- `make docs-site` builds the versioned site that production serves into `bin/versioned-site` (`hack/docs/sitebuild`): this tree as
  `dev/`, and every final tag of the release lines in `hack/docs/versions.yaml` (newest first) as `X.Y.Z/`, each in a
  temporary worktree with that tag's own pinned requirements, the newest as `latest`. It writes `versions.json` (Material's
  version selector), `_redirects` (the root and the old unversioned paths to `/latest/`, each line's `/X.Y/` to its newest
  tag) and `_headers` at the root. It needs the tags.
- `make docs-check` fails on a misspelt product name (`hack/docs/namecheck` reads `mkdocs.yml`, the theme
  overrides, `docs/` except `brand` and `spikes` (of an SVG, the text and not the markup), and the files that pages of `docs/` include with `--8<--`, found under the
  snippets `base_path` in `mkdocs.yml`; an include it cannot find fails too, and `kourier/` as a path segment is not a misspelling) and, in the public sources (`README.md`, the chart
  README, all of `charts/agent-kourier/values.yaml`, the generated CRDs under `charts/agent-kourier/files/crds`, every text file under `docs/` except `brand` and `spikes`, the theme
  overrides, and every `README.md` and YAML file under `examples/recipes`), on internal plan, decision and risk IDs and `tasks/` paths, removed field and page names, API fields the types do
  not have yet (a field stops being flagged once `api/v1alpha1` has it), a release version other than `charts/agent-kourier/Chart.yaml`'s, wording that says there is no release,
  a `helm` install of `./charts/agent-kourier` (the release gate refuses a tag that is not the chart's version, so a
  release starts with a pull request that bumps both), owner or workspace identifiers (from the private
  pattern in `CONTENTCHECK_WORKSPACE`, which CI sets from a secret; unset, that rule does not run and the check says so), and relative links or
  anchors in the two READMEs and the recipe READMEs that do not resolve (`hack/docs/contentcheck`; a deliberate exception goes in its `allowlist.txt`
  with a reason, and a stale entry fails too).
- `make release-image VERSION=vX.Y.Z` builds the release image and chart locally; see `RELEASING.md`.
- Add tools with `go get -tool <module>/cmd/<tool>@<version>`.

## Work-package workflow

1. The orchestrator dispatches one WP from the plan to `agent-kourier-implementer` on branch
   `wp/<id>-<slug>` in its own worktree.
2. Implementer works test-first, runs `make fmt lint test` (plus `make e2e-fast` when the
   WP says so), commits, and reports.
3. `agent-kourier-reviewer` gates the diff. Fixes go back to the same implementer, at most two rounds.
4. The orchestrator squash-merges approved branches onto `main`.

No line cap (owner, 2026-10-01): one concern per WP is the only size rule. Split only when
a WP mixes concerns. At most four agents (implementers and reviewers together) run at once.

## Go conventions

- Standard library `testing`, table-driven where it fits; e2e tests use the `e2e` build tag.
- Accept interfaces, return structs. Wrap errors with `%w` and context.
- `context.Context` is the first parameter of anything that does I/O.
- No global mutable state (the ldflags-set `version` is the sole exception). No `panic`
  outside `main` and test helpers. Exported identifiers have doc comments.
- No new module dependency unless the WP names it.
- Nothing but the Slack adapter, the broker and cmd (and tests) imports `internal/chat/slack` or slack-go: depguard denies it to every other package, new ones included.
- Never log, print, or commit tokens or Secret contents; test tokens are fake constants.

## Documentation

- Every statement of behaviour in `README.md`, `docs/` (except `docs/brand` and `docs/spikes`), the chart README
  and Helm values comments is checked against the code, the chart or the manifests when it is written or changed.
  A sentence copied from another doc, from `spec.md` or from an agent's summary is not a check: open the code path
  and read it.
- Words carry behaviour: say what the code does (denies, drops, queues, retries, cancels), not a near synonym.
  "Refuses" and "denies" are different outcomes.
- Say where a claim came from when it cannot be read in this repository (an upstream project's behaviour, a
  date, a version); mark it in the PR or commit body as unverified rather than stating it as fact.
- A reviewer, human or agent, verifies against the code and names the file and line; agreement between docs is
  not evidence.
- A change that adds or moves a limit (a bound, a cap, a default timeout, a version) updates `docs/reference/limits.md` in the same change:
  the value, what happens past it, and whether a setting changes it. A value that is a constant or a default of the code is written in a code
  span followed at once by a marker (copy a row of the page), and a test in the package that holds it calls `limitsdoc.Count`, `Bytes`,
  `Duration` or `Text` with the marker's ID, so that the constant cannot change without the page; `internal/limitsdoc` fails a marker that no
  test calls, and a guard given a literal. The guard passes the value that production runs with: the constant production reads, or the
  default read where the constructor fills it in (not a parallel default of the test's own, and not a number typed again). A default that the
  binary fills in from its environment is read from the binary (`cmd/agent-kourier`). A chart value is read from the chart's files
  (`internal/limitsdoc`). Check a new guard by changing the production value and seeing the test fail. A test file with a build tag is not
  counted as a guard.
- No work-package, decision or risk IDs, `tasks/` paths, personal or workspace identifiers in public pages.
- Run `make docs-ref docs-check docs-build` before pushing a docs change. Pushing to `main` deploys the site.

## Commits

Conventional Commits (`feat(session): ...`, `test(e2e): ...`). Never add `Co-Authored-By`
or any AI attribution trailer to commits or PR descriptions.

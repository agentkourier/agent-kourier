# Agent Kourier

`CLAUDE.md` is a symlink to this file; edit `AGENTS.md`.

Agent Kourier is a Go broker between Slack and A2A agents (kagent 1.0 or any A2A backend). It
turns Slack messages, including the alerts Alertmanager posts to Slack, into agent sessions and
renders the agent's output back into Slack threads. It has no Alertmanager webhook: alerts enter
only through `chat.triggers` (spec D39). `spec.md` is the source of truth (decisions in section 5,
architecture in section 6); the build plan is `tasks/plans/agent-courier-pilot.md`.

## Layout

Directories appear when the work package that owns them lands; do not create empty ones.

```
cmd/agent-kourier/      entrypoint: the broker, the dry-run subcommand, --version
api/v1alpha1/           resource types (Binding, AgentBackend, ChatConnection) and their validation
internal/config         file loader for resources (Binding resolution, reload)
internal/session        session manager (turns, reply queue, controls, recovery)
internal/agent          A2A client: streaming, reconnect, typed outcomes, client pool
internal/dialect        agent-dialect adapters (kagent, generic a2a)
internal/identity       Binding token source and credentials for the agent pool
internal/secret         redacting Secret value type
internal/chat           chat-neutral model, chat.Platform, triggers and their limits (chattest: conformance suite, fake platform)
internal/chat/slack     Slack adapter (Block Kit, Socket Mode); the only code, with the broker, that may import Slack or slack-go
internal/store          SQLite and Postgres persistence, leader fencing
internal/outbox         durable outbound posting (the versioned neutral message)
internal/queue          durable work queue worker
internal/render         agent output to neutral messages
internal/feedback       useful / not-useful votes on investigations
internal/audit          audit log
internal/telemetry      Prometheus metrics and OpenTelemetry traces
internal/broker         composition: Bindings, chat platform factory, lifecycle, health, leader election, shutdown order
internal/interact       interactions: pause intake, button presses, timeout sweep
internal/redact         shared secret redaction for display and audit text
internal/testhooks      test-only hooks, compiled in under the testhooks build tag
charts/agent-kourier    Helm chart
examples/ax             reference A2A agent for Google AX (separate module)
docs/                   the documentation site (mkdocs; docs/brand is the brand kit and is not published)
test/{fakeslack,stubagent,e2e,fixtures,kindmanifests,chart,...}
hack/kind/              kind tier scripts (fast, full, ax, sandbox)
hack/{docs,release,postgres}  doc generators, release scripts, Postgres test helpers
spike/                  throwaway A2A and Slack experiments
```

## Make targets

Run `make` for help. Only Go and the tools pinned in `go.mod` (`tool` directive) are needed;
golangci-lint runs as `go tool golangci-lint`, never a global install.

- `make fmt lint test build tidy`: `test` runs `go test -race ./...` and then `go test -race -tags testhooks ./internal/...`; `build` writes `bin/agent-kourier`.
- `make e2e-fast-up` / `e2e-full-up` / `e2e-ax-up` bring up the kind tiers (`hack/kind/`); `e2e-fast-down`,
  `e2e-full-down`, `e2e-ax-down` and `e2e-down` (all) tear them down. `e2e-fast` / `e2e-full` / `e2e-ax` run the E2E tests in that tier
  as an in-cluster Job (`hack/kind/e2e-run.sh`).
- AX fixtures use the separate `examples/ax` module and unmodified pinned AX/Substrate APIs; see
  `hack/kind/ax/README.md` for shared scenarios and capability differences. `make ax-smoke` exercises Bedrock/Haiku.
- `make image` builds `agent-kourier:dev`, the production image, from the `Dockerfile` (pinned Go and distroless
  images, in `hack/kind/lib.sh`); `make image-testhooks` builds `agent-kourier-e2e/agent-kourier-testhooks:dev`, the
  labelled test image the kind tiers run and nothing pushes; `make image-check` proves the production
  binary lacks the test hooks. All three need Docker.
- `make docs-ref` regenerates `docs/reference/` from the API types and the chart values; `make docs-build` builds the site (strict) and runs `docs-check`.
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
- No work-package, decision or risk IDs, `tasks/` paths, personal or workspace identifiers in public pages.
- Run `make docs-ref docs-check docs-build` before pushing a docs change. Pushing to `main` deploys the site.

## Commits

Conventional Commits (`feat(session): ...`, `test(e2e): ...`). Never add `Co-Authored-By`
or any AI attribution trailer to commits or PR descriptions.

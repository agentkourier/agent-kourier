# agent-courier

agent-courier is a Go broker between Alertmanager, Slack, and A2A agents (kagent 1.0 or any
A2A backend). It turns alerts and Slack messages into agent sessions and renders the agent's
output back into Slack threads. `spec.md` is the source of truth (decisions in section 5,
architecture in section 6); the build plan is `tasks/plans/agent-courier-pilot.md`.

## Layout

Directories appear when the work package that owns them lands; do not create empty ones.

```
cmd/courier/            entrypoint (--version, later the broker)
api/v1alpha1/           resource types (Binding, etc.)
internal/config         file loader for resources
internal/router         Binding resolution, dedup, rate limit, daily cap
internal/session        session manager (turns, reply queue, controls, recovery)
internal/agent          A2A client: streaming, reconnect, typed outcomes, client pool
internal/dialect        agent-dialect adapters (kagent, generic a2a)
internal/identity       Binding token source and credentials for the agent pool
internal/secret         redacting Secret value type
internal/chat/slack     Slack adapter
internal/alert/alertmanager   Alertmanager receiver
internal/store          SQLite (later Postgres) persistence
internal/outbox         durable outbound posting
internal/render         agent output to Slack messages
internal/audit          audit log
test/{fakeslack,stubagent,e2e,fixtures}
hack/kind/              kind cluster scripts
spike/                  throwaway A2A and Slack experiments
```

## Make targets

Run `make` for help. Only Go and the tools pinned in `go.mod` (`tool` directive) are needed;
golangci-lint runs as `go tool golangci-lint`, never a global install.

- `make fmt lint test build tidy`: `test` runs `go test -race ./...`; `build` writes `bin/courier`.
- `make e2e-fast-up` / `e2e-full-up` bring up the kind tiers (`hack/kind/`); `e2e-fast-down`,
  `e2e-full-down` and `e2e-down` (both) tear them down. `e2e-fast` / `e2e-full` run the E2E tests in that tier
  as an in-cluster Job (`hack/kind/e2e-run.sh`).
- Add tools with `go get -tool <module>/cmd/<tool>@<version>`.

## Work-package workflow

1. The orchestrator dispatches one WP from the plan to `courier-implementer` on branch
   `wp/<id>-<slug>` in its own worktree.
2. Implementer works test-first, runs `make fmt lint test` (plus `make e2e-fast` when the
   WP says so), commits, and reports.
3. `courier-reviewer` gates the diff. Fixes go back to the same implementer, at most two rounds.
4. The orchestrator squash-merges approved branches onto `main`.

Budget: at most 400 changed lines per WP (excluding `go.sum`, generated code, fixtures),
one concern per WP. If it will not fit, stop and propose a split.

## Go conventions

- Standard library `testing`, table-driven where it fits; e2e tests use the `e2e` build tag.
- Accept interfaces, return structs. Wrap errors with `%w` and context.
- `context.Context` is the first parameter of anything that does I/O.
- No global mutable state (the ldflags-set `version` is the sole exception). No `panic`
  outside `main` and test helpers. Exported identifiers have doc comments.
- No new module dependency unless the WP names it.
- Never log, print, or commit tokens or Secret contents; test tokens are fake constants.

## Commits

Conventional Commits (`feat(session): ...`, `test(e2e): ...`). Never add `Co-Authored-By`
or any AI attribution trailer to commits or PR descriptions.

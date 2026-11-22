# AX add-on

An ADK agent on Amazon Bedrock that serves A2A, with one tool, `ask_user`, whose question a person answers in the
thread. A Binding drives it with the existing `a2a` dialect and no change to Agent Kourier. It is a Go module of its own,
so ADK, AX and the AWS SDK never enter Agent Kourier's binary. It is released with Agent Kourier as the image
`ghcr.io/agentkourier/agent-kourier-ax`, which the chart runs with `addons.ax`; it can also run inside Google AX as a
Task, from an image you build with `Dockerfile.ax`.

It is experimental.

The documentation site has the user's pages:

- [Add-ons](../../docs/explanation/add-ons.md): what an add-on is.
- [Connect the AX add-on](../../docs/how-to/connect-ax.md): install it with the chart, the AgentBackend and Binding that
  reach it, and the route inside Google AX (`task.yaml`, `template-overlay.yaml`, `egress-policy.json`, `gateway.conf`,
  `agent-kourier.yaml`).
- [AX add-on](../../docs/reference/ax.md): how A2A maps to ADK, restarts, the store, replicas, the caller check, every
  setting and the telemetry.
- [Limits](../../docs/reference/limits.md#ax-add-on): every limit, with its value.

This file keeps the developer's notes: building the add-on and its tests.

## Build and test

From the repository root:

```sh
make ax-test          # the module's tests, with the race detector
make ax-lint
make ax-image         # agent-kourier-ax:dev, the image the chart runs
make ax-runner-image  # agent-kourier-ax-runner:dev, the image of the route inside Google AX
make ax-smoke         # a real Bedrock question, answer and replay
```

The store's tests run on SQLite, and on Postgres too when `AGENTKOURIER_POSTGRES_DSN` names one
(`hack/postgres/run.sh make ax-test`). Unit tests use a mock of Bedrock's event streams and need no credentials.

`ax-smoke` reads the mode-600 sandbox credentials file (`SANDBOX_ENV` overrides its path), asks a real model a
question, answers it and replays a message through a loopback A2A server, and prints no credentials. It spends Bedrock
usage.

AX is pinned to the revision in `go.mod`. `Dockerfile.ax` builds AX's task runner (`cmd/ax-task-runner`) from that
module, and the AX kind tier (`make e2e-ax-up`, `make e2e-ax`; see `hack/kind/ax/README.md`) runs the agent with a
deterministic model (`cmd/e2e-agent`, `e2emodel`, `Dockerfile.e2e`), which neither the released image nor the runner
image contains.

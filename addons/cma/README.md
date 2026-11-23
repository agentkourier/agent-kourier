# Claude Managed Agents add-on

Agent Kourier talks to agents over A2A. Claude Managed Agents (CMA) has no A2A endpoint: it runs the agent loop and
exposes sessions through the Anthropic API. This add-on is an A2A server in front of one CMA agent, so a Binding drives
it with the existing `a2a` dialect and no change to Agent Kourier. It is a Go module of its own, released with Agent
Kourier as the image `ghcr.io/agentkourier/agent-kourier-cma`, and the chart runs it with `addons.cma`.

It is experimental. It was tested against a self-hosted CMA environment (the sandbox runs in your own cluster) with a
Haiku agent; it has not been run against a cloud environment.

The documentation site has the user's pages:

- [Add-ons](../../docs/explanation/add-ons.md): what an add-on is.
- [Connect Claude Managed Agents](../../docs/how-to/connect-claude-managed-agents.md): install it with the chart, and
  the AgentBackend and Binding that reach it.
- [Claude Managed Agents add-on](../../docs/reference/claude-managed-agents.md): how A2A maps to sessions and turns,
  restarts, replicas, the caller check, every setting and the telemetry.
- [Limits](../../docs/reference/limits.md#claude-managed-agents-add-on): every limit, with its value.

This file keeps the developer's notes: running the add-on by hand, and its tests.

## Run it with Slack on kind

`kind/` holds what the end-to-end test used: Agent Kourier in a kind cluster, the adapter on the Docker host.

1. Create a CMA agent with bash set to `always_ask` (so approvals show up) and a self-hosted environment whose worker
   runs in your cluster.
2. Create a Slack app from `kind/slack-app-manifest.yaml`, install it, create an app-level token with
   `connections:write`, and invite the bot to the test channel.
3. Run the adapter on the host. Kind nodes reach the host as `host.docker.internal`:

   ```sh
   cd addons/cma
   printf '%s' "$(openssl rand -hex 32)" > cma-token
   CMA_AGENT_ID=agent_... CMA_ENVIRONMENT_ID=env_... AUTH_TOKEN_FILE=cma-token \
   LISTEN_ADDR=0.0.0.0:8090 PUBLIC_URL=http://host.docker.internal:8090/a2a \
   go run ./cmd/adapter
   ```

   The token file has no trailing newline: Agent Kourier sends the Secret's value as it is, and refuses one with a
   control character. The adapter keeps its state in `cma-addon.db` in the working directory.
4. Build and load the image (`make image`, then `kind load docker-image agent-kourier:dev --name <cluster>`), create the `slack` Secret
   (`botToken`, `appToken`) and the `cma-adapter-token` Secret
   (`kubectl -n agent-kourier create secret generic cma-adapter-token --from-file=token=cma-token`) in the
   `agent-kourier` namespace, replace `C0123456789` in `kind/config/kourier.yaml` with the
   channel's ID, create the `cma-kourier-config` ConfigMap from it (key `kourier.yaml`), and install the chart with
   `kind/values.yaml`.
5. Mention the bot in the channel. Replies in the thread continue the same session.

## Test it

Unit tests use a fake backend and spend nothing. The store's tests run on SQLite, and on Postgres too when
`AGENTKOURIER_POSTGRES_DSN` names one (each test makes and drops a schema of its own):

```sh
cd addons/cma && go test -race ./...
```

`livecheck` is a separate module that drives a running adapter with Agent Kourier's own A2A client and `a2a` dialect, the
code a Binding uses. Its offline test runs against a scripted backend, behind the caller check; its live scenarios (`text`, `allowed`, `approve`,
`continuity`, `deny`, `cancel`) need an agent with bash set to `always_ask` and spend model tokens. `ONLY=text,deny`
runs a subset.

```sh
cd addons/cma/livecheck
go test -race ./...
ADAPTER_URL=http://127.0.0.1:8090/a2a ADAPTER_TOKEN="$(cat ../cma-token)" go run .
```

CI runs the same through `make cma-test` (with Postgres), which also runs the tests of the shared kit's packages it
links (`addons/kit`) under the add-on's module graph, and `make cma-lint`, and `make cma-image` builds the image.


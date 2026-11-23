# Claude Managed Agents add-on

Agent Kourier talks to agents over A2A. Claude Managed Agents (CMA) has no A2A endpoint: it runs the agent loop and
exposes sessions through the Anthropic API. This add-on is an A2A server in front of one CMA agent, so a Binding drives
it with the existing `a2a` dialect and no change to Agent Kourier. It is a Go module of its own, released with Agent
Kourier as the image `ghcr.io/agentkourier/agent-kourier-cma`, and the chart runs it with `addons.cma`.

It is experimental. It was tested against a self-hosted CMA environment (the sandbox runs in your own cluster) with a
Haiku agent, and, on a build before the release (22 and 23 November 2026), on Postgres with two replicas, where a leader
killed during an approval left the standby to take over and complete the same task. It has not been run against a cloud
environment.

The documentation site has the user's pages:

- [Add-ons](../../docs/explanation/add-ons.md): what an add-on is.
- [Connect Claude Managed Agents](../../docs/how-to/connect-claude-managed-agents.md): install it with the chart, and
  the AgentBackend and Binding that reach it.
- [Claude Managed Agents add-on](../../docs/reference/claude-managed-agents.md): how A2A maps to sessions and turns,
  restarts, replicas, the caller check, every setting and the telemetry.
- [Limits](../../docs/reference/limits.md#claude-managed-agents-add-on): the add-on's own limits, and
  [those every add-on shares](../../docs/reference/limits.md#every-add-on), each with its value.

This file keeps the developer's notes: running the add-on by hand, and its tests.

## Run it with Slack on kind

`kind/` holds what the end-to-end test used: Agent Kourier and the add-on in a kind cluster, both installed by the chart
(`addons.cma`) from images built here, with Agent Kourier's config in a ConfigMap. Run the commands from the repository
root.

1. Create a CMA agent with bash set to `always_ask` (so approvals show up) and a self-hosted environment whose worker
   runs in your cluster.
2. Create a Slack app from `kind/slack-app-manifest.yaml`, install it, create an app-level token with
   `connections:write`, and invite the bot to the test channel.
3. Build both images, `agent-kourier:dev` and `agent-kourier-cma:dev`, and load them into the cluster:

   ```sh
   make image cma-image
   kind load docker-image agent-kourier:dev agent-kourier-cma:dev --name <cluster>
   ```

4. Create the Secrets in the `agent-kourier` namespace: `slack` (the Slack app's tokens), `cma-anthropic` (the API key)
   and `cma-adapter-token` (the token Agent Kourier presents to the add-on):

   ```sh
   kubectl create namespace agent-kourier
   kubectl -n agent-kourier create secret generic slack \
     --from-literal=botToken="$SLACK_BOT_TOKEN" --from-literal=appToken="$SLACK_APP_TOKEN"
   kubectl -n agent-kourier create secret generic cma-anthropic --from-literal=apiKey="$ANTHROPIC_API_KEY"
   kubectl -n agent-kourier create secret generic cma-adapter-token --from-literal=token="$(openssl rand -hex 32)"
   ```

   `--from-literal` stores the token with no trailing newline: Agent Kourier sends the Secret's value as it is, and
   refuses one with a control character.
5. In `kind/config/kourier.yaml`, replace `C0123456789` with the chat channel's ID, and `C0ALERTS000` and `B0ALERTS00`
   with the alert channel's ID and the bot ID of the app that posts alerts there. Create the ConfigMap from it:

   ```sh
   kubectl -n agent-kourier create configmap cma-kourier-config \
     --from-file=kourier.yaml=addons/cma/kind/config/kourier.yaml
   ```

6. In `kind/values.yaml`, replace `agent_REPLACE` and `env_REPLACE` with the agent's and the environment's IDs, and
   install the chart. The release must be `agent-kourier`, because the AgentBackend in `kourier.yaml` names the add-on's
   Service, `agent-kourier-cma`:

   ```sh
   helm install agent-kourier charts/agent-kourier -n agent-kourier -f addons/cma/kind/values.yaml
   kubectl -n agent-kourier rollout status deployment/agent-kourier-cma
   ```

   The add-on keeps its state in SQLite on the claim `agent-kourier-cma-data`.
7. Mention the bot in the chat channel. Replies in the thread continue the same session.

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
kubectl -n agent-kourier port-forward service/agent-kourier-cma 8090:8090 &
ADAPTER_URL=http://127.0.0.1:8090/a2a \
ADAPTER_TOKEN="$(kubectl -n agent-kourier get secret cma-adapter-token -o jsonpath='{.data.token}' | base64 -d)" \
go run .
```

CI runs the same through `make cma-test` (with Postgres), which also runs the tests of the shared kit's packages it
links (`addons/kit`) under the add-on's module graph, and `make cma-lint`, and `make cma-image` builds the image.


# Connect Claude Managed Agents

For a platform team with an agent in Claude Managed Agents: at the end, the chart runs the Claude Managed Agents
add-on, and a Binding sends its threads to that agent. Each thread is one session of the agent, and a tool call whose
permission policy asks first waits on Approve and Deny in the thread. The add-on is experimental.

[Add-ons](../explanation/add-ons.md) says what an add-on is. The
[Claude Managed Agents add-on](../reference/claude-managed-agents.md) reference has how the add-on maps A2A to sessions,
every setting, and what a restart keeps.

## Why Claude Managed Agents behind a Binding

Claude Managed Agents runs the agent: Anthropic runs the model loop, and an agent is a stored, versioned object with its
own tools, permission policies and skills. Each session gets a sandbox, which a self-hosted environment runs in your own
cluster, next to the systems it works on. What it does not have is a way into chat: putting such an agent in a channel
means writing a chat app, with its threads, approval buttons and audit.

Agent Kourier is that front door already. A Binding gives the agent a session per thread, triggers such as the alerts
another app posts, audit, metrics and traces, on Slack, and on Microsoft Teams and webhooks (both experimental), with
tool approvals as buttons on Slack and Teams. The add-on is the piece between them: an A2A server in front of one agent, so the
broker needs no change. A tool set to `always_ask` in the agent's permission policy becomes Approve and Deny in the
thread, so a command in your cluster runs only after a person approves it.

## Before you start

- **An agent in Claude Managed Agents**, and its ID. Give each tool that a person should approve first the
  `always_ask` permission policy.
- **A self-hosted environment whose worker runs in your cluster**, and its ID. Cloud environments have not been tested
  with the add-on and are not supported. The add-on does not refuse one.
- **An Anthropic API key** for the organization that holds the agent.
- **Agent Kourier installed with the Helm chart** ([Install with Helm](install-with-helm.md)), from a release whose
  [release notes](../release-notes.md) list the add-on. The 0.6.0 chart has no `addons` block, and its values schema
  refuses one.
- **For more than one replica of the add-on, a Postgres database of its own**, apart from Agent Kourier's.

The commands below use the release `agent-kourier` in the namespace `agent-kourier`. The add-on's Service is then
`agent-kourier-cma`; in general it is `<fullname>-cma`.

## 1. Create the Secrets

The add-on needs the API key and a token that Agent Kourier presents to it. Both go in Secrets in the release's
namespace:

```sh
kubectl -n agent-kourier create secret generic anthropic \
  --from-literal=apiKey="${ANTHROPIC_API_KEY}"
kubectl -n agent-kourier create secret generic cma-token \
  --from-literal=token="$(openssl rand -hex 32)"
```

`--from-literal` stores the value with no trailing newline. Keep it that way: Agent Kourier sends the token as the
Secret holds it, and refuses to send a value with a control character, a newline included.

With Postgres, create a third Secret with the add-on's DSN:

```sh
kubectl -n agent-kourier create secret generic cma-postgres \
  --from-literal=dsn='postgres://cma:...@postgres.example.internal:5432/cma'
```

## 2. Turn the add-on on

Add the block to your values file:

```yaml
addons:
  cma:
    enabled: true
    agentId: agent_0123456789 # (1)!
    environmentId: env_0123456789 # (2)!
    anthropicApiKeySecret: {name: anthropic, key: apiKey}
    auth:
      tokenSecret: {name: cma-token} # (3)!
    sessionIdleTTL: 2160h # (4)!
rbac:
  secretNames:
    agent-kourier: [cma-token] # (5)!
```

1. The agent's ID, as Claude Managed Agents shows it. Sessions run its latest version.
2. The self-hosted environment's ID.
3. The key defaults to `token`. The add-on reads the Secret as a file of accepted tokens, one per line, and reads it
   again every few seconds, so a change needs no restart.
4. Set it to the Binding's `session.threadTTL` (step 4).
5. On custom resources, the chart grants Agent Kourier only the Secrets it can see. A Binding you apply yourself names
   `cma-token` in `identity.tokenSecretRef`, so grant it here. A Binding in `config.bindings` needs no entry: the chart
   grants the Secrets that `config.*` names.

For more than one replica, keep the state in Postgres. The replicas elect a leader through the database, and a standby
forwards what it receives to the leader:

```yaml
addons:
  cma:
    replicaCount: 2
    store:
      type: postgres
      postgres:
        dsnSecret: {name: cma-postgres, key: dsn}
```

With SQLite, the default, the chart refuses `replicaCount` above 1, keeps the database on a ReadWriteOnce volume, and
replaces the pod with the Recreate strategy. With `networkPolicy.enabled`, the add-on gets a NetworkPolicy of its own,
and Postgres needs `addons.cma.networkPolicy.postgres.to`. [Helm values](../reference/helm-values.md#addons) has every
value of the block.

Upgrade the release with the file, then wait for the add-on:

```sh
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
  -n agent-kourier -f my-values.yaml
kubectl -n agent-kourier rollout status deployment/agent-kourier-cma
kubectl -n agent-kourier logs deployment/agent-kourier-cma | grep 'adapter listening'
```

The add-on serves only after its start-up work: it reads its store, checks each thread's session against the agent's
sessions in Claude Managed Agents, and fails the turns a restart cut off. A setting it cannot use stops it at start,
with an error that names the variable.

## 3. Declare the backend and the Binding

The AgentBackend names the add-on's A2A endpoint with the `a2a` dialect. The Binding sends the token through
`identity.tokenSecretRef`, as `Authorization: Bearer`:

```yaml
apiVersion: agentkourier.dev/v1alpha1
kind: AgentBackend
metadata:
  name: cma
  namespace: agent-kourier
spec:
  dialect: a2a
  url: http://agent-kourier-cma.agent-kourier.svc:8090/a2a # (1)!
---
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata:
  name: cma-chat
  namespace: agent-kourier
spec:
  agent:
    backendRef: {name: cma}
    namespace: cma # (2)!
    name: claude
  identity:
    userId: agent-kourier-cma
    tokenSecretRef: {name: cma-token} # (3)!
  chat:
    connectionRef: {name: slack}
    channel: C0123456789 # the channel's ID, not its name
    output: live
  interactions:
    toolApprovals: true # (4)!
  session:
    threadTTL: 2160h
```

1. The URL the agent card names too. Agent Kourier fetches the card at this URL plus `/.well-known/agent-card.json`.
2. On the `a2a` dialect `agent.namespace` and `agent.name` are labels: the URL selects the agent, and they are not
   sent.
3. The same Secret the add-on reads. A Binding in another namespace reads a Secret in its own namespace, so that
   namespace needs a copy of the token, and the add-on's Secret must hold it.
4. Without it, Agent Kourier denies every tool call that asks for approval, with the reason that the Binding does
   not allow tool approvals. `approverGroups` limits who may decide; see [Let the agent ask questions](agent-questions.md).

```sh
kubectl apply --dry-run=server -f cma.yaml
kubectl apply -f cma.yaml
kubectl -n agent-kourier get akbinding cma-chat
```

On the ConfigMap route, or to let the chart render them, put the same specs in `config.agentBackends.cma.spec` and
`config.bindings.cma-chat.spec`.

### Or send the token in a header of your own

To keep `identity` for something else, the AgentBackend can carry the token in a header instead. Name the header in the
add-on's values, and put an entry with that name in the AgentBackend's `headers`, whose value comes from the Secret:

```yaml
addons:
  cma:
    auth:
      tokenSecret: {name: cma-token}
      header: X-Cma-Token
```

```yaml
spec:
  dialect: a2a
  url: http://agent-kourier-cma.agent-kourier.svc:8090/a2a
  headers:
    - name: X-Cma-Token
      valueSecretRef: {name: cma-token}
```

The header's value is then the whole token, with no `Bearer`. The header cannot be one that carries the Binding's
identity: `Authorization`, `Proxy-Authorization`, `Cookie`, `X-User-Id`, `X-Agent-Name`, `X-Share-Token` or
`X-Kagent-Insecure-Runtime-Identity`. The Secret is read in the AgentBackend's namespace, and the Binding's `identity`
still needs a `userId` or a `tokenSecretRef`.

## 4. Match the session lifetimes

Agent Kourier gives a thread a new A2A context once it has had no activity for the Binding's `session.threadTTL`
(default `2160h`). The add-on archives a context's session once the context has had no turn for `sessionIdleTTL`
(`SESSION_IDLE_TTL`, the same default), and an archived session cannot be used again. Keep the two equal:

- Shorter than `threadTTL`, the add-on archives sessions that a thread can still continue, and the thread's next turn
  starts a new session without the old history.
- Longer than `threadTTL`, sessions that no thread will use again stay unarchived until it passes.

`sessionIdleTTL` does not free an idle sandbox's memory: the environment's own idle timeout does that, and the session
stays resumable.

Set `toolResultTimeout` above your slowest command. A turn that waits on a tool the sandbox runs, with no event from the
session for that long, is interrupted and fails with Agent Kourier's timeout notice: that is how a sandbox that cannot be
scheduled shows up, and a command that runs silently for longer is cut off the same way.

## Check it

Mention the bot in the channel. The answer streams into the thread, and replies in the thread continue the same
session. Ask for something that runs a tool set to `always_ask`: the thread shows Approve and Deny, and the command runs
only after a press.

If the thread shows nothing, read the two logs:

```sh
kubectl -n agent-kourier logs deployment/agent-kourier | grep 'session turn'
kubectl -n agent-kourier logs deployment/agent-kourier-cma
```

`refused an A2A request` in the add-on's log, with the reason `missing` or `invalid`, means Agent Kourier sent no
token, or one the add-on does not hold: check that the Binding's Secret and the add-on's hold the same value, with no
trailing newline in the Binding's.

## Rotate the token

The add-on accepts every token in its file, so a new one can be added before the old one is removed. Give the add-on a
Secret of its own (not the one the Binding reads), then:

1. Add the new token to the add-on's Secret, on a line of its own.
2. Change the Binding's Secret to the new token ([Rotate the Binding token](rotate-binding-token.md)).
3. Remove the old line from the add-on's Secret.

The kubelet updates the mounted Secret after its own sync delay, and the add-on reads the file again every few seconds.
If a read fails or finds no token, the add-on keeps the tokens it had.

## Turn it off

`addons.cma.replicaCount: 0` stops the add-on and keeps its volume or database. `addons.cma.enabled: false` removes its
objects; the SQLite volume's claim is kept unless `addons.cma.persistence.retainOnUninstall` is `false`, and the
sessions in Claude Managed Agents stay until they are archived there.

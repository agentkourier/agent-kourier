# Run Agent Kourier on a cluster

This guide takes one install from nothing to an answered mention in a Slack thread, on a
cluster you already have. It follows the first real deployment, so the order is the order
that worked, and the checks are the ones that caught problems.

The [sandbox guide](../hack/kind/sandbox/README.md) does the same on a laptop in kind. Use
that to try the project. Use this to run it.

You do not need an agent for the first install. Without one, Agent Kourier connects to
Slack and tells the thread it cannot reach the agent. That reply proves Slack in, Slack
out, the volume and the policies, and it makes the agent the only thing left to wire.

## What you need

- A Kubernetes cluster whose nodes can pull your image, and a namespace for Agent Kourier.
- A Slack workspace where you can create an app, and a channel the bot can join.
- A registry for the image. Nodes need read access to it.
- Optionally an A2A agent. [Step 7](#7-connect-an-agent) covers the choices.

## 1. Create the Slack app

Create the app from a manifest. [`hack/kind/sandbox/slack-app-manifest.yaml`](../hack/kind/sandbox/slack-app-manifest.yaml)
holds the scopes, the three bot events, interactivity and Socket Mode, so copy it and change
the name.

Then:

1. Install the app to the workspace and copy the bot token, which starts with `xoxb-`.
2. Create an app-level token with the `connections:write` scope. Socket Mode needs it, and
   it starts with `xapp-`.
3. Invite the bot to the channel.
4. Copy the channel ID, which starts with `C`. The Binding takes the ID, not the name.

The bot's display name is the handle people type. It is Slack's setting and Agent Kourier
does not read it.

One app token should have one consumer. Socket Mode spreads events across every open
connection, so a second process on the same token, a laptop sandbox for instance, receives
some of them and your install seems to ignore messages. Stop the other one first.

## 2. Get an image the nodes can pull

A tagged release publishes a multi-arch image and the chart. Without a release, build one.
You need docker, helm and jq, and a buildx builder that can make attestations:

```sh
docker buildx create --name agent-kourier-release --driver docker-container
PLATFORMS=linux/amd64 BUILDER=agent-kourier-release make release-image VERSION=v0.0.1-dev.1
```

That builds an image and a chart package in `bin/release` and pushes nothing. To push, run
it from a clean checkout, tag that commit with the same version, log in to the registry, and
add:

```sh
PUSH=1 REGISTRY=<registry>/<repo>/agent-kourier CHART_REGISTRY=oci://<registry>/<repo>/charts \
  make release-image VERSION=v0.0.1-dev.1
```

Three things that cost time:

- A push refuses a tree with uncommitted or untracked files, and a version whose tag is not
  at HEAD. Keep `my-values.yaml` outside the checkout. A build that pushes nothing skips both
  checks.
- Many registries make tags immutable. Each build needs a new version then.
- The chart comes out stamped with the image digest. You set no image values.

The first pull of a new repository path is the real test of node access. If the nodes pull
with their own credentials, check that the repository falls under whatever policy grants
that access before you debug anything else.

## 3. Create the secrets

Create the namespace first, with `kubectl create namespace agent-kourier`. A sealed secret is
bound to its namespace and name, so it cannot be applied before the namespace exists.

The ChatConnection's Secret holds two keys with exactly these names:

```sh
kubectl -n agent-kourier create secret generic agent-kourier-slack \
  --from-literal=botToken="$SLACK_BOT_TOKEN" \
  --from-literal=appToken="$SLACK_APP_TOKEN"
```

A Binding sends an identity to the agent. With only a `userId`, Agent Kourier sends it as
`X-User-Id`. With a `tokenSecretRef`, it sends that Secret's `token` key as a bearer token
instead. Add the Secret when the agent or a front door in front of it verifies tokens:

```sh
kubectl -n agent-kourier create secret generic agent-kourier-agent-token --from-literal=token="$AGENT_TOKEN"
```

Tokens never go in the values file. The chart refuses a value that looks like a Slack token.

## 4. Write the config

The chart takes the three resource kinds under `config`. This is a complete first install,
with an agent that does not exist yet:

```yaml
# my-values.yaml
config:
  chatConnections:
    slack:
      spec:
        platform: slack
        mode: socket
        credentialsSecretRef: {name: agent-kourier-slack}
  agentBackends:
    agent:
      spec:
        dialect: a2a
        url: http://my-agent.my-namespace.svc.cluster.local:8083/a2a/   # used exactly as written
  bindings:
    dogfood:
      spec:
        agent:
          backendRef: {name: agent}
          namespace: my-namespace
          name: my-agent
        identity:
          userId: agent-kourier
        chat:
          connectionRef: {name: slack}
          channel: C0123456789
          output: live
        interactions: {askUser: false, toolApprovals: false}
```

The Binding's `agent.namespace` and `agent.name` name the agent on the backend. Add
`tokenSecretRef: {name: agent-kourier-agent-token}` under `identity` when step 3 created that
Secret.

Two things to know about `url`. The `a2a` dialect uses it exactly as you write it, and it
follows no redirect. A router that wants a trailing slash answers a POST without one with a
307, so write the slash if your agent's route has one. The card is fetched under the URL
with the slash trimmed.

The `kagent-v1` dialect takes the front door's URL and adds the agent's path itself.

## 5. Install

Install the chart you pushed. It is stamped with its image, so it needs no image values:

```sh
helm install agent-kourier oci://<registry>/<repo>/charts/agent-kourier --version 0.0.1-dev.1 \
  -n agent-kourier -f my-values.yaml
```

A chart from the source tree has no image, and the render fails without one. Pass the one you
built:

```sh
helm install agent-kourier ./charts/agent-kourier -n agent-kourier -f my-values.yaml \
  --set image.repository=<registry>/<repo>/agent-kourier --set image.digest=sha256:<digest>
```

The defaults run one replica with SQLite on a 1 GiB volume. The [chart README](../charts/agent-kourier/README.md)
covers Postgres, a second replica, the NetworkPolicy and the Secrets RBAC.

If you turn on `networkPolicy` on a cluster that runs Cilium, read
[Cilium and the API server](../charts/agent-kourier/README.md#cilium-and-the-api-server)
first. Without the extra policy the pod cannot start.

## 6. Check it

```sh
kubectl -n agent-kourier get pods
kubectl -n agent-kourier logs deploy/agent-kourier
```

A healthy start logs these lines, in this order:

```
config reloaded
serving a ChatConnection
opened the store
ready
slack: socket mode connecting
slack: socket mode websocket opened
slack: socket mode connected
```

`ready` means the config loaded and the store opened. It does not mean Slack connected.
Wait for the last line. It carries a `num_connections` field, the number of connections open
on the app token. A value above 1 on a fresh start is worth a look.

Then mention the bot in the channel. With no agent, the thread gets "I can't reach the agent
right now, and I'm trying again." The log shows `session turn started` and then
`session turn failed` with the reason in `error`.

## 7. Connect an agent

Any agent whose card advertises streaming and a JSON-RPC interface works through the `a2a`
dialect. Agent Kourier reads A2A 1.0 cards and A2A 0.3 cards.

**kagent 1.0** runs every session on Agent Substrate, which needs the pod-certificate APIs.
Check your cluster first, with the commands in
[Running kagent 1.0 off kind](../README.md#running-kagent-10-off-kind).

**kagent 0.9.x** works through the `a2a` dialect, and it is what runs on a cluster without
those APIs. The agent's A2A endpoint is the controller's, at
`http://<controller-service>.<namespace>.svc.cluster.local:8083/api/a2a/<agent-namespace>/<agent>/`.
Keep the trailing slash. The Service name depends on the release name. Argo CD names the
release after the Application, so an Application called `cluster-gitops-kagent` makes the
Service `cluster-gitops-kagent-controller`. Run `kubectl get svc -n <namespace>` and use
what it shows.

Know four limits of this setup:

- A 0.3 agent has no task list. After a restart, or after a send whose outcome is unknown,
  Agent Kourier cannot ask whether the agent already has the turn. It sends again, and the
  agent may run the turn twice. That is harmless for a read-only agent and not for one that
  changes things. On the `a2a` dialect Agent Kourier does not see a tool call, so the agent's
  own tool list and the role behind its tools are the only fence on a write. Tool approvals
  from Slack are planned for every backend.
- The `a2a` dialect shows a text `input-required` pause as a question and sends the reply as
  text, but only when the Binding sets `interactions.askUser: true`. Otherwise it refuses the
  question, and the example above sets it to false. The dialect does not negotiate kagent's
  structured human-in-the-loop extension. Whether kagent 0.9.x ever pauses with a text
  question has not been tested.
- The `a2a` dialect never renders tool calls as step cards, for any agent on it. The answer
  text renders.
- The controller accepted a placeholder token in the first install, so nothing verifies the
  Binding's identity. Put a verifying front door in front of it if that matters.

**Do not install kagent 0.9.x with its defaults.** Its chart binds the bundled tool server
to `cluster-admin` and serves write tools such as delete, apply and exec. For an agent that
answers alerts, run the tool server with `--read-only`, bind it to a role that cannot read
Secrets, and list the agent's tools by name. The sandbox tier does this for kagent 1.0, in
[`hack/kind/sandbox`](../hack/kind/sandbox). The defaults also run ten bundled agents, about
2.3 GiB of memory requests, which a small cluster may not have.

## Stop it and keep the data

Set `replicaCount: 0`. Agent Kourier stops, and the volume stays.

On `helm uninstall` the chart keeps the claim by default. `persistence.retainOnUninstall: true`
sets `helm.sh/resource-policy: keep` on it. A GitOps tool may not honour that annotation when
it prunes, and the first install did not test it. If the data matters, create the claim
yourself and set `persistence.existingClaim`, so no release owns it. Stop with
`replicaCount: 0` and not by removing the app.

## Where next

- [Troubleshooting](troubleshooting.md) for the failures a first install runs into.
- [Observability](observability.md) for metrics, traces and the audit log.
- [Stored data](data.md) for what Agent Kourier keeps and for how long.

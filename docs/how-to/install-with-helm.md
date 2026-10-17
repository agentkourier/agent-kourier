# Install with Helm

For a platform engineer with a Kubernetes cluster: at the end, Agent Kourier runs from the Helm chart and answers a
mention in a Slack thread.

The checks below are the ones that catch the usual problems. You do
not need an agent for the first install. Without one, Agent Kourier connects to Slack and tells the thread it cannot
reach the agent. That reply proves Slack in, Slack out, the volume and the policies, and leaves the agent as the only
thing to wire.

## Before you start

- A Kubernetes cluster whose nodes can pull your image, and a namespace for Agent Kourier.
- A Slack app with a bot token and an app-level token, invited to a channel: see [Configure Slack](configure-slack.md).
- A registry for the image. The nodes need read access to it.
- `docker` with a buildx builder that can make attestations, `helm` and `jq`.

## Build an image the nodes can pull

A tagged release publishes a multi-arch image and the chart. There is no release yet, so build one:

```sh
docker buildx create --name agent-kourier-release --driver docker-container
PLATFORMS=linux/amd64 BUILDER=agent-kourier-release make release-image VERSION=v0.0.1-dev.1
```

That builds an image and a chart package in `bin/release` and pushes nothing. To push, run it from a clean checkout,
tag that commit with the same version, log in to the registry, and add:

```sh
PUSH=1 REGISTRY=<registry>/<repo>/agent-kourier CHART_REGISTRY=oci://<registry>/<repo>/charts \
  make release-image VERSION=v0.0.1-dev.1
```

- A push refuses a tree with uncommitted or untracked files, and a version whose tag is not at `HEAD`. Keep
  `my-values.yaml` outside the checkout. A build that pushes nothing skips both checks.
- Many registries make tags immutable. Each build then needs a new version.
- The chart comes out stamped with the image digest, so you set no image values.

The first pull of a new repository path is the real test of node access. If the nodes pull with their own
credentials, check that the repository falls under whatever policy grants that access before you debug anything else.

## Create the Secrets

Create the namespace first. A sealed secret is bound to its namespace and name, so it cannot be applied before the
namespace exists.

```sh
kubectl create namespace agent-kourier
```

The ChatConnection's Secret holds two keys with exactly these names:

```sh
kubectl -n agent-kourier create secret generic agent-kourier-slack \
  --from-literal=botToken="$SLACK_BOT_TOKEN" \
  --from-literal=appToken="$SLACK_APP_TOKEN"
```

If the agent, or a front door in front of it, verifies tokens, first
[create the Binding identity](create-binding-identity.md) and obtain its token from the accepted issuer.
Agent Kourier does not mint `$AGENT_TOKEN`; it is the issued bearer token. Store it in the Binding's namespace, which
for a Binding under the chart's `config` is the release namespace (`agent-kourier` here) unless the entry sets its
own `namespace`. The Dex example on that page
stores its token itself, as `payments-agent-token` in `payments`; for this install, change its last command to
`kubectl -n agent-kourier create secret generic agent-kourier-agent-token`. With a token from any other issuer:

```sh
kubectl -n agent-kourier create secret generic agent-kourier-agent-token --from-literal=token="$AGENT_TOKEN"
```

Tokens never go in the values file. The chart refuses a value that looks like a Slack token.

## Write the values

The chart takes the three resource kinds under `config`. This is a complete first install, with an agent that does
not exist yet:

```yaml title="my-values.yaml"
config:
  chatConnections:
    slack:
      spec:
        platform: slack
        credentialsSecretRef: {name: agent-kourier-slack}
  agentBackends:
    agent:
      spec:
        dialect: a2a
        url: http://my-agent.my-namespace.svc.cluster.local:8083/a2a/ # (1)!
  bindings:
    my-binding:
      spec:
        agent:
          backendRef: {name: agent}
          namespace: my-namespace
          name: my-agent
        identity:
          userId: agent-kourier # (2)!
        chat:
          connectionRef: {name: slack}
          channel: C0123456789 # (3)!
          output: live
        interactions: {askUser: false}
```

1. The `a2a` dialect sends to this URL exactly as written and follows no redirect. Write the trailing slash if the
   agent's route has one.
2. Add `tokenSecretRef: {name: agent-kourier-agent-token}` here when you created that Secret.
3. The channel ID, which starts with `C` (`G` for an older private channel), not the channel's name.

Every field is in the [Binding](../reference/binding.md), [AgentBackend](../reference/agentbackend.md) and
[ChatConnection](../reference/chatconnection.md) references, and every chart value in [Helm values](../reference/helm-values.md).

## Install the chart

Install the chart you pushed. It is stamped with its image:

```sh
helm install agent-kourier oci://<registry>/<repo>/charts/agent-kourier --version 0.0.1-dev.1 \
  -n agent-kourier -f my-values.yaml
```

A chart from the source tree has no image, and the render fails without one. Pass the one you built:

```sh
helm install agent-kourier ./charts/agent-kourier -n agent-kourier -f my-values.yaml \
  --set image.repository=<registry>/<repo>/agent-kourier --set image.digest=sha256:<digest>
```

The defaults run one replica with SQLite on a 1 GiB volume. For Postgres and a standby, see
[Run two replicas (high availability)](high-availability.md).

!!! warning "NetworkPolicy on Cilium"

    If you turn on `networkPolicy` on a cluster that runs Cilium, add the policy in
    [The pod exits with code 4](troubleshoot.md#the-pod-exits-with-code-4-and-never-becomes-ready) first. Without it
    the pod cannot reach the API server and never starts.

## Check it

```sh
kubectl -n agent-kourier get pods
kubectl -n agent-kourier logs deploy/agent-kourier
```

A healthy start logs these lines, in this order:

```text
config reloaded
serving a ChatConnection
opened the store
ready
slack: socket mode connecting
slack: socket mode websocket opened
slack: socket mode connected
```

`ready` means the config loaded and the store opened. It does not mean Slack connected: wait for the last line. It
carries a `num_connections` field, the number of connections open on the app token. A value above 1 on a fresh start
is worth a look.

Then mention the bot in the channel. With no agent, the first failed send is retried silently; after the second the thread gets "I can't reach the agent
right now, and I'm trying again." If the agent stays unreachable the turn eventually fails with "This request failed,
and I won't try it again." The log shows `session turn started` and then `session turn failed`, with the reason in
`error`.

Now connect a real agent. If you have none yet, start with the `docs-agent` [recipe](recipes.md): a plain A2A agent
that runs on any cluster, with no kagent. To connect your own: [kagent](connect-kagent.md),
[any A2A agent](connect-a2a-agent.md) or [an agent on Google AX](connect-ax.md).

## Stop it and keep the data

Set `replicaCount: 0`. Agent Kourier stops, and the volume stays.

On `helm uninstall` the chart keeps the claim by default: `persistence.retainOnUninstall: true` sets
`helm.sh/resource-policy: keep` on it. A GitOps tool may not honour that annotation when it prunes, and that
behaviour is not guaranteed. If the data matters, create the claim yourself and set `persistence.existingClaim`, so no
release owns it, and stop with `replicaCount: 0` rather than by removing the app.

## Related

- [Troubleshoot an install](troubleshoot.md) for the failures a first install runs into.
- [Export metrics and traces](export-telemetry.md).
- [Stored data](../reference/stored-data.md) for what Agent Kourier keeps and for how long.

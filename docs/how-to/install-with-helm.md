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
- A checkout of this repository, `docker` and `helm`. The release route below needs more; it lists what.

## Build an image the nodes can pull

There is no published release yet, so you build the image and push it to your registry.

`make image` builds the production image, `agent-kourier:dev`, for the architecture of the Docker daemon that builds it.
If the nodes run another architecture, as amd64 nodes do when you build on an Apple silicon laptop, name theirs with
`DOCKER_DEFAULT_PLATFORM`. The Go build cross-compiles, so this needs no emulation. Then tag the image for your
registry and push it:

```sh
DOCKER_DEFAULT_PLATFORM=linux/amd64 make image   # the nodes' platform
docker image inspect --format '{{.Os}}/{{.Architecture}}' agent-kourier:dev
docker tag agent-kourier:dev <registry>/<repo>/agent-kourier:<tag>
docker push <registry>/<repo>/agent-kourier:<tag>
```

- Push each build under a new tag. The chart's `image.pullPolicy` is `IfNotPresent`, so a node that already has a tag
  runs what it has and does not pull a rebuild pushed under the same tag.
- Many registries make tags immutable, which enforces the same thing.

The first pull of a new repository path is the real test of node access. If the nodes pull with their own
credentials, check that the repository falls under whatever policy grants that access before you debug anything else.

### Or build a release

`make release-image` builds what a tagged release publishes: an image for `linux/amd64` and `linux/arm64` with
provenance and SBOM attestations, and the chart package stamped with that image's digest. It is the route for a build
you want to promote, and it needs more than the route above:

- A buildx builder that can make multi-platform images with attestations. Docker's default `docker` driver cannot,
  unless it uses the containerd image store.
- `helm` and `jq`.
- To push: a clean checkout (no uncommitted or untracked files, so keep `my-values.yaml` outside it), a git tag equal to
  `VERSION` at `HEAD`, a registry login of your own, and both `REGISTRY` and `CHART_REGISTRY`.
- To push: [Trivy](https://trivy.dev). Every push scans each platform's image first and fails on any `HIGH` or
  `CRITICAL` finding, including one that has no fix yet. `hack/security/install-trivy.sh` downloads the version CI uses,
  checks its checksum, and prints its path for `TRIVY_BIN`.

Without `PUSH=1` it pushes nothing and skips the checkout, tag and scan checks. It writes the image as an OCI archive to
`bin/release`, which no node can pull, with the chart package beside it. That run checks that the build works:

```sh
docker buildx create --name agent-kourier-release --driver docker-container
BUILDER=agent-kourier-release make release-image VERSION=v0.0.1-dev.1
```

To push, tag the commit `v0.0.1-dev.1`, log in to the registry, and run:

```sh
TRIVY_BIN=$(hack/security/install-trivy.sh) BUILDER=agent-kourier-release PUSH=1 \
  REGISTRY=<registry>/<repo>/agent-kourier CHART_REGISTRY=oci://<registry>/<repo>/charts \
  make release-image VERSION=v0.0.1-dev.1
```

The pushed chart carries the image's repository and digest, so you set no image values when you install it. If the
registry makes tags immutable, each push needs a new version, and so a new git tag.

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

## Check the values before you install

The chart passes each resource's `spec` through as written, so `helm lint` and `helm template` accept a misspelt field
such as `chanel:`. Agent Kourier rejects it when it starts, and the pod restarts in a loop. `agent-kourier dry-run`
loads the same files with the same loader, so it finds those errors before you install. Render the files the chart
writes, with `yq`:

```sh
mkdir -p rendered
helm template agent-kourier ./charts/agent-kourier -n agent-kourier -f my-values.yaml \
  --set image.repository=unused --show-only templates/configmap.yaml |
  yq -r '.data[] | "---\n" + .' > rendered/resources.yaml
```

Then run the dry run from the image you built, with the bot token in `SLACK_BOT_TOKEN` (exported) and the Binding
named as `namespace/name`:

```sh
docker run --rm -e SLACK_BOT_TOKEN -v "$PWD/rendered:/config:ro" \
  agent-kourier:dev dry-run --config /config --binding agent-kourier/my-binding
```

If the image is for another architecture than your machine's, Docker runs it under emulation where it can, as Docker
Desktop does. With Go installed, `go run ./cmd/agent-kourier dry-run --config rendered ...` from the checkout does the
same without the image.

An error that starts `agent-kourier dry-run: load /config:` names the file, the resource and the field. The files load
before the token is read, so without a token the dry run still checks them, and then stops at `no bot token`. With one,
it reads the Binding's channel, and it exits 1 if Slack rejects the token. It does not check references between
resources or the Secrets: those errors show only when the pod starts. Reading the rest of its output is in
[Tune triggers with a dry run](tune-triggers.md).

## Install the chart

The chart in the source tree has no image, and the render fails without one. Pass the image you pushed:

```sh
helm install agent-kourier ./charts/agent-kourier -n agent-kourier -f my-values.yaml \
  --set image.repository=<registry>/<repo>/agent-kourier --set image.tag=<tag>
```

To pin the image, pass `--set image.digest=sha256:<digest>` in place of the tag; a digest wins over a tag.

If you pushed a release, install the chart you pushed instead. It is stamped with its image:

```sh
helm install agent-kourier oci://<registry>/<repo>/charts/agent-kourier --version 0.0.1-dev.1 \
  -n agent-kourier -f my-values.yaml
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

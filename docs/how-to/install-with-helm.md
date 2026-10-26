# Install with Helm

For a platform engineer with a Kubernetes cluster: at the end, Agent Kourier runs from the published Helm chart and
answers a mention in a Slack thread.

The checks below are the ones that catch the usual problems. You do
not need an agent for the first install. Without one, Agent Kourier connects to Slack and tells the thread it cannot
reach the agent. That reply proves Slack in, Slack out, the volume and the policies, and leaves the agent as the only
thing to wire.

## Before you start

- A Kubernetes cluster whose nodes can pull from `ghcr.io`, and a namespace for Agent Kourier. The image is public, so
  the nodes need no pull Secret.
- A Slack app with a bot token and an app-level token, invited to a channel: see [Configure Slack](configure-slack.md).
- `helm`, which pulls the chart from an OCI registry, and `kubectl`.
- For the check before you install: `yq`, and `docker` to run the image.

## The release

Each release publishes a Helm chart and a container image for `linux/amd64` and `linux/arm64` to GHCR:

| Artifact | Reference |
|---|---|
| Chart | `oci://ghcr.io/agentkourier/charts/agent-kourier`, version `0.1.4` |
| Image | `ghcr.io/agentkourier/agent-kourier:v0.1.4` |

The chart pins the image by digest, so you set no image values to install it. The
[GitHub Release](https://github.com/agentkourier/agent-kourier/releases) of each version names both digests.

### Verify the release

The release workflow signs the image and the chart with cosign, keyless, as itself. To check a signature, take the
digest from the GitHub Release and name the workflow's exact identity:

```sh
cosign verify ghcr.io/agentkourier/agent-kourier@sha256:<image digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/agentkourier/agent-kourier/.github/workflows/release.yml@refs/tags/v0.1.4
```

The chart is `ghcr.io/agentkourier/charts/agent-kourier@sha256:<chart digest>`, checked the same way.

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

The chart takes Agent Kourier's three resource kinds under `config`: the ChatConnection, the AgentBackend and the
Binding. They are nested, so they go in a values file rather than in `--set` flags. There are two ways to get one: let
`agent-kourier init` write it, or download a starter file and edit it.

### Write it with `init`

`agent-kourier init` asks for the agent's dialect (`a2a` or `kagent-v1`), URL, namespace and name, and for the channel.
It writes `my-values.yaml` in the shape of the starter file below, loads what the chart would render from it with the
loader that Agent Kourier starts with, and prints the `kubectl` commands for the Secrets above and the `helm install`
command below, with your namespace in them. It writes nothing if the values do not load, and it does not overwrite a file
unless you pass `--force`.

Run it from the released image, in the directory that should get `my-values.yaml`. `-it` lets it ask its questions,
and `--user` runs it as you, so it can write the file into your directory (the image's own user is 65532):

```sh
docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work \
  ghcr.io/agentkourier/agent-kourier:v0.1.4 init
```

The `helm install` command it prints names the chart version of the image's release. Releases before 0.1.3 do not
have `init`. From a checkout of this repository, `go run ./cmd/agent-kourier init` runs the same command; a build that is
not a release has no chart version to name, so the command it prints has no `--version`, and Helm takes the newest chart.

Every question has an option, so the same run needs no terminal. Give the channel by ID, or by name when a bot token is
set:

```sh
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work \
  ghcr.io/agentkourier/agent-kourier:v0.1.4 init --dialect a2a \
  --agent-url http://my-agent.my-namespace.svc.cluster.local:8083/a2a/ \
  --agent-namespace my-namespace --agent-name my-agent --channel C0123456789
```

With the bot token in `SLACK_BOT_TOKEN` (exported, and passed with `-e SLACK_BOT_TOKEN`) or in a file named by
`--token-file` (mounted into the container), `init` also looks a channel name up and
checks that the bot can read the channel, as the dry run below does. Without a token it skips Slack, says so, and needs
the channel's ID. Add `--agent-token` if the agent verifies tokens: the Binding then reads the token from the Secret
`agent-kourier-agent-token`, and `init` prints the command that creates it. The options are in
[Command line and environment](../reference/cli.md#agent-kourier-init).

### Download the starter file

Or download the starter file, a complete first install with an agent that does not exist yet:

```sh
curl -fsSLo my-values.yaml https://raw.githubusercontent.com/agentkourier/agent-kourier/main/examples/helm/my-values.yaml
```

It is this file. Change the four lines marked `yours`: the agent's URL, namespace and name, and the channel's ID, which
starts with `C` (`G` for an older private channel), not the channel's name. `init` writes the same
resources with those four values filled in. The `-f` flags below pass it to Helm.

```yaml title="my-values.yaml"
--8<-- "my-values.yaml"
```

Every field is in the [Binding](../reference/binding.md), [AgentBackend](../reference/agentbackend.md) and
[ChatConnection](../reference/chatconnection.md) references, and every chart value in [Helm values](../reference/helm-values.md).
`helm show values oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.1.4` prints the chart's defaults with their
comments. Keep the file: an upgrade passes it again.

## Check the values before you install

The chart passes each resource's `spec` through as written, so `helm lint` and `helm template` accept a misspelt field
such as `chanel:`. Agent Kourier rejects it when it starts, and the pod restarts in a loop. `agent-kourier dry-run`
loads the same files with the same loader, so it finds those errors before you install. Render the files the chart
writes, with `yq`:

```sh
mkdir -p rendered
helm template agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.1.4 \
  -n agent-kourier -f my-values.yaml --show-only templates/configmap.yaml |
  yq -r '.data[] | "---\n" + .' > rendered/resources.yaml
```

Then run the dry run from the released image, with the bot token in `SLACK_BOT_TOKEN` (exported) and the Binding
named as `namespace/name`:

```sh
docker run --rm -e SLACK_BOT_TOKEN -v "$PWD/rendered:/config:ro" \
  ghcr.io/agentkourier/agent-kourier:v0.1.4 dry-run --config /config --binding agent-kourier/my-binding
```

An error that starts `agent-kourier dry-run: load /config:` names the file, the resource and the field. The files load
before the token is read, so without a token the dry run still checks them, and then stops at `no bot token`. With one,
it reads the Binding's channel, and it exits 1 if Slack rejects the token. It does not check references between
resources or the Secrets: those errors show only when the pod starts. Reading the rest of its output is in
[Tune triggers with a dry run](tune-triggers.md).

## Install the chart

```sh
helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.1.4 \
  -n agent-kourier -f my-values.yaml
```

The defaults run one replica with SQLite on a 1 GiB volume. For Postgres and a standby, see
[Run two replicas (high availability)](high-availability.md).

If the nodes may pull only from your own registry, copy the image there by digest and pass
`--set image.repository=<registry>/<repo>/agent-kourier`. The chart keeps its pinned digest, so the copy must have the
same one.

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

## Build from source

For a change that is not in a release yet. You need a checkout of this repository and `docker`.

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

The chart in the source tree has no image, and the render fails without one. Install it from the checkout with the
image you pushed:

```sh
helm install agent-kourier ./charts/agent-kourier -n agent-kourier -f my-values.yaml \
  --set image.repository=<registry>/<repo>/agent-kourier --set image.tag=<tag>
```

To pin the image, pass `--set image.digest=sha256:<digest>` in place of the tag; a digest wins over a tag. To run the
dry run from the checkout without an image, use `go run ./cmd/agent-kourier dry-run --config rendered ...`.

`make release-image` builds what the release workflow publishes, an image for both platforms with provenance and SBOM
attestations and the chart stamped with its digest, and checks them, without pushing anything. The repository's
`RELEASING.md` covers it and how a release is cut.

## Related

- [Troubleshoot an install](troubleshoot.md) for the failures a first install runs into.
- [Upgrade](upgrade.md) to a newer release.
- [Export metrics and traces](export-telemetry.md).
- [Stored data](../reference/stored-data.md) for what Agent Kourier keeps and for how long.

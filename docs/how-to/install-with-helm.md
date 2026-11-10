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
- For custom resources, the chart's default route: Kubernetes 1.33 or later, and a right to create CustomResourceDefinitions (they are
  cluster-scoped), or an administrator who applies them once ([Apply the CRDs yourself](migrate-config-source.md#apply-the-crds-yourself)).
- For the check before you install a ConfigMap: `yq`, and `docker` to run the image.

## Choose the route

Agent Kourier reads its `ChatConnection`, `AgentBackend` and `Binding` resources from one of two places, and the chart's default is the first.

- **Custom resources** (`config.source: crds`, the default) are the route to run on Kubernetes. The chart installs the CRDs and the broker, and you apply the
  three resources with `kubectl` or a GitOps tool, so that each team can own its Bindings and see what became of them in `kubectl get akbinding`. It needs Kubernetes 1.33 or later.
- **A ConfigMap** (`config.source: files`) is the quick route for a proof of concept. The chart writes the resources of your values file into a ConfigMap, which needs
  no CRDs, no cluster-scoped right and no minimum version. The starter file and `agent-kourier init` below write this route.

[Choose a config route](../explanation/config-routes.md) says how they differ when something is wrong.

## The release

Each release publishes a Helm chart and a container image for `linux/amd64` and `linux/arm64` to GHCR:

| Artifact | Reference |
|---|---|
| Chart | `oci://ghcr.io/agentkourier/charts/agent-kourier`, version `0.3.1` |
| Image | `ghcr.io/agentkourier/agent-kourier:v0.3.1` |

The chart pins the image by digest, so you set no image values to install it. The
[GitHub Release](https://github.com/agentkourier/agent-kourier/releases) of each version names both digests.

### Verify the release

The release workflow signs the image and the chart with cosign, keyless, as itself. To check a signature, take the
digest from the GitHub Release and name the workflow's exact identity:

```sh
cosign verify ghcr.io/agentkourier/agent-kourier@sha256:<image digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/agentkourier/agent-kourier/.github/workflows/release.yml@refs/tags/v0.3.1
```

The chart is `ghcr.io/agentkourier/charts/agent-kourier@sha256:<chart digest>`, checked the same way.

## Create the Secrets

Create the namespace first. A sealed secret is bound to its namespace and name, so it cannot be applied before the
namespace exists.

```sh
kubectl create namespace agent-kourier
```

The ChatConnection's Secret holds two keys with exactly these names. It is named `slack`, like the ChatConnection below, so that the
Role the chart makes names it (see [Install on custom resources](#install-on-custom-resources)):

```sh
kubectl -n agent-kourier create secret generic slack \
  --from-literal=botToken="$SLACK_BOT_TOKEN" \
  --from-literal=appToken="$SLACK_APP_TOKEN"
```

If the agent, or a front door in front of it, verifies tokens, first
[create the Binding identity](create-binding-identity.md) and obtain its token from the accepted issuer.
Agent Kourier does not mint `$AGENT_TOKEN`; it is the issued bearer token. Store it in the Binding's namespace. On custom
resources, as below, the Binding is the team's and sits in the team's namespace (`my-team` here). For a Binding under the
chart's `config`, as on the ConfigMap route, it is the release namespace (`agent-kourier` here) unless the entry sets its
own `namespace`. The Dex example on that page stores its token itself, as `payments-agent-token` in `payments`; for this
install, change its last command to `kubectl -n my-team create secret generic agent-kourier-agent-token` (`-n agent-kourier`
on the ConfigMap route). With a token from any other issuer (on the ConfigMap route, `-n agent-kourier`):

```sh
kubectl -n my-team create secret generic agent-kourier-agent-token --from-literal=token="$AGENT_TOKEN"
```

Tokens never go in the values file. The chart refuses a value that looks like a Slack token.

## Install on custom resources

![A terminal: kubectl creates the namespace and the Slack Secret from environment variables, and helm installs the agent-kourier chart. As the platform team, vim fills in the three lines marked yours in platform.yaml, a ChatConnection and an AgentBackend that each allow the team's namespace. As the app team, vim fills in the four lines marked yours in team.yaml, a Binding in the team's own namespace. kubectl applies both after a server-side dry run, and kubectl get akbinding shows the Binding go from no status to Unknown Pending to True OK. Then Slack: the mention of the bot and the agent's answer in its thread.](../assets/clip/install.gif)

The steps below, recorded on a kind cluster with the
[echo agent](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/echo-agent/agent/README.md), a plain A2A agent
with no model. The wait for the Binding to turn Ready, about 30 seconds here, is cut.

The chart installs the CRDs and the broker, and you apply the resources. They are two steps because a release that renders the CRDs and
resources of them fails in its first apply (Helm cannot map a kind that is not yet served), so the chart refuses to render
`config.chatConnections`, `config.agentBackends` or `config.bindings` as custom resources into a release that is installing their CRDs. A release with none of them renders no custom resource, and installs the CRDs itself.

The values name the ChatConnection the broker serves, as `namespace/name`. The chart cannot read the resources you apply yourself, but it knows which
connection the broker serves, so it makes a Role in that connection's namespace that grants `list` and `watch` on one Secret by name: the Secret
that has the connection's name (`slack` here). Without that Role the broker could not read the Secret and would exit with code 3 in a loop. If the
ChatConnection's `credentialsSecretRef` names another Secret, set `config.chatConnectionSecret` to that name.

This install gives the broker a ChatConnection of its own. A broker that serves only the ChatConnections of other namespaces, whose teams bring their own Slack apps,
leaves `config.chatConnection` empty and lists those namespaces in `config.connectionNamespaces` instead: the chart then renders no `AGENTKOURIER_CHAT_CONNECTION`,
grants the Secret of no own connection, and the pod starts without a ChatConnection to find
([A broker with no connection of its own](team-slack-app.md#a-broker-with-no-connection-of-its-own)).

```yaml title="my-values.yaml"
config:
  chatConnection: agent-kourier/slack
```

```sh
helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.3.1 \
  -n agent-kourier -f my-values.yaml
```

The pod exits with code 2 and restarts until the ChatConnection it is told to serve exists (`ChatConnection agent-kourier/slack was not found`), so it
is not ready yet. Apply the resources: the starter file's three, as objects, with the Binding moved to the team's namespace and
the two others shared with it.

The platform team's file holds the shared ChatConnection and AgentBackend, in the release's namespace. Each lists the team's
namespace in `allowedNamespaces`, without which a Binding of another namespace may not name it. Change its three lines marked `yours`:

```yaml title="platform.yaml"
apiVersion: agentkourier.dev/v1alpha1
kind: ChatConnection
metadata:
  name: slack
  namespace: agent-kourier
spec:
  platform: slack
  credentialsSecretRef: {name: slack}
  allowedNamespaces: [my-team] # yours: the team's namespace
---
apiVersion: agentkourier.dev/v1alpha1
kind: AgentBackend
metadata:
  name: agent
  namespace: agent-kourier
spec:
  dialect: a2a
  url: http://my-agent.my-team.svc.cluster.local:8083/a2a/ # yours: the agent's A2A URL
  allowedNamespaces: [my-team] # yours: the team's namespace
```

The team's file holds its Binding, in the team's own namespace (here the agent's), naming the two by namespace. The team can
commit it next to its agent's manifests. Change its four lines marked `yours`:

```yaml title="team.yaml"
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata:
  name: my-binding
  namespace: my-team # yours: the team's namespace
spec:
  agent:
    backendRef: {name: agent, namespace: agent-kourier}
    namespace: my-team # yours: the agent's namespace
    name: my-agent # yours: the agent's name
  identity:
    userId: agent-kourier
  chat:
    connectionRef: {name: slack, namespace: agent-kourier}
    channel: C0123456789 # yours: the channel's ID (starts with C), not its name
    output: live
  interactions: {askUser: false}
```

```sh
kubectl apply --dry-run=server -f platform.yaml -f team.yaml
kubectl apply -f platform.yaml
kubectl apply -f team.yaml
kubectl get akbinding -A
```

The Role covers the one Secret `slack`. A Binding's token Secret, such as `agent-kourier-agent-token` in the team's namespace above, is in no
value the chart reads, so nothing grants it. If the agent verifies tokens, add `tokenSecretRef: {name: agent-kourier-agent-token}` under `identity`
in the Binding, and grant the Secret: the team with a Role of its own that names it, as
[Let a team add its own Binding](migrate-config-source.md#let-a-team-add-its-own-binding) shows, or the platform with `rbac.secretNames`, a map of
namespace to Secret names, and a `helm upgrade` with the values:

```yaml
rbac:
  secretNames:
    my-team: [agent-kourier-agent-token]
```

The dry run asks the API server to check each object against the CRDs' schema, and names the field of a refusal. The checks that only the broker
makes show afterwards in the Binding's status: [Check that it worked](migrate-config-source.md#check-that-it-worked) reads it. The pod starts at its
next restart after the ChatConnection exists, which Kubernetes does by itself. [Let a team add its own Binding](migrate-config-source.md#let-a-team-add-its-own-binding)
says what a team needs to write its own.

If the chart refuses with `config.source crds needs Kubernetes 1.33 or later`, the cluster is older: install the ConfigMap route below, which has no minimum.
An installer that may not create CRDs sets `crds.install: false` (unset, it is true with custom resources), and an administrator applies them first. So does a second broker in the same cluster: the CRDs are
the first release's, and a second release that renders them is refused by Helm (`invalid ownership metadata`).

## The ConfigMap route

[Write the values](#write-the-values), [Check the values before you install](#check-the-values-before-you-install) and [Install the chart](#install-the-chart)
install the ConfigMap route, which the starter file and `agent-kourier init` name with `config.source: files`. With `files`, `crds.install` is false unless you set it, so they install no CRDs.
Moving to custom resources later is [the how-to's](migrate-config-source.md).

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
  ghcr.io/agentkourier/agent-kourier:v0.3.1 init
```

The `helm install` command it prints names the chart version of the image's release. Releases before 0.1.3 do not
have `init`. From a checkout of this repository, `go run ./cmd/agent-kourier init` runs the same command; a build that is
not a release has no chart version to name, so the command it prints has no `--version`, and Helm takes the newest chart.

Every question has an option, so the same run needs no terminal. Give the channel by ID, or by name when a bot token is
set:

```sh
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work \
  ghcr.io/agentkourier/agent-kourier:v0.3.1 init --dialect a2a \
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
`helm show values oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.3.1` prints the chart's defaults with their
comments. Keep the file: an upgrade passes it again.

## Check the values before you install

The chart passes each resource's `spec` through as written, so `helm lint` and `helm template` accept a misspelt field
such as `chanel:`. Agent Kourier rejects it when it starts, and the pod restarts in a loop. `agent-kourier dry-run`
loads the same files with the same loader, so it finds those errors before you install. Render the files the chart
writes, with `yq`:

```sh
mkdir -p rendered
helm template agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.3.1 \
  -n agent-kourier -f my-values.yaml --show-only templates/configmap.yaml |
  yq -r '.data[] | "---\n" + .' > rendered/resources.yaml
```

Then run the dry run from the released image, with the bot token in `SLACK_BOT_TOKEN` (exported) and the Binding
named as `namespace/name`:

```sh
docker run --rm -e SLACK_BOT_TOKEN -v "$PWD/rendered:/config:ro" \
  ghcr.io/agentkourier/agent-kourier:v0.3.1 dry-run --config /config --binding agent-kourier/my-binding
```

An error that starts `agent-kourier dry-run: load /config:` names the file, the resource and the field. The files load
before the token is read, so without a token the dry run still checks them, and then stops at `no bot token`. With one,
it reads the Binding's channel, and it exits 1 if Slack rejects the token. It does not check references between
resources or the Secrets: those errors show only when the pod starts. Reading the rest of its output is in
[Tune triggers with a dry run](tune-triggers.md).

This checks the files of the ConfigMap route. With custom resources (`config.source: crds`, the default) the chart renders no ConfigMap and the
resources are objects: check each against the CRDs' schema with `kubectl apply --dry-run=server -f <file>`, which names the field of a
refusal, as [above](#install-on-custom-resources). The checks that only the broker makes show afterwards, in the Binding's status
([Move the config between a ConfigMap and custom resources](migrate-config-source.md)).

## Install the chart

```sh
helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.3.1 \
  -n agent-kourier -f my-values.yaml
```

The defaults run one replica with SQLite on a 1 GiB volume. For Postgres and a standby, see
[Run two replicas (high availability)](high-availability.md).

The starter file names `config.source: files`, so this install reads the resources from a ConfigMap and installs no CRDs. To run on custom resources
instead, so that other teams write their own Bindings, use [Install on custom resources](#install-on-custom-resources) for a new install, and
[Move the config between a ConfigMap and custom resources](migrate-config-source.md) on a running one; [Choose a config route](../explanation/config-routes.md) says when that is worth it.

If the nodes may pull only from your own registry, copy the image there by digest and pass
`--set image.repository=<registry>/<repo>/agent-kourier`. The chart keeps its pinned digest, so the copy must have the
same one.

!!! warning "NetworkPolicy on Cilium"

    If you turn on `networkPolicy` on a cluster that runs Cilium, add the policy in
    [The pod exits with code 4](troubleshoot.md#the-pod-exits-with-code-4-and-never-becomes-ready) first. Without it
    the pod cannot reach the API server and never starts.

## Install behind an egress proxy

Agent Kourier reads the standard proxy variables (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`, in upper or lower case) in some of its connections and not in others. Set them in
[`extraEnv`](../reference/helm-values.md#extraenv) when the cluster reaches Slack through a proxy:

```yaml
extraEnv:
  - name: HTTPS_PROXY
    value: http://proxy.example.com:3128   # a proxy that needs a login: build the URL in a Secret and use valueFrom.secretKeyRef
  - name: NO_PROXY
    value: 10.96.0.1,.svc,.cluster.local   # the kubernetes Service's cluster IP, as below, and the in-cluster domains
```

| Connection | Uses the proxy variables |
|---|---|
| Slack Web API calls and the Socket Mode websocket | Yes |
| The Kubernetes API server: the Secret and custom resource informers, status writes and the leader Lease | Yes |
| OTLP trace export, when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set | Yes |
| Agents: the card, calls and streams to an AgentBackend's `url` | No. The connection is made directly, on purpose, so that an environment variable cannot redirect a Binding's token |
| The database | No |

What to put in `NO_PROXY`:

- **The Kubernetes API server.** The broker dials the address in `KUBERNETES_SERVICE_HOST`, which Kubernetes sets in every pod: the cluster IP of the `kubernetes`
  Service in the `default` namespace (`kubectl -n default get service kubernetes`), not the control plane's own address. Put that IP, or the cluster's service CIDR, in `NO_PROXY`; a CIDR is read
  by both the Kubernetes client and Go's own proxy code. Without it the broker's requests to the API server go to the proxy. If the proxy does not carry them, the pod exits with code 4, at the latest when its sync wait (`AGENTKOURIER_SYNC_TIMEOUT`, 30 seconds by default) ends: the Secret
  caches never sync, and on `config.source: crds` the first list of the custom resources fails as soon as the proxy refuses the connection. See [the pod exits with code 4](troubleshoot.md#the-pod-exits-with-code-4-and-never-becomes-ready)
  (read from the clients' code and not run through a proxy here).
- **An in-cluster OTLP collector.** Add its domain suffix (`.svc` and `.cluster.local` above), or the spans go to the proxy.
- **Agents need no entry.** They never go to the proxy, so a `NO_PROXY` rule changes nothing for them. An agent that is reachable only through a proxy needs a reverse proxy
  that its AgentBackend's `url` names.

With `networkPolicy.enabled`, egress to the proxy's address has to be allowed in `networkPolicy.extraEgress`, and `networkPolicy.slack.cidrs` can then be narrowed to `[]`
(see [`networkPolicy`](../reference/helm-values.md#networkpolicy)). Narrowing it removes the only rule that allows port 443 to public addresses, and agents never use
the proxy, so an AgentBackend at a public address then needs an `extraEgress` rule of its own. The policy's `networkPolicy.kubeApiServer` rule still has to name the control plane's addresses, since the policy sees them after the Service's translation.

## Check it

```sh
kubectl -n agent-kourier get pods
kubectl -n agent-kourier logs deploy/agent-kourier
```

A healthy start logs these lines, in this order:

```text
config route
config reloaded
serving a ChatConnection
opened the store
ready
slack: socket mode connecting
slack: socket mode websocket opened
slack: socket mode connected
```

`config route` is the first line, and it is how the log says which of [the two routes](../explanation/config-routes.md) the broker reads: its `config_source` field is
`crds` or `files`. With `crds` and an own connection it also has a `connection` field, the namespace/name of the ChatConnection the broker is set to serve
(`AGENTKOURIER_CHAT_CONNECTION`), and the later `serving a ChatConnection` line confirms that the cluster has it. With `crds` and a list of namespaces
(`AGENTKOURIER_CONNECTION_NAMESPACES`) it also has a `connection_namespaces` field with the namespace names, comma-joined; a broker with no own connection has no `connection` field,
and `serving a ChatConnection` is logged each time it starts a team connection, after `ready`, since the leader starts them. With `files` there is no `connection` field: the one
ChatConnection of the directory is named by `serving a ChatConnection` alone. The line is logged at `info`: at the default level, the log of a container that has just started and has no `config route` line is from a release older than the one that added it. With `logLevel: warn` or `error` no start line is shown, and a log that kubelet has rotated no longer has its first lines. The order of the first five lines is read from the code and was checked, on both
routes, against the broker started with a fake cluster; the three `slack:` lines were not re-run against a cluster for this page.

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

The CustomResourceDefinitions that `crds.install` renders are kept on `helm uninstall` the same way (`helm.sh/resource-policy: keep`), and that too is
Helm's guarantee only. Under Argo CD with prune, turning `crds.install` off, or deleting the Application, may delete the CRDs, and deleting a CRD deletes every
Binding, AgentBackend or ChatConnection of that kind in the cluster, which is every team's. The chart renders `argocd.argoproj.io/sync-options: Prune=false,Delete=false`
on each CRD, which Argo CD's documentation says stops that. This is unverified: Argo CD has not been run against this chart. A tool other than Helm or Argo CD needs its own way to be told to leave them,
or have the CRDs applied outside what it prunes (see [Apply the CRDs yourself](migrate-config-source.md#apply-the-crds-yourself)).

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

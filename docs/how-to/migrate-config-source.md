# Move the config between a ConfigMap and custom resources

By default Agent Kourier reads its `ChatConnection`, `AgentBackend` and `Binding` resources from files, usually a ConfigMap.
The Helm chart can instead have it read them from the cluster as custom resources, so that each team owns its own
Bindings. This page moves a running install from the ConfigMap to custom resources and back. At the end, `kubectl get
akbinding` lists your Bindings with their status, and you know what a move back keeps and what it deletes.

Read [Choose a config route](../explanation/config-routes.md) first if you have not decided which route fits. The short
version: the ConfigMap is the simple route for one team, and custom resources are for several teams that each write their
own Bindings.

## Before you start

- **A chart release that has custom resources.** Release 0.1.6 does not: `crds` and `config.source` are not among its
  values. Check the chart you will install with `helm show values oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> | grep -A1 '^crds:'`.
- **Kubernetes 1.33 or later, for the custom resource route only.** The chart refuses to render `config.source: crds` for an
  API server it can see is older, and says so. The ConfigMap route has no minimum.
- **A right to create CustomResourceDefinitions.** They are cluster-scoped. If your installer may not create them, a platform
  administrator applies them once (see [Apply the CRDs yourself](#apply-the-crds-yourself)).
- **Helm 4.** The Helm behaviour on this page, such as the server-side apply that refuses an unknown field, is Helm 4's.
- **Namespaces that exist.** A file can name a namespace that does not exist. A custom resource cannot.
- **Check the files against this release's bounds.** The release that adds custom resources also bounds triggers, extract entries and
  headers on both routes. [Upgrade](upgrade.md#upgrade-to-the-release-that-bounds-triggers-extract-entries-and-headers) lists them and shows how
  to run `agent-kourier dry-run` against your files before you upgrade.

## Choose how the resources are held

There are two ways to hold the resources on the custom resource route, and you can mix them.

- **The chart renders them.** The `config.chatConnections`, `config.agentBackends` and `config.bindings` values you already have
  become custom resources, each in its `namespace` (default: the release's). Nothing else changes in your values file. This is the
  shortest move, and `helm upgrade` is how you edit them.
- **You apply them yourself.** Leave `config.*` empty, name the ChatConnection the broker serves in
  `config.chatConnection`, and apply the resources with `kubectl` or a GitOps tool. Teams do this for their own
  Bindings even when the platform team's resources are rendered.

The chart reads `config.*` to work out which Secrets to grant, so for resources you apply yourself it cannot see their
Secrets: see [Grant the Secrets](#grant-the-secrets).

## Move to custom resources

Two upgrades, each a working install. The first installs the CRDs and changes nothing about how the broker runs. The second
switches the route. They cannot be one upgrade: a release that renders the CRDs and custom resources of them in its first
apply fails (`resource mapping not found ... ensure CRDs are installed first`), so the chart refuses to render the
custom resources until the cluster already serves `agentkourier.dev/v1alpha1`. Skip step 1 and the second upgrade fails with
`config.source crds needs the CRDs (group agentkourier.dev, version v1alpha1) to be installed before ...`, and changes nothing.

Put `crds.install: true` in your values file and keep it there. A release that has it turned off no longer holds the CRDs in its
manifest (`helm get manifest` shows none), so a later chart upgrade does not change them. The CRDs stay in the cluster either way.

### 1. Install the CRDs, still on the ConfigMap route

```sh
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
  -n agent-kourier -f my-values.yaml --set crds.install=true
kubectl get crd | grep agentkourier.dev
```

The `grep` prints three lines: `agentbackends.agentkourier.dev`, `bindings.agentkourier.dev` and `chatconnections.agentkourier.dev`.
The broker still reads its files, and the pod does not restart. The CRDs are
inert until a broker runs on the custom resource route. `kubectl get akbinding -A` now answers `No resources found`.

### 2. Switch the route

Edit `my-values.yaml`:

```yaml
crds:
  install: true
config:
  source: crds
  # chatConnections, agentBackends and bindings stay as they are: the chart renders them as custom resources.
  # Without them, name the ChatConnection this broker serves, as namespace/name:
  # chatConnection: agent-kourier/slack
```

If `config.chatConnections` has exactly one entry, that is the connection and `config.chatConnection` can stay empty.
Otherwise set it: a cluster can hold the ChatConnections of several brokers, and each broker serves only the one it is told
and only the Bindings whose `connectionRef` names it.

```sh
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
  -n agent-kourier -f my-values.yaml
kubectl -n agent-kourier rollout status deploy/agent-kourier
```

The pod restarts: the Deployment now sets `AGENTKOURIER_CONFIG_SOURCE=crds` and `AGENTKOURIER_CHAT_CONNECTION`, mounts no
ConfigMap, and the chart removes the ConfigMap it rendered. A session records its Binding as `namespace/name` and the move keeps
the names, so a conversation continues where it was: a reply in a thread that began before the move is answered after it. For about 15
seconds a Binding reads `Unknown` with the reason `Pending`, and then `BackendReachable:Pending` until its first probes end.

### Move a ConfigMap you manage yourself

With `config.existingConfigMap`, the chart never made the ConfigMap, so it renders nothing for you to keep. Turn each file into a
custom resource instead. The files are already valid manifests, with the same `apiVersion`, `kind`, `metadata` and `spec`. After
step 1, check them against the CRDs' schema and CEL rules with a server-side dry run, then apply them:

```sh
kubectl -n agent-kourier get configmap my-config -o json | jq -r '.data | to_entries[] | "---\n" + .value' > resources.yaml
kubectl apply --dry-run=server -f resources.yaml
kubectl apply -f resources.yaml
```

Apply them before step 2, because a broker that starts without its ChatConnection exits (see [When it does not work](#when-it-does-not-work)). Then
in step 2 remove `config.existingConfigMap` (the chart refuses it with `config.source: crds`, since nothing would read it), and set
`config.chatConnection` and `rbac.secretNames` as below. The ConfigMap itself is not deleted: the chart did not make it, and it
is the way back.

### Grant the Secrets

With the ConfigMap route the chart grants `list` and `watch` on every Secret of each namespace that holds one its config names.
With `config.source: crds` it names the Secrets instead (`resourceNames`): the ones in your `config.*` values and the Postgres
DSN's. A Secret that only a resource you apply yourself names is not granted until you say so, with `rbac.secretNames`, a map of
namespace to Secret names:

```yaml
rbac:
  secretNames:
    agent-kourier: [agent-kourier-slack, agent-kourier-agent-token]
```

`rbac.secretNamespaces` still grants a whole namespace, as before. A Binding whose Secret is not granted stays served, and its
`BackendReachable` condition says `CredentialsUnreadable`. Only the Secrets the broker itself needs, the ChatConnection's tokens and
the Postgres DSN's, stop the start (exit code 3) when they cannot be read.

### Apply the CRDs yourself

An installer that may not create CRDs leaves `crds.install` off, and an administrator applies the CRDs once, from the chart:

```sh
helm pull oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> --untar
kubectl apply --server-side -f agent-kourier/files/crds
```

Choose one owner for the CRDs. Helm will not take over CRDs that `kubectl` made (`invalid ownership metadata`), and two releases
that render the same CRD conflict.

## Check that it worked

```sh
kubectl get akbinding,akbackend,akconnection -A
kubectl get agentkourier -A
kubectl -n agent-kourier wait --for=condition=Ready akbinding/<name> --timeout=120s
```

`akbinding`, `akbackend` and `akconnection` are the short names; `bindings.agentkourier.dev` and so on also work, and
`agentkourier` is the category that lists all three. **Never type `kubectl get bindings`.** That is the core Kubernetes `bindings`
resource, which is something else. `kubectl get bindings -n <namespace>` answers `MethodNotAllowed` and with `-A` it answers `NotFound`, and a named `get`, `patch`, `edit` or `delete` of
`binding/<name>` answers `NotFound`: use `akbinding` or `bindings.agentkourier.dev`.

```text
NAMESPACE       NAME                           READY   REASON   CHANNEL   AGE
agent-kourier   binding.agentkourier.dev/sre   True    OK       C1        2m
```

The columns come from the Binding's status. A Binding that shows `False` has a reason in the `REASON` column, and the
conditions with their messages are in `kubectl -n <namespace> get akbinding <name> -o yaml`. [Binding status](../reference/binding-status.md)
explains each, and [Troubleshoot an install](troubleshoot.md#a-binding-is-not-accepted) says what to do about each reason.

Two more checks:

```sh
kubectl -n agent-kourier logs deploy/agent-kourier | grep 'config: '
kubectl get crd bindings.agentkourier.dev -o jsonpath='{.metadata.annotations.agentkourier\.dev/schema-sha256}'
```

The log says `config: a Binding is not accepted as written` or `config: an object is refused` for what it could not serve, and
`config: every object is accepted` when the last of them is fixed. On a clean install the `grep` prints nothing, and that silence is the good result. The hash is the one the broker compares with the one it was built with, every minute:
`agentkourier_crd_schema_state` says `match`, or `mismatch` when the CRDs and the image come from different chart releases.

## Let a team add its own Binding

On this route a team writes its Binding, and an AgentBackend if it has its own, in its own namespace. One thing is the platform team's
to do, and one is the team's.

1. **The platform team lists the namespace.** The ChatConnection must allow it: add the namespace to the connection's `allowedNamespaces`, which with
   rendered resources is `config.chatConnections.<name>.spec.allowedNamespaces` in your values. A Binding from another namespace reads
   `Accepted=False` with the reason `NamespaceNotAllowed` until it is listed.
2. **The team lets the broker read its Secret.** The team makes a Role in its own namespace that names the Secret, and a RoleBinding to the
   broker's ServiceAccount (here `agent-kourier` in the namespace `agent-kourier`):

   ```yaml
   apiVersion: rbac.authorization.k8s.io/v1
   kind: Role
   metadata: {name: agent-kourier-team-a-secrets, namespace: team-a}
   rules:
     - apiGroups: [""]
       resources: [secrets]
       resourceNames: [team-a-agent-token]
       verbs: [list, watch]
   ---
   apiVersion: rbac.authorization.k8s.io/v1
   kind: RoleBinding
   metadata: {name: agent-kourier-team-a-secrets, namespace: team-a}
   roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: agent-kourier-team-a-secrets}
   subjects:
     - {kind: ServiceAccount, name: agent-kourier, namespace: agent-kourier}
   ```

   The verbs are `list` and `watch`, not `get`. Give the Role a name that the chart would not (`<release>-secrets` is its own), so that the two never meet. Check with `kubectl auth can-i list secrets/team-a-agent-token -n team-a --as
   system:serviceaccount:agent-kourier:agent-kourier`: it says `yes`, and `list secrets` without the name says `no`.
   Until the Role exists the Binding is `BackendReachable=False` with `CredentialsUnreadable`. It recovers on its own, with no restart, once the Role is
   applied.

The team then applies its resources, for example the Binding of [Write a Binding](write-a-binding.md) and an AgentBackend of its own. A Binding
that names a ChatConnection of another namespace must say which, as `connectionRef: {namespace: agent-kourier, name: slack}`; without the namespace the
connection is looked up in the Binding's own, and the status says `ConnectionNotFound`.

```sh
kubectl apply -f team-a.yaml
kubectl -n team-a wait --for=condition=Ready akbinding/team-a-chat --timeout=120s
```

What a team that can write these objects can do with the broker's access is in [Security model](../explanation/security.md#what-the-people-who-write-these-objects-can-do).

## Move back to the ConfigMap

Where the chart rendered the resources from your `config.*` values, set `config.source: files` and upgrade. The ConfigMap is rendered from the same values:

```sh
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
  -n agent-kourier -f my-values.yaml --set config.source=files
```

**If the config is held only in custom resources**, because you applied them yourself or a team did, that command renders an empty ConfigMap, and the pod
crash-loops with exit code 2: `v1 serves exactly one ChatConnection, and the config has 0 []`. Put the config in a ConfigMap first, then name it:

```sh
kubectl get akconnection,akbackend,akbinding -A -o json |
  jq -r '.items[] | select(.kind != "Binding" or any(.status.conditions[]?; .type == "Accepted" and .status == "True")) | "---", ({apiVersion, kind, metadata: {name: .metadata.name, namespace: .metadata.namespace}, spec} | tojson)' > resources.yaml
kubectl -n agent-kourier create configmap my-config --from-file=resources.yaml --dry-run=client -o yaml | kubectl apply -f -
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
  -n agent-kourier -f my-values.yaml --set config.source=files,config.existingConfigMap=my-config
```

The `jq` keeps only what a file may hold, since a file that sets `status` is refused, and only the Bindings the broker accepted. **One refused entry stops the broker's
start with exit code 2**, because on the files route one bad resource fails the whole config: a Binding that lost its channel (`is already bound by Binding ...`) or
names an AgentBackend that does not exist (`... not found`) is served by nothing on the custom resource route, and in a file it would take every other Binding down with it.
`agent-kourier dry-run` ([Check the values before you install](install-with-helm.md#check-the-values-before-you-install)) checks each resource, and not the references between them or a channel
that two Bindings claim, so it does not find these; the filter does. The files route also serves exactly one ChatConnection, so keep the one in `config.chatConnection` and the
Bindings that name it, and drop the rest from `resources.yaml` before you create the ConfigMap. The `kubectl apply` form replaces the ConfigMap of that name if it exists,
which it does if you came through [Move a ConfigMap you manage yourself](#move-a-configmap-you-manage-yourself) and nobody has added resources since: then skip the dump.

`config.existingConfigMap` is refused while `config.agentBackends`, `config.chatConnections` or `config.bindings` are set (`two ways to supply the same files: set one`). If your
values rendered some resources and teams applied others, the dump holds both, so remove `config.*` from the values after the dump. The alternative is to
leave the team's Bindings out of the ConfigMap and use plain `--set config.source=files`, which drops them.

A Secret outside the release namespace needs a grant on this route, and `rbac.secretNames` is ignored. The Role that a team made for its own Secret stays after the move
and is that grant. The other way is `rbac.secretNamespaces`, which replaces the chart's default of the release namespace, so list the release namespace as well; and if a team's Role has the name
the chart would give its own there (`<release>-secrets`), delete it first, or the upgrade fails with `invalid ownership metadata`.

Keep `crds.install: true` while you do; it does not need to change. What the move back does:

| | After the move back |
|---|---|
| The ConfigMap | Returns, rendered from the same `config.*` values. A `config.existingConfigMap` is never touched. |
| Custom resources the chart rendered | Deleted, because the chart stops rendering them. Rendering them again later makes new objects, with new creation times, so by [the age rule](../explanation/config-routes.md#who-holds-a-channel) they can lose a channel to a Binding that a team made in the meantime. |
| Custom resources you applied | Stay in the cluster and are not read. The broker serves what the ConfigMap holds, so a team's Binding is **not** served until it is in the ConfigMap. |
| Their status | Stays as it was and is no longer written: a `Ready` of `True` in the `READY` column can be days old. Compare a condition's `observedGeneration` with `metadata.generation`: after an edit they differ for good. |
| The CRDs | Stay, with every custom resource: a CRD is never deleted by the chart. |
| The ClusterRole and its binding | Deleted (they render only with `crds`). A team's own Role and RoleBinding stay. |
| Sessions, the outbox and the audit log | Kept, since they live in the store. A session of a Binding that is not in the config is not recovered, and a reply in its thread gets "This thread's agent is no longer set up, so I can't pass your messages on." Such a session is deleted once it has been idle for the longest `threadTTL` of any Binding (at least the default of 90 days) plus a grace of 90 days. |

**Never delete a CRD to get out of a refusal.** Deleting it deletes every custom resource of that kind in the cluster, which is every team's. What a running
broker does depends on what goes. Deleting the Bindings CRD alone stops every Binding, within about 20 seconds (60 at most): their `agentkourier_binding_ready`
series are gone and a new mention gets no answer. Deleting all three CRDs takes the ChatConnection too, and the broker then holds what went with it (below) and keeps answering.
Either way the broker sets `agentkourier_config_stale` to 1 and logs `custom resource informer cannot list or watch`, and a restart exits with code 2, since the CRDs are not installed.

**The move back keeps answering.** The chart deletes what it rendered one object after another while the old pods still run: the AgentBackend, the Bindings, the ChatConnection last.
A broker that finds its own ChatConnection deleted holds it at its last good generation, and holds with it, at theirs, the Bindings that are gone and the AgentBackends that
those Bindings and the Bindings still there use, until the connection is back or the broker restarts. So the old pods keep routing until they end, a failover keeps routing,
and the new pods take over with the ConfigMap's config. A broker that restarts while it holds has no last good generation and fails at start. A deletion takes effect only at a read of
the cluster that finds nothing newly gone, at least 8 seconds after the last read that did, so the deletes of one upgrade count as one change. A deletion that goes on for longer than a
minute between its first object and the connection can still lose what went first: an AgentBackend deleted first leaves its Bindings not served. `agentkourier_config_held_objects`
says what is held ([Metrics](../reference/metrics.md#connection-configuration-and-turns)), and [Alert on what matters](export-telemetry.md#alert-on-what-matters) has a rule for it.

<!-- ROLLBACK NUMBER: the measured loss of a move back goes here, as one sentence, if a reader needs it; pending from the migration rerun. Delete this marker either way. -->

**The cost.** Deleting a ChatConnection on purpose does not stop routing until the broker restarts. A Binding deleted alone, while its connection exists, stops in about 10 to
20 seconds, later behind a stream of other deletions and after 60 seconds at most. One deleted within a minute before its connection comes back as held. While the connection is held, a
Binding that is deleted stays held: bring the connection back, then delete the Binding.

## When it does not work

| What you see | Why, and what to do |
|---|---|
| `helm upgrade` fails with `config.source crds needs the CRDs` | Step 1 was skipped, or the CRDs were deleted. Install them first. Nothing changed. |
| `helm upgrade` fails with `config.source crds needs Kubernetes 1.33 or later` | The API server is older. The ConfigMap route has no minimum: stay on it. With `helm template`, pass the cluster's version with `--kube-version`. |
| The pod crash-loops and `kubectl logs` says `ChatConnection agent-kourier/slack was not found` (exit code 2) | The broker owns one ChatConnection, named by `AGENTKOURIER_CHAT_CONNECTION`, and does not start without it. Apply it, or name the one that exists in `config.chatConnection`. Once the resources exist the pod starts at its next restart, with nothing else to do. |
| `helm upgrade` fails with `conflict with "kubectl-patch"` or another manager | Someone edited a chart-rendered resource by hand, and Helm will not overwrite a field another manager set. Put the change in your values. To discard the hand edit, add `--force-conflicts`. |
| `helm upgrade` fails with `field not declared in schema`, or `Invalid value` | A rendered resource has a field the schema does not have, or a value it refuses. The release goes to `failed` and the objects stay as they were; fix the values and upgrade again. |
| The pod exits with code 2 and `the custom resource definitions are not installed` | The CRDs are not in the cluster. Install them, as in step 1. |
| A reinstall under another release name fails with `invalid ownership metadata` | The kept CRDs name the release that made them. See the [chart README](https://github.com/agentkourier/agent-kourier/blob/main/charts/agent-kourier/README.md#custom-resources-instead-of-a-configmap) for the two ways on that delete nothing. |

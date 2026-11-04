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
- **Helm 4.** What this page says Helm does was measured with Helm 4.3.0. Helm 3 was not run.
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
apply fails (measured: `resource mapping not found ... ensure CRDs are installed first`), so the chart refuses to render the
custom resources until the cluster already serves `agentkourier.dev/v1alpha1`. Skip step 1 and the second upgrade fails with
`config.source crds needs the CRDs (group agentkourier.dev, version v1alpha1) to be installed before this release`, and changes nothing.

Put `crds.install: true` in your values file and keep it there. A release that has it turned off no longer holds the CRDs in its
manifest, so a later chart upgrade does not change them (measured with `helm get manifest`, which goes from three CRDs to none).
The CRDs stay in the cluster either way.

### 1. Install the CRDs, still on the ConfigMap route

```sh
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
  -n agent-kourier -f my-values.yaml --set crds.install=true
kubectl get crd | grep agentkourier.dev
```

The `grep` prints three lines: `agentbackends.agentkourier.dev`, `bindings.agentkourier.dev` and `chatconnections.agentkourier.dev`.
The broker still reads its files, and the pod does not restart (measured: the pod's age did not change). The CRDs are
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
the names, so a conversation continues where it was (measured with a fake Slack: a reply in a thread that began before the move got
an answer after it). A Binding that the new broker has not yet probed reads `Unknown` with the reason `BackendReachable:Pending`.

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
resource, which is something else. It answers `MethodNotAllowed` in a namespace and `NotFound` with `-A` (measured).

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
`config: every object is accepted` when none is left. The hash is the one the broker compares with the one it was built with, every minute:
`agentkourier_crd_schema_state` says `match`, or `mismatch` when the CRDs and the image come from different chart releases.

## Let a team add its own Binding

On this route a team writes its Binding, and an AgentBackend if it has its own, in its own namespace. Two grants are the platform
team's to make.

1. **The ChatConnection must allow the namespace.** List it in the connection's `allowedNamespaces`, which with rendered resources is
   `config.chatConnections.<name>.spec.allowedNamespaces` in your values. A Binding from another namespace reads
   `Accepted=False` with the reason `NamespaceNotAllowed` until it is listed.
2. **The broker must be allowed to read the team's Secret.** The team makes a Role that names the Secret, and a RoleBinding to the
   broker's ServiceAccount (here `agent-kourier` in the namespace `agent-kourier`):

   ```yaml
   apiVersion: rbac.authorization.k8s.io/v1
   kind: Role
   metadata: {name: agent-kourier-secrets, namespace: team-a}
   rules:
     - apiGroups: [""]
       resources: [secrets]
       resourceNames: [team-a-agent-token]
       verbs: [list, watch]
   ---
   apiVersion: rbac.authorization.k8s.io/v1
   kind: RoleBinding
   metadata: {name: agent-kourier-secrets, namespace: team-a}
   roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: agent-kourier-secrets}
   subjects:
     - {kind: ServiceAccount, name: agent-kourier, namespace: agent-kourier}
   ```

   The verbs are `list` and `watch`, not `get`. Check with `kubectl auth can-i list secrets/team-a-agent-token -n team-a --as
   system:serviceaccount:agent-kourier:agent-kourier`: it says `yes`, and `list secrets` without the name says `no`.
   Until the Role exists the Binding is `BackendReachable=False` with `CredentialsUnreadable`; measured here, it became `Ready` 31 seconds
   after the Role was applied.

The team then applies its resources:

```sh
kubectl apply -f team-a.yaml
kubectl -n team-a wait --for=condition=Ready akbinding/team-a-chat --timeout=120s
```

What a team that can write these objects can do with the broker's access is in [Security model](../explanation/security.md#what-the-people-who-write-these-objects-can-do).

## Move back to the ConfigMap

Set `config.source: files` (the other values of the route may stay set: with `files` they are ignored) and upgrade:

```sh
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
  -n agent-kourier -f my-values.yaml --set config.source=files
```

Keep `crds.install: true` while you do; it does not need to change. What happens, as measured:

| | After the move back |
|---|---|
| The ConfigMap | Returns, rendered from the same `config.*` values. A `config.existingConfigMap` was never touched. |
| Custom resources the chart rendered | Deleted, because the chart stops rendering them. Rendering them again later, by the same values, makes the same objects. |
| Custom resources you applied | Stay in the cluster and are not read. The broker serves what the ConfigMap holds, so a team's Binding is **not** served until it is in the ConfigMap, and the sessions of a Binding that is not in the config are left as they are. |
| Their status | Stays as it was and is no longer written: a `Ready` of `True` in the `READY` column can be days old. Compare a condition's `observedGeneration` with `metadata.generation`: after an edit they differ for good (measured: generation 2 against `observedGeneration` 1, 40 seconds after the edit). |
| The CRDs | Stay, with every custom resource: a CRD is never deleted by the chart. |
| The ClusterRole and its binding | Deleted (they render only with `crds`). A team's own Role and RoleBinding stay. |
| Sessions, the outbox and the audit log | Untouched: they live in the store. |

**Never delete a CRD to get out of a refusal.** Deleting it deletes every custom resource of that kind in the cluster, which is every
team's. A broker that is running when its CRDs are deleted keeps serving the config it had (measured: it logs
`custom resource informer cannot list or watch` and sets `agentkourier_config_stale` to 1), and a restart then exits with code 2.

> **[PLACEHOLDER: not yet decided by the owner. Replace this paragraph before the page is published.]**
> This is where the page says what the move back does to a mention that arrives while the pods change: whether a mention
> in that window can go unanswered until the new leader takes over, how long that lasts on SQLite (one replica, Recreate strategy)
> and on Postgres (a rolling update), and whether to schedule the move. Nothing here has been measured for the window yet.

## When it does not work

| What you see | Why, and what to do |
|---|---|
| `helm upgrade` fails with `config.source crds needs the CRDs ... to be installed before this release` | Step 1 was skipped, or the CRDs were deleted. Install them first. Nothing changed. |
| `helm upgrade` fails with `config.source crds needs Kubernetes 1.33 or later` | The API server is older. The ConfigMap route has no minimum: stay on it. With `helm template`, pass the cluster's version with `--kube-version`. |
| The pod crash-loops and `kubectl logs` says `ChatConnection agent-kourier/slack was not found` (exit code 2) | The broker owns one ChatConnection, named by `AGENTKOURIER_CHAT_CONNECTION`, and does not start without it. Apply it, or name the one that exists in `config.chatConnection`. Measured: with the resources applied after the release, the pod recovered at its next restart. |
| `helm upgrade` fails with `conflict with "kubectl-patch"` or another manager | Someone edited a chart-rendered resource by hand, and Helm will not overwrite a field another manager set. Put the change in your values. To discard the hand edit, add `--force-conflicts`. Measured with Helm 4.3.0. |
| `helm upgrade` fails with `field not declared in schema`, or `Invalid value` | A rendered resource has a field the schema does not have, or a value it refuses. The release goes to `failed` and the objects stay as they were; fix the values and upgrade again. |
| The pod exits with code 2 and `the custom resource definitions are not installed` | The CRDs are not in the cluster. Install them, as in step 1. |
| A reinstall under another release name fails with `invalid ownership metadata` | The kept CRDs name the release that made them. See the [chart README](https://github.com/agentkourier/agent-kourier/blob/main/charts/agent-kourier/README.md#custom-resources-instead-of-a-configmap) for the two ways on that delete nothing. |

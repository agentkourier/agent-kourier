# Agent Kourier

Installs agent-kourier, the broker between chat and A2A agents: one Deployment of the
production image, its state (a SQLite volume, or a Postgres database you run), its resources (custom resources, or a ConfigMap), and the read
access to the Secrets they name.

**One replica with SQLite, or two with Postgres.** SQLite is one file on a volume that one pod may use, so the
chart refuses `replicaCount` above 1 with it. With `store.type: postgres`, `replicaCount: 2` is an active/passive pair (see High availability). `replicaCount: 0` is the kill switch (Agent Kourier stops, the volume or the database and the config stay);
removing a Binding from the values is the other.

Agent Kourier does nothing without its resources: a ChatConnection, an AgentBackend and a Binding. **The chart's default,
and the route to run on Kubernetes, reads them as custom resources** (`config.source: crds`, with the CRDs installed by `crds.install`, which follows the route and is true with this one),
so that each team owns its own Bindings: see [Custom resources instead of a ConfigMap](#custom-resources-instead-of-a-configmap), which also says what it needs
(Kubernetes 1.33 or later, a ChatConnection to name, and a grant of the Secret it reads). **The quick route for a proof of concept is a ConfigMap** that the chart writes from the resources under
`config` (`config.source: files`), which needs none of that. The starter file below is that route. Put the resources in a values file,
`my-values.yaml`: download the commented starter and change the lines it marks `yours`:

```sh
curl -fsSLo my-values.yaml https://raw.githubusercontent.com/agentkourier/agent-kourier/main/examples/helm/my-values.yaml
```

It holds the same resources as this, and says the route (the ConfigMap, which installs no CRDs) in its first lines:

```yaml
config:
  source: files
  chatConnections:
    slack:
      spec:
        platform: slack
        credentialsSecretRef: {name: slack}   # a Secret with botToken and appToken keys
  agentBackends:
    agent:
      spec:
        dialect: a2a
        url: http://my-agent.my-namespace.svc.cluster.local:8083/a2a/
  bindings:
    my-binding:
      spec:
        agent: {backendRef: {name: agent}, namespace: my-namespace, name: my-agent}
        identity: {userId: agent-kourier}
        chat: {connectionRef: {name: slack}, channel: C0123456789, output: live}
        interactions: {askUser: false}
```

Then install the published chart with it:

```sh
helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.2.1 \
  -n agent-kourier --create-namespace -f my-values.yaml
```

For the default route, give the chart no resources, name the ChatConnection the broker serves, and apply the three resources yourself once the CRDs are in
(they come with the release, and the pod waits for the ChatConnection: it exits and restarts until the object exists). The chart refuses to render
`config.chatConnections`, `config.agentBackends` or `config.bindings` as custom resources in the release that installs their CRDs, since Helm cannot map a kind in the same
first apply (see below):

```yaml
config:
  chatConnection: agent-kourier/slack
```

```sh
helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.2.1 \
  -n agent-kourier --create-namespace -f my-values.yaml
kubectl apply -f my-resources.yaml   # a ChatConnection slack (reading the Secret slack), an AgentBackend and a Binding, in namespace agent-kourier
kubectl -n agent-kourier get akbinding
```

With `config.*` empty the chart cannot see the ChatConnection's Secret, but it knows which connection the broker serves, so it renders a Role in that connection's
namespace that names one Secret: the Secret of the same name as the connection (`slack` here). A ChatConnection whose `credentialsSecretRef` names another Secret needs
`config.chatConnectionSecret` set to that name (an `rbac.secretNames` entry for it works too); otherwise the broker could not read it and would exit with code 3 in a loop. Any other Secret that the chart
cannot see, such as a Binding's token in the release namespace, still needs a line in `rbac.secretNames` or a Role of its own.

[Install with Helm](https://agentkourier.dev/how-to/install-with-helm/#write-the-values) walks through each field, the
Secrets, and a dry run that finds a misspelt field before the pod does. The published chart has `image.repository` and
`image.digest` stamped with the release's image. From source (`charts/agent-kourier` in a checkout) the chart also
needs `image.repository`. Check a values file with `helm template` before installing, or from a checkout with
`helm lint --strict -f my-values.yaml charts/agent-kourier`. `ci/` holds the values the tests render: `minimal`, `crds` (the route of custom resources, with grants of Secrets by name, which `helm template` renders only with `--kube-version` and `--api-versions agentkourier.dev/v1alpha1`), `postgres` (minimal on Postgres, with a NetworkPolicy), `full` (everything switched on), and the kind tiers' Agent Kourier, which
`hack/kind` installs with this chart: `fast` (the fast tier's, and the full tier's), `fast-postgres` (the fast tier's on Postgres:
`E2E_STORE=postgres make e2e-fast-up`; `fast-postgres-ha` is an overlay on it, layered with a second `-f`, for two replicas and a shorter lease:
`E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up`, which the two-replica failover end-to-end test runs on), `ax` (the AX tier's, with a database file of its own) and `sandbox` (the sandbox
tier's, with the token-refresh CronJob).

## What it installs

| Object | When | Notes |
|---|---|---|
| Deployment `<fullname>` | always | one container, config at `/etc/agent-kourier` (the ConfigMap of `config.source: files`; the default, `crds`, mounts none), SQLite at `/data` (with `store.type: sqlite`, strategy Recreate), `/tmp` an emptyDir; with Postgres strategy RollingUpdate (one surge, none unavailable) |
| ServiceAccount, and a Role and RoleBinding for Secrets | always (`rbac.create`) | one Role in each namespace that holds a Secret the config names (with `config.source: crds`, only the Secrets the chart can name: `config.*`, the served ChatConnection's Secret (`config.chatConnection`, named like it unless `config.chatConnectionSecret` says otherwise) and `rbac.secretNames`), and in the release namespace with Postgres (the DSN's Secret). It covers every Secret of the namespace; with `config.source: crds` it names the Secrets (`resourceNames`), except in the namespaces of `rbac.secretNamespaces` |
| Lease `<fullname>-leader` | `store.type: postgres` | empty; the replicas elect their leader with it. Kept on `helm uninstall` (`helm.sh/resource-policy: keep`), with its release annotations, so a reinstall under the same release name adopts it; only another release that renders the same name (`fullnameOverride`) would conflict. Do not delete it while Agent Kourier runs: Agent Kourier may not create a Lease, so leadership stops until `helm upgrade` recreates it |
| Role and RoleBinding `<fullname>-leader` | `store.type: postgres` (`rbac.create`) | `get` and `update` on that one Lease, in the release namespace; no `create`, which cannot be limited by name |
| ConfigMap `<fullname>-config` | `config.source: files`, unless `config.existingConfigMap` | one `*.yaml` file per resource, from `config.*` |
| ChatConnection, AgentBackend and Binding custom resources | `config.source: crds` (the default), and an entry in `config.*` | one per entry of `config.*`, in its namespace (default: the release's), which must exist; deleted when the release stops rendering them |
| CustomResourceDefinitions `agentbackends`, `bindings` and `chatconnections` of `agentkourier.dev` | `crds.install`: unset, true with `config.source: crds` and false with `files`; an explicit value wins | the generated files of `files/crds`; kept on `helm uninstall` and when `crds.install` is turned off (`helm.sh/resource-policy: keep`, which is Helm's; for Argo CD see [below](#custom-resources-instead-of-a-configmap)) |
| ClusterRole and ClusterRoleBinding `<fullname>-<namespace>-crs` | `config.source: crds`, the default (`rbac.create`) | `list` and `watch` on the three kinds, `patch` on `bindings/status`, `get` on the three CRDs by name; see Custom resources instead of a ConfigMap |
| PersistentVolumeClaim `<fullname>-data` | `store.type: sqlite`, unless `persistence.existingClaim` | kept on `helm uninstall` (`persistence.retainOnUninstall`); none with Postgres |
| Service | `service.enabled` | ClusterIP on the health and metrics port, and nothing else |
| ServiceMonitor | `serviceMonitor.enabled` | prometheus-operator; needs the Service |
| NetworkPolicy | `networkPolicy.enabled` | see below |
| PodDisruptionBudget | `podDisruptionBudget.enabled` | off; `minAvailable: 1` keeps a drain from taking both replicas at `replicaCount: 2`; the render refuses a budget that keeps every pod (`minAvailable` >= replicas, 100%, `maxUnavailable: 0`), since it would block every node drain |
| CronJob, ServiceAccount, Role, RoleBinding `<fullname>-token-refresh` | `tokenRefresh.enabled` | keeps a Binding's Dex ID token fresh |

`<fullname>` is the release name, or `<release>-agent-kourier` when the name does not contain "agent-kourier" (`fullnameOverride` sets it).

Argo CD names the Helm release after the Application. An Application called `my-agent-kourier` makes that the
fullname, so the Deployment and the Service are not called `agent-kourier`. Anything that addresses them from outside, a
NetworkPolicy in another namespace for instance, needs the real name, or you set `fullnameOverride`.

## Values

`values.yaml` documents every key; `values.schema.json` rejects an unknown or mistyped one. The ones you will set:

- `image.repository`, `image.tag` (default: the chart's appVersion) or `image.digest` (wins; pin production to it).
- `config.chatConnections`, `config.agentBackends`, `config.bindings`: maps of name to `{namespace, spec}`, where `spec` is the
  resource's spec exactly as the [resource reference](../../docs/reference/binding.md) describes it (the chart does not interpret it; Agent Kourier's loader validates it at start).
  `namespace` defaults to the release's. Or `config.existingConfigMap`, a ConfigMap you manage, with `rbac.secretNamespaces` to say where
  its Secrets are. Not both.
- `config.source` (`crds`, the default, or `files`), `config.chatConnection`, `config.chatConnectionSecret`, `config.connectionNamespaces` and `crds.install` (unset: true with `crds`, false with `files`): where Agent Kourier reads those resources from
  (see Custom resources instead of a ConfigMap). An install of the ConfigMap route that upgrades with the values it had gets the default: set `config.source: files` to keep it.
- `store.type` (`sqlite`, the default, or `postgres`) and, with Postgres, `store.postgres.dsnSecret` and `store.postgres.timeouts` (see Store).
- `replicaCount` (2 with Postgres for a standby) and `leaderElection.*`, the Lease's timings (see High availability).
- `logLevel`, `syncTimeout`, `auditRetention` (empty keeps the audit log for ever; a duration of at least 24h purges older entries), `extraEnv` (OpenTelemetry settings go here), `resources`, `persistence.*` (SQLite's volume).
- `service`, `serviceMonitor`, `networkPolicy`, `podDisruptionBudget`, `tokenRefresh`: off until you switch them on.

With the files route a config change rolls the pod (a `checksum/config` annotation). Agent Kourier also reloads its files in place, and follows a Secret
that a reload adds, in any namespace its ServiceAccount may list and watch it in. A Secret it may not read fails that reload: the last
good config stays in force, and the reload is tried again at the next recheck, which runs every minute. Turn the roll off with `config.rollOnChange: false`.

## Custom resources instead of a ConfigMap

`config.source` says where Agent Kourier reads its resources, and the pod's `AGENTKOURIER_CONFIG_SOURCE` follows it. **`crds`, the default and the route to run on Kubernetes,**
reads `Binding`, `AgentBackend` and `ChatConnection` custom resources of group `agentkourier.dev` from the cluster, so that other teams can own their Bindings:
a resource that fails validation is left out (or, if it had a good generation, that generation keeps serving) and the others carry on. **`files`, the quick route
for a proof of concept,** is the ConfigMap (or your own) of the sections above, loaded all or nothing. The two are never merged and there is no
fallback: with `files`, custom resources in the cluster are ignored, and with `crds` there is no ConfigMap and no config directory. The route is
chosen at start; moving between them is an upgrade of the release. With `config.source: files`, the chart renders the objects it rendered when
the ConfigMap was the default and the route adds nothing to the pod's template (`test/chart` pins eight values files against golden files, each naming its route).
`crds.install`, unset, follows the route: true with `crds` and false with `files`. An explicit value wins: `true` with `files` renders the three CRDs, inert there, which is the first step of a move, and `false` with `crds` is for an installer that applied them itself.

**An install of the ConfigMap route that upgrades with the values it had.** It gets the default route, and the chart refuses it where it can tell, with a message that
names the way out: `config.existingConfigMap` with the default (set `config.source: files`, or move by the how-to below), a cluster older than 1.33, and `config.*` resources that
the cluster cannot yet map because their CRDs are not served. A cluster that already serves the CRDs, with `config.*` resources, is not refused: the chart renders them as custom
resources and the ConfigMap goes, which is the second step of the move below; check the objects first, as the how-to says. A release with no `config.source` in its values (`helm upgrade --reuse-values`
from 0.1.6) reads as `files`, so that it keeps its route (rendered with `--set config.source=null`; that Helm hands the chart no new default there is unverified).

What the route needs:

- **Kubernetes 1.33 or later.** 1.33 is the first minor tried on which a server-side apply of a Binding's status, to a Binding stored invalid by a rule of the CRDs (which a later, stricter schema can cause), ratchets: it is refused on 1.30.13, 1.31.0, 1.32.0 and 1.32.5 and accepted on 1.33.1, 1.35.0 and 1.37.0 (measured on kind; 1.33.0 and 1.34 were not tried). The answer carries a warning when the rule that the stored spec breaks is a CEL rule, and none when it is an OpenAPI schema pattern such as the channel's (measured on 1.37.0 only). Ordinary updates of such a Binding ratchet from 1.30. The chart refuses an API server
  it can see is older, and says to set `config.source: files`, which has no minimum.
- **The CRDs installed before a release that renders custom resources from `config.*`.** A first install that renders the CRDs and custom resources of them in
  one release fails (measured: `resource mapping not found ... ensure CRDs are installed first`), so the chart refuses to render the custom
  resources unless the cluster already serves `agentkourier.dev/v1alpha1`, and says what to do. A release with nothing in `config.*` renders no custom resource, so it may install the
  CRDs itself (`crds.install`, true with this route unless you set it): that is the first install above. `crds.install: true` renders the CRDs, from the
  generated files in `files/crds`; or apply those files yourself (`kubectl apply --server-side -f charts/agent-kourier/files/crds`) and set it
  to `false`, which is the way for an installer that may not create CRDs. Helm will not take over CRDs that `kubectl` made (`invalid ownership metadata`), so choose one of the two and keep to it.
- **`config.chatConnection`**: the one `ChatConnection` this broker serves, as `namespace/name` (or `name`, in the release namespace). It may be
  left empty when `config.chatConnections` has exactly one entry. A cluster may hold the connections of several brokers, and each owns only the
  Bindings whose `connectionRef` names its connection.
- **`config.chatConnectionSecret`**: the name of the Secret that connection reads, in the connection's namespace, when it is not named like the connection
  (empty, the default, is the connection's own name). With `config.source: crds` and `config.chatConnection` set, the chart grants `list` and `watch` on that
  one Secret by name, since it cannot read the ChatConnection you apply. It is skipped when `config.chatConnections` renders that connection (its
  `credentialsSecretRef` is granted instead), and ignored with `files` or an empty `config.chatConnection`.
- **`config.connectionNamespaces`**: the admission list, a list of namespace names (empty, the default). The broker serves the `ChatConnection`s of these namespaces
  beside its own, at most 16 connections in all with its own counted first and the others oldest first; no setting changes the 16. Two
  `ChatConnection`s of one Slack app (one app ID, even in two workspaces) are not both served: the older is. The chart renders the list as
  `AGENTKOURIER_CONNECTION_NAMESPACES` (comma-joined) and renders nothing when it is empty, and it refuses a non-empty list with `config.source: files`. The ClusterRole
  above already reads the three kinds in every namespace, so the list adds no rule, and the chart grants no Secret of a listed namespace: each team grants the credentials
  Secret of its connection to the ServiceAccount with a Role of its own that names it (`resourceNames`, as for a Binding's token below), or you name it in `rbac.secretNames`
  or its namespace in `rbac.secretNamespaces`. A team connection whose Secret the broker cannot read yet is started once it can, and never stops the broker.
- **Namespaces that exist.** A file can name a namespace that does not exist; a custom resource cannot.

Move a running install of the ConfigMap route to it in two upgrades, each a working install, and back by the same values (keep `crds: {install: true}` in the values
file: a release without it no longer holds the CRDs, so no later chart upgrade changes them; measured with `helm get
manifest`, which goes from three CRDs to none). The values file of that install names the route it has, `config.source: files`, since the default is now the other:

```sh
# 1. The CRDs, with the route still files: they are inert.
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <this chart's version> -n agent-kourier \
  -f my-values.yaml --set config.source=files,crds.install=true
# 2. The route: the same config.* values become custom resources, and the ConfigMap and its mount go. crds.install stays on.
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <this chart's version> -n agent-kourier \
  -f my-values.yaml --set config.source=crds,crds.install=true
kubectl get akbinding,akbackend,akconnection -A
```

`--reuse-values` works from the previous release (0.1.6) too: the chart reads the keys it adds as unset when the old values lack them, and an unset `config.source` as `files`.

`kubectl get akbinding`, `akbackend` and `akconnection` (or `bindings.agentkourier.dev` and so on, and `kubectl get agentkourier` for all
three) list them. **Never `kubectl get bindings`**: that is the core `bindings` resource, which is something else and fails. Going back, with
`config.source=files`: the ConfigMap returns from the same values, and Helm deletes the custom resources the chart rendered (it stops rendering
them), while the ones made by hand stay in the cluster, unread, as do the CRDs. The values of the route (`config.chatConnection`, `config.chatConnectionSecret`, `rbac.secretNames`)
may stay set: with `files` they are ignored, so going back is the same values with `config.source: files`, except `config.connectionNamespaces`, which the chart refuses with `files`: set it to `[]` in the same upgrade. `helm uninstall` likewise deletes the
rendered resources and keeps the CRDs, and with them every custom resource of the cluster: **never delete a CRD to get out of a refusal**, since
that deletes every team's resources. A reinstall under another release name (or in another namespace) with `crds.install: true`, which is what an unset value is on the default route, is refused by
Helm (`invalid ownership metadata`), because the kept CRDs name the release that made them. The same holds for a second broker in the same cluster, which sets `crds.install: false`. There are two ways on, neither of which deletes
anything:

- Install the new release with `crds.install: false` (a second broker does this too). The CRDs are used as they are, and nothing then upgrades them, so a later chart that changes
  a schema needs one of the next.
- Hand the CRDs to the new release, then install it with `crds.install: true`: `kubectl annotate crd agentbackends.agentkourier.dev
  bindings.agentkourier.dev chatconnections.agentkourier.dev --overwrite meta.helm.sh/release-name=<new release>
  meta.helm.sh/release-namespace=<its namespace>`. Both annotations are needed when the namespace differs (measured: with only the name, Helm
  still refuses; with both it installs and adopts the CRDs).

Install the CRDs from one release only.

**"Kept" is Helm's guarantee only.** Under Argo CD with prune, turning `crds.install` off, or deleting the Application, may delete the CRDs, and every team's
Binding, AgentBackend and ChatConnection with them. The chart renders `argocd.argoproj.io/sync-options: Prune=false,Delete=false` on each CRD, which Argo CD's documentation says stops
that; this is **unverified**, since Argo CD has not been run against this chart. Another tool that prunes needs its own way to be told to leave the CRDs, or the CRDs applied outside
what it prunes.

The documentation has the same move as a how-to, with the checks to run and the grant a team makes for its own Secrets:
[Move the config between a ConfigMap and custom resources](https://agentkourier.dev/how-to/migrate-config-source/).

**What the route grants.** With `rbac.create`, a ClusterRole `<fullname>-<namespace>-crs`, bound to the ServiceAccount:

| Rule | Why |
|---|---|
| `list`, `watch` on `bindings`, `agentbackends`, `chatconnections` of `agentkourier.dev` | every replica reads the three kinds with informers, in every namespace (it never issues a `get`) |
| `patch` on `bindings/status` | for the leader to write a Binding's status with a server-side apply, which is a `patch` on the subresource and needs no `create`, `update` or `get` (measured on kind) |
| `get` on the three CRDs, by name | the broker compares, at start and every minute, the annotation `agentkourier.dev/schema-sha256` of each CRD with the hash it was built with, and says the result in its log (when a CRD's result changes) and in `agentkourier_crd_schema_state`. The generated CRD files carry the annotation (their SHA-256 without that line), so a Helm install and a `kubectl apply` apply it (measured), and a GitOps tool that applies the files should too (a statement of what such a tool should do; none has been run). A mismatch warns and stops nothing: update the CRDs the way they were installed, to those of the chart release that matches the image: `helm upgrade` with `crds.install` for CRDs that Helm made, `kubectl apply --server-side -f` of that release's `files/crds` for CRDs that `kubectl` made, and the next minute's comparison says so. What was measured on kind is a forced server-side apply of the files over Helm-made CRDs, which cleared a mismatch; it also moves those fields to `kubectl`, which a later `helm upgrade` may conflict with (not measured). The annotation is a statement by whoever wrote the CRD, not proof of what the API server serves: a CRD edited by hand keeps its hash. A CRD from an older chart or file has none and is reported as `unannotated`, which is not a fault; one the broker may not get is `unknown` |

The Secret Roles change too: with `crds` each names the Secrets the chart can see (`resourceNames`) instead of the whole namespace: those that
`config.*` and the Postgres DSN refer to, the one the served `ChatConnection` reads (`config.chatConnection`: the Secret of the same name, or
`config.chatConnectionSecret`, in the connection's namespace), and `rbac.secretNames` (a map of namespace to Secret names) for the ones it cannot
see, such as a Binding's token in the release namespace. That works because Agent Kourier's informers ask for each Secret by name (a `metadata.name`
field selector), and Kubernetes authorizes `list` and `watch` by `resourceNames` for such a request: a list without the selector, a Secret the Role
does not name, and a `get` are forbidden (measured on kind, Kubernetes 1.30.13 and 1.37.0). A namespace in `rbac.secretNamespaces` is still granted whole.
A team that makes its own Bindings grants its own Secrets the same way, with a Role in its namespace and a RoleBinding to this ServiceAccount
(here `agent-kourier` in namespace `agent-kourier`; without the binding the Role grants nothing, and `kubectl auth can-i` says no). Give the Role a name the chart would not (`<fullname>-secrets` is the chart's own, which is `agent-kourier-secrets` here), so that the two never meet (a chart that renders its own Role of that name in the team's namespace, as `rbac.secretNamespaces` does, fails with `invalid ownership metadata` over a Role it did not make):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: agent-kourier-secrets-team-a, namespace: team-a}
rules:
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [team-a-agent-token]
    verbs: [list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: agent-kourier-secrets-team-a, namespace: team-a}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: agent-kourier-secrets-team-a}
subjects:
  - {kind: ServiceAccount, name: agent-kourier, namespace: agent-kourier}
```

A Binding whose Secret is not granted does not stop the broker or fail a reload: it stays served, and its `BackendReachable` condition says
`CredentialsUnreadable` (the log gives that reason; the metric `agentkourier_binding_condition` shows only `status="false"` for the condition). Only the Secrets the broker itself needs (the connection's tokens, the
Postgres DSN's) fail the start with exit code 3.

Whoever can write a Binding and an AgentBackend in a namespace can have any Secret of that namespace that Agent Kourier may read sent to the
AgentBackend's URL, so that grant is also what bounds what they can reach.

To preview a render, use `helm template` and give it what a cluster would: `--kube-version` (Helm's default is its own) and `--api-versions
agentkourier.dev/v1alpha1`, which a tool that renders with `helm template` for you (a GitOps controller) must pass from the cluster. Without them
`helm template` fails with the chart's message, and `helm install --dry-run=client` likewise. `helm lint` takes no API versions, and in its mode Helm
logs the chart's refusal and goes on, so a lint of such a values file passes without having checked the CRDs.

## Store

`store.type: sqlite` (the default) keeps Agent Kourier's state in one file on the volume, for **one replica**: a SQLite file may be open in one
pod only. Agent Kourier cannot see how many replicas run, so the chart is what refuses more than one with it (`replicaCount`
above 1 fails the render with SQLite).

`store.type: postgres` keeps it in a database you run (the chart installs none). Name the Secret that holds the DSN, in the release
namespace, and the key in it:

```yaml
store:
  type: postgres
  postgres:
    dsnSecret: {name: agent-kourier-pg, key: dsn}   # a postgres:// URL or a key/value string; never a value in the values
```

The render refuses `store.type: postgres` without the Secret, a `dsnSecret` (or `timeouts`) with SQLite, an `existingClaim` with
Postgres, and a `postgres://` URL with a password anywhere in the values. With Postgres no volume is mounted and no claim is made.
Agent Kourier reads the Secret through the API, like its others, so the Role below covers it: list and watch on every Secret of the
release namespace. The DSN is read once at start; rotate the password by changing the Secret and the database role, then restart the
pod. An unreadable or empty Secret fails the start with an error that names the Secret and the key, never the DSN. The role in the
DSN needs the right to create tables in its schema, as Agent Kourier applies its own migrations at start (under an advisory lock). With
`networkPolicy.enabled`, `networkPolicy.postgres.to` (NetworkPolicyPeers: an ipBlock, or a namespaceSelector and podSelector) and
`.port` (default 5432) say where the database is, and the render refuses Postgres without `to`: the Slack rule excludes private ranges,
so without it Agent Kourier could not reach the database.

**Connection poolers.** Agent Kourier's idle-in-transaction and statement timeouts are sent as startup parameters on each connection:
they are what ends a stalled leader's transaction and frees its locks. So connect directly, or through a pooler in
**session** mode. A pooler in **transaction** mode (PgBouncer's `pool_mode = transaction`) rejects startup parameters
("unsupported startup parameter"), and listing them in `ignore_startup_parameters` silently drops the protection. There, set the
timeouts on the role and turn Agent Kourier's off:

```sql
ALTER ROLE agentkourier SET idle_in_transaction_session_timeout = '10s';
ALTER ROLE agentkourier SET statement_timeout = '30s';
```

and set `store.postgres.timeouts: role`.

## High availability

With `store.type: postgres` Agent Kourier elects a leader with a Kubernetes Lease, named `<fullname>-leader` in the release namespace, which the chart
makes; the Role above is all the access that needs. The leader runs all of Agent Kourier's work (Socket Mode, the session runners, the outbox, the
sweeps and digests). The other replica is **Ready and idle**: it serves `/healthz`, `/readyz` and `/metrics`, keeps its config and Secret caches
fresh, and waits. Readiness never depends on the lease (nothing routes traffic to Agent Kourier in Socket Mode, and a standby is healthy), so
find the leader with the metric `agentkourier_leader` (1 on the leader; `agentkourier_leader_takeovers_total` counts takeovers) or the
`leadership` table's `holder`. Postgres elects at one replica too: that replica leads at once, and a rolling update starts the new pod
as a standby beside the old one.

`leaderElection.leaseDuration` (default 15s), `renewDeadline` (10s) and `retryPeriod` (2s) set the timings: the leader gives up
renewing after the renew deadline and **exits at once**, 5 s before a standby may take the lease with the defaults, and the standby
takes it a lease duration after the leader's last renewal. After a crash, a deleted pod or a lost node, work resumes in about 15 s plus
Agent Kourier's start-up. A leader that stops on a signal (a rolling update, a drain) finishes its ordered shutdown and hands the lease over, so
the standby takes it after the old leader's ordered shutdown (up to 25 s), at its next attempt. Every write is also fenced by an epoch in the database, so a leader that was paused past its
lease cannot write when it wakes: the write fails and the process exits (exit code 5). Its chat writes and its sends, answers and cancels to the agent are refused too, once its lease is past the renew deadline. What fencing cannot cover is a Slack post or an
agent send that the paused leader had already begun, which can happen once more (see [High availability and failover](../../docs/explanation/high-availability.md)).

Set `affinity` (a `podAntiAffinity` on `app.kubernetes.io/name: agent-kourier`) so the two replicas do not share a node, which the chart does not
do for you, and `podDisruptionBudget.enabled` with `minAvailable: 1` so a drain cannot take both.

## Secrets

Nothing secret is in the values. The config names Secrets (`credentialsSecretRef`, `tokenSecretRef`, `valueSecretRef`) and you
make them (by hand, Sealed Secrets, an external operator) before Agent Kourier starts:

- the ChatConnection's Secret, with `botToken` and `appToken`;
- each Binding's `tokenSecretRef` Secret, with a `token` key (or the key the reference says);
- with Postgres, the DSN Secret (`store.postgres.dsnSecret`), in the release namespace;
- the Dex credentials Secret for `tokenRefresh`, with `clientSecret`, `username` and `password`. The token-refresh Job reads it through its environment; Agent Kourier does not read it.

Agent Kourier reads them through the Kubernetes API, never from its environment or a mounted file. The render refuses what would put
a secret in the ConfigMap: a Slack token (`xoxb-`, `xoxp-`, `xoxa-`, `xoxe-`, `xoxr-` or `xapp-`, followed by digits) anywhere in the values, and an AgentBackend header `value` whose name
looks like a credential (key, token, secret, auth, cookie, password), which belongs in `valueSecretRef`.

## Security posture

- The pod runs as UID 65532, non-root, with the `RuntimeDefault` seccomp profile; the container has a read-only root
  filesystem, `allowPrivilegeEscalation: false` and every capability dropped. The only writable paths are the data volume and `/tmp`.
- The pod label `app.kubernetes.io/name: agent-kourier` is fixed, whatever the release is named, because NetworkPolicies elsewhere
  (the front door's, in the kind tiers) admit pods by it. Nothing else the chart makes carries it on a pod; in
  particular the token-refresh Job's pod does not.
- **The chart's Secrets RBAC is namespace-wide on the files route, and by name with `config.source: crds`** (see Custom resources instead
  of a ConfigMap; the rest of this paragraph is the files route). Agent Kourier follows each Secret the config names with an informer of
  its own, which needs `list` and `watch` on that Secret and asks for it by name (a `metadata.name` field selector); `get` is never
  used or granted. Kubernetes authorizes such a request by `resourceNames`, so with `rbac.create: false` you can grant `list` and
  `watch` on named Secrets instead. The chart's Roles name no Secret, so each reads every Secret in its namespace, although Agent
  Kourier keeps in memory only the Opaque ones that the config names, and of each only the keys that the config names (a Secret's other
  keys are not kept), and with Postgres the DSN's with every key, plus, until the next reload, any
  other Secret, or other key of one, that a turn begun under an earlier config still reads (internal/config/k8ssecrets documents it). The chart grants the Role only in namespaces that hold a Secret the config names. Give Agent Kourier's Secrets a
  namespace of their own when the namespace is shared. With Postgres the DSN's Secret is in the release namespace, which therefore gets this Role too.
- `klogVerbosity` stays 0 (the schema caps it at 7): at 8 and above client-go logs response bodies, Secret lists included.
- **NetworkPolicy** (`networkPolicy.enabled`, which needs a CNI that enforces it): egress to DNS, to the API server (you give
  its addresses in `kubeApiServer.cidrs`, since a policy cannot select it), to Slack on
  443 (to anywhere outside the private, CGNAT and link-local ranges, since a policy cannot name a host) and to what `extraEgress` adds (the agent
  backend's front door, an OTLP collector); ingress only from `metricsFrom` on the metrics port. Agent Kourier calls the verifying
  front door, never the controller on 8083; put the front door in `extraEgress`, and nothing else in that namespace.
  - On kind or kubeadm, `kubectl get endpoints kubernetes` gives the API server's addresses as /32s. On EKS those are
    control-plane ENI addresses that change when AWS upgrades the control plane: use the CIDRs of the control-plane subnets
    (`aws eks describe-cluster`, `resourcesVpcConfig.subnetIds`, each subnet's CIDR). The VPC CNI enforces NetworkPolicy only
    with `enableNetworkPolicy=true` on the vpc-cni add-on; without it the policy does nothing.
- The token-refresh Job's Role allows `patch` on one named Secret and nothing else. The Dex credentials reach `curl` through
  `--variable` from the environment, so they are never on an argument list; the Job prints no token.

## Cilium and the API server

`networkPolicy.kubeApiServer.cidrs` takes the control plane's addresses, and the chart renders them as an `ipBlock`. kind
enforces that as written. Cilium, by default, does not. It treats the API server as the `kube-apiserver` entity and not as a
CIDR, so an `ipBlock` on a node address does not match. The `policy-cidr-match-mode` setting in `cilium-config` changes that,
and it was empty on the first install. With the policy enabled, the pod's Secret informer times out and the pod exits with
code 4: `bootstrap: start the secret resolver: secret caches not synced`.

Add a `CiliumNetworkPolicy` beside the chart's. The two add up, so the chart's policy keeps fencing DNS, Slack and the agent:

```yaml
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: agent-kourier-kube-apiserver
  namespace: agent-kourier
spec:
  endpointSelector:
    matchLabels:
      app.kubernetes.io/name: agent-kourier
  egress:
    - toEntities:
        - kube-apiserver
```

To check it, run two throwaway pods in the release namespace, one with the Deployment's pod labels, `app.kubernetes.io/name:
agent-kourier` and `app.kubernetes.io/instance: <release>`, and one without, each calling `curl -sk
https://kubernetes.default.svc/version`. The chart's policy selects on both labels, so a pod with only one is not subject to
it. Before the Cilium policy exists, the labelled pod times out and the other gets a 200. The chart does not render this
object yet.

## Probes

`/healthz` is the process, for the startup (80 s) and liveness probes. `/readyz` is the config loaded, the Secret caches synced and the store
answering. **Ready does not mean Slack is connected**, by design: a Slack outage must not take Agent Kourier out of its Service or restart
it: the pod recovers when Slack does, and a not-ready pod would stall a rollout meanwhile. Watch `agentkourier_chat_connected`.

## Token refresh

For an AgentBackend whose front door verifies Dex ID tokens (the sandbox tier's, `ci/sandbox-values.yaml`),
`tokenRefresh.enabled` adds an hourly CronJob that asks Dex for a token (the password grant) and patches it into the Secret named
by `targetSecret` and `targetKey` in `targetNamespace` (default: the release's): the Binding's `identity.tokenSecretRef` and the
Binding's namespace. With the config inline on the files route the render fails unless some Binding in that namespace names that Secret and key, so
a token is never refreshed where nothing reads it. With `config.source: crds` the chart does not look (a team applies its own Binding, which the chart cannot see), so that check is yours. The Job's Role and RoleBinding are made in that namespace (the Job's
ServiceAccount stays in the release's). The Secret must exist before Agent Kourier starts; make it with a placeholder, then run the
first refresh by hand:

```sh
kubectl -n <ns> create job --from=cronjob/<fullname>-token-refresh first-token
```

## Moving from hand-made manifests

The kind tiers used to apply hand-made manifests (a Deployment that selected pods by `app.kubernetes.io/name` alone). The chart's
selector adds `app.kubernetes.io/instance`, and a Deployment's selector cannot change, so the old Deployment is deleted first (Agent Kourier
stops; sessions and the outbox are on the volume), and the chart is installed over the rest. Helm refuses to manage an object it did
not create unless it is marked as the release's, so the objects the chart also makes (the ServiceAccount, Role, RoleBinding, Service
and volume claim) are marked, not deleted. `hack/kind/lib.sh` does exactly this (`replace_unowned_deployment`, `adopt_into_release`), so that
re-running a tier's bring-up is how a running tier moves over. By hand, for release `agent-kourier` in namespace `agent-kourier`:

```sh
kubectl -n agent-kourier delete deploy agent-kourier --cascade=foreground
for obj in serviceaccount/agent-kourier role/agent-kourier-secrets rolebinding/agent-kourier-secrets service/agent-kourier pvc/agent-kourier-data; do
  kubectl -n agent-kourier annotate --overwrite "$obj" meta.helm.sh/release-name=agent-kourier meta.helm.sh/release-namespace=agent-kourier
  kubectl -n agent-kourier label --overwrite "$obj" app.kubernetes.io/managed-by=Helm
done
helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.2.1 -n agent-kourier -f my-values.yaml
```

Or leave the volume out of the release: `--set persistence.existingClaim=agent-kourier-data`, and nothing the chart manages can delete it.
A ConfigMap you keep is `config.existingConfigMap`, with `rbac.secretNamespaces` naming where its Secrets are.

## Footprint

Measured on a three-worker cluster, idle after a first answered question, the Agent Kourier pod used 15 MiB of memory and about
2 millicores. The chart's defaults are a 64 MiB request, 50m of CPU and a 256 MiB limit. The agent side is larger. A kagent
0.9.x controller, its Postgres, the UI, a read-only tool server and one agent together used about 390 MiB, and the agent pod
was the largest of them at 188 MiB.

## Tests

`go test ./test/chart` renders the chart with `helm template` and checks it: golden files for eight of the `ci/` values files (`fast-postgres-ha` is an overlay, checked layered on `fast-postgres`), the
refusals (replicas above 1 with SQLite, Postgres without its Secret, a monitor without a Service, a NetworkPolicy without the API server, and the rest), the security
properties above, and what the kind tiers' values render (the object names, labels, image and volume that `hack/kind`'s scripts and the E2E tests rely
on), and the fast and AX tiers' config, which is loaded through Agent Kourier's own loader. It needs `helm` on PATH and skips without it, so that `make test` runs on a machine with only Go;
set `AGENTKOURIER_REQUIRE_HELM=1` (as a pipeline should) to fail instead of skipping. After a deliberate chart change:
`go test ./test/chart -update`, and read the diff.

## Upgrading across the rename to Agent Kourier

The chart was `courier` before the project became Agent Kourier. A release installed under that name
(`helm install courier ...`) gets the fullname `courier-agent-kourier` from this chart, so an upgrade makes a new
Deployment and, with `store.type: sqlite`, a new and empty `<fullname>-data` claim beside the old one. The old claim
survives, because `persistence.retainOnUninstall` defaults to true, and the database file in it keeps its old name
(`courier.db`). The default `persistence.dbFile` is now `agent-kourier.db`, so set `persistence.dbFile: courier.db` to keep an
existing database; there is no fallback in the code. To carry the data over, upgrade with
`persistence.existingClaim: <old release>-data`, or keep the old names with `fullnameOverride: <old release>`.
With `store.type: postgres` nothing is stored in the chart's volume: migration 0024 drops the purge function and setting
that carried the old name, and renames the stored system user and the auto-deny decider. The rollout is the one thing to
watch. The Postgres Deployment rolls with `maxSurge: 1` and `maxUnavailable: 0`, at any replica count, so the new pod starts
and runs 0024 while the old leader is still working. Until the old pods are gone, a turn the old leader enqueues for an
alert, or runs (it records the turn's user on the session as the task starts), is stored under the old system user, which
this version then reads as a person's turn. Scaling to one replica does not close that window; only stopping every old
pod first does. Upgrade once with `replicaCount: 0` (the kill switch), then again with `replicaCount` restored. Agent
Kourier is down between the two; if `podDisruptionBudget` is enabled, switch it off or to `maxUnavailable` before scaling to
zero (see values.yaml).
`store.type: sqlite` uses the Recreate strategy and is not affected.

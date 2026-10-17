# Agent Kourier

Installs agent-kourier, the broker between chat and A2A agents: one Deployment of the
production image, its state (a SQLite volume, or a Postgres database you run), the ConfigMap of its resources, and the read
access to the Secrets they name.

**One replica with SQLite, or two with Postgres.** SQLite is one file on a volume that one pod may use, so the
chart refuses `replicaCount` above 1 with it. With `store.type: postgres`, `replicaCount: 2` is an active/passive pair (see High availability). `replicaCount: 0` is the kill switch (Agent Kourier stops, the volume or the database and the config stay);
removing a Binding from the values is the other.

Agent Kourier does nothing without its resources, which the chart takes under `config`: a ChatConnection, an
AgentBackend and a Binding. Put them in a values file, `my-values.yaml`. Download the commented starter and change the
lines it marks `yours`:

```sh
curl -fsSLo my-values.yaml https://raw.githubusercontent.com/agentkourier/agent-kourier/main/examples/helm/my-values.yaml
```

It holds the same resources as this:

```yaml
config:
  chatConnections:
    slack:
      spec:
        platform: slack
        credentialsSecretRef: {name: agent-kourier-slack}   # a Secret with botToken and appToken keys
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
helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.1.0 \
  -n agent-kourier --create-namespace -f my-values.yaml
```

[Install with Helm](https://agentkourier.dev/how-to/install-with-helm/#write-the-values) walks through each field, the
Secrets, and a dry run that finds a misspelt field before the pod does. The published chart has `image.repository` and
`image.digest` stamped with the release's image. From source (`charts/agent-kourier` in a checkout) the chart also
needs `image.repository`. Check a values file with `helm template` before installing, or from a checkout with
`helm lint --strict -f my-values.yaml charts/agent-kourier`. `ci/` holds the values the tests render: `minimal`, `postgres` (minimal on Postgres, with a NetworkPolicy), `full` (everything switched on), and the kind tiers' Agent Kourier, which
`hack/kind` installs with this chart: `fast` (the fast tier's, and the full tier's), `fast-postgres` (the fast tier's on Postgres:
`E2E_STORE=postgres make e2e-fast-up`; `fast-postgres-ha` is an overlay on it, layered with a second `-f`, for two replicas and a shorter lease:
`E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up`, which the two-replica failover end-to-end test runs on), `ax` (the AX tier's, with a database file of its own) and `sandbox` (the sandbox
tier's, with the token-refresh CronJob).

## What it installs

| Object | When | Notes |
|---|---|---|
| Deployment `<fullname>` | always | one container, config at `/etc/agent-kourier`, SQLite at `/data` (with `store.type: sqlite`, strategy Recreate), `/tmp` an emptyDir; with Postgres strategy RollingUpdate (one surge, none unavailable) |
| ServiceAccount, and a Role and RoleBinding for Secrets | always (`rbac.create`) | one Role in each namespace that holds a Secret the config names, and in the release namespace with Postgres (the DSN's Secret) |
| Lease `<fullname>-leader` | `store.type: postgres` | empty; the replicas elect their leader with it. Kept on `helm uninstall` (`helm.sh/resource-policy: keep`), with its release annotations, so a reinstall under the same release name adopts it; only another release that renders the same name (`fullnameOverride`) would conflict. Do not delete it while Agent Kourier runs: Agent Kourier may not create a Lease, so leadership stops until `helm upgrade` recreates it |
| Role and RoleBinding `<fullname>-leader` | `store.type: postgres` (`rbac.create`) | `get` and `update` on that one Lease, in the release namespace; no `create`, which cannot be limited by name |
| ConfigMap `<fullname>-config` | unless `config.existingConfigMap` | one `*.yaml` file per resource, from `config.*` |
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
- `store.type` (`sqlite`, the default, or `postgres`) and, with Postgres, `store.postgres.dsnSecret` and `store.postgres.timeouts` (see Store).
- `replicaCount` (2 with Postgres for a standby) and `leaderElection.*`, the Lease's timings (see High availability).
- `logLevel`, `syncTimeout`, `auditRetention` (empty keeps the audit log for ever; a duration of at least 24h purges older entries), `extraEnv` (OpenTelemetry settings go here), `resources`, `persistence.*` (SQLite's volume).
- `service`, `serviceMonitor`, `networkPolicy`, `podDisruptionBudget`, `tokenRefresh`: off until you switch them on.

A config change rolls the pod (a `checksum/config` annotation): Agent Kourier reloads its files, but a reload that adds a Secret
namespace fails until restart, and the roll covers it. Turn it off with `config.rollOnChange: false`.

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
- **Secrets RBAC is namespace-wide, not by name.** Agent Kourier caches Secrets with an informer, which needs `list` and `watch`, and
  Kubernetes cannot limit those verbs by `resourceNames`. Each Role therefore reads every Secret in its namespace, and Agent Kourier
  keeps the Opaque ones in memory (internal/config/k8ssecrets documents it). The chart grants it only in namespaces that hold a
  Secret the config names, and `get` is never granted. Give Agent Kourier's Secrets a namespace of their own when the namespace is shared. With Postgres the DSN's Secret is in the release namespace, which therefore gets this Role too.
  Narrowing to named Secrets is an Agent Kourier change (one informer per Secret), a possible later change.
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
Binding's namespace. With the config inline the render fails unless some Binding in that namespace names that Secret and key, so
a token is never refreshed where nothing reads it. The Job's Role and RoleBinding are made in that namespace (the Job's
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
helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.1.0 -n agent-kourier -f my-values.yaml
```

Or leave the volume out of the release: `--set persistence.existingClaim=agent-kourier-data`, and nothing the chart manages can delete it.
A ConfigMap you keep is `config.existingConfigMap`, with `rbac.secretNamespaces` naming where its Secrets are.

## Footprint

Measured on a three-worker cluster, idle after a first answered question, the Agent Kourier pod used 15 MiB of memory and about
2 millicores. The chart's defaults are a 64 MiB request, 50m of CPU and a 256 MiB limit. The agent side is larger. A kagent
0.9.x controller, its Postgres, the UI, a read-only tool server and one agent together used about 390 MiB, and the agent pod
was the largest of them at 188 MiB.

## Tests

`go test ./test/chart` renders the chart with `helm template` and checks it: golden files for seven of the `ci/` values files (`fast-postgres-ha` is an overlay, checked layered on `fast-postgres`), the
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

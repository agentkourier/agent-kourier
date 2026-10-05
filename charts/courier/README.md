# courier

Installs [agent-courier](../../spec.md), the broker between Alertmanager, Slack and A2A agents: one Deployment of the
production image, its state (a SQLite volume, or a Postgres database you run), the ConfigMap of its resources, and the read
access to the Secrets they name.

**One replica.** That is what courier supports today, and the chart enforces it: `replicaCount` above 1 fails the render. Two
replicas need `store.type: postgres` (SQLite is one file on a volume that one pod may use, spec R7) and leader election with
epoch fencing (H4-5); this chart gains a Lease Role and a standby that is Ready but idle with the latter, and not before.
`replicaCount: 0` is the kill switch (courier stops, the volume or the database and the config stay); removing a Binding from
the values is the other.

```sh
helm install courier ./charts/courier -n courier-system --create-namespace -f my-values.yaml
```

From source, the chart needs `image.repository` (a released chart has it stamped, with the release's digest) and, in practice, a
ChatConnection. Check any values file with `helm lint --strict -f my-values.yaml charts/courier` and
`helm template` before installing. `ci/` holds the values the tests render: `minimal`, `postgres` (minimal on Postgres, with a NetworkPolicy), `full` (everything switched on), and the kind tiers' courier, which
`hack/kind` installs with this chart: `fast` (the fast tier's, and the full tier's), `fast-postgres` (the fast tier's on Postgres:
`E2E_STORE=postgres make e2e-fast-up`), `ax` (the AX tier's, with a database file of its own) and `sandbox` (the sandbox
tier's, with the token-refresh CronJob).

## What it installs

| Object | When | Notes |
|---|---|---|
| Deployment `<fullname>` | always | strategy Recreate, one container, config at `/etc/courier`, SQLite at `/data` (with `store.type: sqlite`), `/tmp` an emptyDir |
| ServiceAccount, and a Role and RoleBinding for Secrets | always (`rbac.create`) | one Role in each namespace that holds a Secret the config names, and in the release namespace with Postgres (the DSN's Secret) |
| ConfigMap `<fullname>-config` | unless `config.existingConfigMap` | one `*.yaml` file per resource, from `config.*` |
| PersistentVolumeClaim `<fullname>-data` | `store.type: sqlite`, unless `persistence.existingClaim` | kept on `helm uninstall` (`persistence.retainOnUninstall`); none with Postgres |
| Service | `service.enabled` | ClusterIP on the health and metrics port; the Alertmanager receiver is not served yet |
| ServiceMonitor | `serviceMonitor.enabled` | prometheus-operator; needs the Service |
| NetworkPolicy | `networkPolicy.enabled` | see below |
| PodDisruptionBudget | `podDisruptionBudget.enabled` | off; the render refuses a budget that keeps the only pod (`minAvailable` >= replicas, 100%, `maxUnavailable: 0`), since it would block every node drain |
| CronJob, ServiceAccount, Role, RoleBinding `<fullname>-token-refresh` | `tokenRefresh.enabled` | keeps a Binding's Dex ID token fresh |

`<fullname>` is the release name, or `<release>-courier` when the name does not contain "courier" (`fullnameOverride` sets it).

## Values

`values.yaml` documents every key; `values.schema.json` rejects an unknown or mistyped one. The ones you will set:

- `image.repository`, `image.tag` (default: the chart's appVersion) or `image.digest` (wins; pin production to it).
- `config.chatConnections`, `config.agentBackends`, `config.bindings`: maps of name to `{namespace, spec}`, where `spec` is the
  resource's spec exactly as in spec.md section 7 (the chart does not interpret it; courier's loader validates it at start).
  `namespace` defaults to the release's. Or `config.existingConfigMap`, a ConfigMap you manage, with `rbac.secretNamespaces` to say where
  its Secrets are. Not both.
- `store.type` (`sqlite`, the default, or `postgres`) and, with Postgres, `store.postgres.dsnSecret` and `store.postgres.timeouts` (see Store).
- `logLevel`, `syncTimeout`, `extraEnv` (OpenTelemetry settings go here), `resources`, `persistence.*` (SQLite's volume).
- `service`, `serviceMonitor`, `networkPolicy`, `podDisruptionBudget`, `tokenRefresh`: off until you switch them on.

A config change rolls the pod (a `checksum/config` annotation): courier reloads its files, but a reload that adds a Secret
namespace fails until restart, and the roll covers it. Turn it off with `config.rollOnChange: false`.

## Store

`store.type: sqlite` (the default) keeps courier's state in one file on the volume, for **one replica**: a SQLite file may be open in one
pod only (spec R7). Courier cannot see how many replicas run, so the chart is what refuses more than one (`replicaCount` above 1
fails the render, whatever the store: more than one replica needs Postgres **and** leader election, H4-5).

`store.type: postgres` keeps it in a database you run (the chart installs none). Name the Secret that holds the DSN, in the release
namespace, and the key in it:

```yaml
store:
  type: postgres
  postgres:
    dsnSecret: {name: courier-pg, key: dsn}   # a postgres:// URL or a key/value string; never a value in the values
```

The render refuses `store.type: postgres` without the Secret, a `dsnSecret` (or `timeouts`) with SQLite, an `existingClaim` with
Postgres, and a `postgres://` URL with a password anywhere in the values. With Postgres no volume is mounted and no claim is made.
Courier reads the Secret through the API, like its others, so the Role below covers it: list and watch on every Secret of the
release namespace. The DSN is read once at start; rotate the password by changing the Secret and the database role, then restart the
pod. An unreadable or empty Secret fails the start with an error that names the Secret and the key, never the DSN. The role in the
DSN needs the right to create tables in its schema, as courier applies its own migrations at start (under an advisory lock). With
`networkPolicy.enabled`, `networkPolicy.postgres.to` (NetworkPolicyPeers: an ipBlock, or a namespaceSelector and podSelector) and
`.port` (default 5432) say where the database is, and the render refuses Postgres without `to`: the Slack rule excludes private ranges,
so without it courier could not reach the database.

**Connection poolers.** Courier's idle-in-transaction and statement timeouts are sent as startup parameters on each connection:
they are what ends a stalled leader's transaction and frees its locks (spec D37). So connect directly, or through a pooler in
**session** mode. A pooler in **transaction** mode (PgBouncer's `pool_mode = transaction`) rejects startup parameters
("unsupported startup parameter"), and listing them in `ignore_startup_parameters` silently drops the protection. There, set the
timeouts on the role and turn courier's off:

```sql
ALTER ROLE courier SET idle_in_transaction_session_timeout = '10s';
ALTER ROLE courier SET statement_timeout = '30s';
```

and set `store.postgres.timeouts: role`.

## Secrets

Nothing secret is in the values. The config names Secrets (`credentialsSecretRef`, `tokenSecretRef`, `valueSecretRef`) and you
make them (by hand, Sealed Secrets, an external operator) before courier starts:

- the ChatConnection's Secret, with `botToken` and `appToken`;
- each Binding's `tokenSecretRef` Secret, with a `token` key (or the key the reference says);
- with Postgres, the DSN Secret (`store.postgres.dsnSecret`), in the release namespace;
- the Dex credentials Secret for `tokenRefresh`, with `clientSecret`, `username` and `password`.

Courier reads them through the Kubernetes API, never from its environment or a mounted file. The render refuses what would put
a secret in the ConfigMap: a Slack token (`xox*-`, `xapp-`) anywhere in the values, and an AgentBackend header `value` whose name
looks like a credential (key, token, secret, auth, cookie, password), which belongs in `valueSecretRef`.

## Security posture

- The pod runs as UID 65532, non-root, with the `RuntimeDefault` seccomp profile; the container has a read-only root
  filesystem, `allowPrivilegeEscalation: false` and every capability dropped. The only writable paths are the data volume and `/tmp`.
- The pod label `app.kubernetes.io/name: courier` is fixed, whatever the release is named, because NetworkPolicies elsewhere
  (the front door's, in the kind tiers and the pilot) admit pods by it. Nothing else the chart makes carries it on a pod; in
  particular the token-refresh Job's pod does not.
- **Secrets RBAC is namespace-wide, not by name.** Courier caches Secrets with an informer, which needs `list` and `watch`, and
  Kubernetes cannot limit those verbs by `resourceNames`. Each Role therefore reads every Secret in its namespace, and courier
  keeps the Opaque ones in memory (internal/config/k8ssecrets documents it). The chart grants it only in namespaces that hold a
  Secret the config names, and `get` is never granted. Give courier's Secrets a namespace of their own when the namespace is shared. With Postgres the DSN's Secret is in the release namespace, which therefore gets this Role too.
  Narrowing to named Secrets is a courier change (one informer per Secret), after the pilot.
- `klogVerbosity` stays 0 (the schema caps it at 7): at 8 and above client-go logs response bodies, Secret lists included.
- **NetworkPolicy** (`networkPolicy.enabled`, which needs a CNI that enforces it): egress to DNS, to the API server (you give
  its addresses in `kubeApiServer.cidrs`, since a policy cannot select it), to Slack on
  443 (to anywhere outside the private, CGNAT and link-local ranges, since a policy cannot name a host) and to what `extraEgress` adds (the agent
  backend's front door, an OTLP collector); ingress only from `metricsFrom` on the metrics port. Courier calls the verifying
  front door, never the controller on 8083 (spec D32); put the front door in `extraEgress`, and nothing else in that namespace.
  - On kind or kubeadm, `kubectl get endpoints kubernetes` gives the API server's addresses as /32s. On EKS those are
    control-plane ENI addresses that change when AWS upgrades the control plane: use the CIDRs of the control-plane subnets
    (`aws eks describe-cluster`, `resourcesVpcConfig.subnetIds`, each subnet's CIDR). The VPC CNI enforces NetworkPolicy only
    with `enableNetworkPolicy=true` on the vpc-cni add-on; without it the policy does nothing.
- The token-refresh Job's Role allows `patch` on one named Secret and nothing else. The Dex credentials reach `curl` through
  `--variable` from the environment, so they are never on an argument list; the Job prints no token.

## Probes

`/healthz` is the process, for the startup (80 s) and liveness probes. `/readyz` is the config loaded, the Secret caches synced and the store
answering. **Ready does not mean Slack is connected**, by design: a Slack outage must not take courier out of its Service or restart
it, and queued alerts post later. Watch `courier_chat_connected`.

## Token refresh

For an AgentBackend whose front door verifies Dex ID tokens (the sandbox tier's, `ci/sandbox-values.yaml`),
`tokenRefresh.enabled` adds an hourly CronJob that asks Dex for a token (the password grant) and patches it into the Secret named
by `targetSecret` and `targetKey` in `targetNamespace` (default: the release's): the Binding's `identity.tokenSecretRef` and the
Binding's namespace. With the config inline the render fails unless some Binding in that namespace names that Secret and key, so
a token is never refreshed where nothing reads it. The Job's Role and RoleBinding are made in that namespace (the Job's
ServiceAccount stays in the release's). The Secret must exist before courier starts; make it with a placeholder, then run the
first refresh by hand:

```sh
kubectl -n <ns> create job --from=cronjob/<fullname>-token-refresh first-token
```

## Moving from hand-made manifests

The kind tiers used to apply hand-made manifests (a Deployment that selected pods by `app.kubernetes.io/name` alone). The chart's
selector adds `app.kubernetes.io/instance`, and a Deployment's selector cannot change, so the old Deployment is deleted first (courier
stops; sessions and the outbox are on the volume), and the chart is installed over the rest. Helm refuses to manage an object it did
not create unless it is marked as the release's, so the objects the chart also makes (the ServiceAccount, Role, RoleBinding, Service
and volume claim) are marked, not deleted. `hack/kind/lib.sh` does exactly this (`replace_unowned_deployment`, `adopt_into_release`), so that
re-running a tier's bring-up is how a running tier moves over. By hand, for release `courier` in namespace `courier`:

```sh
kubectl -n courier delete deploy courier --cascade=foreground
for obj in serviceaccount/courier role/courier-secrets rolebinding/courier-secrets service/courier pvc/courier-data; do
  kubectl -n courier annotate --overwrite "$obj" meta.helm.sh/release-name=courier meta.helm.sh/release-namespace=courier
  kubectl -n courier label --overwrite "$obj" app.kubernetes.io/managed-by=Helm
done
helm install courier ./charts/courier -n courier -f my-values.yaml
```

Or leave the volume out of the release: `--set persistence.existingClaim=courier-data`, and nothing the chart manages can delete it.
A ConfigMap you keep is `config.existingConfigMap`, with `rbac.secretNamespaces` naming where its Secrets are.

## Tests

`go test ./test/chart` renders the chart with `helm template` and checks it: golden files for the seven `ci/` values files, the
refusals (replicas above 1, Postgres without its Secret, a monitor without a Service, a NetworkPolicy without the API server, and the rest), the security
properties above, and what the kind tiers' values render (the object names, labels, image and volume that `hack/kind`'s scripts and the E2E tests rely
on), and the fast and AX tiers' config, which is loaded through courier's own loader. It needs `helm` on PATH and skips without it, so that `make test` runs on a machine with only Go;
set `COURIER_REQUIRE_HELM=1` (as a pipeline should) to fail instead of skipping. After a deliberate chart change:
`go test ./test/chart -update`, and read the diff.

## What comes next

- **H4-5** (leader election and fencing): replicas above 1 (with Postgres), a Role for the Lease, the standby Ready but idle, a meaningful PodDisruptionBudget.
- The Alertmanager receiver (WP-47) adds its port, a Service and an Ingress or route when it is served.

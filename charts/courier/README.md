# courier

Installs [agent-courier](../../spec.md), the broker between Alertmanager, Slack and A2A agents: one Deployment of the
production image, its SQLite volume, the ConfigMap of its resources, and the read access to the Secrets they name.

**One replica, SQLite on a volume.** That is what courier supports today, and the chart enforces it: `replicaCount` above 1
fails the render. Two replicas need the Postgres store (H4-3) and leader election with epoch fencing (H4-5); this chart gains
`store` values, a Lease Role and a standby that is Ready but idle with them, and not before. `replicaCount: 0` is the kill
switch (courier stops, the volume and the config stay); removing a Binding from the values is the other.

```sh
helm install courier ./charts/courier -n courier-system --create-namespace -f my-values.yaml
```

From source, the chart needs `image.repository` (a released chart has it stamped, with the release's digest) and, in practice, a
ChatConnection. Check any values file with `helm lint --strict -f my-values.yaml charts/courier` and
`helm template` before installing. `ci/` holds the values the tests render: `minimal`, `sandbox` (the sandbox tier's courier
with the token-refresh CronJob), `full` (everything switched on) and `fast` (the fast kind tier's courier).

## What it installs

| Object | When | Notes |
|---|---|---|
| Deployment `<fullname>` | always | strategy Recreate, one container, config at `/etc/courier`, SQLite at `/data`, `/tmp` an emptyDir |
| ServiceAccount, and a Role and RoleBinding for Secrets | always (`rbac.create`) | one Role in each namespace that holds a Secret the config names |
| ConfigMap `<fullname>-config` | unless `config.existingConfigMap` | one `*.yaml` file per resource, from `config.*` |
| PersistentVolumeClaim `<fullname>-data` | unless `persistence.existingClaim` | kept on `helm uninstall` (`persistence.retainOnUninstall`) |
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
- `logLevel`, `syncTimeout`, `extraEnv` (OpenTelemetry settings go here), `resources`, `persistence.*`.
- `service`, `serviceMonitor`, `networkPolicy`, `podDisruptionBudget`, `tokenRefresh`: off until you switch them on.

A config change rolls the pod (a `checksum/config` annotation): courier reloads its files, but a reload that adds a Secret
namespace fails until restart, and the roll covers it. Turn it off with `config.rollOnChange: false`.

## Secrets

Nothing secret is in the values. The config names Secrets (`credentialsSecretRef`, `tokenSecretRef`, `valueSecretRef`) and you
make them (by hand, Sealed Secrets, an external operator) before courier starts:

- the ChatConnection's Secret, with `botToken` and `appToken`;
- each Binding's `tokenSecretRef` Secret, with a `token` key (or the key the reference says);
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
  Secret the config names, and `get` is never granted. Give courier's Secrets a namespace of their own when the namespace is shared.
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

For an AgentBackend whose front door verifies Dex ID tokens (the sandbox tier's, hack/kind/sandbox/token-refresh.yaml),
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

The kind tiers (`hack/kind/*/courier`) and any hand-made Deployment select pods by `app.kubernetes.io/name` alone; the chart's
selector adds `app.kubernetes.io/instance`, and a Deployment's selector cannot change. So delete the old Deployment first
(courier stops; sessions and the outbox are on the volume), then install the chart pointing at the volume you have:

```sh
kubectl -n courier delete deploy courier
helm install courier ./charts/courier -n courier -f my-values.yaml --set persistence.existingClaim=courier-data
```

Leave `courier-data` (or yours) out of the chart that way and nothing it manages can delete it. Keep the old ServiceAccount, Role and
ConfigMap names out of the way too, or let the chart take them over by deleting them first; helm refuses to adopt objects it did not make. A
ConfigMap you keep is `config.existingConfigMap`, with `rbac.secretNamespaces` naming where its Secrets are.

## Tests

`go test ./test/chart` renders the chart with `helm template` and checks it: golden files for the four `ci/` values files, the
refusals (replicas above 1, a monitor without a Service, a NetworkPolicy without the API server, and the rest), the security
properties above, and parity with the fast tier's manifests (`hack/kind/fast/courier`) and with its config, which is loaded
through courier's own loader. It needs `helm` on PATH and skips without it, so that `make test` runs on a machine with only Go;
set `COURIER_REQUIRE_HELM=1` (as a pipeline should) to fail instead of skipping. After a deliberate chart change:
`go test ./test/chart -update`, and read the diff.

## What comes next

- **H4-3** (store selection): `store` values for SQLite or Postgres, the DSN from a Secret by reference. Until it lands the
  chart is SQLite only.
- **H4-5** (leader election and fencing): replicas above 1, a Role for the Lease, the standby Ready but idle, a meaningful PodDisruptionBudget.
- The Alertmanager receiver (WP-47) adds its port, a Service and an Ingress or route when it is served.

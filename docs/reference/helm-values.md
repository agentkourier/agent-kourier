# Helm values

This page lists every value of the `agent-kourier` Helm chart, with its default and the comment `charts/agent-kourier/values.yaml` gives it.

## replicaCount

Defaults for the Agent Kourier chart.

This chart runs ONE replica by default, with its state in SQLite on a PersistentVolumeClaim (store.type sqlite, the default) or in Postgres (store.type postgres). A SQLite file may be open in one pod, and the session manager keeps per-thread state in memory, so SQLite is one replica: the render refuses replicaCount above 1 with it. With Postgres, replicaCount 2 gives an active/passive pair: the replicas elect a leader with a Kubernetes Lease (leaderElection below), the leader does all of Agent Kourier's work, and the standby is Ready and idle until the leader's lease lapses or it stops. Every write is fenced by an epoch in the database, so a leader that has been paused past its lease cannot write when it wakes. Postgres at replicaCount 1 elects too, and is a standby-less leader. replicaCount 0 is the kill switch: Agent Kourier stops, and the volume (or the database) stays.

Default: `1`

## image

| Key | Default | Description |
|---|---|---|
| `image.repository` | `""` | Required. A released chart has it stamped (ghcr.io/agentkourier/agent-kourier), with digest; in the source tree it is empty. Locally: agent-kourier (the image `make image` builds is agent-kourier:dev). |
| `image.tag` | `""` | A tag, which defaults to the chart's appVersion. Ignored when digest is set: a released chart sets the digest, so to run another tag with it, set image.digest="" as well (or set image.digest to the other release's digest). |
| `image.digest` | `""` | Pin production to a digest (sha256:...), which wins over tag. |
| `image.pullPolicy` | `IfNotPresent` |  |

## imagePullSecrets

Default: `[]`

## fullnameOverride

Resource names are &lt;release&gt;-agent-kourier (or the release name when it contains "agent-kourier"). The label app.kubernetes.io/name=agent-kourier is fixed either way: NetworkPolicies elsewhere trust pods by it.

Default: `""`

## serviceAccount

| Key | Default | Description |
|---|---|---|
| `serviceAccount.create` | `true` |  |
| `serviceAccount.name` | `""` | Defaults to the fullname. Agent Kourier reads Secrets through the API, so the token stays mounted. |
| `serviceAccount.annotations` | `{}` |  |

## listen

Agent Kourier's process settings. These are the environment cmd/agent-kourier reads (its package comment lists all of them).

| Key | Default | Description |
|---|---|---|
| `listen.port` | `8080` | The port of /healthz, /readyz and /metrics (AGENTKOURIER_LISTEN). |

## logLevel

Default: `info`

debug, info, warn or error (AGENTKOURIER_LOG_LEVEL)

## klogVerbosity

client-go's verbosity (AGENTKOURIER_KLOG_V). Keep it 0: at 8 and above client-go logs response bodies, Secret lists included.

Default: `0`

## syncTimeout

How long to wait for the Secret caches before startup fails (AGENTKOURIER_SYNC_TIMEOUT). Empty is Agent Kourier's default, 30s.

Default: `""`

## auditRetention

How long audit entries are kept (AGENTKOURIER_AUDIT_RETENTION): empty or "keep" is for ever, which is what runs until you set one; a duration of at least 24h (8760h is a year) purges the entries older than it, hourly, in batches. The audit holds IDs, outcomes, and up to 20 cleaned tool names for approval decisions, never message text, tool arguments or rejection reasons. The purge is the only way an entry leaves the table.

Default: `""`

## extraEnv

Extra container environment, in Kubernetes' own form. This is where the OpenTelemetry settings go (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_TRACES_EXPORTER, ...); take a header or other credential from a Secret with valueFrom.secretKeyRef, never as a value.

Default: `[]`

## config

What Agent Kourier reads. Either the resources below, which the chart writes into a ConfigMap mounted at /etc/agent-kourier, or existingConfigMap, the name of a ConfigMap you manage (one key per file, each ending .yaml). Not both.

Each entry is keyed by its name and holds the resource's spec exactly as the resource reference describes it (the chart does not interpret it) and, optionally, the namespace (default: the release's). Secrets are referenced by name (credentialsSecretRef, tokenSecretRef, valueSecretRef) and live in the cluster, made by you or Sealed Secrets: no token value belongs in this file or in any values file (the render refuses a Slack token, and a header value whose name looks like a credential).

```yaml
config:
  chatConnections:
    slack-main:
      spec:
        platform: slack
        credentialsSecretRef: {name: slack-main}   # keys botToken and appToken
  agentBackends:
    agents:
      spec: {dialect: kagent-v1, url: "http://kagent-frontdoor.kagent.svc.cluster.local:4180"}
  bindings:
    sre-alerts:
      spec:
        agent: {backendRef: {name: agents}, namespace: kagent, name: k8s-agent}
        identity: {userId: agent-kourier-sre, tokenSecretRef: {name: agent-kourier-agent-token}}
        chat: {connectionRef: {name: slack-main}, channel: C0123456789, output: live, threadReplies: all, acknowledgment: {enabled: true}}  # the default; the platform's own reaction (Slack: hourglass_flowing_sand); set emoji to a bare reaction name to change it
```

| Key | Default | Description |
|---|---|---|
| `config.existingConfigMap` | `""` |  |
| `config.agentBackends` | `{}` |  |
| `config.chatConnections` | `{}` |  |
| `config.bindings` | `{}` |  |
| `config.rollOnChange` | `true` | Roll the pod when the rendered ConfigMap changes. Agent Kourier reloads its files in place, but a reload that adds a Secret namespace fails until the process restarts. Has no effect with existingConfigMap. |

## store

Where Agent Kourier keeps its state. SQLite is one file on a volume (persistence below), for one replica. Postgres is a database you run (the chart does not install one), for more than one replica: they elect a leader (leaderElection below).

| Key | Default | Description |
|---|---|---|
| `store.type` | `sqlite` | sqlite or postgres (AGENTKOURIER_STORE) |
| `store.postgres.dsnSecret` |  | The Secret that holds the DSN, a postgres:// URL or key/value string, in the release namespace: required with store.type postgres, and refused with sqlite. Make it yourself (by hand, Sealed Secrets, an external operator); the DSN, password included, never goes in a values file (the render refuses a postgres:// URL with a password anywhere in them). Agent Kourier reads it through the API like its other Secrets, which the Role below grants: list and watch on every Secret of the release namespace, so keep Agent Kourier's Secrets in a namespace of their own when it is shared. Percent-encode the reserved characters of a password in a URL. The role in the DSN needs the right to create tables in its schema: agent-kourier applies its own migrations at start. |
| `store.postgres.dsnSecret.name` | `""` |  |
| `store.postgres.dsnSecret.key` | `""` |  |
| `store.postgres.timeouts` | `agent-kourier` | Who sets the idle-in-transaction and statement timeouts, which end a stalled leader's transaction and free its locks. "agent-kourier" (the default) sends them as startup parameters on every connection. Connect to Postgres directly or through a pooler in SESSION mode. A pooler in TRANSACTION mode (PgBouncer's pool_mode = transaction) refuses startup parameters ("unsupported startup parameter"), and listing them in its ignore_startup_parameters silently drops the protection: there, set both on the database role (ALTER ROLE agentkourier SET idle_in_transaction_session_timeout = '10s'; ALTER ROLE agentkourier SET statement_timeout = '30s') and set this to "role", which turns Agent Kourier's own off. |

## leaderElection

Leader election, which runs with store.type postgres only (with sqlite none of this is allowed). The replicas hold one Lease, named &lt;fullname&gt;-leader in the release namespace, which the chart's Role grants (rbac.create). The leader renews it every retryPeriod. A standby takes it leaseDuration after the last renewal it saw, so the worst case before work resumes after a crash or a lost node is the lease duration plus Agent Kourier's start-up (a few seconds). A leader that cannot renew for renewDeadline gives up and exits at once, which is 5 s before a standby may take over with the defaults. The leader hands the lease over when it stops on a signal (a rolling update), so that takeover is quick. Empty is Agent Kourier's default; each is a Go duration, and leaseDuration &gt; renewDeadline &gt; 1.2 x retryPeriod. See also docs/explanation/high-availability.md.

| Key | Default | Description |
|---|---|---|
| `leaderElection.leaseDuration` | `""` | 15s |
| `leaderElection.renewDeadline` | `""` | 10s |
| `leaderElection.retryPeriod` | `""` | 2s |

## livenessProbe

How often the kubelet asks /healthz, and how many misses in a row make it restart the container. The defaults restart a process that does not answer for about 30 seconds. A process that is frozen answers nothing, so this is also how long a paused pod is left before it is restarted.

| Key | Default | Description |
|---|---|---|
| `livenessProbe.periodSeconds` | `10` |  |
| `livenessProbe.failureThreshold` | `3` |  |

## persistence

SQLite lives here (store.type sqlite; with postgres none of this is used or rendered). The claim is ReadWriteOnce and the Deployment's strategy is Recreate, so the old pod has released the volume before the new one starts.

| Key | Default | Description |
|---|---|---|
| `persistence.existingClaim` | `""` | Use a claim you manage instead of creating one. |
| `persistence.dbFile` | `agent-kourier.db` | The SQLite file in the volume (AGENTKOURIER_DB_PATH is /data/&lt;dbFile&gt;). Change it to start a fresh database beside an old one; an install from before the rename sets it to courier.db to keep its database (docs/how-to/upgrade.md, "Upgrade across the rename"). |
| `persistence.size` | `1Gi` |  |
| `persistence.storageClassName` | `""` | Empty is the cluster's default class; "-" is an explicitly empty one. |
| `persistence.accessModes` | `[ReadWriteOnce]` |  |
| `persistence.retainOnUninstall` | `true` | Keep the claim (helm.sh/resource-policy: keep) when the release is uninstalled: it holds sessions, the outbox and the dedup record. Delete it by hand when you mean to. |

## terminationGracePeriodSeconds

Kubernetes gives Agent Kourier up to this long after SIGTERM; its ordered shutdown takes up to 25 s.

Default: `40`

## podAnnotations

Default: `{}`

## podLabels

Added to the pod. app.kubernetes.io/name and /instance cannot be changed.

Default: `{}`

## priorityClassName

Default: `""`

## nodeSelector

Default: `{}`

## tolerations

Default: `[]`

## affinity

Default: `{}`

## podSecurityContext

| Key | Default | Description |
|---|---|---|
| `podSecurityContext.runAsNonRoot` | `true` |  |
| `podSecurityContext.runAsUser` | `65532` |  |
| `podSecurityContext.runAsGroup` | `65532` |  |
| `podSecurityContext.fsGroup` | `65532` |  |
| `podSecurityContext.seccompProfile` | `{type: RuntimeDefault}` |  |

## securityContext

| Key | Default | Description |
|---|---|---|
| `securityContext.allowPrivilegeEscalation` | `false` |  |
| `securityContext.readOnlyRootFilesystem` | `true` |  |
| `securityContext.capabilities` | `{drop: [ALL]}` |  |

## resources

| Key | Default | Description |
|---|---|---|
| `resources.requests` | `{cpu: 50m, memory: 64Mi}` |  |
| `resources.limits` | `{memory: 256Mi}` |  |

## rbac

Agent Kourier reads the Secrets that its config names, and the Postgres DSN's, through the API (list and watch, in a namespace-scoped Role; it never issues a get). Roles are made in each namespace that holds a referenced Secret, found from config.\* above, and the release namespace with store.type postgres.

| Key | Default | Description |
|---|---|---|
| `rbac.create` | `true` |  |
| `rbac.secretNamespaces` | `[]` | More namespaces to grant that Role in. Needed with config.existingConfigMap, which the chart cannot read. |

## service

A ClusterIP Service on the metrics port (/metrics, and the health paths). Never published; there is nothing else to serve yet.

| Key | Default | Description |
|---|---|---|
| `service.enabled` | `false` |  |
| `service.port` | `8080` |  |
| `service.annotations` | `{}` |  |

## serviceMonitor

A prometheus-operator ServiceMonitor for the Service (needs service.enabled).

| Key | Default | Description |
|---|---|---|
| `serviceMonitor.enabled` | `false` |  |
| `serviceMonitor.namespace` | `""` | The namespace of the ServiceMonitor; default is the release's. |
| `serviceMonitor.labels` | `{}` | Labels your Prometheus selects ServiceMonitors by (for kube-prometheus-stack, release: &lt;its release&gt;). |
| `serviceMonitor.interval` | `30s` |  |
| `serviceMonitor.scrapeTimeout` | `10s` |  |

## networkPolicy

Fences Agent Kourier's network. Egress is the Kubernetes API server, DNS, Slack on 443 and what extraEgress adds (the agent backend, an OTLP collector); ingress is the metrics scrape and nothing else. Needs a CNI that enforces NetworkPolicy.

| Key | Default | Description |
|---|---|---|
| `networkPolicy.enabled` | `false` |  |
| `networkPolicy.kubeApiServer.cidrs` | `[]` | The API server's addresses, as the pod sees them after the Service's translation: required when enabled. A NetworkPolicy cannot select the API server by pod. Where the control plane is yours (kind, kubeadm), the addresses of `kubectl get endpoints kubernetes`, as /32s. On EKS those are control-plane ENI addresses that change when AWS upgrades or replaces the control plane, so give the CIDRs of the control-plane subnets instead (`aws eks describe-cluster --name <cluster>`, resourcesVpcConfig.subnetIds, then each subnet's CIDR). The VPC CNI enforces NetworkPolicy only with enableNetworkPolicy=true on the vpc-cni add-on; without it this policy does nothing. |
| `networkPolicy.kubeApiServer.ports` | `[443, 6443]` |  |
| `networkPolicy.dns.namespaceSelector` | `{matchLabels: {kubernetes.io/metadata.name: kube-system}}` |  |
| `networkPolicy.dns.podSelector` | `{matchLabels: {k8s-app: kube-dns}}` |  |
| `networkPolicy.slack.cidrs` | `["0.0.0.0/0"]` | A NetworkPolicy cannot name a host, so Slack (slack.com, wss-\*.slack.com, files.slack.com) is port 443 to anywhere outside the private ranges. Put an egress proxy's address in extraEgress and narrow this to [] to close it. |
| `networkPolicy.slack.except` | `["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"]` |  |
| `networkPolicy.postgres` |  | The Postgres database (store.type postgres), as NetworkPolicyPeers: required when this policy is enabled with Postgres, since the Slack rule excludes private ranges and is 443 only. An ipBlock for a managed database, or the pods of an in-cluster one: to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: db}}, podSelector: {matchLabels: {app: postgres}}}] Egress is on `port` (the server's, 5432 by default). Behind a pooler, name the pooler. |
| `networkPolicy.postgres.to` | `[]` |  |
| `networkPolicy.postgres.port` | `5432` |  |
| `networkPolicy.metricsFrom` | `[]` | Who may scrape the metrics port: NetworkPolicyPeers, for example the Prometheus pods: - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: monitoring}} podSelector: {matchLabels: {app.kubernetes.io/name: prometheus}} Empty admits nobody. Kubelet's probes are node traffic, which the CNIs we know admit regardless. |
| `networkPolicy.extraEgress` | `[]` | More egress rules, in Kubernetes' own form. The agent backend goes here, for example the front door: - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: kagent}}, podSelector: {matchLabels: {app.kubernetes.io/name: kagent-frontdoor}}}] ports: [{port: 4180}] |

## podDisruptionBudget

Off, and meaningful with the standby (replicaCount 2 on Postgres): minAvailable: 1 keeps a drain from taking both replicas, and renders there. At one replica a budget that keeps the pod (minAvailable 1 or 100%, maxUnavailable 0) blocks every voluntary eviction, a node drain included, so the render refuses it. Set exactly one of minAvailable and maxUnavailable; with one replica only a nonzero maxUnavailable renders, and that permits what it was meant to stop. A minAvailable that renders at replicaCount 0 (the kill switch) is refused again on scaling back to 1: switch the budget off, or to maxUnavailable, before scaling to zero.

| Key | Default | Description |
|---|---|---|
| `podDisruptionBudget.enabled` | `false` |  |
| `podDisruptionBudget.minAvailable` | `null` |  |
| `podDisruptionBudget.maxUnavailable` | `null` |  |

## tokenRefresh

Keeps a Binding's identity token fresh when the AgentBackend sits behind a verifying front door that takes Dex ID tokens (the sandbox tier does this, with ci/sandbox-values.yaml). An hourly CronJob asks Dex for a token with the password grant and patches it into one Secret through the API, with its own ServiceAccount, whose Role allows `patch` on that Secret and nothing else. The Dex credentials are read from a Secret into the Job's environment; they never pass through an argument list or this file.

The target Secret is the Binding's identity.tokenSecretRef: its name, key and namespace (the Binding's), which the render checks against the inline config. It must exist before Agent Kourier starts. After install, run the first refresh by hand:

```yaml
kubectl -n <ns> create job --from=cronjob/<fullname>-token-refresh first-token
```

| Key | Default | Description |
|---|---|---|
| `tokenRefresh.enabled` | `false` |  |
| `tokenRefresh.schedule` | `0 * * * *` |  |
| `tokenRefresh.targetSecret` | `""` | The Secret to patch, its key and its namespace (default: the release's), which are the Binding's identity.tokenSecretRef and the Binding's namespace. The Job's Role, in that namespace, allows `patch` on this one Secret. |
| `tokenRefresh.targetKey` | `token` |  |
| `tokenRefresh.targetNamespace` | `""` |  |
| `tokenRefresh.dex.tokenURL` | `""` | for example http://dex.dex.svc.cluster.local:5556/dex/token |
| `tokenRefresh.dex.clientId` | `""` |  |
| `tokenRefresh.dex.scope` | `openid email` |  |
| `tokenRefresh.dex.credentialsSecret` | `""` | A Secret in the release namespace with the keys clientSecret, username and password. |
| `tokenRefresh.image.repository` | `curlimages/curl` |  |
| `tokenRefresh.image.tag` | `8.11.1` |  |
| `tokenRefresh.image.digest` | `sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69` |  |
| `tokenRefresh.resources.requests` | `{cpu: 10m, memory: 16Mi}` |  |
| `tokenRefresh.resources.limits` | `{memory: 64Mi}` |  |

---

Generated by `hack/docs/refgen` from `charts/agent-kourier/values.yaml`. Do not edit this page; change the source and run `make docs-ref`.

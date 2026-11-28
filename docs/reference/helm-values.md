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
| `serviceAccount.name` | `""` | Defaults to the fullname, or to the namespace's "default" ServiceAccount with create: false. Agent Kourier reads Secrets through the API, so the token stays mounted. |
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

What Agent Kourier reads, and from where. config.source picks the route (AGENTKOURIER_CONFIG_SOURCE). With crds, the default and the route to run on Kubernetes, it reads Binding, AgentBackend and ChatConnection custom resources of the cluster with informers: other teams may make their own, and one bad resource does not stop the others. The resources below, if you give any, are rendered as custom resources and not as a ConfigMap. That needs Kubernetes 1.33 or later and the CRDs (crds.install below, which follows this route, or yours) installed BEFORE a release that renders any resource below (a release that gives none installs them with it, and you apply the resources yourself). The broker serves a ChatConnection of its own (config.chatConnection below, or the only one of chatConnections), the ChatConnections of the namespaces it admits, or both (the table at config.chatConnection). The pod then mounts no ConfigMap, and a config change does not roll it. With files, the quick route for a proof of concept, it reads the resources below, which the chart writes into a ConfigMap mounted at /etc/agent-kourier, or existingConfigMap, the name of a ConfigMap you manage (one key per file, each ending .yaml). Not both. A config with one bad resource is not loaded: all or nothing; and files has no Kubernetes minimum. The two routes are never merged and there is no fallback: with files, custom resources in the cluster are ignored.

An install of the ConfigMap route that upgrades with its old values gets the default, crds: set config.source files to keep it. The chart refuses the cases it can tell are wrong (existingConfigMap, a cluster below 1.33, CRDs not yet served for resources it would render) and says so; https://agentkourier.dev/how-to/upgrade/ has the rest.

Each entry is keyed by its name and holds the resource's spec exactly as the resource reference describes it (the chart does not interpret it) and, optionally, the namespace (default: the release's; with crds it must exist). Secrets are referenced by name (credentialsSecretRef, tokenSecretRef, valueSecretRef) and live in the cluster, made by you or Sealed Secrets: no token value belongs in this file or in any values file (the render refuses a Slack token, and a header value whose name looks like a credential).

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

A commented first-install file for any A2A agent, to download and fill in, is the repository's examples/helm/my-values.yaml: https://agentkourier.dev/how-to/install-with-helm/#write-the-values

| Key | Default | Description |
|---|---|---|
| `config.source` | `crds` | crds or files. The route is chosen at start and a reload cannot change it: switching is an upgrade of the release, and with resources in config.\* the CRDs come first (crds.install below, and the chart README). The chart passes AGENTKOURIER_CONFIG_SOURCE only for crds, and reads an unset source as files, so `helm upgrade --reuse-values` from a release that had none keeps the route it had. |
| `config.chatConnection` | `""` | With source crds, the broker's own ChatConnection, as namespace/name or, for one in the release's namespace, name (AGENTKOURIER_CHAT_CONNECTION). When set, it is always served, and the broker owns the Bindings whose connectionRef names it, and those of the connections of the namespaces it admits: the ones on connectionNamespaces and the ones whose labels match connectionNamespaceSelector (below; {} matches every namespace). With chatConnection set and neither of those, it serves its own alone. With chatConnection empty and either of them set, it has no connection of its own: the chart renders no AGENTKOURIER_CHAT_CONNECTION, and every ChatConnection it serves is a team connection. With all three empty (the list empty, the selector null), the one entry of chatConnections below is the own connection; with no entry the broker serves the ChatConnections of every namespace, and two or more are refused. So adding one entry to an install that serves every namespace makes that entry its own connection, served alone: set connectionNamespaces or connectionNamespaceSelector to keep serving the teams' connections. Every namespace is a setting for a cluster with one broker: a second broker's connection, its own included, is served by this one too, and the two Socket Mode connections of one Slack app split its events. With every namespace, what lets a team attach a Slack app is the right to create a ChatConnection (the chart adds none to the admin or edit role) and the team's own Role that grants its Secret. Ignored with files, which serves the one ChatConnection of its directory, so a rollback to files may keep it set; but with the AX add-on on, the files route's Role in the release namespace still names its Secret when the connection is in that namespace. |
| `config.chatConnectionSecret` | `""` | With source crds and chatConnection set, the Secret that connection reads, by name, in the connection's namespace: the chart cannot see the ChatConnection you apply, and a broker that cannot read its Secret exits at start, so the chart grants this one Secret (rbac below). Empty is the connection's own name: a ChatConnection `slack` whose credentialsSecretRef names the Secret `slack` needs nothing here. Set it when the Secret is named otherwise. Ignored when chatConnections below renders the connection (its credentialsSecretRef is granted instead), which the one entry that is the own connection does, and with files (except that with the AX add-on on, the files route's Role in the release namespace still names it there). The chart refuses it when the broker has no connection of its own (chatConnection empty and connectionNamespaces or connectionNamespaceSelector set, or every namespace): there is none for it to name. |
| `config.connectionNamespaces` | `[]` | With source crds, the platform's admission list (AGENTKOURIER_CONNECTION_NAMESPACES): the namespaces whose ChatConnections this broker admits. It serves them beside the one chatConnection names, if any, at most 16 connections in all, the own connection counted first and the others oldest first (creationTimestamp, then namespace and name); a connection past the 16 is not served, and no setting changes the 16. Each item is a namespace name (a DNS-1123 label, no wildcard). Empty, the default, admits no namespace by name: the broker serves its own connection alone, or every namespace with no own connection and no selector (see chatConnection above). With chatConnection empty and no selector, a non-empty list is the broker with no connection of its own, and the chart refuses a chatConnections entry whose namespace is not in it (it would be rendered and never served). The broker reads the list at start, and a change to it changes the pod's environment. Listing a namespace is the platform's consent to the ChatConnections created there. Two ChatConnections of one Slack app (one app ID, even in two workspaces) are not both served: the older is. Each team grants the credentials Secret of its connection to this chart's ServiceAccount with a Role of its own in its namespace that names the Secret (resourceNames), as it grants its Binding tokens (see rbac below); rbac.secretNames or rbac.secretNamespaces may grant it instead. A team connection whose Secret the broker cannot read yet is started once it can, and never stops the broker. The chart refuses a non-empty list with files, which serves the one ChatConnection of its directory. |
| `config.connectionNamespaceSelector` | `null` | With source crds, a label selector on namespaces (AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR, which the chart renders as JSON): the broker also admits the ChatConnections of the namespaces whose labels match it, beside its own connection and the list's namespaces, under the same cap of 16. It is a Kubernetes label selector: matchLabels, and matchExpressions of key, operator (In, NotIn, Exists, DoesNotExist) and values. {} selects every namespace; null, the default, is no selector. The broker reads the selector at start, as it reads the list, but it follows the namespaces' labels live, with an informer on namespaces: a namespace that gains a matching label is admitted at the next read, with no restart, and one that loses it stops being admitted, and the Bindings of its connections are told ConnectionNotServed. While the broker cannot reach the API server it keeps the labels it last read. Whoever may set a namespace's labels therefore decides whether its ChatConnections are admitted. A selector that selects by label adds list and watch on namespaces (no get) to the ClusterRole (rbac below); {} and null add nothing. With chatConnection empty, a selector makes the entries of chatConnections team connections, which the chart does not check against it: it cannot see a namespace's labels. The chart refuses a selector, {} included, with files, as it refuses the list. For example, connectionNamespaceSelector: {matchLabels: {agentkourier.dev/serve: "true"}} admits the namespaces labelled agentkourier.dev/serve=true. Helm merges a map across values files, so a selector in a later -f file does not replace one in an earlier file: {} there keeps the earlier selector, and another selector's matchLabels are added to the earlier ones, so a namespace must match both (a matchExpressions list replaces the earlier list). To change the selector, edit the one file that sets it, give the new one with --set-json 'config.connectionNamespaceSelector=...' (which replaces it; the whole-object form, --set-json '{"config":{...}}', merges as a file does), or set it to null in a file before the one with the new selector. helm upgrade --reuse-values and --reset-then-reuse-values keep the release's selector and merge a new one into it, given with --set-json or after a null too (with --reset-then-reuse-values a null alone clears it, which with no own connection and no list is every namespace): upgrade with neither, passing all your values files. |
| `config.existingConfigMap` | `""` |  |
| `config.agentBackends` | `{}` |  |
| `config.chatConnections` | `{}` |  |
| `config.bindings` | `{}` |  |
| `config.rollOnChange` | `true` | Roll the pod when the rendered ConfigMap changes. Agent Kourier reloads its files in place, and follows a Secret that a reload adds, in any namespace its ServiceAccount may list and watch it in; a Secret it may not read fails that reload, and the last good config stays. Has no effect with existingConfigMap, and none with source crds, which renders no ConfigMap. Without the roll (rollOnChange false, or existingConfigMap), a reload that renames the ChatConnection or adds a second is refused, and the last good config stays until the pod restarts. |

## crds

The CustomResourceDefinitions of Binding, AgentBackend and ChatConnection (group agentkourier.dev), which config.source crds reads. They are generated from the API types (make crds) and rendered from the chart's templates, not its crds/ directory, which Helm's documentation says it installs once and never upgrades or deletes, so a chart upgrade carries a changed schema.

Unset (null), the default, this follows the route: true with config.source crds, which reads the CRDs, and false with files, which needs none. An explicit true or false always wins: true with files is the first step of a move to custom resources, and false with crds is for an installer that may not create CRDs (creating one needs a cluster-scoped right that a namespace-scoped installer lacks). A platform team can install the CRDs once itself (kubectl apply --server-side -f charts/agent-kourier/files/crds) and set this to false: Helm will not take over CRDs that kubectl made (invalid ownership metadata). Render them from one release only: two releases that render the same CRD conflict.

Each carries the annotation agentkourier.dev/schema-sha256, which the generated file holds (the SHA-256 of the file without that line), so that whatever applies the file applies it (Helm and kubectl do, measured; a GitOps tool should, unverified). The broker compares it with the hash it was built with, at start and every minute (a mismatch warns when a CRD's result changes, and is a metric, agentkourier_crd_schema_state; it stops nothing). It states which files were applied: it is not proof of what the API server serves. A CRD from an older chart or file has none, which is reported as unannotated and is no fault.

They carry helm.sh/resource-policy: keep, so uninstalling the release, or turning this off, leaves them there, and with them every custom resource: deleting a CRD deletes its resources too. Nor does a release that has this off upgrade them: a chart upgrade that changes a schema needs it set, so a release that goes back to files keeps it true in the values file for as long as the chart is meant to look after the CRDs.

"Kept" is Helm's guarantee only. Under Argo CD with prune, turning this off, or deleting the Application, may delete the CRDs, and every team's Binding, AgentBackend and ChatConnection with them. The chart also renders argocd.argoproj.io/sync-options: Prune=false,Delete=false on each CRD, which Argo CD's documentation says stops that (unverified: Argo CD has not been run against this chart). Another tool that prunes needs its own way to be told to leave the CRDs, or the CRDs applied outside what it prunes.

| Key | Default | Description |
|---|---|---|
| `crds.install` | `null` |  |

## store

Where Agent Kourier keeps its state. SQLite is one file on a volume (persistence below), for one replica. Postgres is a database you run (the chart does not install one), for more than one replica: they elect a leader (leaderElection below).

| Key | Default | Description |
|---|---|---|
| `store.type` | `sqlite` | sqlite or postgres (AGENTKOURIER_STORE) |
| `store.postgres.dsnSecret` |  | The Secret that holds the DSN, a postgres:// URL or key/value string, in the release namespace: required with store.type postgres, and refused with sqlite. Make it yourself (by hand, Sealed Secrets, an external operator); the DSN, password included, never goes in a values file (the render refuses a postgres:// URL with a password anywhere in them). Agent Kourier reads it through the API like its other Secrets, which the Role below grants: list and watch on that Secret by name with config.source crds, and on every Secret of the release namespace with config.source files (by name there too while the AX add-on is on) or with the release namespace in rbac.secretNamespaces (which the AX add-on refuses), so keep Agent Kourier's Secrets in a namespace of their own when it is shared. Percent-encode the reserved characters of a password in a URL. The role in the DSN needs the right to create tables in its schema: agent-kourier applies its own migrations at start. |
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

The memory limit sets GOMEMLIMIT too: when limits.memory is a plain quantity (a number of bytes, optionally fractional, with no suffix or one of Ki, Mi, Gi, Ti, Pi, k, M, G, T, P), the container gets GOMEMLIMIT at 80% of it, in bytes, so that the Go runtime collects harder as its memory nears the limit. It is a soft limit: it caps nothing, and memory that is live (many large replies from agents decoded at once) can still pass the limit and be OOM-killed. No variable is set without a memory limit, for a quantity this does not read (an exponent, milli-bytes) or one under 1Mi, or when extraEnv sets GOMEMLIMIT, which then wins. The variable is part of the pod template: the first upgrade to a chart that sets it restarts the pod.

| Key | Default | Description |
|---|---|---|
| `resources.requests` | `{cpu: 50m, memory: 64Mi}` |  |
| `resources.limits` | `{memory: 256Mi}` |  |

## rbac

Agent Kourier reads the Secrets that its config names, and the Postgres DSN's, through the API (list and watch, in a namespace-scoped Role; it never issues a get). Roles are made in each namespace that holds a referenced Secret, found from config.\* above, and the release namespace with store.type postgres.

With config.source files, such a Role covers every Secret of its namespace, except in the release namespace while the AX add-on is on, where it names Secrets as with crds. With config.source crds it covers only the Secrets that the chart can name: the ones config.\* and the Postgres DSN refer to, the one the broker's own ChatConnection reads, if it has one (config.chatConnection, and config.chatConnectionSecret when the Secret is not named like it), and rbac.secretNames, which list and watch authorize by resourceNames because Agent Kourier's requests carry a metadata.name field selector. Teams that make their own Bindings grant their own Secrets the same way (a Role with resourceNames in their namespace, bound to this ServiceAccount), or you name their namespace in secretNamespaces.

With crds the chart also makes a ClusterRole (list and watch on the three kinds, patch on bindings/status, get on the three CRDs by name, and list and watch on namespaces with a config.connectionNamespaceSelector other than {}), and binds it to the ServiceAccount, unless rbac.create is false. For example, secretNames: {agent-kourier: [slack-main]} grants the one Secret slack-main of namespace agent-kourier.

| Key | Default | Description |
|---|---|---|
| `rbac.create` | `true` |  |
| `rbac.secretNamespaces` | `[]` | More namespaces to grant the Role in, for every Secret of the namespace. Needed with config.existingConfigMap, which the chart cannot read. With crds a namespace listed here is namespace-wide, whatever rbac.secretNames says of it. With the AX add-on on, the chart refuses the release namespace here: list the broker's Secrets there in rbac.secretNames instead, existingConfigMap included. |
| `rbac.secretNames` | `{}` | With config.source crds, more Secrets to grant by name, as a map of namespace to Secret names. For Secrets the chart cannot see, other than the broker's own ChatConnection's (which config.chatConnection already grants): a Binding's token in the release namespace, say. A team's own Secrets are the team's to grant with a Role in its namespace. Ignored with files, whose Roles are namespace-wide, so a rollback to files may keep it set; the exception is the release namespace while the AX add-on is on, whose Role names these Secrets with the others the chart can see. |

## service

A ClusterIP Service on the metrics port (/metrics, and the health paths), which selects every pod, a standby's included. Never published. Chat requests never come through it: the chat listener has a Service of its own (chatListener below).

| Key | Default | Description |
|---|---|---|
| `service.enabled` | `false` |  |
| `service.port` | `8080` |  |
| `service.annotations` | `{}` |  |

## chatListener

The chat listener (AGENTKOURIER_CHAT_LISTEN): a port of its own, apart from the metrics port, that a chat platform sends its requests to, POST /chat/&lt;platform&gt;/&lt;namespace&gt;/&lt;name&gt;. A Slack ChatConnection with spec.slack.mode http needs it, and so does a Microsoft Teams one in http mode (spec.teams.mode unset or http) or with a spec.teams.notificationURL in either mode, for Microsoft Graph's notifications, and every webhook ChatConnection (spec.platform webhook), for its service's webhooks. Slack's Socket Mode, and Teams' socket mode without a notificationURL, do not. A connection that needs it on a broker without it is not run: the broker's own connection fails the start. Off by default, and then the broker opens no such port.

Switched on, the chart passes the pod's name and namespace from the downward API (AGENTKOURIER_POD_NAME, AGENTKOURIER_POD_NAMESPACE). Every replica takes the label agentkourier.dev/role=leader off its own pod as it starts, and is not Ready until a write of the label has succeeded, that removal or the label put on (so a replica whose patches of its pod keep failing, while the API server answers its reads, is out of every Service until one succeeds); the leader puts it on once it leads, and takes it off at shutdown. With SQLite the one replica leads, and carries it. podLabels may not set it. The chat Service, &lt;fullname&gt;-chat, selects the Deployment's pods that carry the label, so it sends requests to the leader alone; while no replica leads (a takeover) it has no endpoint, and the platform's requests fail until one does.

With rbac.create the chart grants the label with a Role in the release namespace: patch on pods. A Deployment's pods are named by its ReplicaSet, so the chart cannot name the pod in resourceNames, and the Role covers every pod of the release namespace. With it the chart installs a ValidatingAdmissionPolicy and its binding (cluster-scoped) that let the broker's ServiceAccount change only that label, and only on the pod its token is bound to. Keep Agent Kourier in a namespace of its own all the same. With networkPolicy.enabled the chart admits networkPolicy.chatFrom to this port, which it then requires.

| Key | Default | Description |
|---|---|---|
| `chatListener.enabled` | `false` |  |
| `chatListener.port` | `8081` | The container port; AGENTKOURIER_CHAT_LISTEN is :&lt;port&gt;. It must differ from listen.port. |
| `chatListener.service.type` | `ClusterIP` | ClusterIP, NodePort or LoadBalancer. Behind the Ingress below, ClusterIP is enough. |
| `chatListener.service.port` | `8081` |  |
| `chatListener.service.annotations` | `{}` |  |
| `chatListener.ingress` |  | An Ingress, &lt;fullname&gt;-chat, that sends the path prefix /chat/ to the chat Service, and nothing else. Needs chatListener.enabled. |
| `chatListener.ingress.enabled` | `false` |  |
| `chatListener.ingress.className` | `""` | The IngressClass; empty leaves it to the cluster's default class. |
| `chatListener.ingress.annotations` | `{}` |  |
| `chatListener.ingress.host` | `""` | The host its rule matches; empty matches every host. |
| `chatListener.ingress.tls` | `[]` | In Kubernetes' own form, for example [{hosts: [chat.example.com], secretName: chat-tls}]. |

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

Fences Agent Kourier's network. Egress is the Kubernetes API server, DNS, Slack on 443 and what extraEgress adds (the agent backend, an OTLP collector); ingress is the metrics scrape and, with chatListener.enabled, chatFrom to the chat port, and nothing else. Needs a CNI that enforces NetworkPolicy.

| Key | Default | Description |
|---|---|---|
| `networkPolicy.enabled` | `false` |  |
| `networkPolicy.kubeApiServer.cidrs` | `[]` | The API server's addresses, as the pod sees them after the Service's translation: required when enabled. A NetworkPolicy cannot select the API server by pod. Where the control plane is yours (kind, kubeadm), the addresses of `kubectl get endpoints kubernetes`, as /32s. On EKS those are control-plane ENI addresses that change when AWS upgrades or replaces the control plane, so give the CIDRs of the control-plane subnets instead (`aws eks describe-cluster --name <cluster>`, resourcesVpcConfig.subnetIds, then each subnet's CIDR). The VPC CNI enforces NetworkPolicy only with enableNetworkPolicy=true on the vpc-cni add-on; without it this policy does nothing. |
| `networkPolicy.kubeApiServer.ports` | `[443, 6443]` |  |
| `networkPolicy.dns.namespaceSelector` | `{matchLabels: {kubernetes.io/metadata.name: kube-system}}` |  |
| `networkPolicy.dns.podSelector` | `{matchLabels: {k8s-app: kube-dns}}` |  |
| `networkPolicy.slack.cidrs` | `["0.0.0.0/0"]` | A NetworkPolicy cannot name a host, so Slack (slack.com, wss-\*.slack.com, files.slack.com) is port 443 to anywhere outside the private ranges. The same rule lets a Microsoft Teams connection reach Microsoft Entra, the Bot Connector, Microsoft Graph and, in socket mode, Teams' sockets, all on 443. Put an egress proxy's address in extraEgress and narrow this to [] to close it. |
| `networkPolicy.slack.except` | `["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"]` |  |
| `networkPolicy.postgres` |  | The Postgres database (store.type postgres), as NetworkPolicyPeers: required when this policy is enabled with Postgres, since the Slack rule excludes private ranges and is 443 only. An ipBlock for a managed database, or the pods of an in-cluster one: to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: db}}, podSelector: {matchLabels: {app: postgres}}}] Egress is on `port` (the server's, 5432 by default). Behind a pooler, name the pooler. |
| `networkPolicy.postgres.to` | `[]` |  |
| `networkPolicy.postgres.port` | `5432` |  |
| `networkPolicy.metricsFrom` | `[]` | Who may scrape the metrics port: NetworkPolicyPeers, for example the Prometheus pods: - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: monitoring}} podSelector: {matchLabels: {app.kubernetes.io/name: prometheus}} Empty admits nobody. Kubelet's probes are node traffic, which the CNIs we know admit regardless. |
| `networkPolicy.chatFrom` | `[]` | Who may reach the chat port (chatListener.enabled): NetworkPolicyPeers, for example the ingress controller's pods: - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress-nginx}} or an ipBlock for a load balancer that keeps the client's address. Required when this policy and the chat listener are both enabled, since nobody could reach the port without it; refused without the chat listener, which would ignore it. |
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

The target Secret is the Binding's identity.tokenSecretRef: its name, key and namespace (the Binding's), which the render checks against the inline config with config.source files (with crds, or an existingConfigMap, it cannot see the Bindings and does not check). The Job patches the Secret and does not create it, and with config.source files it must exist before Agent Kourier starts. After install, run the first refresh by hand:

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

## addons

Add-ons: services the chart can run beside Agent Kourier, each off until you enable it. An add-on is released with Agent Kourier, under the same version, and runs as a Deployment of its own named &lt;fullname&gt;-&lt;add-on&gt;. Its objects carry app.kubernetes.io/name=agent-kourier-&lt;add-on&gt;, never app.kubernetes.io/name=agent-kourier, so nothing that selects Agent Kourier's pods or Services (its Deployment, Service, ServiceMonitor, NetworkPolicy and PodDisruptionBudget, or a NetworkPolicy elsewhere that trusts that label) selects an add-on's.

addons.cma is the Claude Managed Agents (CMA) add-on: an A2A server in front of one CMA agent, so that a Binding drives that agent with the a2a dialect. It renders &lt;fullname&gt;-cma: a Deployment, a ServiceAccount whose token is not mounted (the add-on does not call the Kubernetes API), a ClusterIP Service on port 8090, named http, and with addons.cma.store.type sqlite a PersistentVolumeClaim. The add-on serves A2A at /a2a, its agent card, /healthz, /readyz and /metrics on that one port. Agent Kourier reaches it through an AgentBackend with dialect a2a and the url http://&lt;fullname&gt;-cma.&lt;namespace&gt;.svc:8090/a2a, which is also the endpoint the agent card names (PUBLIC_URL); a Binding names that AgentBackend in agent.backendRef (on the a2a dialect, agent.name and agent.namespace are labels: the URL selects the agent). With networkPolicy.enabled the add-on gets a NetworkPolicy of its own (addons.cma.networkPolicy), and with serviceMonitor.enabled a ServiceMonitor of its own, which scrapes /metrics on port http with serviceMonitor's namespace, labels, interval and timeout. For example, with the release agent-kourier in the namespace agent-kourier:

```yaml
config:
  agentBackends:
    cma:
      spec: {dialect: a2a, url: "http://agent-kourier-cma.agent-kourier.svc:8090/a2a"}
  bindings:
    cma-chat:
      spec:
        agent: {backendRef: {name: cma}, namespace: cma, name: claude}
        identity: {userId: agent-kourier-cma, tokenSecretRef: {name: cma-token}}
        chat: {connectionRef: {name: slack-main}, channel: C0123456789, output: live, triggers: [{type: mention}]}
        interactions: {toolApprovals: true}
addons:
  cma:
    enabled: true
    agentId: agent_0123
    environmentId: env_0123
    anthropicApiKeySecret: {name: anthropic, key: apiKey}
    auth: {tokenSecret: {name: cma-token}}
```

addons.ax is the AX add-on: a launcher that starts a Google AX Task from the template in addons.ax.taskTemplate for each accepted turn, and returns the result that the Task's wrapper posts to addons.ax.resultCallback.url. It serves A2A, so that a Binding drives it with the a2a dialect. It renders &lt;fullname&gt;-ax: a Deployment, a ServiceAccount whose token is not mounted (the add-on calls the AX control plane at addons.ax.server.address, not the Kubernetes API), a ClusterIP Service on port 8090, named http, and with addons.ax.store.type sqlite a PersistentVolumeClaim. With addons.ax.resultCallback.ingress.enabled it also renders &lt;fullname&gt;-ax-results, an Ingress for the two callback paths. The add-on serves A2A at /a2a, its agent card, the result callback, /healthz, /readyz and /metrics on that one port. Agent Kourier reaches it through an AgentBackend with dialect a2a and the url http://&lt;fullname&gt;-ax.&lt;namespace&gt;.svc:8090/a2a, which is also the endpoint the agent card names (PUBLIC_URL). With networkPolicy.enabled the add-on gets a NetworkPolicy of its own (addons.ax.networkPolicy), and with serviceMonitor.enabled a ServiceMonitor of its own, as addons.cma does. For example, with the release agent-kourier in the namespace agent-kourier:

```yaml
config:
  agentBackends:
    ax:
      spec: {dialect: a2a, url: "http://agent-kourier-ax.agent-kourier.svc:8090/a2a"}
  bindings:
    ax-chat:
      spec:
        agent: {backendRef: {name: ax}, namespace: ax, name: assistant}
        identity: {userId: agent-kourier-ax, tokenSecretRef: {name: ax-token}}
        chat: {connectionRef: {name: slack-main}, channel: C0123456789, output: live, triggers: [{type: mention}]}
addons:
  ax:
    enabled: true
    server: {address: ax-server.ax-system.svc:8080}
    taskTemplate: {configMap: {name: ax-task}}
    auth: {tokenSecret: {name: ax-a2a-auth}}
    resultCallback: {url: "https://ax-results.example.test/ax/results/v1", tokenSecret: {name: ax-result-auth}}
```

| Key | Default | Description |
|---|---|---|
| `addons.cma` |  | The Claude Managed Agents add-on, off by default. |
| `addons.cma.enabled` | `false` |  |
| `addons.cma.image` |  | The add-on's image. Its pod uses imagePullSecrets above, as Agent Kourier's does. |
| `addons.cma.image.repository` | `ghcr.io/agentkourier/agent-kourier-cma` |  |
| `addons.cma.image.tag` | `""` | A tag, which defaults to the chart's appVersion, as image.tag does: a released chart's appVersion is its release tag, and the add-on's image is published under the same tag. Ignored when digest is set. |
| `addons.cma.image.digest` | `""` | Pin production to a digest (sha256:...), which wins over tag. |
| `addons.cma.image.pullPolicy` | `IfNotPresent` |  |
| `addons.cma.replicaCount` | `1` | 1, or 2 and more with addons.cma.store.type postgres (the render refuses more than 1 with sqlite: the add-on's SQLite file is on a ReadWriteOnce volume and may be open in one pod). With Postgres the replicas elect a leader (addons.cma.leaderElection), every replica is Ready and behind the Service, and the Deployment rolls one new pod in before it takes an old one away; at 2 and more the chart adds a PodDisruptionBudget, &lt;fullname&gt;-cma, with maxUnavailable 1, so that a node drain does not take every replica at once. 0 stops the add-on, and its volume or database stays. |
| `addons.cma.agentId` | `""` | Required. The ID of the CMA agent the add-on serves (CMA_AGENT_ID). |
| `addons.cma.environmentId` | `""` | Required. The ID of the CMA environment its sessions run in (CMA_ENVIRONMENT_ID). |
| `addons.cma.anthropicApiKeySecret` |  | Required. The Secret, in the release namespace, and its key, that hold the Anthropic API key. The chart passes it into the container's environment (ANTHROPIC_API_KEY, with valueFrom.secretKeyRef), which Kubernetes reads when the container starts: after a change to the Secret, restart the add-on (kubectl rollout restart deployment/&lt;fullname&gt;-cma). |
| `addons.cma.anthropicApiKeySecret.name` | `""` |  |
| `addons.cma.anthropicApiKeySecret.key` | `""` |  |
| `addons.cma.sessionIdleTTL` | `""` | How long a thread's context may go without activity (a message accepted for it, or a turn of it starting or ending), with no turn running, before the add-on archives its CMA session (SESSION_IDLE_TTL), a Go duration. Empty is the add-on's default, 2160h, which is Agent Kourier's default session.threadTTL: set it to the Binding's. "0" never archives. |
| `addons.cma.toolResultTimeout` | `""` | How long a turn waits, with no session event, for the result of a tool that the sandbox runs (TOOL_RESULT_TIMEOUT), a Go duration. Empty is the add-on's default, 10m; "0" waits for ever. |
| `addons.cma.taskRetention` | `""` | How long the add-on keeps a settled task, and its message IDs, before it deletes them (TASK_RETENTION), a Go duration. Empty is the add-on's default, 720h; "0" keeps them. |
| `addons.cma.logLevel` | `info` | debug, info, warn or error (LOG_LEVEL) |
| `addons.cma.auth` |  | Who may call the add-on. The chart has it listen on every interface of the pod (LISTEN_ADDR :8090), where the add-on refuses to start without a caller check, so the chart offers no way to turn the check off: a request without an accepted token is refused. The chart mounts tokenSecret's key as a file (AUTH_TOKEN_FILE) that holds the accepted tokens, one per line (blank lines, and lines that begin with #, are ignored). The add-on reads the file again every few seconds, and the kubelet updates a mounted Secret in place (after its own sync delay), so a change of the tokens needs no restart. Agent Kourier sends the token in one of two ways, which header below picks. With header empty, a Binding's identity.tokenSecretRef names a Secret with the token, which Agent Kourier sends as Authorization: Bearer &lt;token&gt;. With header set, the AgentBackend has an entry of headers with that name and a valueSecretRef that names a Secret with the token, which Agent Kourier sends as the header's whole value. The broker and the add-on may read one Secret and key while it holds a single token. To rotate without a refused turn, give the add-on a Secret of its own: add the new token to it on a line of its own, change the broker's Secret to the new token, then remove the old line. With config.source crds and a Binding or AgentBackend that config.\* does not render, grant the broker's Secret to Agent Kourier (rbac.secretNames, or the team's own Role). |
| `addons.cma.auth.tokenSecret` |  | Required: the Secret, in the release namespace, and its key. The key defaults to token, the key that identity.tokenSecretRef and valueSecretRef read when they name none. |
| `addons.cma.auth.tokenSecret.name` | `""` |  |
| `addons.cma.auth.tokenSecret.key` | `token` |  |
| `addons.cma.auth.header` | `""` | The header that carries the token (AUTH_HEADER). Empty is the add-on's default, Authorization, whose value is Bearer &lt;token&gt;; any other name carries the token as its whole value, which an AgentBackend header with a valueSecretRef sends. The values schema and the add-on refuse, in any case, the other names Agent Kourier reserves for the Binding's identity, which no AgentBackend header may set and Agent Kourier never fills with the token: Proxy-Authorization, Cookie, X-User-Id, X-Agent-Name, X-Share-Token and X-Kagent-Insecure-Runtime-Identity. |
| `addons.cma.store` |  | Where the add-on keeps its tasks, the message IDs it has seen and which CMA session serves each context. This is the add-on's own setting, apart from Agent Kourier's store.type. |
| `addons.cma.store.type` | `sqlite` | sqlite or postgres (STORE) |
| `addons.cma.store.postgres.dsnSecret` |  | The Secret, in the release namespace, and its key, that hold the DSN: required with addons.cma.store.type postgres, and refused with sqlite. The chart passes it into the container's environment (POSTGRES_DSN, with valueFrom.secretKeyRef). The DSN, password included, never goes in a values file (the render refuses a postgres:// URL with a password anywhere in them). Give the add-on a database of its own, apart from Agent Kourier's. |
| `addons.cma.store.postgres.dsnSecret.name` | `""` |  |
| `addons.cma.store.postgres.dsnSecret.key` | `""` |  |
| `addons.cma.leaderElection` |  | Leader election, with addons.cma.store.type postgres only (with sqlite none of this is allowed). The replicas hold a lease in the database, not a Kubernetes Lease, so the add-on needs no RBAC. The leader serves A2A; a standby forwards each request it receives to the leader's pod address (ADVERTISE_URL, http://&lt;pod IP&gt;:8090, which the chart sets from the downward API, with POD_NAME, the lease holder's name). Empty is the add-on's default; each is a Go duration. |
| `addons.cma.leaderElection.leaseDuration` | `""` | 15s (LEASE_DURATION) |
| `addons.cma.leaderElection.renewDeadline` | `""` | 10s (LEASE_RENEW_DEADLINE) |
| `addons.cma.leaderElection.retryPeriod` | `""` | 2s (LEASE_RETRY_PERIOD) |
| `addons.cma.persistence` |  | The add-on's SQLite volume (addons.cma.store.type sqlite; with postgres none of this is used or rendered). The database is /data/cma.db (SQLITE_PATH) on the claim &lt;fullname&gt;-cma-data, which is ReadWriteOnce, and the Deployment's strategy is Recreate, so the old pod has released the volume before the new one starts. |
| `addons.cma.persistence.existingClaim` | `""` | Use a claim you manage instead of creating one. |
| `addons.cma.persistence.size` | `1Gi` |  |
| `addons.cma.persistence.storageClassName` | `""` | Empty is the cluster's default class; "-" is an explicitly empty one. |
| `addons.cma.persistence.accessModes` | `[ReadWriteOnce]` |  |
| `addons.cma.persistence.retainOnUninstall` | `true` | Keep the claim (helm.sh/resource-policy: keep) when the release is uninstalled. Delete it by hand when you mean to. |
| `addons.cma.networkPolicy` |  | With networkPolicy.enabled, the add-on's NetworkPolicy. Ingress is port 8090 from Agent Kourier's pods, from networkPolicy.metricsFrom (the metrics share the port with A2A: /a2a and the agent card need a token, and /healthz, /readyz and /metrics do not) and, with Postgres, from the add-on's other replicas, which forward to the leader. Egress is DNS (networkPolicy.dns), the Anthropic API, with Postgres the add-on's other replicas on 8090 and its database, and what addons.cma.networkPolicy.extraEgress adds. Agent Kourier's own policy then also allows egress to the add-on's pods on 8090. |
| `addons.cma.networkPolicy.anthropic` |  | The Anthropic API, on 443. A NetworkPolicy cannot name a host, so this is port 443 to anywhere outside the private ranges, as networkPolicy.slack is for Agent Kourier. Put an egress proxy's address in extraEgress and narrow this to [] to close it. |
| `addons.cma.networkPolicy.anthropic.cidrs` | `["0.0.0.0/0"]` |  |
| `addons.cma.networkPolicy.anthropic.except` | `["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"]` |  |
| `addons.cma.networkPolicy.postgres` |  | The add-on's database (addons.cma.store.type postgres), as NetworkPolicyPeers: required when the policy is enabled with Postgres, as networkPolicy.postgres.to is for Agent Kourier's. Egress is on port. |
| `addons.cma.networkPolicy.postgres.to` | `[]` |  |
| `addons.cma.networkPolicy.postgres.port` | `5432` |  |
| `addons.cma.networkPolicy.extraEgress` | `[]` | More egress rules, in Kubernetes' own form: an OTLP collector, an egress proxy. |
| `addons.cma.extraEnv` | `[]` | Extra container environment, in Kubernetes' own form. This is where the OpenTelemetry settings go (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_TRACES_EXPORTER, OTEL_SERVICE_NAME, whose default is agent-kourier-cma, ...); take a header or other credential from a Secret with valueFrom.secretKeyRef, never as a value. |
| `addons.cma.resources` |  | As resources does for Agent Kourier, a memory limit that is a plain quantity sets GOMEMLIMIT to 80% of it, unless extraEnv sets GOMEMLIMIT. |
| `addons.cma.resources.requests` | `{cpu: 50m, memory: 64Mi}` |  |
| `addons.cma.resources.limits` | `{memory: 256Mi}` |  |
| `addons.cma.podAnnotations` | `{}` |  |
| `addons.cma.podLabels` | `{}` | Added to the pod. app.kubernetes.io/name, /instance and /component cannot be changed. |
| `addons.cma.nodeSelector` | `{}` |  |
| `addons.cma.tolerations` | `[]` |  |
| `addons.cma.affinity` | `{}` |  |
| `addons.cma.podSecurityContext.runAsNonRoot` | `true` |  |
| `addons.cma.podSecurityContext.runAsUser` | `65532` |  |
| `addons.cma.podSecurityContext.runAsGroup` | `65532` |  |
| `addons.cma.podSecurityContext.fsGroup` | `65532` |  |
| `addons.cma.podSecurityContext.seccompProfile` | `{type: RuntimeDefault}` |  |
| `addons.cma.securityContext.allowPrivilegeEscalation` | `false` |  |
| `addons.cma.securityContext.readOnlyRootFilesystem` | `true` |  |
| `addons.cma.securityContext.capabilities` | `{drop: [ALL]}` |  |
| `addons.ax` |  | The AX add-on, off by default. |
| `addons.ax.enabled` | `false` |  |
| `addons.ax.image` |  | The AX launcher image. The pod uses imagePullSecrets above. |
| `addons.ax.image.repository` | `ghcr.io/agentkourier/agent-kourier-ax` |  |
| `addons.ax.image.tag` | `""` | A tag, which defaults to the chart's appVersion, as image.tag does: a released chart's appVersion is its release tag, and the add-on's image is published under the same tag. Ignored when digest is set. |
| `addons.ax.image.digest` | `""` | Pin production to a digest (sha256:...), which wins over tag. |
| `addons.ax.image.pullPolicy` | `IfNotPresent` |  |
| `addons.ax.replicaCount` | `1` | 1, or 2 and more with addons.ax.store.type postgres (the render refuses more than 1 with sqlite: the add-on's SQLite file is on a ReadWriteOnce volume and may be open in one pod). With Postgres the replicas elect a leader (addons.ax.leaderElection), every replica is Ready and behind the Service, and the Deployment rolls one new pod in before it takes an old one away; at 2 and more the chart adds a PodDisruptionBudget, &lt;fullname&gt;-ax, with maxUnavailable 1, so that a node drain does not take every replica at once. 0 stops the add-on, and its volume or database stays. |
| `addons.ax.server` |  | Private AX gRPC endpoint (AX_ADDR). |
| `addons.ax.server.address` | `""` |  |
| `addons.ax.atespace` | `default` | AX_ATESPACE |
| `addons.ax.launcherId` | `""` | Stable ownership scope (AX_LAUNCHER_ID). Empty uses the add-on's full name. |
| `addons.ax.taskTemplate.configMap` |  | ConfigMap in the release namespace with one AX Task template (AX_TASK_TEMPLATE). |
| `addons.ax.taskTemplate.configMap.name` | `""` |  |
| `addons.ax.taskTemplate.configMap.key` | `task.yaml` |  |
| `addons.ax.resultCallback.url` | `""` | HTTPS endpoint ending in /ax/results/v1 (AX_RESULT_URL). |
| `addons.ax.resultCallback.tokenSecret` |  | Dedicated callback bearer mounted only in the add-on (AX_RESULT_TOKEN_FILE). |
| `addons.ax.resultCallback.tokenSecret.name` | `""` |  |
| `addons.ax.resultCallback.tokenSecret.key` | `token` |  |
| `addons.ax.resultCallback.ingress` |  | Optional Ingress for the two exact callback paths. Use a private controller. |
| `addons.ax.resultCallback.ingress.enabled` | `false` |  |
| `addons.ax.resultCallback.ingress.annotations` | `{}` | Additional annotations for the callback Ingress. |
| `addons.ax.resultCallback.ingress.className` | `""` |  |
| `addons.ax.resultCallback.ingress.tlsSecretName` | `""` |  |
| `addons.ax.promptKey` | `prompt` | AX_PROMPT_KEY |
| `addons.ax.history` | `true` | AX_HISTORY |
| `addons.ax.maxRunning` | `10` | AX_MAX_RUNNING |
| `addons.ax.pollInterval` | `2s` | AX_POLL_INTERVAL |
| `addons.ax.turnTimeout` | `30m` | AX_TURN_TIMEOUT |
| `addons.ax.keepFailed` | `24h` | AX_KEEP_FAILED |
| `addons.ax.serviceAccount` |  | The add-on's own ServiceAccount, &lt;fullname&gt;-ax, which the chart grants nothing. |
| `addons.ax.serviceAccount.annotations` | `{}` | Additional annotations. |
| `addons.ax.sessionIdleTTL` | `""` | How long a thread's context may go without activity (a message accepted for it, or a create or an update of one of its tasks, which a turn makes as it starts and as it ends), with no turn running, before the add-on deletes the context's tasks (SESSION_IDLE_TTL), a Go duration. Empty is the add-on's default, 2160h, which is Agent Kourier's default session.threadTTL: set it to the Binding's. "0" keeps them. |
| `addons.ax.taskRetention` | `""` | How long the add-on keeps a settled task, and its message IDs, before it deletes them (TASK_RETENTION), a Go duration. Empty is the add-on's default, 720h; "0" keeps them. |
| `addons.ax.logLevel` | `info` | debug, info, warn or error (LOG_LEVEL) |
| `addons.ax.auth` |  | Who may call the add-on. The chart has it listen on every interface of the pod (LISTEN_ADDR :8090), where the add-on refuses to start without a caller check, so the chart offers no way to turn the check off: a request without an accepted token is refused. The chart mounts tokenSecret's key as a file (AUTH_TOKEN_FILE) that holds the accepted tokens, one per line (blank lines, and lines that begin with #, are ignored). The add-on reads the file again every few seconds, and the kubelet updates a mounted Secret in place (after its own sync delay), so a change of the tokens needs no restart. Agent Kourier sends the token in one of two ways, which header below picks. With header empty, a Binding's identity.tokenSecretRef names a Secret with the token, which Agent Kourier sends as Authorization: Bearer &lt;token&gt;. With header set, the AgentBackend has an entry of headers with that name and a valueSecretRef that names a Secret with the token, which Agent Kourier sends as the header's whole value. The broker and the add-on may read one Secret and key while it holds a single token. To rotate without a refused turn, give the add-on a Secret of its own: add the new token to it on a line of its own, change the broker's Secret to the new token, then remove the old line. With config.source crds and a Binding or AgentBackend that config.\* does not render, grant the broker's Secret to Agent Kourier (rbac.secretNames, or the team's own Role). |
| `addons.ax.auth.tokenSecret` |  | Required: the Secret, in the release namespace, and its key. The key defaults to token, the key that identity.tokenSecretRef and valueSecretRef read when they name none. |
| `addons.ax.auth.tokenSecret.name` | `""` |  |
| `addons.ax.auth.tokenSecret.key` | `token` |  |
| `addons.ax.auth.header` | `""` | The header that carries the token (AUTH_HEADER). Empty is the add-on's default, Authorization, whose value is Bearer &lt;token&gt;; any other name carries the token as its whole value, which an AgentBackend header with a valueSecretRef sends. The values schema and the add-on refuse, in any case, the other names Agent Kourier reserves for the Binding's identity, which no AgentBackend header may set and Agent Kourier never fills with the token: Proxy-Authorization, Cookie, X-User-Id, X-Agent-Name, X-Share-Token and X-Kagent-Insecure-Runtime-Identity. |
| `addons.ax.store` |  | Where the add-on keeps its tasks, the message IDs it has seen and its AX launch records. This is the add-on's own setting, apart from Agent Kourier's store.type. |
| `addons.ax.store.type` | `sqlite` | sqlite or postgres (STORE) |
| `addons.ax.store.postgres.dsnSecret` |  | The Secret, in the release namespace, and its key, that hold the DSN: required with addons.ax.store.type postgres, and refused with sqlite. The chart passes it into the container's environment (POSTGRES_DSN, with valueFrom.secretKeyRef). The DSN, password included, never goes in a values file (the render refuses a postgres:// URL with a password anywhere in them). Give the add-on a database of its own, apart from Agent Kourier's and the CMA add-on's. |
| `addons.ax.store.postgres.dsnSecret.name` | `""` |  |
| `addons.ax.store.postgres.dsnSecret.key` | `""` |  |
| `addons.ax.leaderElection` |  | Leader election, with addons.ax.store.type postgres only (with sqlite none of this is allowed). The replicas hold a lease in the database, not a Kubernetes Lease, so the add-on needs no RBAC. The leader serves A2A; a standby forwards each request it receives to the leader's pod address (ADVERTISE_URL, http://&lt;pod IP&gt;:8090, which the chart sets from the downward API, with POD_NAME, the lease holder's name). Empty is the add-on's default; each is a Go duration. |
| `addons.ax.leaderElection.leaseDuration` | `""` | 15s (LEASE_DURATION) |
| `addons.ax.leaderElection.renewDeadline` | `""` | 10s (LEASE_RENEW_DEADLINE) |
| `addons.ax.leaderElection.retryPeriod` | `""` | 2s (LEASE_RETRY_PERIOD) |
| `addons.ax.persistence` |  | The add-on's SQLite volume (addons.ax.store.type sqlite; with postgres none of this is used or rendered). The database is /data/ax.db (SQLITE_PATH) on the claim &lt;fullname&gt;-ax-data, which is ReadWriteOnce, and the Deployment's strategy is Recreate, so the old pod has released the volume before the new one starts. |
| `addons.ax.persistence.existingClaim` | `""` | Use a claim you manage instead of creating one. |
| `addons.ax.persistence.size` | `1Gi` |  |
| `addons.ax.persistence.storageClassName` | `""` | Empty is the cluster's default class; "-" is an explicitly empty one. |
| `addons.ax.persistence.accessModes` | `[ReadWriteOnce]` |  |
| `addons.ax.persistence.retainOnUninstall` | `true` | Keep the claim (helm.sh/resource-policy: keep) when the release is uninstalled. Delete it by hand when you mean to. |
| `addons.ax.networkPolicy` |  | When NetworkPolicy is enabled, specify the AX server and the controller that forwards HTTPS callbacks. |
| `addons.ax.networkPolicy.server.to` | `[]` |  |
| `addons.ax.networkPolicy.server.port` | `8080` |  |
| `addons.ax.networkPolicy.callbackFrom` | `[]` |  |
| `addons.ax.networkPolicy.postgres` |  | The add-on's database (addons.ax.store.type postgres), as NetworkPolicyPeers: required when the policy is enabled with Postgres, as networkPolicy.postgres.to is for Agent Kourier's. Egress is on port. |
| `addons.ax.networkPolicy.postgres.to` | `[]` |  |
| `addons.ax.networkPolicy.postgres.port` | `5432` |  |
| `addons.ax.networkPolicy.extraEgress` | `[]` | More egress rules, in Kubernetes' own form: for example an OTLP collector. |
| `addons.ax.extraEnv` | `[]` | Extra container environment, in Kubernetes' own form. This is where the OpenTelemetry settings go (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_TRACES_EXPORTER, OTEL_SERVICE_NAME, whose default is agent-kourier-ax, ...); take a header or other credential from a Secret with valueFrom.secretKeyRef, never as a value. |
| `addons.ax.resources` |  | As resources does for Agent Kourier, a memory limit that is a plain quantity sets GOMEMLIMIT to 80% of it, unless extraEnv sets GOMEMLIMIT. |
| `addons.ax.resources.requests` | `{cpu: 50m, memory: 128Mi}` |  |
| `addons.ax.resources.limits` | `{memory: 512Mi}` |  |
| `addons.ax.podAnnotations` | `{}` |  |
| `addons.ax.podLabels` | `{}` | Added to the pod. app.kubernetes.io/name, /instance and /component cannot be changed. |
| `addons.ax.nodeSelector` | `{}` |  |
| `addons.ax.tolerations` | `[]` |  |
| `addons.ax.affinity` | `{}` |  |
| `addons.ax.podSecurityContext.runAsNonRoot` | `true` |  |
| `addons.ax.podSecurityContext.runAsUser` | `65532` |  |
| `addons.ax.podSecurityContext.runAsGroup` | `65532` |  |
| `addons.ax.podSecurityContext.fsGroup` | `65532` |  |
| `addons.ax.podSecurityContext.seccompProfile` | `{type: RuntimeDefault}` |  |
| `addons.ax.securityContext.allowPrivilegeEscalation` | `false` |  |
| `addons.ax.securityContext.readOnlyRootFilesystem` | `true` |  |
| `addons.ax.securityContext.capabilities` | `{drop: [ALL]}` |  |

---

Generated by `hack/docs/refgen` from `charts/agent-kourier/values.yaml`. Do not edit this page; change the source and run `make docs-ref`.

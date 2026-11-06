{{/*
The name every resource of the release carries. The app.kubernetes.io/name label is always "agent-kourier", whatever
fullnameOverride says: NetworkPolicies elsewhere (the front door's, in the kind tiers) trust pods by that label.
*/}}
{{- define "agentkourier.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains "agent-kourier" .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-agent-kourier" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* The labels that select Agent Kourier's pods. They are immutable on a Deployment: do not add to them. */}}
{{- define "agentkourier.selectorLabels" -}}
app.kubernetes.io/name: agent-kourier
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "agentkourier.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "agentkourier.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "agentkourier.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "agentkourier.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The Agent Kourier image: a digest wins over a tag, and the tag defaults to the chart's appVersion. */}}
{{- define "agentkourier.image" -}}
{{- $repository := required "image.repository is required (a released chart has it; from source, set it)" .Values.image.repository -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/* The ConfigMap Agent Kourier reads its resources from. */}}
{{- define "agentkourier.configMapName" -}}
{{- default (printf "%s-config" (include "agentkourier.fullname" .)) .Values.config.existingConfigMap -}}
{{- end -}}

{{/*
True (non-empty) when Agent Kourier reads its resources from custom resources (config.source crds), not from files. The chart's default is crds. An
unset source reads as files: `helm upgrade --reuse-values` from a release that had no config.source hands the chart no such key, and that release
read files.
*/}}
{{- define "agentkourier.crMode" -}}
{{- if eq (default "files" .Values.config.source) "crds" -}}true{{- end -}}
{{- end -}}

{{/* True (non-empty) when config.* holds a resource of any kind, which the chart renders as a custom resource with config.source crds. */}}
{{- define "agentkourier.inlineResources" -}}
{{- if or .Values.config.agentBackends .Values.config.chatConnections .Values.config.bindings -}}true{{- end -}}
{{- end -}}

{{/*
The ChatConnection this broker serves with config.source crds, as namespace/name (AGENTKOURIER_CHAT_CONNECTION): config.chatConnection,
in the release namespace when it names no namespace, or the one entry of config.chatConnections when it is empty. A cluster may hold
the connections of several brokers, so a second entry is no guess to make.
*/}}
{{- define "agentkourier.chatConnection" -}}
{{- $ref := .Values.config.chatConnection -}}
{{- if not $ref -}}
{{- if ne (len .Values.config.chatConnections) 1 -}}
{{- fail "config.source crds (the chart's default) needs config.chatConnection: the namespace/name of the one ChatConnection this broker serves. A cluster may hold the connections of several brokers, so it is left out only when config.chatConnections has exactly one entry. To supply the config as a ConfigMap instead, set config.source: files (and crds.install: false, unless you want the CRDs too); https://agentkourier.dev/how-to/install-with-helm/ describes both routes" -}}
{{- end -}}
{{- range $name, $r := .Values.config.chatConnections -}}
{{- $ref = printf "%s/%s" (default $.Release.Namespace $r.namespace) $name -}}
{{- end -}}
{{- else if not (contains "/" $ref) -}}
{{- $ref = printf "%s/%s" .Release.Namespace $ref -}}
{{- end -}}
{{- $ref -}}
{{- end -}}

{{/* The ClusterRole (and its binding) that reads the custom resources: cluster-scoped, so the release namespace is in the name. */}}
{{- define "agentkourier.clusterRoleName" -}}
{{- printf "%s-%s-crs" (include "agentkourier.fullname" .) .Release.Namespace | trunc 253 | trimSuffix "-" -}}
{{- end -}}

{{/* True (non-empty) when Agent Kourier keeps its state in SQLite on a volume. */}}
{{- define "agentkourier.sqlite" -}}
{{- if ne .Values.store.type "postgres" -}}true{{- end -}}
{{- end -}}

{{/* The Lease the replicas elect their leader with: one for the Deployment, in the release namespace. */}}
{{- define "agentkourier.leaseName" -}}
{{- printf "%s-leader" (include "agentkourier.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agentkourier.pvcName" -}}
{{- default (printf "%s-data" (include "agentkourier.fullname" .)) .Values.persistence.existingClaim -}}
{{- end -}}

{{/*
The Go runtime's soft memory limit (GOMEMLIMIT) in bytes: four fifths of resources.limits.memory, which leaves the rest for what the Go
runtime does not count and for the moment between the limit being reached and a collection finishing. Empty, so that no variable is rendered, when:
there is no memory limit; the limit is not a plain, binary (Ki Mi Gi Ti Pi) or decimal (k M G T P) quantity, which this does not read and
Kubernetes still accepts (it is never a reason to fail the render); it is under 1 MiB; or extraEnv sets GOMEMLIMIT itself, which then wins and is
not duplicated. The value may arrive as a string ("256Mi"), a whole number from --set, or a float64 from a values file (1000000000).
resourceFieldRef with a divisor cannot do this: a divisor only scales the limit to a unit, never to a percentage, and with no limit it yields
the node's allocatable memory.
*/}}
{{- define "agentkourier.goMemLimit" -}}
{{- $own := false -}}
{{- range .Values.extraEnv -}}
{{- if and (kindIs "map" .) (eq (toString (get . "name")) "GOMEMLIMIT") -}}{{- $own = true -}}{{- end -}}
{{- end -}}
{{- $mem := dig "limits" "memory" "" (.Values.resources | default dict) -}}
{{- $text := toString $mem -}}
{{- if kindIs "float64" $mem -}}{{- $text = printf "%.0f" $mem -}}{{- end -}}
{{- $number := regexFind "^[0-9]+(\\.[0-9]+)?" $text -}}
{{- $unit := trimPrefix $number $text -}}
{{- $units := dict "" 1 "k" 1000 "M" 1000000 "G" 1000000000 "T" 1000000000000 "P" 1000000000000000 "Ki" 1024 "Mi" 1048576 "Gi" 1073741824 "Ti" 1099511627776 "Pi" 1125899906842624 -}}
{{- if and (not $own) $number (hasKey $units $unit) -}}
{{- $bytes := int64 (floor (mulf (float64 $number) (float64 (get $units $unit)))) -}}
{{- if and (ge $bytes 1048576) (lt $bytes 1000000000000000000) -}}
{{- div (mul $bytes 4) 5 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Checks that no values file can get past. Included by the Deployment, which every install has.
*/}}
{{- define "agentkourier.validate" -}}
{{- if and (gt (int .Values.replicaCount) 1) (include "agentkourier.sqlite" .) -}}
{{- fail "replicaCount > 1 needs store.type postgres: SQLite is one file on a ReadWriteOnce volume, which one pod may use, and Agent Kourier's session state is in memory, so only one replica may work. With Postgres the replicas elect a leader (a Lease) and the standby waits. replicaCount 0 is the kill switch." -}}
{{- end -}}
{{- if and (include "agentkourier.sqlite" .) (or .Values.leaderElection.leaseDuration .Values.leaderElection.renewDeadline .Values.leaderElection.retryPeriod) -}}
{{- fail "leaderElection is for store.type postgres: with sqlite there is one replica and no election, and the settings would be ignored" -}}
{{- end -}}
{{- with .Values.store -}}
{{- if eq .type "postgres" -}}
{{- if not (and .postgres.dsnSecret.name .postgres.dsnSecret.key) -}}
{{- fail "store.type postgres needs store.postgres.dsnSecret.name and store.postgres.dsnSecret.key: the Secret, in the release namespace, that holds the DSN" -}}
{{- end -}}
{{- if $.Values.persistence.existingClaim -}}
{{- fail "persistence.existingClaim is for store.type sqlite: with postgres no volume is mounted" -}}
{{- end -}}
{{- else -}}
{{- if or .postgres.dsnSecret.name .postgres.dsnSecret.key -}}
{{- fail "store.postgres.dsnSecret is for store.type postgres: with sqlite it would be ignored, and the state would be kept on the volume" -}}
{{- end -}}
{{- if ne .postgres.timeouts "agent-kourier" -}}
{{- fail "store.postgres.timeouts is for store.type postgres: with sqlite it would be ignored" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* A DSN with a password: postgres://user:pass@host. A reference to a Secret never looks like this. */ -}}
{{- if regexMatch `(?i)postgres(ql)?://[^/\s:@"]*:[^/\s@"]+@` (toJson .Values) -}}
{{- fail "the values hold what looks like a Postgres DSN with a password: no DSN belongs in a values file. Put it in a Secret and name the Secret in store.postgres.dsnSecret" -}}
{{- end -}}
{{- if and .Values.config.existingConfigMap (or .Values.config.agentBackends .Values.config.chatConnections .Values.config.bindings) -}}
{{- fail "config.existingConfigMap and config.agentBackends/chatConnections/bindings are two ways to supply the same files: set one" -}}
{{- end -}}
{{- if include "agentkourier.crMode" . -}}
{{- if .Values.config.existingConfigMap -}}
{{- fail "config.existingConfigMap is for config.source files, and config.source is crds (the chart's default): Agent Kourier would read custom resources, mount no ConfigMap and ignore it. To keep reading your ConfigMap, set config.source: files (crds.install is on by default and renders the CRDs with files too, so set crds.install: false to leave them out). To move to custom resources instead, remove config.existingConfigMap and follow https://agentkourier.dev/how-to/migrate-config-source/#move-a-configmap-you-manage-yourself" -}}
{{- end -}}
{{- /* The minimum is 1.33, the first minor on which the status write of a stored-invalid Binding ratchets (measured; it is refused on 1.30 to 1.32). The -0 lets a
       vendor suffix (v1.33.1-gke.1, v1.33.0-eks-abc) pass. */ -}}
{{- if not (semverCompare ">=1.33.0-0" .Capabilities.KubeVersion.Version) -}}
{{- fail (printf "config.source crds needs Kubernetes 1.33 or later (it is the chart's default): the API server is %s. 1.33 is the first minor tried on which a status write of a Binding that a rule of the CRDs now refuses is accepted (it is refused on 1.30 to 1.32). To install on this cluster, set config.source: files (and crds.install: false): the ConfigMap route has no minimum. With helm template, pass the cluster's version (--kube-version): Helm's default is its own" .Capabilities.KubeVersion.Version) -}}
{{- end -}}
{{- /* A CRD and the custom resources of it in one release can fail where Helm maps the kind, so the CRDs come first: they must already be served
       when the chart renders. A render that does not ask the cluster (helm template, helm install --dry-run=client) has only the versions it is given.
       With crds.install and nothing in config.* the release renders no custom resource, so there is nothing for Helm to map and the CRDs may come
       with it: that is a first install of the default, where the teams apply their own resources. */ -}}
{{- if not (.Capabilities.APIVersions.Has "agentkourier.dev/v1alpha1") -}}
{{- if (.Values.crds).install -}}
{{- if include "agentkourier.inlineResources" . -}}
{{- fail "config.source crds needs the CRDs (group agentkourier.dev, version v1alpha1) to be installed before a release that renders config.agentBackends, config.chatConnections or config.bindings as custom resources, and the cluster does not serve them yet: crds.install installs them with this release, too late for the resources of it. To keep the ConfigMap route, set config.source: files (and crds.install: false); https://agentkourier.dev/how-to/migrate-config-source/ describes moving to custom resources. To use custom resources, install the CRDs first: in an earlier install or upgrade set crds.install=true with config.source files, or leave config.* empty here, name the ChatConnection in config.chatConnection and apply the resources with kubectl once the CRDs are in, then set config.source crds. With helm template, add --api-versions agentkourier.dev/v1alpha1 to render as if they were installed" -}}
{{- end -}}
{{- else -}}
{{- fail "config.source crds needs the CRDs (group agentkourier.dev, version v1alpha1) to be installed before this release, and the cluster does not serve them (config.source crds is the chart's default). To keep the ConfigMap route, set config.source: files; https://agentkourier.dev/how-to/migrate-config-source/ describes moving to custom resources. To use custom resources, in an earlier install or upgrade set crds.install=true with config.source files, or apply the chart's files/crds yourself (kubectl apply --server-side -f charts/agent-kourier/files/crds), then set config.source crds. With helm template, add --api-versions agentkourier.dev/v1alpha1 to render as if they were installed" -}}
{{- end -}}
{{- end -}}
{{- $_ := include "agentkourier.chatConnection" . -}}
{{- /* With files, config.chatConnection and rbac.secretNames are ignored, and the chart does not refuse them: a rollback to files keeps the values
       it had, in an incident, and they are harmless there. */ -}}
{{- end -}}
{{- if and .Values.serviceMonitor.enabled (not .Values.service.enabled) -}}
{{- fail "serviceMonitor.enabled needs service.enabled: the ServiceMonitor selects the metrics Service" -}}
{{- end -}}
{{- /* A token's shape, from a word boundary: xoxb-123-..., xapp-1-...; never a name that merely contains xapp- (taxapp-slack). */ -}}
{{- if regexMatch `\bxox[abper]-[0-9]+-|\bxapp-[0-9]+-` (toJson .Values) -}}
{{- fail "the values hold what looks like a Slack token (xox*- or xapp-): no token belongs in a values file. Put it in a Secret and name the Secret in the config" -}}
{{- end -}}
{{- range $name, $r := .Values.config.agentBackends -}}
{{- range $h := (default (list) (dig "headers" (list) (default (dict) $r.spec))) -}}
{{- if and $h.value (regexMatch `(?i)(^|[-_])(api-?key|key|token|secret|auth[a-z]*|cookie|password|passwd)([-_]|$)` (toString $h.name)) -}}
{{- fail (printf "AgentBackend %q: header %q looks like a credential, so its value cannot be in the config (a ConfigMap): use valueSecretRef" $name $h.name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- with .Values.podDisruptionBudget -}}
{{- if .enabled -}}
{{- $minSet := not (kindIs "invalid" .minAvailable) -}}
{{- $maxSet := not (kindIs "invalid" .maxUnavailable) -}}
{{- if and $minSet $maxSet -}}
{{- fail "podDisruptionBudget: set minAvailable or maxUnavailable, not both" -}}
{{- end -}}
{{- if not (or $minSet $maxSet) -}}
{{- fail "podDisruptionBudget.enabled needs minAvailable or maxUnavailable" -}}
{{- end -}}
{{- if $maxSet -}}
{{- if eq (int (toString .maxUnavailable | trimSuffix "%")) 0 -}}
{{- fail "podDisruptionBudget.maxUnavailable 0 blocks every voluntary eviction, a node drain included" -}}
{{- end -}}
{{- else if gt (int $.Values.replicaCount) 0 -}}
{{- if kindIs "string" .minAvailable -}}
{{- /* Kubernetes rounds a percentage up to whole pods: it keeps them all once ceil(pct * replicas / 100) reaches replicas. */ -}}
{{- $pct := int (trimSuffix "%" .minAvailable) -}}
{{- $r := int $.Values.replicaCount -}}
{{- if ge (div (add (mul $pct $r) 99) 100) $r -}}
{{- fail (printf "podDisruptionBudget.minAvailable %s with replicaCount %d rounds up to every pod and blocks every voluntary eviction, a node drain included; with one replica use maxUnavailable 1 or leave it off" .minAvailable $r) -}}
{{- end -}}
{{- else if ge (int .minAvailable) (int $.Values.replicaCount) -}}
{{- fail (printf "podDisruptionBudget.minAvailable %v with replicaCount %v blocks every voluntary eviction, a node drain included; with one replica use maxUnavailable 1 or leave it off" .minAvailable $.Values.replicaCount) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and .Values.networkPolicy.enabled (eq .Values.store.type "postgres") -}}
{{- if not .Values.networkPolicy.postgres.to -}}
{{- fail "networkPolicy.enabled with store.type postgres needs networkPolicy.postgres.to: the NetworkPolicyPeers (ipBlock, or namespaceSelector and podSelector) of the database. Without it Agent Kourier cannot reach Postgres, and the Slack rule excludes private ranges" -}}
{{- end -}}
{{- end -}}
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.kubeApiServer.cidrs) -}}
{{- fail "networkPolicy.enabled needs networkPolicy.kubeApiServer.cidrs: Agent Kourier reads Secrets through the API server, and a NetworkPolicy cannot select it by pod. On EKS use the control-plane subnet CIDRs (aws eks describe-cluster, then the subnets' CIDRs), not the endpoint IPs, which change on upgrades; elsewhere: kubectl get endpoints kubernetes" -}}
{{- end -}}
{{- end -}}

{{/*
The namespaces holding Secrets that Agent Kourier's config refers to, as a JSON list. A reference names a Secret in the
namespace of the resource that holds it, so these are the namespaces of the ChatConnections, the Bindings with a
tokenSecretRef and the AgentBackends with a header from a Secret, and the release namespace with store.type postgres (the
DSN's Secret is there); rbac.secretNamespaces adds to them (it is how a config supplied through config.existingConfigMap
says where its Secrets are). With none of these, the release namespace.
*/}}
{{- define "agentkourier.secretNamespaces" -}}
{{- $found := dict -}}
{{- range $_, $r := .Values.config.chatConnections -}}
{{- $_ := set $found (default $.Release.Namespace $r.namespace) true -}}
{{- end -}}
{{- range $_, $r := .Values.config.bindings -}}
{{- if or (dig "identity" "tokenSecretRef" "" (default (dict) $r.spec)) (dig "alerts" "tokenSecretRef" "" (default (dict) $r.spec)) -}}
{{- $_ := set $found (default $.Release.Namespace $r.namespace) true -}}
{{- end -}}
{{- end -}}
{{- range $_, $r := .Values.config.agentBackends -}}
{{- range $h := (default (list) (dig "headers" (list) (default (dict) $r.spec))) -}}
{{- if $h.valueSecretRef -}}
{{- $_ := set $found (default $.Release.Namespace $r.namespace) true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range $ns := .Values.rbac.secretNamespaces -}}
{{- $_ := set $found $ns true -}}
{{- end -}}
{{- if eq .Values.store.type "postgres" -}}
{{- $_ := set $found $.Release.Namespace true -}}
{{- end -}}
{{- if not $found -}}
{{- $_ := set $found $.Release.Namespace true -}}
{{- end -}}
{{- keys $found | sortAlpha | toJson -}}
{{- end -}}

{{/*
With config.source crds, the Secrets that the chart can name, by namespace, as a JSON object of sorted lists: the ChatConnections'
credentials, the Bindings' identity tokens, the AgentBackends' header values, the Postgres DSN's (in the release namespace) and
rbac.secretNames. The Roles of the Secrets name exactly these (resourceNames). It parallels agentkourier.secretNamespaces, which files
mode keeps as it is: test/chart holds the two to the same references.
*/}}
{{- define "agentkourier.secretNames" -}}
{{- $found := dict -}}
{{- range $_, $r := .Values.config.chatConnections -}}
{{- with dig "credentialsSecretRef" "name" "" (default (dict) $r.spec) -}}
{{- include "agentkourier.addSecret" (dict "found" $found "namespace" (default $.Release.Namespace $r.namespace) "name" .) -}}
{{- end -}}
{{- end -}}
{{- range $_, $r := .Values.config.bindings -}}
{{- with dig "identity" "tokenSecretRef" "name" "" (default (dict) $r.spec) -}}
{{- include "agentkourier.addSecret" (dict "found" $found "namespace" (default $.Release.Namespace $r.namespace) "name" .) -}}
{{- end -}}
{{- end -}}
{{- range $_, $r := .Values.config.agentBackends -}}
{{- range $h := (default (list) (dig "headers" (list) (default (dict) $r.spec))) -}}
{{- with dig "valueSecretRef" "name" "" $h -}}
{{- include "agentkourier.addSecret" (dict "found" $found "namespace" (default $.Release.Namespace $r.namespace) "name" .) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if eq .Values.store.type "postgres" -}}
{{- include "agentkourier.addSecret" (dict "found" $found "namespace" $.Release.Namespace "name" .Values.store.postgres.dsnSecret.name) -}}
{{- end -}}
{{- range $ns, $names := .Values.rbac.secretNames -}}
{{- range $name := $names -}}
{{- include "agentkourier.addSecret" (dict "found" $found "namespace" $ns "name" $name) -}}
{{- end -}}
{{- end -}}
{{- toJson $found -}}
{{- end -}}

{{/* Adds a Secret name to the list of its namespace in a dict (found), keeping each list sorted and without repeats. Prints nothing. */}}
{{- define "agentkourier.addSecret" -}}
{{- $names := default (list) (get .found .namespace) -}}
{{- $_ := set .found .namespace (append $names .name | uniq | sortAlpha) -}}
{{- end -}}

{{/*
One Agent Kourier resource, as a file of the config directory or, with labels, as a custom resource. Called with the kind, the name, the
entry, the root and optionally labels. The files do not carry the labels: they stay as they always were.
*/}}
{{- define "agentkourier.resource" -}}
apiVersion: agentkourier.dev/v1alpha1
kind: {{ .kind }}
metadata:
  name: {{ .name }}
  namespace: {{ default .root.Release.Namespace .entry.namespace }}
  {{- if .labels }}
  labels:
    {{- include "agentkourier.labels" .root | nindent 4 }}
  {{- end }}
spec:
  {{- required (printf "config entry %s %q needs a spec" .kind .name) .entry.spec | toYaml | nindent 2 }}
{{- end -}}

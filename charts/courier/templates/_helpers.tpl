{{/*
The name every resource of the release carries. The app.kubernetes.io/name label is always "courier", whatever
fullnameOverride says: NetworkPolicies elsewhere (the front door's, in the kind tiers) trust pods by that label.
*/}}
{{- define "courier.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains "courier" .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-courier" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* The labels that select courier's pods. They are immutable on a Deployment: do not add to them. */}}
{{- define "courier.selectorLabels" -}}
app.kubernetes.io/name: courier
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "courier.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "courier.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "courier.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "courier.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The courier image: a digest wins over a tag, and the tag defaults to the chart's appVersion. */}}
{{- define "courier.image" -}}
{{- $repository := required "image.repository is required (a released chart has it; from source, set it)" .Values.image.repository -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/* The ConfigMap courier reads its resources from. */}}
{{- define "courier.configMapName" -}}
{{- default (printf "%s-config" (include "courier.fullname" .)) .Values.config.existingConfigMap -}}
{{- end -}}

{{- define "courier.pvcName" -}}
{{- default (printf "%s-data" (include "courier.fullname" .)) .Values.persistence.existingClaim -}}
{{- end -}}

{{/*
Checks that no values file can get past. Included by the Deployment, which every install has.
*/}}
{{- define "courier.validate" -}}
{{- if gt (int .Values.replicaCount) 1 -}}
{{- fail "replicaCount > 1 is not supported: courier keeps its state in SQLite on a ReadWriteOnce volume, which one pod may use, and its session state is in memory. Two replicas wait for Postgres (H4-3) and leader election with fencing (H4-5). replicaCount 0 is the kill switch." -}}
{{- end -}}
{{- if and .Values.config.existingConfigMap (or .Values.config.agentBackends .Values.config.chatConnections .Values.config.bindings) -}}
{{- fail "config.existingConfigMap and config.agentBackends/chatConnections/bindings are two ways to supply the same files: set one" -}}
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
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.kubeApiServer.cidrs) -}}
{{- fail "networkPolicy.enabled needs networkPolicy.kubeApiServer.cidrs: courier reads Secrets through the API server, and a NetworkPolicy cannot select it by pod. On EKS use the control-plane subnet CIDRs (aws eks describe-cluster, then the subnets' CIDRs), not the endpoint IPs, which change on upgrades; elsewhere: kubectl get endpoints kubernetes" -}}
{{- end -}}
{{- end -}}

{{/*
The namespaces holding Secrets that courier's config refers to, as a JSON list. A reference names a Secret in the
namespace of the resource that holds it, so these are the namespaces of the ChatConnections, the Bindings with a
tokenSecretRef and the AgentBackends with a header from a Secret; rbac.secretNamespaces adds to them (it is how a
config supplied through config.existingConfigMap says where its Secrets are). With neither, the release namespace.
*/}}
{{- define "courier.secretNamespaces" -}}
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
{{- if not $found -}}
{{- $_ := set $found $.Release.Namespace true -}}
{{- end -}}
{{- keys $found | sortAlpha | toJson -}}
{{- end -}}

{{/* One courier resource as a file of the config directory. Called with the kind, the name, the entry and the root. */}}
{{- define "courier.resource" -}}
apiVersion: agentcourier.dev/v1alpha1
kind: {{ .kind }}
metadata:
  name: {{ .name }}
  namespace: {{ default .root.Release.Namespace .entry.namespace }}
spec:
  {{- required (printf "config entry %s %q needs a spec" .kind .name) .entry.spec | toYaml | nindent 2 }}
{{- end -}}

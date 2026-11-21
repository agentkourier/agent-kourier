{{/*
The Claude Managed Agents add-on (addons.cma). Its objects are named <fullname>-cma and labelled app.kubernetes.io/name=agent-kourier-cma:
never agent-kourier, which Agent Kourier's own selectors (Deployment, Services, ServiceMonitor, NetworkPolicy, PodDisruptionBudget) and
NetworkPolicies elsewhere match, so neither side selects the other's pods or Services.
*/}}

{{/* True (non-empty) when the add-on is enabled. addons, or addons.cma, may be absent: a release upgraded with --reuse-values from a chart
     before the block hands the chart no such key, and that reads as off. */}}
{{- define "agentkourier.cma.enabled" -}}
{{- if ((.Values.addons).cma).enabled -}}true{{- end -}}
{{- end -}}

{{- define "agentkourier.cma.fullname" -}}
{{- printf "%s-cma" (include "agentkourier.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The labels that select the add-on's pods. They are immutable on a Deployment: do not add to them. */}}
{{- define "agentkourier.cma.selectorLabels" -}}
app.kubernetes.io/name: agent-kourier-cma
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "agentkourier.cma.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "agentkourier.cma.selectorLabels" . }}
app.kubernetes.io/component: cma-addon
app.kubernetes.io/part-of: agent-kourier
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* The one port of the add-on: A2A, the agent card, the health paths and /metrics. */}}
{{- define "agentkourier.cma.port" -}}8090{{- end -}}

{{/* The URL Agent Kourier reaches the add-on at, through its Service, and the endpoint its agent card names (PUBLIC_URL). <service>.<namespace>.svc
     resolves through the pod's DNS search path, whatever the cluster's domain is. */}}
{{- define "agentkourier.cma.publicURL" -}}
{{- printf "http://%s.%s.svc:%s/a2a" (include "agentkourier.cma.fullname" .) .Release.Namespace (include "agentkourier.cma.port" .) -}}
{{- end -}}

{{/* The add-on's image: a digest wins over a tag, and the tag defaults to the chart's appVersion, as Agent Kourier's does. */}}
{{- define "agentkourier.cma.image" -}}
{{- with .Values.addons.cma.image -}}
{{- $repository := required "addons.cma.image.repository is required" .repository -}}
{{- if .digest -}}
{{- printf "%s@%s" $repository .digest -}}
{{- else -}}
{{- printf "%s:%s" $repository (default $.Chart.AppVersion .tag) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* True (non-empty) when the add-on keeps its state in SQLite on a volume. */}}
{{- define "agentkourier.cma.sqlite" -}}
{{- if ne .Values.addons.cma.store.type "postgres" -}}true{{- end -}}
{{- end -}}

{{- define "agentkourier.cma.pvcName" -}}
{{- default (printf "%s-data" (include "agentkourier.cma.fullname" .)) .Values.addons.cma.persistence.existingClaim -}}
{{- end -}}

{{/* Checks that no values file can get past, for an enabled add-on. Included by its Deployment. */}}
{{- define "agentkourier.cma.validate" -}}
{{- $cma := .Values.addons.cma -}}
{{- if not $cma.agentId -}}
{{- fail "addons.cma.enabled needs addons.cma.agentId: the ID of the CMA agent the add-on serves (CMA_AGENT_ID)" -}}
{{- end -}}
{{- if not $cma.environmentId -}}
{{- fail "addons.cma.enabled needs addons.cma.environmentId: the ID of the CMA environment its sessions run in (CMA_ENVIRONMENT_ID)" -}}
{{- end -}}
{{- if not (and $cma.anthropicApiKeySecret.name $cma.anthropicApiKeySecret.key) -}}
{{- fail "addons.cma.enabled needs addons.cma.anthropicApiKeySecret.name and addons.cma.anthropicApiKeySecret.key: the Secret, in the release namespace, that holds the Anthropic API key" -}}
{{- end -}}
{{- if not (and $cma.auth.tokenSecret.name $cma.auth.tokenSecret.key) -}}
{{- fail "addons.cma.enabled needs addons.cma.auth.tokenSecret.name and addons.cma.auth.tokenSecret.key: the Secret, in the release namespace, of the tokens the add-on accepts. The add-on listens on every interface of its pod, where it refuses to start without a caller check, and the chart has no setting that turns the check off" -}}
{{- end -}}
{{- if eq $cma.store.type "postgres" -}}
{{- if not (and $cma.store.postgres.dsnSecret.name $cma.store.postgres.dsnSecret.key) -}}
{{- fail "addons.cma.store.type postgres needs addons.cma.store.postgres.dsnSecret.name and addons.cma.store.postgres.dsnSecret.key: the Secret, in the release namespace, that holds the DSN" -}}
{{- end -}}
{{- if $cma.persistence.existingClaim -}}
{{- fail "addons.cma.persistence.existingClaim is for addons.cma.store.type sqlite: with postgres no volume is mounted" -}}
{{- end -}}
{{- if and $.Values.networkPolicy.enabled (not $cma.networkPolicy.postgres.to) -}}
{{- fail "networkPolicy.enabled with addons.cma.store.type postgres needs addons.cma.networkPolicy.postgres.to: the NetworkPolicyPeers (ipBlock, or namespaceSelector and podSelector) of the add-on's database. Without it the add-on cannot reach Postgres, and the Anthropic rule excludes private ranges" -}}
{{- end -}}
{{- else -}}
{{- if gt (int $cma.replicaCount) 1 -}}
{{- fail "addons.cma.replicaCount > 1 needs addons.cma.store.type postgres: SQLite is one file on a ReadWriteOnce volume, which one pod may use. With Postgres the replicas elect a leader and the standbys forward to it. addons.cma.replicaCount 0 stops the add-on." -}}
{{- end -}}
{{- if or $cma.store.postgres.dsnSecret.name $cma.store.postgres.dsnSecret.key -}}
{{- fail "addons.cma.store.postgres.dsnSecret is for addons.cma.store.type postgres: with sqlite it would be ignored, and the state would be kept on the volume" -}}
{{- end -}}
{{- if or $cma.leaderElection.leaseDuration $cma.leaderElection.renewDeadline $cma.leaderElection.retryPeriod -}}
{{- fail "addons.cma.leaderElection is for addons.cma.store.type postgres: with sqlite there is one replica and no election, and the add-on refuses the settings" -}}
{{- end -}}
{{- end -}}
{{- end -}}

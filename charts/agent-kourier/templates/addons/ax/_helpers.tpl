{{/*
The AX add-on (addons.ax), an ADK agent on Amazon Bedrock that serves A2A. Its objects are named <fullname>-ax and labelled
app.kubernetes.io/name=agent-kourier-ax: never agent-kourier, which Agent Kourier's own selectors (Deployment, Services, ServiceMonitor,
NetworkPolicy, PodDisruptionBudget) and NetworkPolicies elsewhere match, so neither side selects the other's pods or Services.
*/}}

{{/* True (non-empty) when the add-on is enabled. addons, or addons.ax, may be absent: a release upgraded with --reuse-values from a chart
     before the block hands the chart no such key, and that reads as off. */}}
{{- define "agentkourier.ax.enabled" -}}
{{- if ((.Values.addons).ax).enabled -}}true{{- end -}}
{{- end -}}

{{- define "agentkourier.ax.fullname" -}}
{{- printf "%s-ax" (include "agentkourier.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The labels that select the add-on's pods. They are immutable on a Deployment: do not add to them. */}}
{{- define "agentkourier.ax.selectorLabels" -}}
app.kubernetes.io/name: agent-kourier-ax
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "agentkourier.ax.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "agentkourier.ax.selectorLabels" . }}
app.kubernetes.io/component: ax-addon
app.kubernetes.io/part-of: agent-kourier
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* The one port of the add-on: A2A, the agent card, the health paths and /metrics. */}}
{{- define "agentkourier.ax.port" -}}8090{{- end -}}

{{/* The URL Agent Kourier reaches the add-on at, through its Service, and the endpoint its agent card names (PUBLIC_URL). <service>.<namespace>.svc
     resolves through the pod's DNS search path, whatever the cluster's domain is. */}}
{{- define "agentkourier.ax.publicURL" -}}
{{- printf "http://%s.%s.svc:%s/a2a" (include "agentkourier.ax.fullname" .) .Release.Namespace (include "agentkourier.ax.port" .) -}}
{{- end -}}

{{/* The add-on's image: a digest wins over a tag, and the tag defaults to the chart's appVersion, as Agent Kourier's does. */}}
{{- define "agentkourier.ax.image" -}}
{{- with .Values.addons.ax.image -}}
{{- $repository := required "addons.ax.image.repository is required" .repository -}}
{{- if .digest -}}
{{- printf "%s@%s" $repository .digest -}}
{{- else -}}
{{- printf "%s:%s" $repository (default $.Chart.AppVersion .tag) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* True (non-empty) when the add-on keeps its state in SQLite on a volume. */}}
{{- define "agentkourier.ax.sqlite" -}}
{{- if ne .Values.addons.ax.store.type "postgres" -}}true{{- end -}}
{{- end -}}

{{- define "agentkourier.ax.pvcName" -}}
{{- default (printf "%s-data" (include "agentkourier.ax.fullname" .)) .Values.addons.ax.persistence.existingClaim -}}
{{- end -}}

{{/* Checks that no values file can get past, for an enabled add-on. Included by its Deployment. */}}
{{- define "agentkourier.ax.validate" -}}
{{- $ax := .Values.addons.ax -}}
{{- if not $ax.model.region -}}
{{- fail "addons.ax.enabled needs addons.ax.model.region: the AWS region the add-on calls Amazon Bedrock in (MODEL_REGION)" -}}
{{- end -}}
{{- if not $ax.model.name -}}
{{- fail "addons.ax.enabled needs addons.ax.model.name: the Bedrock model ID or inference profile the agent runs on (MODEL_NAME)" -}}
{{- end -}}
{{- if ne (empty $ax.bedrock.apiKeySecret.name) (empty $ax.bedrock.apiKeySecret.key) -}}
{{- fail "addons.ax.bedrock.apiKeySecret needs both name and key: the Secret, in the release namespace, and its key, that hold the Bedrock API key. Leave both empty to take AWS credentials from the SDK's default chain (addons.ax.serviceAccount.annotations for IRSA, or EKS Pod Identity)" -}}
{{- end -}}
{{- if not (and $ax.auth.tokenSecret.name $ax.auth.tokenSecret.key) -}}
{{- fail "addons.ax.enabled needs addons.ax.auth.tokenSecret.name and addons.ax.auth.tokenSecret.key: the Secret, in the release namespace, of the tokens the add-on accepts. The add-on listens on every interface of its pod, where it refuses to start without a caller check, and the chart has no setting that turns the check off" -}}
{{- end -}}
{{- if eq $ax.store.type "postgres" -}}
{{- if not (and $ax.store.postgres.dsnSecret.name $ax.store.postgres.dsnSecret.key) -}}
{{- fail "addons.ax.store.type postgres needs addons.ax.store.postgres.dsnSecret.name and addons.ax.store.postgres.dsnSecret.key: the Secret, in the release namespace, that holds the DSN" -}}
{{- end -}}
{{- if $ax.persistence.existingClaim -}}
{{- fail "addons.ax.persistence.existingClaim is for addons.ax.store.type sqlite: with postgres no volume is mounted" -}}
{{- end -}}
{{- if and $.Values.networkPolicy.enabled (not $ax.networkPolicy.postgres.to) -}}
{{- fail "networkPolicy.enabled with addons.ax.store.type postgres needs addons.ax.networkPolicy.postgres.to: the NetworkPolicyPeers (ipBlock, or namespaceSelector and podSelector) of the add-on's database. Without it the add-on cannot reach Postgres, and the Bedrock rule excludes private ranges" -}}
{{- end -}}
{{- else -}}
{{- if gt (int $ax.replicaCount) 1 -}}
{{- fail "addons.ax.replicaCount > 1 needs addons.ax.store.type postgres: SQLite is one file on a ReadWriteOnce volume, which one pod may use. With Postgres the replicas elect a leader and the standbys forward to it. addons.ax.replicaCount 0 stops the add-on." -}}
{{- end -}}
{{- if or $ax.store.postgres.dsnSecret.name $ax.store.postgres.dsnSecret.key -}}
{{- fail "addons.ax.store.postgres.dsnSecret is for addons.ax.store.type postgres: with sqlite it would be ignored, and the state would be kept on the volume" -}}
{{- end -}}
{{- if or $ax.leaderElection.leaseDuration $ax.leaderElection.renewDeadline $ax.leaderElection.retryPeriod -}}
{{- fail "addons.ax.leaderElection is for addons.ax.store.type postgres: with sqlite there is one replica and no election, and the add-on refuses the settings" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The add-ons (addons.<name>): what every add-on renders, from one set of templates. An add-on's objects are named <fullname>-<name> and
labelled app.kubernetes.io/name=agent-kourier-<name>: never agent-kourier, which Agent Kourier's own selectors (Deployment, Services,
ServiceMonitor, NetworkPolicy, PodDisruptionBudget) and NetworkPolicies elsewhere match, so neither side selects the other's pods or Services.

Each template here takes (dict "ctx" <the chart's root context> "name" <the add-on's name>). An add-on has:
  - its name in agentkourier.addon.names, below;
  - its values block, addons.<name>, with the keys every add-on has (TestAddOnValuesShareTheCommonKeys in test/chart holds them
    alike) and its own;
  - templates/addons/<name>/env.tpl, which defines what is its own, each taking the same dict:
      agentkourier.addon.<name>.spec         YAML: description, the lines of the comment above its Deployment; egress, the rule of
                                             egress to its backend on 443 (key, under addons.<name>.networkPolicy; comment, above the
                                             rule; name, in a refusal); optionally serviceAccount.comment, a line above its
                                             ServiceAccount. The templates read it through agentkourier.addon.spec, which checks it.
      agentkourier.addon.<name>.validate     the checks of its own keys, which run before the common ones
      agentkourier.addon.<name>.env          its own container environment (its backend and the backend's credentials), first
      agentkourier.addon.<name>.settingsEnv  its own settings' environment, after SESSION_IDLE_TTL (may render nothing)
  - templates/addons/<name>/*.yaml, one line each that includes the template of its kind here; deployment.yaml also defines
    agentkourier.addon.<name>.startupProbe, the startup probe, which bounds the add-on's start-up work, and a comment that says what that
    work is. The probe is there, not here, because the add-on's limits test reads it from that file.
*/}}

{{/* The add-ons the chart knows, in the order in which Agent Kourier's NetworkPolicy lists its rules to them. */}}
{{- define "agentkourier.addon.names" -}}cma ax{{- end -}}

{{/* The add-on's spec (agentkourier.addon.<name>.spec), as YAML, for fromYaml. Fails the render when the define is not YAML or lacks a
     field the templates read: a mistake in env.tpl, which no values file can make. */}}
{{- define "agentkourier.addon.spec" -}}
{{- $spec := include (printf "agentkourier.addon.%s.spec" .name) . | fromYaml -}}
{{- $egress := $spec.egress | default dict -}}
{{- if or (hasKey $spec "Error") (not $spec.description) (not (kindIs "map" $egress)) (not $egress.key) (not $egress.comment) (not $egress.name) -}}
{{- fail (printf "templates/addons/%[1]s/env.tpl: agentkourier.addon.%[1]s.spec must be YAML with description, egress.key, egress.comment and egress.name (it is %q)" .name (include (printf "agentkourier.addon.%s.spec" .name) .)) -}}
{{- end -}}
{{- toYaml $spec -}}
{{- end -}}

{{/* True (non-empty) when the add-on is enabled. addons, or addons.<name>, may be absent: a release upgraded with --reuse-values from a
     chart before the block hands the chart no such key, and that reads as off. */}}
{{- define "agentkourier.addon.enabled" -}}
{{- if (index (.ctx.Values.addons | default dict) .name).enabled -}}true{{- end -}}
{{- end -}}

{{- define "agentkourier.addon.fullname" -}}
{{- printf "%s-%s" (include "agentkourier.fullname" .ctx) .name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The labels that select the add-on's pods. They are immutable on a Deployment: do not add to them. */}}
{{- define "agentkourier.addon.selectorLabels" -}}
app.kubernetes.io/name: agent-kourier-{{ .name }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
{{- end -}}

{{- define "agentkourier.addon.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .ctx.Chart.Name .ctx.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "agentkourier.addon.selectorLabels" . }}
app.kubernetes.io/component: {{ .name }}-addon
app.kubernetes.io/part-of: agent-kourier
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
{{- end -}}

{{/* The one port of the add-on: A2A, the agent card, the health paths and /metrics. */}}
{{- define "agentkourier.addon.port" -}}8090{{- end -}}

{{/* The URL Agent Kourier reaches the add-on at, through its Service, and the endpoint its agent card names (PUBLIC_URL).
     <service>.<namespace>.svc resolves through the pod's DNS search path, whatever the cluster's domain is. */}}
{{- define "agentkourier.addon.publicURL" -}}
{{- printf "http://%s.%s.svc:%s/a2a" (include "agentkourier.addon.fullname" .) .ctx.Release.Namespace (include "agentkourier.addon.port" .) -}}
{{- end -}}

{{/* The add-on's image: a digest wins over a tag, and the tag defaults to the chart's appVersion, as Agent Kourier's does. */}}
{{- define "agentkourier.addon.image" -}}
{{- $ctx := .ctx -}}
{{- with (index $ctx.Values.addons .name).image -}}
{{- $repository := required (printf "addons.%s.image.repository is required" $.name) .repository -}}
{{- if .digest -}}
{{- printf "%s@%s" $repository .digest -}}
{{- else -}}
{{- printf "%s:%s" $repository (default $ctx.Chart.AppVersion .tag) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* True (non-empty) when the add-on keeps its state in SQLite on a volume. */}}
{{- define "agentkourier.addon.sqlite" -}}
{{- if ne (index .ctx.Values.addons .name).store.type "postgres" -}}true{{- end -}}
{{- end -}}

{{- define "agentkourier.addon.pvcName" -}}
{{- default (printf "%s-data" (include "agentkourier.addon.fullname" .)) (index .ctx.Values.addons .name).persistence.existingClaim -}}
{{- end -}}

{{/* Checks that no values file can get past, for an enabled add-on: its own first, then those of the keys every add-on has. Included by
     its Deployment. */}}
{{- define "agentkourier.addon.validate" -}}
{{- $a := index .ctx.Values.addons .name -}}
{{- $p := printf "addons.%s" .name -}}
{{- $spec := include "agentkourier.addon.spec" . | fromYaml -}}
{{- include (printf "agentkourier.addon.%s.validate" .name) . -}}
{{- if not (and $a.auth.tokenSecret.name $a.auth.tokenSecret.key) -}}
{{- fail (printf "%[1]s.enabled needs %[1]s.auth.tokenSecret.name and %[1]s.auth.tokenSecret.key: the Secret, in the release namespace, of the tokens the add-on accepts. The add-on listens on every interface of its pod, where it refuses to start without a caller check, and the chart has no setting that turns the check off" $p) -}}
{{- end -}}
{{- if eq $a.store.type "postgres" -}}
{{- if not (and $a.store.postgres.dsnSecret.name $a.store.postgres.dsnSecret.key) -}}
{{- fail (printf "%[1]s.store.type postgres needs %[1]s.store.postgres.dsnSecret.name and %[1]s.store.postgres.dsnSecret.key: the Secret, in the release namespace, that holds the DSN" $p) -}}
{{- end -}}
{{- if $a.persistence.existingClaim -}}
{{- fail (printf "%[1]s.persistence.existingClaim is for %[1]s.store.type sqlite: with postgres no volume is mounted" $p) -}}
{{- end -}}
{{- if and .ctx.Values.networkPolicy.enabled (not $a.networkPolicy.postgres.to) -}}
{{- fail (printf "networkPolicy.enabled with %[1]s.store.type postgres needs %[1]s.networkPolicy.postgres.to: the NetworkPolicyPeers (ipBlock, or namespaceSelector and podSelector) of the add-on's database. Without it the add-on cannot reach Postgres, and %[2]s excludes private ranges" $p $spec.egress.name) -}}
{{- end -}}
{{- else -}}
{{- if gt (int $a.replicaCount) 1 -}}
{{- fail (printf "%[1]s.replicaCount > 1 needs %[1]s.store.type postgres: SQLite is one file on a ReadWriteOnce volume, which one pod may use. With Postgres the replicas elect a leader and the standbys forward to it. %[1]s.replicaCount 0 stops the add-on." $p) -}}
{{- end -}}
{{- if or $a.store.postgres.dsnSecret.name $a.store.postgres.dsnSecret.key -}}
{{- fail (printf "%[1]s.store.postgres.dsnSecret is for %[1]s.store.type postgres: with sqlite it would be ignored, and the state would be kept on the volume" $p) -}}
{{- end -}}
{{- if or $a.leaderElection.leaseDuration $a.leaderElection.renewDeadline $a.leaderElection.retryPeriod -}}
{{- fail (printf "%[1]s.leaderElection is for %[1]s.store.type postgres: with sqlite there is one replica and no election, and the add-on refuses the settings" $p) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "agentkourier.addon.deployment" -}}
{{- if include "agentkourier.addon.enabled" . }}
{{- include "agentkourier.addon.validate" . }}
{{- $ctx := .ctx }}
{{- $name := .name }}
{{- $a := index $ctx.Values.addons $name }}
{{- $spec := include "agentkourier.addon.spec" . | fromYaml }}
{{- $port := include "agentkourier.addon.port" . | int }}
{{- range splitList "\n" $spec.description }}
# {{ . }}
{{- end }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "agentkourier.addon.fullname" . }}
  namespace: {{ $ctx.Release.Namespace }}
  labels:
    {{- include "agentkourier.addon.labels" . | nindent 4 }}
spec:
  replicas: {{ $a.replicaCount }}
  {{- if include "agentkourier.addon.sqlite" . }}
  # The SQLite volume is never mounted by two pods: the old one is gone before the new one starts.
  strategy: {type: Recreate}
  {{- else }}
  # With Postgres the new pod starts beside the old one, and the replicas' election says which serves: one pod is never taken away before
  # its replacement is Ready.
  strategy:
    type: RollingUpdate
    rollingUpdate: {maxSurge: 1, maxUnavailable: 0}
  {{- end }}
  selector:
    matchLabels:
      {{- include "agentkourier.addon.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "agentkourier.addon.selectorLabels" . | nindent 8 }}
        app.kubernetes.io/component: {{ $name }}-addon
        {{- with omit $a.podLabels "app.kubernetes.io/name" "app.kubernetes.io/instance" "app.kubernetes.io/component" }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with $a.podAnnotations }}
      annotations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
    spec:
      serviceAccountName: {{ include "agentkourier.addon.fullname" . }}
      # The add-on never calls the Kubernetes API.
      automountServiceAccountToken: false
      {{- with $ctx.Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        {{- toYaml $a.podSecurityContext | nindent 8 }}
      containers:
        - name: {{ $name }}
          image: {{ include "agentkourier.addon.image" . | quote }}
          imagePullPolicy: {{ $a.image.pullPolicy }}
          # The add-on takes no arguments: it is configured by this environment.
          env:
            {{- with include (printf "agentkourier.addon.%s.env" $name) . }}
            {{- . | nindent 12 }}
            {{- end }}
            - {name: LISTEN_ADDR, value: {{ printf ":%d" $port | quote }}}
            - {name: PUBLIC_URL, value: {{ include "agentkourier.addon.publicURL" . | quote }}}
            {{- /* A file, not a variable: the kubelet updates a mounted Secret in place, and the add-on reads the file again every few
                   seconds. */}}
            - {name: AUTH_TOKEN_FILE, value: {{ printf "/etc/agent-kourier-%s/auth/tokens" $name }}}
            {{- with $a.auth.header }}
            - {name: AUTH_HEADER, value: {{ . | quote }}}
            {{- end }}
            - {name: STORE, value: {{ $a.store.type | quote }}}
            {{- if include "agentkourier.addon.sqlite" . }}
            - {name: SQLITE_PATH, value: {{ printf "/data/%s.db" $name }}}
            {{- else }}
            - name: POSTGRES_DSN
              valueFrom: {secretKeyRef: {name: {{ $a.store.postgres.dsnSecret.name | quote }}, key: {{ $a.store.postgres.dsnSecret.key | quote }}}}
            {{- /* The election: the pod's name holds the lease, and its address is where the standbys forward to. POD_IP comes before
                   ADVERTISE_URL, which refers to it. */}}
            - name: POD_NAME
              valueFrom: {fieldRef: {fieldPath: metadata.name}}
            - name: POD_IP
              valueFrom: {fieldRef: {fieldPath: status.podIP}}
            - {name: ADVERTISE_URL, value: {{ printf "http://$(POD_IP):%d" $port | quote }}}
            {{- with $a.leaderElection.leaseDuration }}
            - {name: LEASE_DURATION, value: {{ . | quote }}}
            {{- end }}
            {{- with $a.leaderElection.renewDeadline }}
            - {name: LEASE_RENEW_DEADLINE, value: {{ . | quote }}}
            {{- end }}
            {{- with $a.leaderElection.retryPeriod }}
            - {name: LEASE_RETRY_PERIOD, value: {{ . | quote }}}
            {{- end }}
            {{- end }}
            {{- with $a.sessionIdleTTL }}
            - {name: SESSION_IDLE_TTL, value: {{ . | quote }}}
            {{- end }}
            {{- with include (printf "agentkourier.addon.%s.settingsEnv" $name) . }}
            {{- . | nindent 12 }}
            {{- end }}
            {{- with $a.taskRetention }}
            - {name: TASK_RETENTION, value: {{ . | quote }}}
            {{- end }}
            - {name: LOG_LEVEL, value: {{ $a.logLevel | quote }}}
            {{- /* Before extraEnv, which may set GOMEMLIMIT itself: the helper renders nothing then. It reads .Values.resources and
                   .Values.extraEnv, so it is handed the add-on's. */}}
            {{- with include "agentkourier.goMemLimit" (dict "Values" $a) }}
            - {name: GOMEMLIMIT, value: {{ . | quote }}}
            {{- end }}
            {{- with $a.extraEnv }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
          ports:
            - {name: http, containerPort: {{ $port }}}
          # /healthz says the process is alive: 200 once it listens. /readyz says it can serve: on a standby at once (it forwards to the
          # leader), so every replica is in the Service, and on a leader once its start-up work is done. The startup probe asks /readyz,
          # so it bounds that work, and the liveness and readiness probes begin only once it has passed.
          {{- include (printf "agentkourier.addon.%s.startupProbe" $name) . | nindent 10 }}
          livenessProbe: {httpGet: {path: /healthz, port: http}, periodSeconds: 10, failureThreshold: 3}
          readinessProbe: {httpGet: {path: /readyz, port: http}, periodSeconds: 5}
          resources:
            {{- toYaml $a.resources | nindent 12 }}
          securityContext:
            {{- toYaml $a.securityContext | nindent 12 }}
          volumeMounts:
            - {name: auth, mountPath: {{ printf "/etc/agent-kourier-%s/auth" $name }}, readOnly: true}
            {{- if include "agentkourier.addon.sqlite" . }}
            - {name: data, mountPath: /data}
            {{- end }}
            - {name: tmp, mountPath: /tmp}
      {{- with $a.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $a.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $a.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      volumes:
        # The accepted tokens, the Secret's one key as the file AUTH_TOKEN_FILE names. Mounted without subPath, so the kubelet's updates
        # reach the pod. Readable by the pod's group (fsGroup), not by others.
        - name: auth
          secret:
            secretName: {{ $a.auth.tokenSecret.name | quote }}
            items: [{key: {{ $a.auth.tokenSecret.key | quote }}, path: tokens}]
            defaultMode: 0440
        {{- if include "agentkourier.addon.sqlite" . }}
        - name: data
          persistentVolumeClaim: {claimName: {{ include "agentkourier.addon.pvcName" . }}}
        {{- end }}
        - name: tmp
          emptyDir: {}
{{- end }}
{{- end -}}

{{- define "agentkourier.addon.service" -}}
{{- if include "agentkourier.addon.enabled" . }}
# The add-on's one port, on every Ready replica: a standby forwards A2A to the leader. Agent Kourier's AgentBackend names
# http://<this Service>.<namespace>.svc:8090/a2a. Never published.
apiVersion: v1
kind: Service
metadata:
  name: {{ include "agentkourier.addon.fullname" . }}
  namespace: {{ .ctx.Release.Namespace }}
  labels:
    {{- include "agentkourier.addon.labels" . | nindent 4 }}
spec:
  type: ClusterIP
  selector:
    {{- include "agentkourier.addon.selectorLabels" . | nindent 4 }}
  ports:
    - {name: http, port: {{ include "agentkourier.addon.port" . | int }}, targetPort: http}
{{- end }}
{{- end -}}

{{/* addons.<name>.serviceAccount.annotations, where an add-on has that key (the values schema refuses it for the others), are added to
     the ServiceAccount; the add-on's spec says what they are for (serviceAccount.comment). */}}
{{- define "agentkourier.addon.serviceAccount" -}}
{{- if include "agentkourier.addon.enabled" . }}
{{- $a := index .ctx.Values.addons .name }}
{{- $spec := include "agentkourier.addon.spec" . | fromYaml }}
# The add-on's own ServiceAccount, with no API token mounted: the add-on never calls the Kubernetes API, and the chart grants it nothing.
{{- with ($spec.serviceAccount).comment }}
# {{ . }}
{{- end }}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "agentkourier.addon.fullname" . }}
  namespace: {{ .ctx.Release.Namespace }}
  labels:
    {{- include "agentkourier.addon.labels" . | nindent 4 }}
  {{- with ($a.serviceAccount).annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
automountServiceAccountToken: false
{{- end }}
{{- end -}}

{{- define "agentkourier.addon.podDisruptionBudget" -}}
{{- if and (include "agentkourier.addon.enabled" .) (gt (int (index .ctx.Values.addons .name).replicaCount) 1) }}
# Two replicas and more (Postgres only): a node drain takes one at a time, so a replica is left to serve.
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "agentkourier.addon.fullname" . }}
  namespace: {{ .ctx.Release.Namespace }}
  labels:
    {{- include "agentkourier.addon.labels" . | nindent 4 }}
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      {{- include "agentkourier.addon.selectorLabels" . | nindent 6 }}
{{- end }}
{{- end -}}

{{- define "agentkourier.addon.persistentVolumeClaim" -}}
{{- if include "agentkourier.addon.enabled" . }}
{{- $persistence := (index .ctx.Values.addons .name).persistence }}
{{- if and (include "agentkourier.addon.sqlite" .) (not $persistence.existingClaim) }}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{ include "agentkourier.addon.pvcName" . }}
  namespace: {{ .ctx.Release.Namespace }}
  labels:
    {{- include "agentkourier.addon.labels" . | nindent 4 }}
  {{- if $persistence.retainOnUninstall }}
  annotations:
    helm.sh/resource-policy: keep
  {{- end }}
spec:
  accessModes:
    {{- toYaml $persistence.accessModes | nindent 4 }}
  {{- with $persistence.storageClassName }}
  storageClassName: {{ if eq . "-" }}""{{ else }}{{ . | quote }}{{ end }}
  {{- end }}
  resources:
    requests:
      storage: {{ $persistence.size | quote }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "agentkourier.addon.networkPolicy" -}}
{{- if and (include "agentkourier.addon.enabled" .) .ctx.Values.networkPolicy.enabled }}
{{- $ctx := .ctx }}
{{- $a := index $ctx.Values.addons .name }}
{{- $spec := include "agentkourier.addon.spec" . | fromYaml }}
{{- $egress := index $a.networkPolicy $spec.egress.key }}
{{- $port := include "agentkourier.addon.port" . | int }}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "agentkourier.addon.fullname" . }}
  namespace: {{ $ctx.Release.Namespace }}
  labels:
    {{- include "agentkourier.addon.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "agentkourier.addon.selectorLabels" . | nindent 6 }}
  policyTypes: [Ingress, Egress]
  ingress:
    # A2A from Agent Kourier's pods, which reach the add-on through its Service.
    - from:
        - podSelector:
            matchLabels:
              {{- include "agentkourier.selectorLabels" $ctx | nindent 14 }}
      ports:
        - {port: {{ $port }}, protocol: TCP}
    {{- with $ctx.Values.networkPolicy.metricsFrom }}
    # The scrapers, to /metrics, which shares the port with A2A.
    - from:
        {{- toYaml . | nindent 8 }}
      ports:
        - {port: {{ $port }}, protocol: TCP}
    {{- end }}
    {{- if not (include "agentkourier.addon.sqlite" .) }}
    # The add-on's other replicas: a standby forwards A2A to the leader's pod address.
    - from:
        - podSelector:
            matchLabels:
              {{- include "agentkourier.addon.selectorLabels" . | nindent 14 }}
      ports:
        - {port: {{ $port }}, protocol: TCP}
    {{- end }}
  egress:
    # DNS.
    - to:
        - namespaceSelector:
            {{- toYaml $ctx.Values.networkPolicy.dns.namespaceSelector | nindent 12 }}
          podSelector:
            {{- toYaml $ctx.Values.networkPolicy.dns.podSelector | nindent 12 }}
      ports:
        - {port: 53, protocol: UDP}
        - {port: 53, protocol: TCP}
    {{- if $egress.cidrs }}
    # {{ $spec.egress.comment }}
    - to:
        {{- range $egress.cidrs }}
        - ipBlock:
            cidr: {{ . | quote }}
            {{- with $egress.except }}
            except:
              {{- range . }}
              - {{ . | quote }}
              {{- end }}
            {{- end }}
        {{- end }}
      ports:
        - {port: 443, protocol: TCP}
    {{- end }}
    {{- if not (include "agentkourier.addon.sqlite" .) }}
    # The leader, from a standby that forwards to it.
    - to:
        - podSelector:
            matchLabels:
              {{- include "agentkourier.addon.selectorLabels" . | nindent 14 }}
      ports:
        - {port: {{ $port }}, protocol: TCP}
    # The add-on's database, on its own rule.
    - to:
        {{- toYaml $a.networkPolicy.postgres.to | nindent 8 }}
      ports:
        - {port: {{ int $a.networkPolicy.postgres.port }}, protocol: TCP}
    {{- end }}
    {{- with $a.networkPolicy.extraEgress }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
{{- end }}
{{- end -}}

{{- define "agentkourier.addon.serviceMonitor" -}}
{{- if and (include "agentkourier.addon.enabled" .) .ctx.Values.serviceMonitor.enabled }}
{{- $ctx := .ctx }}
# The add-on's /metrics, with the settings of Agent Kourier's ServiceMonitor. It selects the add-on's Service alone: Agent Kourier's
# ServiceMonitor selects app.kubernetes.io/name=agent-kourier, which the add-on's Service does not carry.
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "agentkourier.addon.fullname" . }}
  namespace: {{ default $ctx.Release.Namespace $ctx.Values.serviceMonitor.namespace }}
  labels:
    {{- include "agentkourier.addon.labels" . | nindent 4 }}
    {{- with $ctx.Values.serviceMonitor.labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  namespaceSelector:
    matchNames: [{{ $ctx.Release.Namespace }}]
  selector:
    matchLabels:
      {{- include "agentkourier.addon.selectorLabels" . | nindent 6 }}
  endpoints:
    - port: http
      path: /metrics
      interval: {{ $ctx.Values.serviceMonitor.interval }}
      scrapeTimeout: {{ $ctx.Values.serviceMonitor.scrapeTimeout }}
{{- end }}
{{- end -}}

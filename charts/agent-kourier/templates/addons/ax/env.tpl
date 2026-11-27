{{- define "agentkourier.addon.ax.spec" -}}
description: |-
  The AX add-on launches AX Tasks and receives command results over HTTPS callbacks.
egress:
  key: server
  comment: The private AX control plane.
  name: the AX control plane
serviceAccount:
  comment: Optional annotations for the add-on's ServiceAccount.
{{- end -}}

{{- define "agentkourier.addon.ax.validate" -}}
{{- $ctx := .ctx -}}
{{- $ax := $ctx.Values.addons.ax -}}
{{- $_ := required "addons.ax.enabled needs addons.ax.server.address" $ax.server.address -}}
{{- $_ := required "addons.ax.enabled needs addons.ax.taskTemplate.configMap.name" $ax.taskTemplate.configMap.name -}}
{{- $_ := required "addons.ax.enabled needs addons.ax.taskTemplate.configMap.key" $ax.taskTemplate.configMap.key -}}
{{- $_ := required "addons.ax.enabled needs addons.ax.resultCallback.tokenSecret.name" $ax.resultCallback.tokenSecret.name -}}
{{- $_ := required "addons.ax.enabled needs addons.ax.resultCallback.tokenSecret.key" $ax.resultCallback.tokenSecret.key -}}
{{- if eq $ax.auth.tokenSecret.name $ax.resultCallback.tokenSecret.name -}}
{{- fail "addons.ax.resultCallback.tokenSecret must name a separate Secret from addons.ax.auth.tokenSecret" -}}
{{- end -}}
{{- if $ctx.Values.rbac.create -}}
{{- if has $ctx.Release.Namespace $ctx.Values.rbac.secretNamespaces -}}
{{- fail "the AX callback Secret needs name-scoped broker Secret grants in the release namespace; use rbac.secretNames" -}}
{{- end -}}
{{- $grants := include "agentkourier.secretNames" $ctx | fromJson -}}
{{- if has $ax.resultCallback.tokenSecret.name (default (list) (get $grants $ctx.Release.Namespace)) -}}
{{- fail "the AX callback Secret must be omitted from broker Secret references and rbac.secretNames" -}}
{{- end -}}
{{- end -}}
{{- if not (regexMatch "^https://[^/?#@]+/ax/results/v1$" $ax.resultCallback.url) -}}
{{- fail "addons.ax.resultCallback.url must be an HTTPS URL ending in /ax/results/v1" -}}
{{- end -}}
{{- if $ctx.Values.networkPolicy.enabled -}}
{{- if not $ax.networkPolicy.server.to -}}
{{- fail "networkPolicy.enabled needs addons.ax.networkPolicy.server.to when the AX add-on is enabled" -}}
{{- end -}}
{{- if not $ax.networkPolicy.callbackFrom -}}
{{- fail "networkPolicy.enabled needs addons.ax.networkPolicy.callbackFrom when the AX add-on is enabled" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "agentkourier.addon.ax.env" -}}
{{- $ax := .ctx.Values.addons.ax -}}
- {name: AX_ADDR, value: {{ $ax.server.address | quote }}}
- {name: AX_ATESPACE, value: {{ $ax.atespace | quote }}}
- {name: AX_LAUNCHER_ID, value: {{ default (include "agentkourier.addon.fullname" .) $ax.launcherId | quote }}}
- {name: AX_TASK_TEMPLATE, value: /etc/agent-kourier-ax/task/task.yaml}
- {name: AX_RESULT_URL, value: {{ $ax.resultCallback.url | quote }}}
- {name: AX_RESULT_TOKEN_FILE, value: /etc/agent-kourier-ax/result/token}
- {name: AX_PROMPT_KEY, value: {{ $ax.promptKey | quote }}}
- {name: AX_HISTORY, value: {{ $ax.history | quote }}}
- {name: AX_MAX_RUNNING, value: {{ $ax.maxRunning | quote }}}
- {name: AX_POLL_INTERVAL, value: {{ $ax.pollInterval | quote }}}
- {name: AX_TURN_TIMEOUT, value: {{ $ax.turnTimeout | quote }}}
- {name: AX_KEEP_FAILED, value: {{ $ax.keepFailed | quote }}}
{{- end -}}

{{- define "agentkourier.addon.ax.settingsEnv" -}}
{{- end -}}

{{/*
What is the Claude Managed Agents add-on's own (addons.cma): the CMA agent and environment it serves, and its Anthropic API key. Everything
else it renders is the add-ons' common templates, in templates/addons/_addon.tpl, which says what each define here is for.
*/}}

{{- define "agentkourier.addon.cma.spec" -}}
description: |-
  The Claude Managed Agents add-on: an A2A server in front of one CMA agent, which Agent Kourier reaches through the Service of the same name.
egress:
  key: anthropic
  comment: The Anthropic API, over TLS.
  name: the Anthropic rule
{{- end -}}

{{- define "agentkourier.addon.cma.validate" -}}
{{- $cma := .ctx.Values.addons.cma -}}
{{- if not $cma.agentId -}}
{{- fail "addons.cma.enabled needs addons.cma.agentId: the ID of the CMA agent the add-on serves (CMA_AGENT_ID)" -}}
{{- end -}}
{{- if not $cma.environmentId -}}
{{- fail "addons.cma.enabled needs addons.cma.environmentId: the ID of the CMA environment its sessions run in (CMA_ENVIRONMENT_ID)" -}}
{{- end -}}
{{- if not (and $cma.anthropicApiKeySecret.name $cma.anthropicApiKeySecret.key) -}}
{{- fail "addons.cma.enabled needs addons.cma.anthropicApiKeySecret.name and addons.cma.anthropicApiKeySecret.key: the Secret, in the release namespace, that holds the Anthropic API key" -}}
{{- end -}}
{{- end -}}

{{- define "agentkourier.addon.cma.env" -}}
{{- $cma := .ctx.Values.addons.cma -}}
- {name: CMA_AGENT_ID, value: {{ $cma.agentId | quote }}}
- {name: CMA_ENVIRONMENT_ID, value: {{ $cma.environmentId | quote }}}
- name: ANTHROPIC_API_KEY
  valueFrom: {secretKeyRef: {name: {{ $cma.anthropicApiKeySecret.name | quote }}, key: {{ $cma.anthropicApiKeySecret.key | quote }}}}
{{- end -}}

{{- define "agentkourier.addon.cma.settingsEnv" -}}
{{- with .ctx.Values.addons.cma.toolResultTimeout -}}
- {name: TOOL_RESULT_TIMEOUT, value: {{ . | quote }}}
{{- end -}}
{{- end -}}

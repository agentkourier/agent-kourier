{{/*
What is the AX add-on's own (addons.ax), an ADK agent on Amazon Bedrock or the Anthropic API that serves A2A: the model it runs on, and the
provider's credentials. Everything else it renders is the add-ons' common templates, in templates/addons/_addon.tpl, which says what each
define here is for.
*/}}

{{- define "agentkourier.addon.ax.spec" -}}
description: |-
  The AX add-on: an ADK agent on Amazon Bedrock or the Anthropic API that serves A2A, which Agent Kourier reaches through the Service of the
  same name. It runs standalone (RUNNER_URL unset): the AX route's runner is not in this pod.
egress:
  key: model
  comment: "The model's provider over TLS: Amazon Bedrock (and, with IRSA, AWS STS), or the Anthropic API."
  name: the rule for the model's provider
serviceAccount:
  comment: Its annotations (addons.ax.serviceAccount.annotations) are where IRSA's role goes, for the AWS SDK's default chain.
{{- end -}}

{{- define "agentkourier.addon.ax.validate" -}}
{{- $ax := .ctx.Values.addons.ax -}}
{{- if not $ax.model.name -}}
{{- fail "addons.ax.enabled needs addons.ax.model.name: the model the agent runs on (MODEL_NAME), a Bedrock model ID or inference profile with bedrock, an Anthropic model ID with anthropic" -}}
{{- end -}}
{{- if eq $ax.model.provider "anthropic" -}}
{{- if $ax.model.region -}}
{{- fail "addons.ax.model.region is for addons.ax.model.provider bedrock: the Anthropic API has no region, and the add-on refuses MODEL_REGION with anthropic" -}}
{{- end -}}
{{- if or $ax.bedrock.apiKeySecret.name $ax.bedrock.apiKeySecret.key -}}
{{- fail "addons.ax.bedrock.apiKeySecret is for addons.ax.model.provider bedrock: with anthropic the key is addons.ax.anthropicApiKeySecret" -}}
{{- end -}}
{{- if not (and $ax.anthropicApiKeySecret.name $ax.anthropicApiKeySecret.key) -}}
{{- fail "addons.ax.model.provider anthropic needs addons.ax.anthropicApiKeySecret.name and addons.ax.anthropicApiKeySecret.key: the Secret, in the release namespace, that holds the Anthropic API key" -}}
{{- end -}}
{{- else -}}
{{- if not $ax.model.region -}}
{{- fail "addons.ax.model.provider bedrock needs addons.ax.model.region: the AWS region the add-on calls Amazon Bedrock in (MODEL_REGION)" -}}
{{- end -}}
{{- if or $ax.anthropicApiKeySecret.name $ax.anthropicApiKeySecret.key -}}
{{- fail "addons.ax.anthropicApiKeySecret is for addons.ax.model.provider anthropic: with bedrock the key, if any, is addons.ax.bedrock.apiKeySecret" -}}
{{- end -}}
{{- if ne (empty $ax.bedrock.apiKeySecret.name) (empty $ax.bedrock.apiKeySecret.key) -}}
{{- fail "addons.ax.bedrock.apiKeySecret needs both name and key: the Secret, in the release namespace, and its key, that hold the Bedrock API key. Leave both empty to take AWS credentials from the SDK's default chain (addons.ax.serviceAccount.annotations for IRSA, or EKS Pod Identity)" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "agentkourier.addon.ax.env" -}}
{{- $ax := .ctx.Values.addons.ax -}}
{{- $anthropic := eq $ax.model.provider "anthropic" -}}
- {name: MODEL_PROVIDER, value: {{ $ax.model.provider | quote }}}
{{- if not $anthropic }}
- {name: MODEL_REGION, value: {{ $ax.model.region | quote }}}
{{- end }}
- {name: MODEL_NAME, value: {{ $ax.model.name | quote }}}
{{- if $anthropic }}
{{- /* The Anthropic SDK reads the key from this variable. */}}
- name: ANTHROPIC_API_KEY
  valueFrom: {secretKeyRef: {name: {{ $ax.anthropicApiKeySecret.name | quote }}, key: {{ $ax.anthropicApiKeySecret.key | quote }}}}
{{- else }}
{{- /* Without a Bedrock API key the AWS SDK's default chain finds the credentials: IRSA or EKS Pod Identity, through the ServiceAccount. */}}
{{- with $ax.bedrock.apiKeySecret }}
{{- if .name }}
- name: AWS_BEARER_TOKEN_BEDROCK
  valueFrom: {secretKeyRef: {name: {{ .name | quote }}, key: {{ .key | quote }}}}
{{- end }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "agentkourier.addon.ax.settingsEnv" -}}
{{- end -}}

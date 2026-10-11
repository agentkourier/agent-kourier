# Export metrics and traces

For an operator wiring Agent Kourier into monitoring: at the end, Prometheus scrapes Agent Kourier's metrics and an
OpenTelemetry collector receives its traces.

## Scrape the metrics

Agent Kourier serves Prometheus metrics at `/metrics` on its listen port (8080). With prometheus-operator, turn on the
chart's Service and ServiceMonitor:

```yaml
service:
  enabled: true
serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack # (1)!
networkPolicy:
  metricsFrom: # (2)!
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: monitoring}}
      podSelector: {matchLabels: {app.kubernetes.io/name: prometheus}}
```

1. The labels your Prometheus selects ServiceMonitors by.
2. Only with `networkPolicy.enabled`: who may scrape. Empty admits nobody.

Without prometheus-operator, scrape port 8080 of the pod, path `/metrics`.

To look by hand:

```sh
kubectl -n agent-kourier port-forward deploy/agent-kourier 8080:8080
curl -s localhost:8080/metrics | grep agentkourier_
```

## Alert on what matters

Starting points, from the [metrics reference](../reference/metrics.md):

```promql
# The chat connection is down
agentkourier_chat_connected == 0

# A Binding's token was refused
increase(agentkourier_credential_rejections_total[15m]) > 0

# The running config lags the files: the last reload failed
agentkourier_config_stale == 1

# Stalled inbound or outbound work
agentkourier_reply_queue_oldest_age_seconds > 300
agentkourier_outbox_oldest_age_seconds > 300

# Replies dropped undelivered after 30 days
increase(agentkourier_retention_removed_total{what="stranded_replies"}[1d]) > 0

# Final output failures
sum(rate(agentkourier_render_final_total{outcome=~"permanent_failure|retry_exhausted|deadline_exceeded"}[5m])) > 0

# p95 agent send time
histogram_quantile(0.95,
  sum by (le) (rate(agentkourier_operation_duration_seconds_bucket{operation="agentkourier.agent.send"}[5m])))
```

`/readyz` does not say whether Slack is connected, on purpose: a Slack outage must not take Agent Kourier out of
service or restart it. Watch `agentkourier_chat_connected` instead.

## Export traces

Set the OpenTelemetry environment through the chart's `extraEnv`. Take credentials from a Secret:

```yaml
extraEnv:
  - name: OTEL_SERVICE_NAME
    value: agent-kourier
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: https://otel-collector.observability.svc:4318
  - name: OTEL_EXPORTER_OTLP_HEADERS # only if the collector wants credentials
    valueFrom:
      secretKeyRef: {name: otlp-credentials, key: headers} # Authorization=Bearer%20<token>
  - name: OTEL_EXPORTER_OTLP_PROTOCOL
    value: http/protobuf
  - name: OTEL_TRACES_SAMPLER
    value: parentbased_traceidratio
  - name: OTEL_TRACES_SAMPLER_ARG
    value: "1"
  - name: AGENTKOURIER_ENVIRONMENT
    value: production
```

- The collector must accept OTLP over HTTP/protobuf. Agent Kourier deploys none.
- Credentials travel only over TLS: Agent Kourier refuses to start with headers on an `http://` endpoint, unless
  `AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS=true` for a collector on a link you trust, such as a sidecar on localhost.
- With `networkPolicy.enabled`, add the collector to `networkPolicy.extraEgress`.

Without an endpoint, tracing is off and metrics still work. Every setting is in
[Logs and traces](../reference/logs-and-traces.md#trace-settings).

## Read the logs

Agent Kourier logs JSON lines to stderr. Set the level with the chart's `logLevel`. Every turn logs
`session turn started` and `session turn ended` with its `outcome`, so `grep 'session turn'` reads a session at a
glance. What a line may and may not carry is in [Logs and traces](../reference/logs-and-traces.md#logs).

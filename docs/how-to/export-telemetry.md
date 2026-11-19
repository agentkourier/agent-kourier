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

# The running config may lag its source: the last reload failed, or the source cannot be reached
agentkourier_config_stale == 1

# The broker holds objects that were deleted: its ChatConnection is gone and it keeps the Bindings that went with it; a Binding whose backend went first is held and not served (the broker logs how many)
agentkourier_config_held_objects > 0

# A Binding cannot be served. One failed probe holds this until the next pass, so give the rule a `for: 10m`
agentkourier_binding_ready == 0

# With custom resources: a Binding, or an object it uses, was refused, so what runs is not what was written (or nothing runs for it).
# This includes the Bindings of a ChatConnection the broker does not serve (reason="ConnectionNotServed": past the 16, its namespace no longer
# admitted after the broker served it (off the list, a label removed, the selector changed, every namespace narrowed), the broker's own connection
# before AGENTKOURIER_CHAT_CONNECTION was changed to another or unset, with its namespace not admitted, or a second connection of one Slack or Microsoft Teams app),
# which only the leader counts; summing over the replicas reads it.
# The Bindings of a namespace that was never admitted get no status and no count
sum(agentkourier_config_binding_rejections) > 0
sum(agentkourier_config_invalid_objects) > 0

# With custom resources: the CRDs and the image come from different chart releases
agentkourier_crd_schema_state{state="mismatch"} == 1

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

### Alert on webhook deliveries

For a [webhook connection](receive-webhooks.md), the sender sees the answer and Agent Kourier does not post anything, so these
counts are where a failure shows. Each series of `agentkourier_webhook_requests_total` is at 0 from when the connection's run
starts, on the replica that leads; the rules sum over the replicas, so that a change of leader does not read as a fall to 0:

```promql
# Deliveries answered 503 by the connection: the sender sends them again on its own schedule, or gives up
sum by (connection) (increase(agentkourier_webhook_requests_total{outcome=~"not_running|withheld|timeout|cancelled"}[15m])) > 0

# Deliveries answered 503 or 413 by the chat listener, before the connection saw them
sum by (connection, outcome) (increase(agentkourier_chat_listener_requests_total{platform="webhook",outcome=~"no_route|busy|too_large"}[15m])) > 0

# Signed with the wrong secret, or outside the timestamp window. `unsigned` is left out: anyone who finds the URL can send one
sum by (connection, reason) (increase(agentkourier_webhook_requests_total{outcome="unverified",reason=~"bad_signature|stale"}[15m])) > 0

# The sender's payload no longer fits the mapping: a thread that is not an identifier, or not there at all
sum by (connection, reason) (increase(agentkourier_webhook_requests_total{outcome="unmapped",reason=~"bad_id|no_thread"}[1h])) > 0

# Events a limit stopped. On a webhook connection they were answered 200 and are lost unless delivered again; the WARN line names each delivery.
# The counter has no connection label: name the webhook Bindings, here the recipes'
sum by (binding, limit) (increase(agentkourier_trigger_limited_total{binding=~"acme-webhooks/.*"}[30m])) > 0

# A first prompt that failed its check, such as an id field with no valid value: no turn, and on a webhook connection no note either.
# No connection label here either: filter by binding
sum by (binding, reason) (increase(agentkourier_trigger_refused_total{binding=~"acme-webhooks/.*"}[30m])) > 0

# Events taken slowly: the 99th percentile is past half of the 2 second wait, after which a delivery is answered 503 (timeout)
histogram_quantile(0.99, sum by (le, connection) (rate(agentkourier_webhook_ack_duration_seconds_bucket{outcome="accepted"}[15m]))) > 1

# No event taken for a day: the webhook may be off at the sender, point elsewhere, or no longer fit the mapping
time() - max by (connection) (max_over_time(agentkourier_webhook_last_accepted_timestamp_seconds{connection="acme-webhooks/github"}[2d])) > 86400

# No event taken in the whole range, as for a connection that has taken none since it was set up: the rule above has no value then
absent_over_time(agentkourier_webhook_last_accepted_timestamp_seconds{connection="acme-webhooks/github"}[2d])
```

The last two rules are for a connection whose service sends events every day; name each such connection, and pick the threshold from
how often its service sends them. `agentkourier_webhook_last_accepted_timestamp_seconds` is the time of the connection's last request
answered `accepted`. Only the leader sets it, and a broker that has just started has no series of it until its first, so the rule
reads it with `max_over_time` over a range longer than the threshold: the value from before a restart or a change of leader still
counts. Use `max`, not `sum`: the value is a time. An event dropped as `self`, `ignored` or `unmapped` does not set it, as the
broker never took it; the unmapped rule above, and `agentkourier_webhook_requests_total` by `outcome`, say which it was. Each answer is
also a log line, `webhook: delivery answered`, with the delivery ID that the sender's own log shows
([Follow a delivery](receive-webhooks.md#follow-a-delivery)).

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

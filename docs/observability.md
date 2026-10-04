# Courier observability

Courier exports Prometheus metrics at `/metrics` and optionally exports traces with
OTLP over HTTP/protobuf. Metrics remain available when trace export is disabled.
Each broker owns its registry and tracer provider. The application does not install
a global OpenTelemetry provider.

## Enable traces

Set these environment variables on the courier Deployment:

```yaml
env:
  - name: OTEL_SERVICE_NAME
    value: agent-courier
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://otel-collector.observability.svc:4318
  - name: OTEL_EXPORTER_OTLP_PROTOCOL
    value: http/protobuf
  - name: OTEL_TRACES_SAMPLER
    value: parentbased_traceidratio
  - name: OTEL_TRACES_SAMPLER_ARG
    value: "1"
  - name: COURIER_ENVIRONMENT
    value: pilot
```

Allow courier to reach the collector under the deployment's egress policy.
The collector must accept OTLP HTTP/protobuf and forward traces to your trace backend.
No collector is deployed by courier.

The default service name is `agent-courier`. Traces include the binary version,
a random service instance ID for each provider lifetime, and the environment when set.

| Setting | Behavior |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Base URL; courier appends `/v1/traces` |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Exact trace URL; overrides the base URL |
| `OTEL_TRACES_EXPORTER` | `otlp`, or `none` to disable export |
| `OTEL_EXPORTER_OTLP_PROTOCOL` / `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | `http/protobuf`; other protocols are rejected |
| `OTEL_EXPORTER_OTLP_HEADERS` / `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | Comma-separated `key=value` headers; trace-specific settings override general settings |
| `OTEL_TRACES_SAMPLER` | `parentbased_traceidratio` |
| `OTEL_TRACES_SAMPLER_ARG` | Root trace sampling ratio in `[0,1]`, default `1`; children respect their parent's sampling flag |
| `OTEL_SERVICE_NAME` | Service resource name |
| `COURIER_ENVIRONMENT` | Optional `deployment.environment.name` |

Without an export endpoint, tracing is disabled. These are the environment settings
courier explicitly supports; this is not a general implementation of every OTel SDK
configuration variable. Put collector credentials in Secret-backed environment values.
Endpoints must not contain URL credentials, query parameters or fragments.

Export runs through a bounded batch queue. After broker shutdown drains its work,
courier allows three seconds for the tracer provider to flush. A collector outage
must not block intake or backend execution; queued spans may be lost when the exporter
cannot keep up or the process is killed. Collector error details are scrubbed from
SDK diagnostics. Metrics are not derived from sampled traces.

## Trace boundaries

A Socket Mode envelope starts a local trace, including durable intake and the ack
attempt. A queued reply restores its saved W3C ancestry when a session runner takes
it up. A2A HTTP calls carry `traceparent` and `tracestate` to the verifying backend
front door after the destination and credential checks. Slack receives no trace
headers or baggage.

SQLite stores trace context beside replies, active tasks, outbound rows,
interactions, button presses, and generic durable events. Duplicate inserts retain
the original carrier. Existing rows without trace context still work. Migration
`0012_trace_context.sql` adds columns with empty defaults.

Each turn attempt, unary A2A operation and delivery attempt has a finite span.
Store operations create child spans only when a valid parent trace context exists,
including restored durable context. Background store polling without a parent records
duration metrics without creating standalone traces. A recovered active task starts
a new execution trace linked to its saved task trace. A human answer follows its
press trace, with a link from the decision to the interaction that produced the
prompt. Courier does not hold a conversation span
open while waiting for human input.

Important span attributes include:

- `courier.binding`, `courier.backend`, `courier.agent.name`, `courier.dialect`
- `courier.turn.id`, `courier.task.id`, `courier.context.id`, `courier.task.state`
- `courier.retry.count`, `courier.recovered`, `courier.outcome`
- `courier.interaction.id`, `courier.interaction.kind`
- `courier.outbox.id`, `courier.outbox.role`, `courier.delivery.mode`, `courier.delivery.adopted`
- `courier.render.mode`, `courier.render.final`, `courier.render.streaming`, `courier.render.attempts`
- HTTP method, server address/port and response status

Task and turn IDs belong only to spans and logs. Metrics use configured Binding names
and closed operation/outcome sets. Span attributes and metric labels exclude prompts,
responses, tool arguments/results, user names, authentication headers, URL queries
and raw errors. Existing application log messages retain their existing fields.
Contextual structured logs include `trace_id` and `span_id`. Trace export adds no
LLM token or cost estimates; those belong to instrumentation in the backend that
actually calls the model.

## Metrics

Existing audit, credential, config, approval, interaction, chat connectivity and
terminal turn outcome metrics retain their names and hooks.

| Added metric | Definition |
|---|---|
| `courier_operation_duration_seconds` | Duration of a finite operation, including failures and retries |
| `courier_agent_operation_duration_seconds` | Send/answer/follow and unary A2A operation duration; execution ends at a terminal state, input-required or auth-required; both pause states are non-error outcomes |
| `courier_agent_first_text_duration_seconds` | Attempt start to first nonempty text observed on send/answer; no observations for follow snapshots |
| `http_client_request_duration_seconds` | HTTP request through response EOF or close; an SSE response includes its stream lifetime |
| `courier_store_operation_duration_seconds` | Domain store call, including database pool wait |
| `courier_intake_total` | Message/press dispositions; `source="reply_queue",disposition="duplicate"` counts durable duplicates separately |
| `courier_slack_ack_duration_seconds` | Envelope handling start to completion of the ack attempt, or return with ack withheld; measures queueing the ack, not confirmation from Slack |
| `courier_turn_queue_wait_seconds` | Enqueue to first execution attempt in this process; a restart may observe the same waiting reply again |
| `courier_sessions_active` / `courier_sessions_paused` | Current in-process runners and runners parked on input |
| `courier_stream_reconnects_total` | Completed reconnect attempts, by outcome |
| `courier_session_recovery_total` | Adopted tasks, recording failures and reconciliation diagnostic notices |
| `courier_reply_queue_depth` / `courier_reply_queue_oldest_age_seconds` | Durable undelivered reply count and oldest age |
| `courier_outbox_pending` / `courier_outbox_oldest_age_seconds` | Unposted, nonabandoned rows and oldest age; includes incomplete stream rows |
| `courier_outbox_delivery_duration_seconds` | Row creation to successful delivery/adoption attempt; replay after a crash can produce another observation |
| `courier_render_final_total` | Final rendering outcomes: delivered, empty output, permanent failure, retry exhaustion, deadline exceeded or cancelled |
| `courier_interactions_pending` | Durable pending interactions |
| `courier_interaction_wait_seconds` | Creation to first committed resolution, by kind/state; retries do not add another observation |
| `courier_chat_api_calls_total` | Slack adapter operations and results, including logical API errors over HTTP 200; cached user lookups are adapter operations too |
| `go_*`, `process_*`, `go_sql_*` | Runtime/process metrics and separate read/write pool statistics |

Duration histograms cover milliseconds through one hour. Use separate latency
queries for execution, human wait and visible delivery. The terminal turn outcome
counter describes the backend task; `courier_render_final_total` describes what
happened to the output in chat. A completed task can have a failed final render.

Backlog scrapes query SQLite with a one-second timeout. A failed query produces an
invalid metric and a failed scrape rather than reporting a false zero backlog.

Example PromQL queries:

```promql
# p95 agent send time
histogram_quantile(0.95,
  sum by (le) (rate(courier_agent_operation_duration_seconds_bucket{operation="send"}[5m])))

# final output failures
sum(rate(courier_render_final_total{outcome=~"permanent_failure|retry_exhausted|deadline_exceeded"}[5m]))

# stalled inbound or outbound work
courier_reply_queue_oldest_age_seconds > 300
courier_outbox_oldest_age_seconds > 300

# SQLite writer pool wait time per second
rate(go_sql_wait_duration_seconds_total{db_name="write"}[5m])
```

Alertmanager ingress/routing limits, alert verdicts and feedback metrics remain with
the Phase 2 adapters that have not been implemented. The generic event queue now
preserves trace context, so those adapters can connect their incoming HTTP traces
to queued execution without another persistence change.

Reference: [OpenTelemetry HTTP span conventions](https://opentelemetry.io/docs/specs/semconv/http/http-spans/) and [span links](https://opentelemetry.io/docs/specs/otel/trace/api/#links-between-spans).

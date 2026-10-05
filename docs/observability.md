# Courier observability

Courier exports Prometheus metrics at `/metrics` and optionally exports traces with
OTLP over HTTP/protobuf. Metrics remain available when trace export is disabled.
Each broker owns its registry and tracer provider. The application does not install
a global OpenTelemetry provider. It does install OpenTelemetry's diagnostic logger and
error handler, once at startup and only when trace export is configured, because the SDK and the exporter reach them only
through process-wide hooks; they log through courier's logger and carry no values
(see "Exporter diagnostics").

## Enable traces

Set these environment variables on the courier Deployment:

```yaml
env:
  - name: OTEL_SERVICE_NAME
    value: agent-courier
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: https://otel-collector.observability.svc:4318
  - name: OTEL_EXPORTER_OTLP_HEADERS  # only if the collector wants credentials
    valueFrom:
      secretKeyRef: {name: otlp-credentials, key: headers}  # Authorization=Bearer%20<token>
  - name: OTEL_EXPORTER_OTLP_PROTOCOL
    value: http/protobuf
  - name: OTEL_TRACES_SAMPLER
    value: parentbased_traceidratio
  - name: OTEL_TRACES_SAMPLER_ARG
    value: "1"
  - name: COURIER_ENVIRONMENT
    value: pilot
```

Credentials travel only over TLS. Courier refuses to start when headers are configured
for an `http://` endpoint, because they would cross the network in cleartext. Use
`https://`, or set `COURIER_OTLP_ALLOW_INSECURE_HEADERS=true` to accept it for a
collector you reach over a link you trust (a sidecar on localhost, say). An `http://`
endpoint with no headers needs no opt-in.

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
| `OTEL_EXPORTER_OTLP_HEADERS` / `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | Comma-separated `key=value` headers, values percent-decoded, names valid HTTP tokens; trace-specific settings override general settings. Both are checked even when the other wins, and a bad one stops startup |
| `COURIER_OTLP_ALLOW_INSECURE_HEADERS` | `true` accepts headers on an `http://` endpoint; default refuses them |
| `OTEL_TRACES_SAMPLER` | `parentbased_traceidratio` |
| `OTEL_TRACES_SAMPLER_ARG` | Root trace sampling ratio in `[0,1]`, default `1`; children respect their parent's sampling flag |
| `OTEL_SERVICE_NAME` | Service resource name |
| `COURIER_ENVIRONMENT` | Optional `deployment.environment.name` |

Without an export endpoint, tracing is disabled. These are the environment settings
courier explicitly supports; this is not a general implementation of every OTel SDK
configuration variable. Put collector credentials in Secret-backed environment values.
Endpoints must not contain URL credentials, query parameters or fragments. Both the
general and the trace-specific endpoint are checked, whichever one is used.

The OTLP exporter also reads some variables of its own from the environment, which
courier does not validate and passes through unchanged:
`OTEL_EXPORTER_OTLP_CERTIFICATE` (a CA bundle file for an `https://` collector),
`OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` with `OTEL_EXPORTER_OTLP_CLIENT_KEY` (mutual TLS),
`OTEL_EXPORTER_OTLP_COMPRESSION` (`gzip` or none) and `OTEL_EXPORTER_OTLP_TIMEOUT`
(milliseconds), each also with the `_TRACES_` infix. `OTEL_EXPORTER_OTLP_INSECURE` is
ignored: the endpoint's scheme alone decides whether TLS is used. A value among these
that the exporter cannot use is reported as a warning (below), without the value.

Span attribute values are cut to 256 bytes, so an agent-supplied task or context ID
cannot grow a span without limit.

Export runs through a bounded batch queue. After broker shutdown drains its work,
courier allows three seconds for the tracer provider to flush. A collector outage
must not block intake or backend execution; queued spans may be lost when the exporter
cannot keep up or the process is killed. Metrics are not derived from sampled traces.

## Exporter diagnostics

Collector error details never reach the SDK's diagnostics, and none of the SDK's own
diagnostics reach stderr outside courier's logger. Three things log, all as warnings:

- A failed export logs `trace export failed` with `error_type`, one of `refused`,
  `timeout`, `dns`, `tls`, `http_<status>` (for example `http_401`), `http_retryable`
  (the exporter retried a 429, 502, 503 or 504 until it gave up, and does not say
  which), `cancelled` or `error`. Each class logs at most once a minute, with the number
  withheld since in `suppressed`. The collector's response body, URL and credentials are
  never logged.
- An exporter complaint about its own configuration, such as an unusable timeout or
  certificate file, logs `otel: <what it was doing>`, with no value from the variable.
- Any other OpenTelemetry error logs `otel: OpenTelemetry error` with `error_type`, at
  most once a minute.

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
and raw errors; logs do carry error text, redacted and capped (see "Logs"). Existing
application log messages retain their existing fields.
Contextual structured logs include `trace_id` and `span_id`. Trace export adds no
LLM token or cost estimates; those belong to instrumentation in the backend that
actually calls the model.

## Logs

Courier logs JSON lines to stderr, at the level `COURIER_LOG_LEVEL` sets. A line carries IDs
(Binding, channel, thread, message, task, interaction, outbox row; `trace_id` and `span_id`
where there is a trace), counts, durations, closed words such as a `reason` or an outcome, and
error text. It never carries a message, an agent's output, status text or error message, a tool's
arguments, an answer a person gave, a token or a Secret's value.

Error text is the one place a line can quote someone, and courier keeps it to what its own code and
its dependencies say. An error the agent answered with is logged as its class only
(`agent error: INTERNAL_ERROR`): a backend such as kagent fills the JSON-RPC message with the exception
of a failed model call or tool, which can quote model output or a tool's input, so courier does not
log it. The agent's own text is in the agent's logs; find it there by the task or context ID of the
line. A Slack response body that slack-go could not parse, a handler's error or panic, and the like
may still quote someone, so courier's JSON handler, built in one place (`telemetry.NewJSONHandler`) and
also the default `slog` logger, passes every attribute that holds an error, and every attribute
named `err`, `error`, `cause`, `reason` or `panic`, through the redaction the audit log uses: tokens,
credentials, URL userinfo, key blocks and the values of secret-named fields become `[redacted]`.
It then cuts the text to 300 bytes (the audit log's cap for a returned error) and marks the cut
with `… (truncated)`. Where the content has no diagnostic value, the line does not carry it at all: a
refused answer logs which answer and why (`answer 2 is not one of the choices`), never the value.
The error courier exits with is printed the same way, as one `courier: ...` line on stderr.

Messages, level, time and attribute keys are not changed by this: an alert on a message or a
`jq` on `.error` keeps working. Text that is not an error attribute is logged as the code gave it,
which is why code logs IDs and classes there. Output written to the standard `log` package is not
redacted: it arrives as a line's `msg`. Nothing in courier writes to it but net/http's error log for
the health server, which courier routes there as a warning and which holds no content. The audit log is separate and
stricter: it stores typed error tags and identifiers, never an agent's error text; its one free-text reason is the
dialect's own, for an unusable pause, redacted and capped at 120 bytes.

## Metrics

Existing audit, credential, config, approval, interaction, chat connectivity and
terminal turn outcome metrics retain their names and hooks.

| Added metric | Definition |
|---|---|
| `courier_operation_duration_seconds` | Duration of a finite operation, including failures and retries, by `operation` (the span name, such as `courier.agent.send` or `courier.store.GetSession`) and `outcome`. Agent execution ends at a terminal state, input-required or auth-required; both pause states are non-error outcomes. Agent and store calls are no longer also recorded in `courier_agent_operation_duration_seconds` and `courier_store_operation_duration_seconds`, which duplicated this histogram: select them with `operation=~"courier.agent.*"` and `operation=~"courier.store.*"` |
| `courier_agent_first_text_duration_seconds` | Attempt start to first nonempty text observed on send/answer; no observations for follow snapshots |
| `http_client_request_duration_seconds` | HTTP request through response EOF or close; an SSE response includes its stream lifetime |
| `courier_intake_total` | Message/press dispositions; `source="reply_queue",disposition="duplicate"` counts durable duplicates separately; a bot's message is `trigger_matched`, `trigger_unmatched` (no trigger took it) or `ignored` (an unbound channel, or a reply in a thread) |
| `courier_slack_ack_duration_seconds` | Envelope handling start to completion of the ack attempt, or return with ack withheld; measures queueing the ack, not confirmation from Slack |
| `courier_turn_queue_wait_seconds` | Enqueue to first execution attempt in this process; a restart may observe the same waiting reply again |
| `courier_sessions_active` / `courier_sessions_paused` | Current in-process runners and runners parked on input |
| `courier_stream_reconnects_total` | Completed reconnect attempts, by outcome |
| `courier_session_recovery_total` | Adopted tasks, recording failures and reconciliation diagnostic notices |
| `courier_reply_queue_depth` / `courier_reply_queue_oldest_age_seconds` | Durable undelivered reply count and oldest age |
| `courier_outbox_pending` / `courier_outbox_oldest_age_seconds` | Unposted, nonabandoned rows and oldest age; includes incomplete stream rows |
| `courier_outbox_delivery_duration_seconds` | Row creation to successful delivery/adoption attempt; replay after a crash can produce another observation |
| `courier_render_final_total` | Final rendering outcomes: delivered, empty output, permanent failure, retry exhaustion, deadline exceeded or cancelled |
| `courier_retention_removed_total` | Stored content that retention removed, by `what`: `stranded_replies` (undelivered replies dropped after 30 days, a turn that was never sent: worth an alert on any increase), `cleared_interactions` (settled interactions whose request and answer were emptied) and `audit_entries` (purged past `COURIER_AUDIT_RETENTION`; an increase with the setting at `keep` is a fault). See [data.md](data.md) |
| `courier_interactions_pending` | Durable pending interactions |
| `courier_interaction_wait_seconds` | Creation to first committed resolution, by kind/state; retries do not add another observation |
| `courier_chat_api_calls_total` | Slack adapter operations and results, including logical API errors over HTTP 200; cached user lookups are adapter operations too. Outcomes include `rate_limited`, `auth_rejected`, `channel_gone`, `not_streaming`, `api_error`, and `mode_refused` (Slack refused the chunks mode of a stream, which makes courier fall back to `markdown_text` and show no more steps) |
| `courier_trigger_matched_total` / `courier_trigger_unmatched_total` | Bot messages a Binding's `botMessage` trigger matched, and bot messages in its channel that matched none (the heartbeat an `exclude` turns away counts here), by `binding`. A rising unmatched count after a template change means the trigger's regexes no longer fit; the debug log line names the channel, message and bot, never the text |
| `courier_trigger_decisions_total` | What a matched bot message did to its key's thread, by `binding` and `decision`: `started` (a new investigation), `repeat` (a note that it fired again), `closed` (a note that it resolved), `close_unknown` (a close with no open thread, ignored), `dedup` (a message delivered again) `empty_key` (a keyed trigger whose key came out empty, so the message is an investigation of its own) and `not_started` (the session hook said the turn will not start, such as a refused prompt: the event is acknowledged, no cooldown begins) and `limited` (a start that one of the trigger's limits stopped: no turn, no key held, nothing posted in the alert's thread; see `courier_trigger_limited_total`). Flapping shows as `repeat` and `closed` rising while `started` stays flat; the key is in the debug and info log lines only as a short hash |
| `courier_trigger_limited_total` | Starts that a limit stopped, so no investigation began, by `binding`, `trigger` (its index in the Binding's `chat.triggers`) and `limit`: `rate` (the trigger started its `rateLimit.maxRuns` in the last `rateLimit.per`), `concurrent` (`rateLimit.maxConcurrent` of its investigations are running) or `daily` (it started `maxRunsPerDay` in the last 24 hours); when more than one is reached the daily cap is reported, then the rate, then the concurrency. A repeat, a close and a note are never limited. The alert is not posted about: it reached people through its own receiver. Once for each window, which opens with the first stopped start and lasts the trigger's `rateLimit.per`, one line in the channel (top level, through the outbox) says how many were not investigated and why, for example "7 alert messages not investigated in the last 10m (rate limit).", and names no alert. A count that keeps rising means the limits are too tight for the storm, or that the investigations are slow (concurrent), or that one source is noisy (daily) |
| `courier_trigger_refused_total` | Starts that began no turn because the first prompt failed its check (spec R2) or could not be rendered, by `binding`, `trigger` (its index in the Binding's `chat.triggers`), `ref` (the value that failed, as a template names it without the dot: `Fields.alertname`, `Key`, `Link`, `State`, `Channel`, `Source`, or `template` for a template that failed to run) and `reason` (`characters`, `too_long`, `not_link`, `render_error`). `ref` and `reason` come from closed sets, and the entry names are the Binding's own. Each refusal is also a note in the alert's thread, "Not investigated: ...", and an audit entry that names the ref, reason and length of the value and never the value; a refusal opens no cooldown, so a fire of the key more than a minute later starts an investigation. A rising count after a template or Alertmanager receiver change means a value the template reads no longer fits its check |
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
  sum by (le) (rate(courier_operation_duration_seconds_bucket{operation="courier.agent.send"}[5m])))

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

# Agent Kourier observability

Agent Kourier exports Prometheus metrics at `/metrics` and optionally exports traces with
OTLP over HTTP/protobuf. Metrics remain available when trace export is disabled.
Each broker owns its registry and tracer provider. The application does not install
a global OpenTelemetry provider. It does install OpenTelemetry's diagnostic logger and
error handler, once at startup and only when trace export is configured, because the SDK and the exporter reach them only
through process-wide hooks; they log through Agent Kourier's logger and carry no values
(see "Exporter diagnostics").

## Enable traces

Set these environment variables on the Agent Kourier Deployment:

```yaml
env:
  - name: OTEL_SERVICE_NAME
    value: agent-kourier
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
  - name: AGENTKOURIER_ENVIRONMENT
    value: pilot
```

Credentials travel only over TLS. Agent Kourier refuses to start when headers are configured
for an `http://` endpoint, because they would cross the network in cleartext. Use
`https://`, or set `AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS=true` to accept it for a
collector you reach over a link you trust (a sidecar on localhost, say). An `http://`
endpoint with no headers needs no opt-in.

Allow Agent Kourier to reach the collector under the deployment's egress policy.
The collector must accept OTLP HTTP/protobuf and forward traces to your trace backend.
No collector is deployed by Agent Kourier.

The default service name is `agent-kourier`. Traces include the binary version,
a random service instance ID for each provider lifetime, and the environment when set.

| Setting | Behavior |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Base URL; Agent Kourier appends `/v1/traces` |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Exact trace URL; overrides the base URL |
| `OTEL_TRACES_EXPORTER` | `otlp`, or `none` to disable export |
| `OTEL_EXPORTER_OTLP_PROTOCOL` / `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | `http/protobuf`; other protocols are rejected |
| `OTEL_EXPORTER_OTLP_HEADERS` / `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | Comma-separated `key=value` headers, values percent-decoded, names valid HTTP tokens; trace-specific settings override general settings. Both are checked even when the other wins, and a bad one stops startup |
| `AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS` | `true` accepts headers on an `http://` endpoint; default refuses them |
| `OTEL_TRACES_SAMPLER` | `parentbased_traceidratio` |
| `OTEL_TRACES_SAMPLER_ARG` | Root trace sampling ratio in `[0,1]`, default `1`; children respect their parent's sampling flag |
| `OTEL_SERVICE_NAME` | Service resource name |
| `AGENTKOURIER_ENVIRONMENT` | Optional `deployment.environment.name` |

Without an export endpoint, tracing is disabled. These are the environment settings
Agent Kourier explicitly supports; this is not a general implementation of every OTel SDK
configuration variable. Put collector credentials in Secret-backed environment values.
Endpoints must not contain URL credentials, query parameters or fragments. Both the
general and the trace-specific endpoint are checked, whichever one is used.

The OTLP exporter also reads some variables of its own from the environment, which
Agent Kourier does not validate and passes through unchanged:
`OTEL_EXPORTER_OTLP_CERTIFICATE` (a CA bundle file for an `https://` collector),
`OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` with `OTEL_EXPORTER_OTLP_CLIENT_KEY` (mutual TLS),
`OTEL_EXPORTER_OTLP_COMPRESSION` (`gzip` or none) and `OTEL_EXPORTER_OTLP_TIMEOUT`
(milliseconds), each also with the `_TRACES_` infix. `OTEL_EXPORTER_OTLP_INSECURE` is
ignored: the endpoint's scheme alone decides whether TLS is used. A value among these
that the exporter cannot use is reported as a warning (below), without the value.

Span attribute values are cut to 256 bytes, so an agent-supplied task or context ID
cannot grow a span without limit.

Export runs through a bounded batch queue. After broker shutdown drains its work,
Agent Kourier allows three seconds for the tracer provider to flush. A collector outage
must not block intake or backend execution; queued spans may be lost when the exporter
cannot keep up or the process is killed. Metrics are not derived from sampled traces.

## Exporter diagnostics

Collector error details never reach the SDK's diagnostics, and none of the SDK's own
diagnostics reach stderr outside Agent Kourier's logger. Three things log, all as warnings:

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
Every store operation is named `agentkourier.store.<Method>` and is recorded once, at the
store seam (`store.Guard`), whichever store is configured; neither store records its
own, and an operation the seam refuses for an illegal argument ends with the outcome
`invalid`. Store operations create child spans only when a valid parent trace context exists,
including restored durable context. Background store polling without a parent records
duration metrics without creating standalone traces. A recovered active task starts
a new execution trace linked to its saved task trace. A human answer follows its
press trace, with a link from the decision to the interaction that produced the
prompt. Agent Kourier does not hold a conversation span
open while waiting for human input.

A lookup of many rows is one store operation, not one for each row: `agentkourier.store.HasReplies`
asks which of up to 10000 message IDs are in the reply queue (one query on Postgres, one
for each 500 IDs on SQLite; the thread context of a mentioned turn uses it for the messages
of a thread window), where `agentkourier.store.HasReply` asks about one.

Important span attributes include:

- `agentkourier.binding`, `agentkourier.backend`, `agentkourier.agent.name`, `agentkourier.dialect`
- `agentkourier.turn.id`, `agentkourier.task.id`, `agentkourier.context.id`, `agentkourier.task.state`
- `agentkourier.retry.count`, `agentkourier.recovered`, `agentkourier.outcome`
- `agentkourier.interaction.id`, `agentkourier.interaction.kind`
- `agentkourier.outbox.id`, `agentkourier.outbox.role`, `agentkourier.delivery.mode`, `agentkourier.delivery.adopted`
- `agentkourier.render.mode`, `agentkourier.render.final`, `agentkourier.render.streaming`, `agentkourier.render.attempts`
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

Agent Kourier logs JSON lines to stderr, at the level `AGENTKOURIER_LOG_LEVEL` sets. A line carries IDs
(Binding, channel, thread, message, task, interaction, outbox row; `trace_id` and `span_id`
where there is a trace), counts, durations, closed words such as a `reason` or an outcome, and
error text. It never carries a message, an agent's output, status text or error message, a tool's
arguments, an answer a person gave, a token or a Secret's value.

Error text is the one place a line can quote someone, and Agent Kourier keeps it to what its own code and
its dependencies say. An error the agent answered with is logged as its class only
(`agent error: INTERNAL_ERROR`): a backend such as kagent fills the JSON-RPC message with the exception
of a failed model call or tool, which can quote model output or a tool's input, so Agent Kourier does not
log it. The agent's own text is in the agent's logs; find it there by the task or context ID of the
line. A Slack response body that slack-go could not parse, a handler's error or panic, and the like
may still quote someone, so Agent Kourier's JSON handler, built in one place (`telemetry.NewJSONHandler`) and
also the default `slog` logger, passes every attribute that holds an error, and every attribute
named `err`, `error`, `cause`, `reason` or `panic`, through the redaction the audit log uses: tokens,
credentials, URL userinfo, key blocks and the values of secret-named fields become `[redacted]`.
It then cuts the text to 300 bytes (the audit log's cap for a returned error) and marks the cut
with `… (truncated)`. Where the content has no diagnostic value, the line does not carry it at all: a
refused answer logs which answer and why (`answer 2 is not one of the choices`), never the value.
The error Agent Kourier exits with is printed the same way, as one `agent-kourier: ...` line on stderr.

Messages, level, time and attribute keys are not changed by this: an alert on a message or a
`jq` on `.error` keeps working. Text that is not an error attribute is logged as the code gave it,
which is why code logs IDs and classes there. Output written to the standard `log` package is not
redacted: it arrives as a line's `msg`. Nothing in Agent Kourier writes to it but net/http's error log for
the health server, which Agent Kourier routes there as a warning and which holds no content. The audit log is separate and
stricter: it stores typed error tags and identifiers, never an agent's error text; its one free-text reason is the
dialect's own, for an unusable pause, redacted and capped at 120 bytes.

## Metrics

Existing audit, credential, config, approval, interaction, chat connectivity and
terminal turn outcome metrics retain their names and hooks.

| Added metric | Definition |
|---|---|
| `agentkourier_operation_duration_seconds` | Duration of a finite operation, including failures and retries, by `operation` (the span name, such as `agentkourier.agent.send` or `agentkourier.store.GetSession`) and `outcome`. Agent execution ends at a terminal state, input-required or auth-required; both pause states are non-error outcomes. Agent and store calls are no longer also recorded in `agentkourier_agent_operation_duration_seconds` and `agentkourier_store_operation_duration_seconds`, which duplicated this histogram: select them with `operation=~"agentkourier.agent.*"` and `operation=~"agentkourier.store.*"` |
| `agentkourier_agent_first_text_duration_seconds` | Attempt start to first nonempty text observed on send/answer; no observations for follow snapshots |
| `http_client_request_duration_seconds` | HTTP request through response EOF or close; an SSE response includes its stream lifetime |
| `agentkourier_intake_total` | Message/press dispositions; `source="reply_queue",disposition="duplicate"` counts durable duplicates separately; a bot's message is `trigger_matched`, `trigger_unmatched` (no trigger took it) or `ignored` (an unbound channel, or a reply in a thread); a person's reply that a mention-only Binding turned away is `unmentioned` |
| `agentkourier_slack_ack_duration_seconds` | Envelope handling start to completion of the ack attempt, or return with ack withheld; measures queueing the ack, not confirmation from Slack |
| `agentkourier_turn_queue_wait_seconds` | Enqueue to first execution attempt in this process; a restart may observe the same waiting reply again |
| `agentkourier_sessions_active` / `agentkourier_sessions_paused` | Current in-process runners and runners parked on input |
| `agentkourier_stream_reconnects_total` | Completed reconnect attempts, by outcome |
| `agentkourier_session_recovery_total` | Adopted tasks, recording failures and reconciliation diagnostic notices |
| `agentkourier_reply_queue_depth` / `agentkourier_reply_queue_oldest_age_seconds` | Durable undelivered reply count and oldest age |
| `agentkourier_outbox_pending` / `agentkourier_outbox_oldest_age_seconds` | Unposted, nonabandoned rows and oldest age; includes incomplete stream rows |
| `agentkourier_outbox_delivery_duration_seconds` | Row creation to successful delivery/adoption attempt; replay after a crash can produce another observation |
| `agentkourier_render_final_total` | Final rendering outcomes: delivered, empty output, permanent failure, retry exhaustion, deadline exceeded or cancelled |
| `agentkourier_retention_removed_total` | Stored content that retention removed, by `what`: `stranded_replies` (undelivered replies dropped after 30 days, a turn that was never sent: worth an alert on any increase), `cleared_interactions` (settled interactions whose request and answer were emptied), `audit_entries` (purged past `AGENTKOURIER_AUDIT_RETENTION`; an increase with the setting at `keep` is a fault), and `expired_sessions` with the rows that went with them, `session_replies`, `session_interactions`, `session_presses` and `session_outbox` (sessions deleted 90 days after their `threadTTL`), and `stale_tasks` with the `stale_task_interactions` cancelled with them (a task that a crash left on a session past that horizon, ended so that the session can be deleted; a standing increase means crashes). See [data.md](data.md) |
| `agentkourier_interactions_pending` | Durable pending interactions |
| `agentkourier_interaction_wait_seconds` | Creation to first committed resolution, by kind/state; retries do not add another observation; the pending interactions that retention cancels with a stale task are not observed |
| `agentkourier_chat_api_calls_total` | Slack adapter operations and results, including logical API errors over HTTP 200; cached user lookups are adapter operations too. Outcomes include `rate_limited`, `auth_rejected`, `channel_gone`, `not_streaming`, `api_error`, and `mode_refused` (Slack refused the chunks mode of a stream, which makes Agent Kourier fall back to `markdown_text` and show no more steps). The `ThreadBetween` operation is a mentioned turn's read of its thread's earlier replies in a `chat.threadReplies: mention` Binding (`conversations.replies`, at most 5 pages, with an `auth.test`): `rate_limited` there is why a turn went out with the line that the replies could not be read |
| `agentkourier_thread_unmentioned_total` | Replies in a thread Agent Kourier owns that a Binding with `chat.threadReplies: mention` ignored because they did not mention Agent Kourier, by `binding`: nothing was written or sent, no reaction was added and the thread's `threadTTL` was not extended. A reply in an expired thread counts too (it gets no expiry notice). A typed answer admitted to a pending question that takes text replies (the `a2a` dialect's free-text question) is not counted here: it needs no mention and is queued as that question's answer. (A redelivery of such an answer's event after the question closed is counted, once, as it is then an ignored reply.) It is the engagement of a thread that people discuss without waking the agent, so a rising count with a flat turn count is the feature working, and a Binding whose people complain that Agent Kourier ignores them shows it here. |
| `agentkourier_thread_context_total` | Mentioned turns of a Binding with `chat.threadReplies: mention` that asked for the replies posted in their thread since the previous turn, by `binding` and `outcome`: `read` (a block of replies went with the turn), `empty` (there was nothing to add: no reply from a person since the previous turn, or a window with none left, as after two near-simultaneous mentions), `cut` (a block went, with a last line that says how many earlier replies it left out: more than 50, past the 1000-unit and 6000-unit caps, or older than the newest 1000 messages of the window), `failed` (the turn went with the fixed line "Agent Kourier could not read the earlier replies in this thread.": the platform's read failed or was rate limited past one wait of at most 5 s, the thread's window was past the 5 pages it will read, or the reply queue could not say which messages were turns) and `unsupported` (the platform cannot read a thread, so the turn went with the mentioned message alone; one info log line for each Binding). A turn is counted once when it is first sent (a resend after the agent was busy does not ask again), and again if a restart sends it anew. Nothing a person wrote is in a log line. A rising `failed` with `agentkourier_chat_api_calls_total{operation="ThreadBetween",outcome="rate_limited"}` says the reads are rate limited, which Slack counts against the same `conversations.replies` budget as the outbox's recovery |
| `agentkourier_trigger_matched_total` / `agentkourier_trigger_unmatched_total` | Bot messages a Binding's `botMessage` trigger matched, and bot messages in its channel that matched none (the heartbeat an `exclude` turns away counts here), by `binding`. A rising unmatched count after a template change means the trigger's regexes no longer fit; the debug log line names the channel, message and bot, never the text |
| `agentkourier_trigger_decisions_total` | What a matched bot message did to its key's thread, by `binding` and `decision`: `started` (a new investigation), `repeat` (a note that it fired again), `closed` (a note that it resolved), `close_unknown` (a close with no open thread, ignored), `dedup` (a message delivered again) `empty_key` (a keyed trigger whose key came out empty, so the message is an investigation of its own) and `not_started` (the session hook said the turn will not start, such as a refused prompt: the event is acknowledged, no cooldown begins) and `limited` (a start that one of the trigger's limits stopped: no turn, no key held, nothing posted in the alert's thread; see `agentkourier_trigger_limited_total`). Flapping shows as `repeat` and `closed` rising while `started` stays flat; the key is in the debug and info log lines only as a short hash |
| `agentkourier_trigger_limited_total` | Starts that a limit stopped, so no investigation began, by `binding`, `trigger` (its index in the Binding's `chat.triggers`) and `limit`: `rate` (the trigger started its `rateLimit.maxRuns` in the last `rateLimit.per`), `concurrent` (`rateLimit.maxConcurrent` of its investigations are running) or `daily` (it started `maxRunsPerDay` in the last 24 hours); when more than one is reached the daily cap is reported, then the rate, then the concurrency. A repeat, a close and a note are never limited. The alert is not posted about: it reached people through its own receiver. Once for each window, which opens with the first stopped start and lasts the trigger's `rateLimit.per`, one line in the channel (top level, through the outbox) says how many were not investigated and why, for example "7 alert messages not investigated in the last 10m (rate limit).", and names no alert. A count that keeps rising means the limits are too tight for the storm, or that the investigations are slow (concurrent), or that one source is noisy (daily) |
| `agentkourier_trigger_refused_total` | Starts that began no turn because the first prompt failed its check (spec R2) or could not be rendered, by `binding`, `trigger` (its index in the Binding's `chat.triggers`), `ref` (the value that failed, as a template names it without the dot: `Fields.alertname`, `Key`, `Link`, `State`, `Channel`, `Source`, or `template` for a template that failed to run) and `reason` (`characters`, `too_long`, `not_link`, `render_error`). `ref` and `reason` come from closed sets, and the entry names are the Binding's own. Each refusal is also a note in the alert's thread, "Not investigated: ...", and an audit entry that names the ref, reason and length of the value and never the value; a refusal opens no cooldown, so a fire of the key more than a minute later starts an investigation. A rising count after a template or Alertmanager receiver change means a value the template reads no longer fits its check |
| `agentkourier_investigation_feedback_total` | Votes cast on the final message of an investigation (the thread a trigger started) or of an answer (a Binding with `feedback.enabled`), by `binding`, `agent`, `alertname` and `verdict` (`useful` or `not_useful`). It counts votes cast, so a person who changes their vote adds one under the new verdict and the first stays counted; pressing the button of the verdict already recorded counts nothing. It therefore reads as activity and as an upper bound, and never as the standing of an alert: the pilot's exit decision (D33) is computed from the `feedback` table, one latest vote for each person and message (`docs/data.md`). A vote counts only from a member of the channel and never from a bot. `alertname` is the alert name of the thread: bounded by the trigger's allowlist for an investigation, the same name for an answer in such a thread, `none` for an answer in a thread no alert started and `unknown` for an investigation whose alert has no name in the name shape; and, whatever the allowlist, a Binding has at most 100 names of its own in this process, after which the votes count under `other` |
| `go_*`, `process_*`, `go_sql_*` | Runtime/process metrics and separate read/write pool statistics |

Duration histograms cover milliseconds through one hour. Use separate latency
queries for execution, human wait and visible delivery. The terminal turn outcome
counter describes the backend task; `agentkourier_render_final_total` describes what
happened to the output in chat. A completed task can have a failed final render.

Backlog scrapes query SQLite with a one-second timeout. A failed query produces an
invalid metric and a failed scrape rather than reporting a false zero backlog.

Example PromQL queries:

```promql
# p95 agent send time
histogram_quantile(0.95,
  sum by (le) (rate(agentkourier_operation_duration_seconds_bucket{operation="agentkourier.agent.send"}[5m])))

# final output failures
sum(rate(agentkourier_render_final_total{outcome=~"permanent_failure|retry_exhausted|deadline_exceeded"}[5m]))

# stalled inbound or outbound work
agentkourier_reply_queue_oldest_age_seconds > 300
agentkourier_outbox_oldest_age_seconds > 300

# SQLite writer pool wait time per second
rate(go_sql_wait_duration_seconds_total{db_name="write"}[5m])
```

Alertmanager ingress/routing limits and alert verdicts remain with
the Phase 2 adapters that have not been implemented. The generic event queue now
preserves trace context, so those adapters can connect their incoming HTTP traces
to queued execution without another persistence change.

Reference: [OpenTelemetry HTTP span conventions](https://opentelemetry.io/docs/specs/semconv/http/http-spans/) and [span links](https://opentelemetry.io/docs/specs/otel/trace/api/#links-between-spans).

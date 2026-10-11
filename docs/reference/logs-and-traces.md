# Logs and traces

This page covers what Agent Kourier exports as traces and writes as logs: the settings, the trace boundaries, the span attributes and what a log line may carry. The metrics are in [Metrics](metrics.md); setting up export is [Export metrics and traces](../how-to/export-telemetry.md).

Agent Kourier exports Prometheus metrics at `/metrics` and optionally exports traces with
OTLP over HTTP/protobuf. Metrics remain available when trace export is disabled.
Each broker owns its registry and tracer provider. The application does not install
a global OpenTelemetry provider. It does install OpenTelemetry's diagnostic logger and
error handler, once at startup and only when trace export is configured, because the SDK and the exporter reach them only
through process-wide hooks; they log through Agent Kourier's logger and carry no values
(see "Exporter diagnostics").

## Trace settings


Credentials travel only over TLS. Agent Kourier refuses to start when headers are configured
for an `http://` endpoint, because they would cross the network in cleartext. Use
`https://`, or set `AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS=true` to accept it for a
collector you reach over a link you trust (a sidecar on localhost, say). An `http://`
endpoint with no headers needs no opt-in.

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

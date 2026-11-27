# Logs and traces

This page covers what Agent Kourier exports as traces and writes as logs: the settings, the trace boundaries, the span attributes and what a log line may carry. The metrics are in [Metrics](metrics.md); setting up export is [Export metrics and traces](../how-to/export-telemetry.md).

Agent Kourier exports Prometheus metrics at `/metrics` and optionally exports traces with
OTLP over HTTP/protobuf. Metrics remain available when trace export is disabled.

Each broker owns its registry and tracer provider, and the application does not install a global OpenTelemetry provider. It does
install OpenTelemetry's diagnostic logger and error handler, once at startup and only when trace export is configured, because the
SDK and the exporter reach them only through process-wide hooks. They log through Agent Kourier's logger and carry no values (see
"Exporter diagnostics").

## Trace settings


Credentials travel only over TLS. Agent Kourier refuses to start when headers are configured
for an `http://` endpoint, because they would cross the network in cleartext.

Use `https://`, or set `AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS=true` to accept it for a
collector you reach over a link you trust (a sidecar on localhost, say). An `http://`
endpoint with no headers needs no opt-in.

The collector must accept OTLP HTTP/protobuf and forward traces to your trace backend.
Agent Kourier deploys no collector.

The default service name is `agent-kourier`. Traces include the binary version,
a random service instance ID for each provider lifetime, and the environment when set.

| Setting | Behavior |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Base URL; Agent Kourier appends `/v1/traces` |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Exact trace URL; overrides the base URL |
| `OTEL_TRACES_EXPORTER` | `otlp`, or `none` to disable export |
| `OTEL_EXPORTER_OTLP_PROTOCOL` / `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | The trace-specific one if set, else the general one, must be `http/protobuf`; another value there is rejected |
| `OTEL_EXPORTER_OTLP_HEADERS` / `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | Comma-separated `key=value` headers, values percent-decoded, names valid HTTP tokens; trace-specific settings override general settings. Both are checked even when the other wins, and a bad one stops startup |
| `AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS` | `true` accepts headers on an `http://` endpoint; default refuses them |
| `OTEL_TRACES_SAMPLER` | `parentbased_traceidratio` |
| `OTEL_TRACES_SAMPLER_ARG` | Root trace sampling ratio in `[0,1]`, default `1`; children respect their parent's sampling flag |
| `OTEL_SERVICE_NAME` | Service resource name |
| `AGENTKOURIER_ENVIRONMENT` | Optional `deployment.environment.name` |

Without an export endpoint, tracing is disabled. These are the environment settings Agent Kourier explicitly supports, not a
general implementation of every OTel SDK configuration variable.

Put collector credentials in Secret-backed environment values. Endpoints must not contain URL credentials, query parameters or
fragments. Both the general and the trace-specific endpoint are checked, whichever one is used.

The OTLP exporter also reads some variables of its own from the environment, which Agent Kourier does not validate and passes
through unchanged, each also with the `_TRACES_` infix:

- `OTEL_EXPORTER_OTLP_CERTIFICATE` (a CA bundle file for an `https://` collector)
- `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` with `OTEL_EXPORTER_OTLP_CLIENT_KEY` (mutual TLS)
- `OTEL_EXPORTER_OTLP_COMPRESSION` (`gzip` or none)
- `OTEL_EXPORTER_OTLP_TIMEOUT` (milliseconds)

`OTEL_EXPORTER_OTLP_INSECURE` is overridden: the endpoint's scheme decides whether TLS is used. A value among these that the
exporter cannot use is reported as a warning (below), without the value.

Span attribute values are cut to 256 characters (up to about 1 KiB of UTF-8), so an agent-supplied task or context ID
cannot grow a span without limit.

Export runs through a bounded batch queue. After broker shutdown drains its work, Agent Kourier allows three seconds for the tracer
provider to flush.

A collector outage must not block intake or backend execution. Queued spans may be lost when the exporter cannot keep up or the
process is killed. Metrics are not derived from sampled traces.

## Exporter diagnostics

Collector error details never reach the SDK's diagnostics, and none of the SDK's own
diagnostics reach stderr outside Agent Kourier's logger. Five things log, all as warnings:

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
- An exporter that fails to shut down logs `trace exporter shutdown failed` with `error_type`.
- A flush of the spans still held at shutdown that fails or takes more than 3 seconds logs
  `trace export flush failed` with `error_type`.

## Trace boundaries

A Slack envelope starts a local trace. In Socket Mode its span, `agentkourier.slack.envelope`, covers the durable intake and the
ack attempt, and in HTTP mode the handoff to the intake. A verified request to a webhook connection starts a local trace too (see
"Webhook requests"), and so does a verified activity request to a Microsoft Teams connection in `http` mode (see "Microsoft Teams
requests").

A queued reply restores its saved W3C ancestry when a session runner takes it up. A2A HTTP calls carry `traceparent` and
`tracestate` to the verifying backend front door after the destination and credential checks. Slack receives no trace headers or
baggage, and the trace headers a webhook sender or Microsoft Teams sends are not read.

Both stores keep trace context beside replies, active tasks, outbound rows, interactions, button presses, and generic durable
events. Duplicate inserts retain the original carrier.

Existing rows without trace context still work. Migration `0012_trace_context.sql` adds columns with empty defaults.

Each turn attempt, unary A2A operation and delivery attempt has a finite span.

Every store operation is named `agentkourier.store.<Method>` and is recorded once, at the store seam (`store.Guard`), whichever
store is configured. Neither store records its own, and an operation the seam refuses for an illegal argument ends with the outcome
`invalid`.

Store operations create child spans only when a valid parent trace context exists, including restored durable context. Background
store polling without a parent records duration metrics without creating standalone traces.

A recovered active task starts a new execution trace linked to its saved task trace. A human answer follows its press trace, with a
link from the decision to the interaction that produced the prompt. Agent Kourier does not hold a conversation span open while
waiting for human input.

A lookup of many rows is one store operation, not one for each row: `agentkourier.store.HasReplies` asks which of up to 10000
message IDs are in the reply queue, where `agentkourier.store.HasReply` asks about one.

`agentkourier.store.HasReplies` makes one query on Postgres, and one for each 500 IDs on SQLite. The thread context of a mentioned
turn uses it for the messages of a thread window.

Important span attributes include:

- `agentkourier.binding`, `agentkourier.backend`, `agentkourier.agent.name`, `agentkourier.dialect`
- `agentkourier.platform`, `agentkourier.connection`, `agentkourier.webhook.event`, `agentkourier.webhook.delivery`, `agentkourier.webhook.reason`
- `agentkourier.teams.activity.type`, `agentkourier.teams.activity.id`
- `agentkourier.turn.id`, `agentkourier.task.id`, `agentkourier.context.id`, `agentkourier.task.state`
- `agentkourier.retry.count`, `agentkourier.recovered`, `agentkourier.outcome`
- `agentkourier.interaction.id`, `agentkourier.interaction.kind`
- `agentkourier.outbox.id`, `agentkourier.outbox.role`, `agentkourier.delivery.mode`, `agentkourier.delivery.adopted`
- `agentkourier.render.mode`, `agentkourier.render.final`, `agentkourier.render.streaming`, `agentkourier.render.attempts`
- HTTP method, server address/port and response status

Task and turn IDs belong only to spans and logs. Metrics use configured Binding names and closed operation/outcome sets.

Span attributes and metric labels exclude prompts, responses, tool arguments/results, user names, authentication headers, URL
queries and raw errors. Logs do carry error text, redacted and capped (see "Logs"). Existing application log messages retain their
existing fields.

Contextual structured logs include `trace_id` and `span_id`. Trace export adds no LLM token or cost estimates. Those belong to
instrumentation in the backend that actually calls the model.

### Webhook requests

Each verified request a webhook connection answers is one span, `agentkourier.webhook.request`, of kind server, from when
the connection's handler is called to its response, the time to read and verify the body included. It is always a root:
the sender's trace headers are not read.

The intake's span (`agentkourier.intake`) is its child, and the turn that the event starts is in its trace. The turn's span is
restored from the trace context the intake stored with the turn, and goes on after the request's span has ended with the response.

The span carries:

- `agentkourier.platform` (`webhook`) and `agentkourier.connection` (the ChatConnection, `namespace/name`)
- `agentkourier.outcome` and `agentkourier.webhook.reason`: the `outcome` and `reason` of `agentkourier_webhook_requests_total`
- `http.response.status_code`
- `agentkourier.webhook.event`, the event's ID (the SHA-256 of the body)
- `agentkourier.webhook.delivery`, the delivery ID, when the connection's `delivery.header` holds an identifier of at most 128 bytes

A `503` (`not_running`, `withheld`, `timeout`, `cancelled`) sets `error.type` to the outcome and the span's status to
error, but for `cancelled` (the sender hung up first), whose status is unset, as a cancelled operation's is. Nothing else
of the request reaches the span: not its body, its signature, or any other header.

A request that is not verified (`unverified`) or whose body could not be read (`malformed`, `unreadable`) has no span, so that a
client that can reach the URL but does not hold the secret cannot start traces. `agentkourier_webhook_requests_total` counts it.
Nor has a request that is not a `POST`.

A verified request that is `malformed` for its body (`schema`, `depth`, `duplicate_key`) has one. The span records no `agentkourier_operation_duration_seconds`: `agentkourier_webhook_ack_duration_seconds` times the response.

The line `webhook: delivery answered` (INFO) is written on the span's context, so it carries its `trace_id` and
`span_id`. The DEBUG line of a request that is not verified or could not be read, and the WARN line that sums a
connection's unverified requests, carry no trace.

### Microsoft Teams requests

Each verified activity request that Microsoft Teams sends a connection in `http` mode is one span, `agentkourier.teams.request`,
of kind server, from when the connection's handler is called to its response, the time to verify the token included. A request
is verified once its Bot Framework token is valid for the connection's app, the token's service URL is the activity's, and the
activity is of the connection's tenant.

The span is always a root: the trace headers Microsoft Teams sends are not read.

A message is answered as soon as it is on its channel's lane, and its post is handed to the intake from the lane after the
response. The intake's span (`agentkourier.intake`) is a child of the request's span all the same, started after that span has
ended, and the turn that the post starts is in its trace.

An invoke (a press of a card's button) is answered within its request: the press's intake span is a child of the request's span
while it is open.

The span carries:

- `agentkourier.platform` (`teams`) and `agentkourier.connection` (the ChatConnection, `namespace/name`)
- `agentkourier.outcome`: the `outcome` of `agentkourier_teams_requests_total`, one of `accepted`, `duplicate`, `invoke`,
  `malformed` (a message with no ID that can name it, or an invoke whose `replyToId` holds `:`), `not_running` or `shed`
- `http.response.status_code`
- `agentkourier.teams.activity.type`, the activity's type, such as `message` or `invoke`
- `agentkourier.teams.activity.id`, the activity's ID, when it has one

A `503` (`not_running`, `shed`) sets `error.type` to the outcome and the span's status to error. Nothing else of the activity
reaches the span: not its text, its sender or its conversation, and never the token.

A request refused at its token (`bad_token`, or `keys_unavailable` when the signing keys could not be read), whose body is no
activity (`malformed`), or whose activity is of another tenant (`wrong_tenant`) has no span, so that a client that can reach the
route, or a tenant that installs the bot, cannot start traces. `agentkourier_teams_requests_total` counts it. The span records no
`agentkourier_operation_duration_seconds`.

The lines logged for a verified request carry its `trace_id` and `span_id`. They include the handler's, such as the WARN of an
activity it did not take (`teams: an activity was not taken; answered 503, ...`), and the lane's, such as
`teams: the sink did not take a message, which is dropped`, written after the response under the request's span. The WARN of a
refused token or of another tenant's activity carries no trace.

Microsoft Graph's change notifications, which come to the same route, the polls of a connection's channels through Graph, and
the activities and invokes of a connection in `socket` mode have no such span: the intake's span of a post or a press that comes
through them is the root of its trace.

## Logs

Agent Kourier logs JSON lines to stderr, at the level `AGENTKOURIER_LOG_LEVEL` sets. A line carries IDs
(Binding, channel, thread, message, task, interaction, outbox row; `trace_id` and `span_id`
where there is a trace), counts, durations, closed words such as a `reason` or an outcome, and
error text. It never carries a message, an agent's output, status text or error message, a tool's
arguments, an answer a person gave, a token or a Secret's value.

Error text is the one place a line can quote someone, and Agent Kourier keeps it to what its own code and its dependencies say.

An error the agent answered with is logged as its class only (`agent error: INTERNAL_ERROR`). A backend such as kagent fills the
JSON-RPC message with the exception of a failed model call or tool, which can quote model output or a tool's input, so Agent
Kourier does not log it. The agent's own text is in the agent's logs: find it there by the task or context ID of the line.

A Slack response body that slack-go could not parse, a handler's error or panic, and the like may still quote someone. So Agent
Kourier's JSON handler, built in one place (`telemetry.NewJSONHandler`) and also the default `slog` logger, passes every attribute
that holds an error, and every attribute named `err`, `error`, `cause`, `reason` or `panic`, through the free-text redaction
(`redact.FreeText`). Tokens, credentials, URL userinfo, key blocks and the values of secret-named fields become `[redacted]`.

The handler then cuts the text to 300 bytes (the audit log's cap for a returned error) and marks the cut with `… (truncated)`.
Where the content has no diagnostic value, the line does not carry it at all: a refused answer logs which answer and why
(`answer 2 is not one of the choices`), never the value. The error Agent Kourier exits with is printed the same way, as one
`agent-kourier: ...` line on stderr.

Messages, level, time and attribute keys are not changed by this: an alert on a message or a `jq` on `.error` keeps working. Text
that is not an error attribute is logged as the code gave it, which is why code logs IDs and classes there.

Output written to the standard `log` package is not redacted: it arrives as a line's `msg`. Nothing in Agent Kourier writes to it
but net/http's error log for the health server, which Agent Kourier routes there as a warning and which holds no content.

The audit log is separate and stricter: it stores typed error tags and identifiers, never an agent's error text. Its one free-text
reason is the dialect's own, for an unusable pause, redacted and capped at 120 bytes.

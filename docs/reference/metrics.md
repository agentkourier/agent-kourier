# Metrics

This page lists every Prometheus metric Agent Kourier exports at `/metrics`, with its labels and what it counts.
Setting up the scrape is [Export metrics and traces](../how-to/export-telemetry.md).

Metric labels carry configured names (Bindings, connections) and values from closed sets, never prompts, answers, user
names or error text.

## Connection, configuration and turns

| Metric | Type | Labels | Definition |
|---|---|---|---|
| `agentkourier_chat_connected` | gauge | `connection` | 1 while the chat connection (Slack Socket Mode) is up, else 0, by ChatConnection. `/readyz` does not say this. |
| `agentkourier_turn_outcomes_total` | counter | `binding`, `outcome` | Turns that ended, by Binding and how: the terminal state of the task, or the class of what stopped it. `outcome` is one of `completed`, `failed`, `rejected`, `canceled`, `credentials_rejected`, `forbidden`, `unreachable`, `session_gone`, `binding_gone`, `wedged`, `uncancellable`, `pause_ended`, `answer_rejected`, `refused`, `no_final`. It is counted before the `session turn ended` log line. |
| `agentkourier_config_stale` | gauge | | 1 while the latest config reload failed, so the running config lags the files. |
| `agentkourier_config_reload_errors_total` | counter | | Config reloads that failed. |
| `agentkourier_credential_rejections_total` | counter | `binding` | Credentials an agent rejected, by Binding: the front door refused the Binding's token. |
| `agentkourier_audit_write_failures_total` | counter | `kind` | Audit entries that could not be written, by kind. |
| `agentkourier_approval_outcomes_total` | counter | `binding`, `outcome` | Pauses a Binding's policy turned away, by Binding and outcome. |
| `agentkourier_interaction_decisions_total` | counter | `result` | Presses decided on an interaction, by result. |
| `agentkourier_interaction_timeouts_total` | counter | | Interactions that timed out unanswered. |
| `agentkourier_outbox_abandoned_total` | counter | `reason` | Outbound posts given up on, by reason: `channel_gone`, `too_long`, `invalid_content` or `other`. |
| `agentkourier_leader` | gauge | | 1 while this replica does Agent Kourier's work (it leads, or it is the only replica: SQLite), 0 while it is a standby. |
| `agentkourier_leader_takeovers_total` | counter | | Times this replica took the leadership: it won the lease and was given the next epoch. |

## Operations, intake, delivery and triggers

| Metric | Definition |
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
| `agentkourier_retention_removed_total` | Stored content that retention removed, by `what`: `stranded_replies` (undelivered replies dropped after 30 days, a turn that was never sent: worth an alert on any increase), `cleared_interactions` (settled interactions whose request and answer were emptied), `audit_entries` (purged past `AGENTKOURIER_AUDIT_RETENTION`; an increase with the setting at `keep` is a fault), and `expired_sessions` with the rows that went with them, `session_replies`, `session_interactions`, `session_presses` and `session_outbox` (sessions deleted 90 days after their `threadTTL`), and `stale_tasks` with the `stale_task_interactions` cancelled with them (a task that a crash left on a session past that horizon, ended so that the session can be deleted; a standing increase means crashes). See [Stored data](stored-data.md) |
| `agentkourier_interactions_pending` | Durable pending interactions |
| `agentkourier_interaction_wait_seconds` | Creation to first committed resolution, by kind/state; retries do not add another observation; the pending interactions that retention cancels with a stale task are not observed |
| `agentkourier_chat_api_calls_total` | Slack adapter operations and results, including logical API errors over HTTP 200; cached user lookups are adapter operations too. Outcomes include `rate_limited`, `auth_rejected`, `channel_gone`, `not_streaming`, `api_error`, and `mode_refused` (Slack refused the chunks mode of a stream, which makes Agent Kourier fall back to `markdown_text` and show no more steps). The `ThreadBetween` operation is a mentioned turn's read of its thread's earlier replies in a `chat.threadReplies: mention` Binding (`conversations.replies`, at most 5 pages, with an `auth.test`): `rate_limited` there is why a turn went out with the line that the replies could not be read |
| `agentkourier_thread_unmentioned_total` | Replies in a thread Agent Kourier owns that a Binding with `chat.threadReplies: mention` ignored because they did not mention Agent Kourier, by `binding`: nothing was written or sent, no reaction was added and the thread's `threadTTL` was not extended. A reply in an expired thread counts too (it gets no expiry notice). A typed answer admitted to a pending question that takes text replies (the `a2a` dialect's free-text question) is not counted here: it needs no mention and is queued as that question's answer. (A redelivery of such an answer's event after the question closed is counted, once, as it is then an ignored reply.) It is the engagement of a thread that people discuss without waking the agent, so a rising count with a flat turn count is the feature working, and a Binding whose people complain that Agent Kourier ignores them shows it here. |
| `agentkourier_thread_context_total` | Mentioned turns of a Binding with `chat.threadReplies: mention` that asked for the replies posted in their thread since the previous turn, by `binding` and `outcome`: `read` (a block of replies went with the turn), `empty` (there was nothing to add: no reply from a person since the previous turn, or a window with none left, as after two near-simultaneous mentions), `cut` (a block went, with a last line that says how many earlier replies it left out: more than 50, past the 1000-unit and 6000-unit caps, or older than the newest 1000 messages of the window), `failed` (the turn went with the fixed line "Agent Kourier could not read the earlier replies in this thread.": the platform's read failed or was rate limited past one wait of at most 5 s, the thread's window was past the 5 pages it will read, or the reply queue could not say which messages were turns) and `unsupported` (the platform cannot read a thread, so the turn went with the mentioned message alone; one info log line for each Binding). A turn is counted once when it is first sent (a resend after the agent was busy does not ask again), and again if a restart sends it anew. Nothing a person wrote is in a log line. A rising `failed` with `agentkourier_chat_api_calls_total{operation="ThreadBetween",outcome="rate_limited"}` says the reads are rate limited, which Slack counts against the same `conversations.replies` budget as the outbox's recovery |
| `agentkourier_trigger_matched_total` / `agentkourier_trigger_unmatched_total` | Bot messages a Binding's `botMessage` trigger matched, and bot messages in its channel that matched none (the heartbeat an `exclude` turns away counts here), by `binding`. A rising unmatched count after a template change means the trigger's regexes no longer fit; the debug log line names the channel, message and bot, never the text |
| `agentkourier_trigger_decisions_total` | What a matched bot message did to its key's thread, by `binding` and `decision`: `started` (a new investigation), `repeat` (a note that it fired again), `closed` (a note that it resolved), `close_unknown` (a close with no open thread, ignored), `dedup` (a message delivered again) `empty_key` (a keyed trigger whose key came out empty, so the message is an investigation of its own) and `not_started` (the session hook said the turn will not start, such as a refused prompt: the event is acknowledged, no cooldown begins) and `limited` (a start that one of the trigger's limits stopped: no turn, no key held, nothing posted in the alert's thread; see `agentkourier_trigger_limited_total`). Flapping shows as `repeat` and `closed` rising while `started` stays flat; the key is in the debug and info log lines only as a short hash |
| `agentkourier_trigger_limited_total` | Starts that a limit stopped, so no investigation began, by `binding`, `trigger` (its index in the Binding's `chat.triggers`) and `limit`: `rate` (the trigger started its `rateLimit.maxRuns` in the last `rateLimit.per`), `concurrent` (`rateLimit.maxConcurrent` of its investigations are running) or `daily` (it started `maxRunsPerDay` in the last 24 hours); when more than one is reached the daily cap is reported, then the rate, then the concurrency. A repeat, a close and a note are never limited. The alert is not posted about: it reached people through its own receiver. Once for each window, which opens with the first stopped start and lasts the trigger's `rateLimit.per`, one line in the channel (top level, through the outbox) says how many were not investigated and why, for example "7 alert messages not investigated in the last 10m (rate limit).", and names no alert. A count that keeps rising means the limits are too tight for the storm, or that the investigations are slow (concurrent), or that one source is noisy (daily) |
| `agentkourier_trigger_refused_total` | Starts that began no turn because the first prompt failed its check (spec R2) or could not be rendered, by `binding`, `trigger` (its index in the Binding's `chat.triggers`), `ref` (the value that failed, as a template names it without the dot: `Fields.alertname`, `Key`, `Link`, `State`, `Channel`, `Source`, or `template` for a template that failed to run) and `reason` (`characters`, `too_long`, `not_link`, `render_error`). `ref` and `reason` come from closed sets, and the entry names are the Binding's own. Each refusal is also a note in the alert's thread, "Not investigated: ...", and an audit entry that names the ref, reason and length of the value and never the value; a refusal opens no cooldown, so a fire of the key more than a minute later starts an investigation. A rising count after a template or Alertmanager receiver change means a value the template reads no longer fits its check |
| `agentkourier_investigation_feedback_total` | Votes cast on the final message of an investigation (the thread a trigger started) or of an answer (a Binding with `feedback.enabled`), by `binding`, `agent`, `alertname` and `verdict` (`useful` or `not_useful`). It counts votes cast, so a person who changes their vote adds one under the new verdict and the first stays counted; pressing the button of the verdict already recorded counts nothing. It therefore reads as activity and as an upper bound, and never as the standing of an alert: the pilot's exit decision (D33) is computed from the `feedback` table, one latest vote for each person and message ([Stored data](stored-data.md)). A vote counts only from a member of the channel and never from a bot. `alertname` is the alert name of the thread: bounded by the trigger's allowlist for an investigation, the same name for an answer in such a thread, `none` for an answer in a thread no alert started and `unknown` for an investigation whose alert has no name in the name shape; and, whatever the allowlist, a Binding has at most 100 names of its own in this process, after which the votes count under `other` |

## Postgres store

Only with `AGENTKOURIER_STORE=postgres`.

| Metric | Type | Definition |
|---|---|---|
| `agentkourier_store_deposed_total` | counter | Writes the store refused because another replica has led since: its epoch was stale. |
| `agentkourier_store_pool_conns` | gauge | Connections of the store's pool, in use, idle or being made. |
| `agentkourier_store_pool_acquired_conns` | gauge | Connections of the store's pool in use. |
| `agentkourier_store_pool_idle_conns` | gauge | Idle connections of the store's pool. |
| `agentkourier_store_pool_max_conns` | gauge | Largest size of the store's pool. |
| `agentkourier_store_pool_acquires_total` | counter | Connections taken from the store's pool. |
| `agentkourier_store_pool_empty_acquires_total` | counter | Takes that had to wait for a connection. |
| `agentkourier_store_pool_canceled_acquires_total` | counter | Takes that a context ended before they got a connection. |
| `agentkourier_store_pool_acquire_seconds_total` | counter | Time spent taking connections from the store's pool. |

## Runtime

| Metric | Definition |
|---|---|
| `go_*`, `process_*` | The Go runtime and process collectors. |
| `go_sql_*` | Database pool statistics, separately for the read and write pools (`db_name`). |

## Reading them

Duration histograms cover milliseconds through one hour. Use separate latency
queries for execution, human wait and visible delivery. The terminal turn outcome
counter describes the backend task; `agentkourier_render_final_total` describes what
happened to the output in chat. A completed task can have a failed final render.

Backlog scrapes query SQLite with a one-second timeout. A failed query produces an
invalid metric and a failed scrape rather than reporting a false zero backlog.

Alertmanager ingress/routing limits and alert verdicts remain with
the Alertmanager webhook receiver, which is not built yet. The generic event queue now
preserves trace context, so those adapters can connect their incoming HTTP traces
to queued execution without another persistence change.

Reference: [OpenTelemetry HTTP span conventions](https://opentelemetry.io/docs/specs/semconv/http/http-spans/) and [span links](https://opentelemetry.io/docs/specs/otel/trace/api/#links-between-spans).

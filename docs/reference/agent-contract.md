# Agent contract

This page lists what an agent must do, beyond speaking A2A, for Agent Kourier to carry its turns, and what happens
when it does not. It applies to both dialects; where `a2a` and `kagent-v1` differ, the row says so. The method names
are A2A 1.0's, with the 0.3 name in brackets.

## Discovery and transport

| The agent must | Otherwise |
|---|---|
| Serve A2A 1.0 or 0.3 over JSON-RPC, on HTTP or HTTPS | Interfaces that are not JSON-RPC are dropped from the card. A card with none left fails the turn with `agent card lists no JSON-RPC interface` |
| Serve its card at the endpoint, with any trailing slash trimmed, plus `/.well-known/agent-card.json`. On `a2a` the endpoint is the AgentBackend's `url` as written; on `kagent-v1` it is `url` plus `/agents/<namespace>/<name>` | The card fetch fails and the turn is retried, then fails after 15 minutes |
| Set `capabilities.streaming: true` on the card | The agent is refused, as above. The non-streaming send is never used |
| Mark no extension `required` that the dialect does not speak. `kagent-v1` speaks kagent's human-in-the-loop extension; `a2a` speaks none | The connect fails with `agent requires an unsupported extension` |
| Answer at the endpoint itself, without a redirect | A 3xx is a failed call; no redirect is followed |
| Send response headers within 30 seconds of every call, streams included | The call fails as a lost connection |

The interface URLs in the card are ignored: every call goes to the endpoint above. The card is fetched with the same
identity headers as every other call, on first use and again when the AgentBackend's URL or the Binding's credentials
change.

## Methods

| Method | Called | If the agent does not implement it |
|---|---|---|
| `SendStreamingMessage` (`message/stream`) | For every turn and every answer to a question | Nothing works |
| `GetTask` (`tasks/get`) | When a stream ends or drops before the task settles; before following a task after a restart; when a typed reply arrives for a question on `a2a`; when a cancel fails | A turn whose stream does not end with a full task (see [Replies](#replies)) cannot finish |
| `SubscribeToTask` (`tasks/resubscribe`) | After a dropped stream, when `GetTask` shows the task still running | Live text stops. The task is read again with `GetTask`, with a backoff of up to a minute, until it ends |
| `CancelTask` (`tasks/cancel`) | When a question times out; for a question the Binding does not allow or the dialect cannot show; for a task that shows no sign of life for an hour after its stream was lost; after the agent rejects an answer | The cancel is retried a few times and reported as failed. The task keeps running on the agent |
| `ListTasks` (1.0 only) | Before a turn whose outcome is unknown is sent again into an existing context, and on recovery after a restart | The lookup is retried, then the turn fails after 15 minutes |

`ListTasks` is called with the session's `contextId` and follows `nextPageToken` for up to 100 pages. A turn counts
as found only if a task's `history` holds a message with the turn's `messageId`. A list without history makes every
turn look absent, so it is sent, and may run, again. An A2A 0.3 agent is never asked; its turns are always sent
again.

## What Agent Kourier sends

Every send is one user message with **exactly one text part**. No data or file parts are sent.

| Field | Value |
|---|---|
| `messageId` | Fixed for each chat message. A turn sent again, after `-32004` or an unknown outcome, keeps it. An answer to a question has its own |
| `contextId` | Empty on a thread's first turn. After that, the context the agent returned |
| `taskId` | Set only on an answer, to the paused task. A new turn never sets it |
| `metadata["agentkourier.dev/actor"]` | On a turn a person wrote: `id`, the person's ID on the chat platform (Slack: `U123`); `platform`, the platform's ID (`slack`); and `name` and `email` when the chat platform knows them. An answer to a question or an approval carries `id` and `platform` for the person who decided, and no `name` or `email`. A denial Agent Kourier makes itself, for an approval that timed out or one the Binding does not allow, carries `id` `agent-kourier`, with the Binding's `platform`. A turn that a trigger started, such as an alert, carries none |

The text of a turn a person wrote starts with `[<Platform>: Name (id) <email>]` and a newline, except in a `kagent-v1`
structured answer. `<Platform>` is the chat platform's display name, so on Slack the prefix reads
`[Slack: Name (U123) <email>]`. Read the actor from the metadata, not by parsing the prefix: the metadata has each
field on its own, and a structured answer carries the metadata alone. Alert fields, and thread replies that did not
mention Agent Kourier, arrive in blocks the text marks as untrusted data. The actor and the prefix are informational:
the identity that calls the agent is always the Binding's.

Every request carries one identity header, `Authorization: Bearer` from the Binding's `identity.tokenSecretRef` or
`X-User-Id` from `identity.userId`, plus the AgentBackend's static `headers`. It may also carry the W3C trace context headers
`traceparent` and `tracestate`. On `kagent-v1`, the `A2A-Extensions` header asks for kagent's human-in-the-loop
extension when the card lists it.

## Replies

| The agent must | Otherwise |
|---|---|
| Answer a send with task events: `Task`, `TaskStatusUpdateEvent` and `TaskArtifactUpdateEvent` | A reply that is a bare `Message` names no task, so the outcome is unknown. The turn is sent again, and may run again, until it fails after 15 minutes with "the agent could not be reached" |
| Name the task and its `contextId` on its events | As for a bare `Message`: the outcome is unknown and the turn is sent again |
| Create a `contextId` on a thread's first turn, and keep it for every later turn | A different `contextId` posts "The agent no longer has this conversation" and the new context is kept |
| Put the answer in **artifact text parts** | Text in the status message, in `history` or in a bare `Message` is not shown. A task that completes with no artifact text posts "The agent finished without any output." |
| End the stream with a full `Task`, or a `TaskStatusUpdateEvent`, in a terminal, `input-required` or `auth-required` state | The stream is treated as dropped: a second later, `GetTask` reads the task, and `SubscribeToTask` follows it if it is still running |
| Send an event at least every 5 minutes, including during a long tool call | The stream is cut and the task followed again with `GetTask` and `SubscribeToTask` |
| Run one task per context at a time | While a task of the session is recorded, no new turn is sent to it; replies queue behind it |
| Treat a repeated `messageId` as the same turn | A thread's first turn has no context to look in, so after an unknown outcome it is sent again with the same `messageId`, and an agent that does not drop it runs it twice |

When the stream closes normally after such a status update, without the full `Task`, Agent Kourier reads the task with one `GetTask`
at once and takes it as the final one. If that call fails, or the task it returns is in none of those states, or the stream ended
with an error, the stream is treated as dropped.

An artifact update with `append: true` adds to that artifact's text; any other replaces it. `lastChunk` is not read.
An update with no text part leaves the artifact's text as it was, so to take text back, replace it with an empty text part.
An artifact whose text is empty is left out of the message.
A task snapshot (from `GetTask`, `SubscribeToTask` or a replayed send) whose text for an artifact is a prefix of the text already
received is taken as older and skipped, so a take-back that is missed when the stream drops is not applied.
File parts are never shown. Data parts are not shown either, except on `kagent-v1`, which shows kagent's tool call
and tool result parts as step cards.

## Task states

| State | What Agent Kourier does |
|---|---|
| `submitted`, `working` | Shows the turn as running |
| `completed` | Ends the turn. Posts a notice only when there was no artifact text |
| `failed` | Ends the turn and posts "The agent's task failed." with a category when one is recognised (see [Errors](#errors)) |
| `rejected`, `canceled` | Ends the turn and posts a notice |
| `input-required` | A question: see [Let the agent ask questions](../how-to/agent-questions.md) |
| `auth-required` | Not supported, on either dialect. Agent Kourier tells the thread that the agent paused in a way it cannot use, cancels the task, and then sends the replies queued behind it |
| Any other terminal state | Treated as `failed` |

A question on `a2a` is a status message made only of non-empty text parts, with a `messageId` or a status
`timestamp` that differs from the last question's. Anything else in `input-required` cannot be shown, so the task is
cancelled and the thread is told. The answer is one text part sent to the paused `taskId` and `contextId`. On
`kagent-v1`, the question is kagent's human-in-the-loop payload in the status message's metadata.

## Errors

| The agent answers | What Agent Kourier does |
|---|---|
| HTTP 401 or 403, at any call | The credentials are rejected: the turn fails without a retry, and `agentkourier_credential_rejections_total` counts it |
| JSON-RPC `-31403` | The identity is refused for this session: the turn fails |
| JSON-RPC `-32004` to a send | Busy: the same message is sent again after a backoff, for as long as it takes, and the thread is told after 2 minutes. Do not use `-32004` for an operation the agent does not support, or a turn waits for ever |
| JSON-RPC `-32603` to an answer | The answer is rejected: the paused task is cancelled and the thread told |
| JSON-RPC `-32001` (task not found) while a task is followed | The session is gone: the thread is told and the next turn starts a new context |
| Another HTTP 4xx, other than 408 and 429, before any event | Not sent: retried, then failed after 15 minutes |
| HTTP 5xx, or a dropped connection, before any event | Outcome unknown: looked up with `ListTasks` where it can be, then sent again |

A `Retry-After` header is honoured on reconnects, up to a day. An error's message is never shown in the thread. For
a `failed` task, the status message's text, up to 32 parts and 4096 characters, and the metadata keys `code`,
`error_code` and `kagent_error_code` are read only to pick a category: context limit, rate limit, authentication,
timeout or permission.

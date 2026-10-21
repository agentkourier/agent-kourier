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
| Mark no extension `required` that the dialect does not speak. Both dialects speak kagent's human-in-the-loop extension and no other | The connect fails with `agent requires an unsupported extension` |
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
`traceparent` and `tracestate`. On both dialects, the `A2A-Extensions` header asks for kagent's human-in-the-loop
extension when the card lists it, and on every message that names the extension in its `extensions` (an answer to one of
its pauses) whatever the card lists.

## Replies

| The agent must | Otherwise |
|---|---|
| Answer a send with task events: `Task`, `TaskStatusUpdateEvent` and `TaskArtifactUpdateEvent`, or with one `Message` and no task (see below) | A stream that ends with neither names no task, so the outcome is unknown. The turn is sent again, and may run again, until it fails after 15 minutes with "the agent could not be reached" |
| Name the task and its `contextId` on its events | With no task ID there is nothing to follow: if the stream then ends before a full `Task`, the thread is told the request failed, and the turn is not sent again. With no `contextId` the context already recorded is kept; a thread's first turn has none, so its next turn starts a new context |
| Create a `contextId` on a thread's first turn, and keep it for every later turn | A different `contextId` posts "The agent no longer has this conversation" and the new context is kept |
| Put the answer in **artifact text parts**, or in the text parts of a `Message` reply | Text in the status message or in `history` is not shown. A task that completes with no artifact text posts "The agent finished without any output." |
| End the stream with a full `Task`, or a `TaskStatusUpdateEvent`, in a terminal, `input-required` or `auth-required` state | The stream is treated as dropped: a second later, `GetTask` reads the task, and `SubscribeToTask` follows it if it is still running |
| Send an event at least every 5 minutes, including during a long tool call | The stream is cut and the task followed again with `GetTask` and `SubscribeToTask` |
| Run one task per context at a time | While a task of the session is recorded, no new turn is sent to it; replies queue behind it |
| Treat a repeated `messageId` as the same turn | A thread's first turn has no context to look in, so after an unknown outcome it is sent again with the same `messageId`, and an agent that does not drop it runs it twice |

When the stream closes normally after such a status update, without the full `Task`, Agent Kourier reads the task with one `GetTask`
at once and takes it as the final one. If that call fails, or the task it returns is in none of those states, or the stream ended
with an error, the stream is treated as dropped.

A reply to a new turn that is one `Message` from the agent (role `agent`) with no `taskId` is the whole turn: its text
parts, joined, are the answer, and the turn ends as completed. A message with no text posts "The agent finished without
any output." The message's `contextId` is kept for the next turn (a message with none leaves the context as it was), and
no task is recorded, so there is nothing to follow, stop or look up. Any other `Message` is ignored: one that names a
task, one from the user, one after the agent has named a task, and any reply to an answer to a question. An answer
replied to with a message alone has an unknown outcome, and the paused task is read with `GetTask`: if it still waits for
input, the thread is told the request failed and the question stays open for another answer. A turn whose stream drops
before the message arrives has an unknown outcome, and since no task holds its `messageId` a lookup finds nothing: it is
sent again and may run twice. If Agent Kourier stops after the message arrives and before it is posted, the answer is
lost once the turn has left the queue; stopped earlier than that, the turn's outcome is unknown and it is handled the
same way as a dropped stream.

An artifact update with `append: true` adds to that artifact's text; any other replaces it. `lastChunk` is not read.
An update with no text part leaves the artifact's text as it was, so to take text back, replace it with an empty text part.
An artifact whose text is empty is left out of the message.
A task snapshot (from `GetTask`, `SubscribeToTask` or a replayed send) whose text for an artifact is a prefix of the text already
received is taken as older and skipped, so a take-back that is missed when the stream drops is not applied.
File parts are never shown. Data parts are not shown either, except the tool call and tool result parts below.

### Tool calls and results

A2A defines no tool call, so Agent Kourier reads the conventions of the frameworks that write one. On every dialect, a data
part is a tool call or a tool result when its metadata holds exactly one of these keys with the string value
`function_call` or `function_response`:

| Metadata key | Written by |
|---|---|
| `adk_type` | Google ADK's A2A converter |
| `kagent_type` | kagent 0.9.x, which uses that converter with its own prefix |
| `kagent.dev/a2a/part-type` | kagent 1.0 |

The part's `data` must be an object with a non-empty string `name`. Its `id` is the call's ID, which the result repeats,
and a call's `args` and a result's `response` are its arguments and what it returned. Any other part stays unread,
including a marked part with no `name`, one that holds two of the keys, and ADK's other types (`tool_call`,
`tool_response`, `executable_code` and `code_execution_result`).

Agent Kourier reads these parts in three places. ADK's newer converter and kagent 1.0 put them in the artifacts of a
task; ADK's older converter and kagent 0.9.x put them in status messages.

- The artifacts, in every chunk and in every task snapshot (`GetTask`, `SubscribeToTask`, a replayed send).
- The message of a status update, when it is the agent's own (role `agent`) and the task is `working`. A task snapshot's
  status message is read on the same terms.
- The agent's own messages in the `history` of a task snapshot since the last message of the user, whatever the task's
  state, since a history message carries none. That is how a call or result sent while the stream was down, or before a
  restart, is found. The messages before the user's last one are an earlier turn's, and are not read. A pause message in
  the history that holds a marked tool part would show as a step too.

The text of a status message is still not shown. A part is delivered once, and what counts as a repeat depends on where
the part is:

- In an artifact, a chunk with `append: true` adds parts after the ones already sent, so each is new even though its
  index restarts at 0: ADK's default output is one artifact for a whole run, and each call and result in it is such a
  chunk. A chunk that replaces the artifact, and a task snapshot, hold the parts again, wherever the replace puts them, and
  a part is new only past the number of parts that say the same that the artifact has delivered. Two calls that say
  the same, with no call ID, are two calls.
- In a status message, a part is its message's `messageId` and its index. A message with no `messageId` is told by what
  it says.
- A part found in an artifact and again in a message is one part when it says the same, and two when it does not. A part
  of the same kind and call ID that says something else is delivered as another part; the card is still decided by the
  first result it gets, because a step finishes at the first result of its call and a later result changes nothing.
- A task snapshot is history and the stream is the news. A snapshot's result for a call whose result the stream already
  delivered from the same source (artifacts, or messages) is ignored, and of several results of one call in a snapshot
  the last counts.
- An answer to a paused task (a question or an approval) is about a task that goes on, whose snapshots hold the paused
  turn's calls and results too. Before it sends the answer, Agent Kourier reads the task with one `GetTask` and counts
  every tool part in it as delivered, so the answer shows its own steps and not the paused turn's again. A task that
  cannot be read is answered without it. A follow after a restart has no such snapshot, so it reads only the history
  since the user's last message, and the artifacts of a task that went on from an earlier turn replay that turn's
  calls and results.
- Two artifacts of one source that hold the same call are two calls. This matters if an agent sends a call in a partial
  artifact and again in the main one; no agent measured does.

A result is delivered after its call. When a result arrives in the stream for a call that the send has not delivered,
and the send answers a paused task, Agent Kourier delivers the call it finds in the task it read before sending,
then the result, so the step begins where the call was made and not after the text. Only that call is taken from the
task, so the error that stood for the pause is not delivered. When the call is nowhere, the result waits for it, and is
delivered when the send ends, or when it fails. A send that fails and is followed starts again from the task's snapshot,
so a follow may deliver a result again; the card keeps the first.

Each tool call is a step card in a chat thread, finished when its result arrives, and a failed one if the result has an
`error` entry that holds something or an `isError` entry that is true. Structured tool approvals are separate: they need
the pause payload of kagent's human-in-the-loop extension, which `kagent-v1` always reads and `a2a` reads when the agent
card lists the extension.

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

On `kagent-v1`, the question is kagent's human-in-the-loop payload in the status message's metadata. On `a2a`, a status
message that names that extension in its `extensions` and carries the payload is read the same way. Any other question
on `a2a` is a status message made only of non-empty text parts, with a `messageId` or a status `timestamp` that differs
from the last question's. Anything else in `input-required` cannot be shown, so the task is cancelled and the thread is
told. The answer to a text question is one text part sent to the paused `taskId` and `contextId`.

## Structured questions and tool approvals

Questions with choices, several questions at once, and tool approvals use kagent's human-in-the-loop extension,
`https://kagent.dev/extensions/hitl/v1`. kagent 1.0 speaks it. Any other agent gets them on the `a2a` dialect by
speaking it the same way:

| The agent must | Otherwise |
|---|---|
| List `https://kagent.dev/extensions/hitl/v1` in the card's `capabilities.extensions` (`required` may be true) | Requests carry no `A2A-Extensions` header for it, so an agent that sends its structured pause only when asked (as kagent does) sends text alone |
| Pause in `input-required` on a task with an ID, with a status message whose `extensions` names the URI and whose `metadata` holds the request under the URI as key | The pause is read as a text question if its parts are text alone, and as unusable otherwise: the task is cancelled and the thread told |
| For a tool approval, send `{"type": "tool_approval_request", "hint": "...", "tools": [{"id": "...", "call_id": "...", "name": "...", "args": {...}}]}`: 1 to 20 tools, each with an `id` | The pause is unusable. A name or argument that Agent Kourier's cleaning would change, or that does not fit in one message, also makes it unusable, because the approver would not see what runs |
| For questions, send `{"type": "ask_user_request", "id": "...", "hint": "...", "questions": [{"question": "...", "choices": [...], "multiple": false}]}`: an `id`, and 1 to 20 questions of at most 100 choices each. No choices (`[]` or none) asks for free text | The pause is unusable |
| For a pause a sub-agent raised, add `"nested": {"subagent_name": "...", "task_id": "...", "context_id": "...", "tools": [...]}`, with the sub-agent's name and task. A nested approval needs exactly one outer tool, with an `id`, and decides the nested tools. A nested question needs exactly one nested tool named `ask_user`, with an `id`, which the answer echoes | The pause is unusable |
| Take the answer on the paused task, and resume it | A JSON-RPC `-32603` to the answer cancels the task and tells the thread |

The answer is a user message to the paused `taskId` and `contextId`, with `extensions` naming the URI, the
`A2A-Extensions` header naming it whatever the card lists, the actor under `agentkourier.dev/actor`, and one text part.
Its payload, under the URI in `metadata`, is:

- For a tool approval: `{"type": "tool_approval_response", "approvals": [{"id": "...", "approved": true}]}`, one entry
  per tool, by its `id`, in the request's order. A denied tool has `"approved": false` and a `rejection_reason`: "denied
  by an approver in chat" for a person's denial, "denied by agent-kourier: no one decided in time" at the timeout, and
  "denied by agent-kourier: this Binding does not allow tool approvals" when the Binding sets no
  `interactions.toolApprovals`. The text part is `approved`, or `rejected` when any tool is denied.
- For questions: `{"type": "ask_user_response", "id": "...", "answers": [{"answer": ["..."]}]}`, echoing the request's
  `id` (for a nested question, the nested tool's), with one entry per question in order, each holding the chosen choices
  or the free text. The text part is each question's answers joined with `, `, and the questions joined with `; `.

The text part is not prefixed with the actor, as a turn's is. Agent Kourier checks every answer before sending it: each
question answered, each answer one of its choices when there are any, each tool decided once.

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

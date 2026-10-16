# WP-02 spike: kagent 1.0.0-alpha5 over A2A

The spike code (`spike/`) is no longer in the repository; this page is the record of what it measured.

Run 2026-09-29 (round 1) and again after review (round 2) against the WP-01 full tier (kagent
1.0.0-alpha5, Go ADK runtime, mockllm v0.1.0) with `spike/a2a/run.sh`. Raw JSON is in
`test/fixtures/kagent/<case>/`, one `NN-step.{request.json,response.json|response.sse,meta.json}`
per call; tokens are never saved. The spike talks raw JSON-RPC over a `kubectl port-forward` to
port 8083 (not from a labelled pod: the payloads do not change behind the front door). Ids and
timestamps change between runs, the shapes do not: two earlier full runs, made before some checks were added, agreed with the final one.

**The run asserts every outcome in this document.** Each case records its checks (308 in the
final run) and fails if any differs: HTTP status, JSON-RPC error code, message and `ErrorInfo`
reason (`-32004`, `-32603`, `-31403`), task states, whether an error arrives mid-stream, the
replay's same task and context, stream shape and the replace-or-append text rule. A wrong code or
state fails the run, so `passes` means the outcomes match, not that no transport error
occurred. This is the acceptance oracle for the stub agent (WP-12). Everything below is
**measured** unless marked *inferred* or *source*. This file is the plan's
`docs/spikes/wp02-a2a.md`; Q1 is answered here.

## What Agent Kourier must know (headline findings)

1. **kagent does not validate ask-user replies the way spec 6.3 says.** A reply with fewer
   answers than questions, or an answer that is not one of the choices, is accepted and the
   task completes. An empty answer list or a wrong request id returns `-32603` and leaves the
   task in `TASK_STATE_WORKING`. Both were still `WORKING` 20 s later, and then a valid answer,
   a plain-text message to the task, and a new turn in the context (no `taskId`) each got
   `-32004`; `CancelTask` cleared it (`CANCELED`). Waiting longer than 20 s, a
   controller or agent restart, and other methods were not tried, so "only `CancelTask`
   clears it" is what was measured, not a proof. A tool-approval reply with no decisions is
   treated as a plain user message, so the tool never runs; an unknown approval id gives
   `-32603` and the task was `WORKING` 3 s later (20 s not measured for approvals).
   `BuildResponse` must therefore refuse anything incomplete or off-list itself, and use
   `CancelTask` as the recovery for a wedged task.
2. **`-32603` arrives as an SSE event, after a `WORKING` `statusUpdate`, over HTTP 200.** The
   stream opens normally and the error is its last event (`ErrorInfo` reason `INTERNAL_ERROR`).
   WP-27 must treat a mid-stream error event as the outcome of the send, not only an HTTP or
   transport error. `-32004` on `SendStreamingMessage` is also HTTP 200 over SSE, but as the
   only event.
3. **A plain-text reply to a paused task is not rejected**: it becomes a new user turn and the
   pending question is dropped. This confirms the queue rule of spec 8.2.
4. **The HITL payload only arrives if the extension is requested.** Without the
   `A2A-Extensions` header the pause is still `INPUT_REQUIRED`, but the structured metadata is
   absent. For ask-user the status text is the question; for a tool approval it is ADK's hint
   sentence ("Please approve or reject the tool call add_numbers() by responding with a
   FunctionResponse with an expected ToolConfirmation payload."). Whether a reply needs the
   header too was not measured.
5. **Q1: a2a-go negotiates it; raw JSON-RPC is not needed** (below). The activator is what puts
   the header on the wire, and only when the card lists the extension (measured).
6. **A replay of an in-flight message is a live stream of the same task**, not an error, with no
   second model turn visible (case 3a). A message rejected with `-32004` can be resent unchanged
   after the task ends (case 4).
7. **A shared sub-agent's ask-user is flat**, not nested; true nesting could not be produced.

## Cases

Method names are A2A 1.0 (`SendStreamingMessage`, `GetTask`, `SubscribeToTask`, `CancelTask`).
Streams are SSE: `id:` and `data:` lines, each `data` a JSON-RPC envelope whose `result`
holds one of `task`, `statusUpdate`, `artifactUpdate`. `GetTask` and `CancelTask` return the
task directly under `result`. Below, `shape` lines are the spike's own run-length summary of a
stream (`artifact+append x7` is seven `artifactUpdate` events with `append: true`).

**1. `session`.** A send with no `contextId` opens with **two** `task` events, both `SUBMITTED`
and both carrying the new `contextId`; the second adds `metadata` with ADK's marker
`https://google.github.io/adk-docs/a2a/a2a-extension/: true` (later events carry it too).
Then `statusUpdate` `WORKING`, `artifactUpdate` (text, then a `lastChunk` copy), and a final
`task` `COMPLETED`. Shape: `task:SUBMITTED x2, statusUpdate:WORKING, artifact, artifact+last,
task:COMPLETED`. A second send with that `contextId` and no `taskId` runs a new task in the same
context. Replaying the first send's `messageId` after it completed returned one event, a `task`
snapshot `COMPLETED` with the same `contextId` and `taskId` (the crash-recovery retry of spec 8.2
is idempotent). The snapshot holds one history message and one artifact, so no second model
turn is visible; the model server's request count was not read.

**2. `livestream`** (round 2). The slow agent streams "tick N" deltas a second apart, read to
the end through `SendStreamingMessage`. Shape: `task:SUBMITTED x2, statusUpdate:WORKING,
artifact, artifact+append x7, artifact+last, task:COMPLETED`. The first chunk ("tick 1 ") is an
`artifactUpdate` **without** `append`; ticks 2 to 8 carry `append: true`; then one
`artifactUpdate` with `lastChunk: true` holds the **full** text and no `append` (replace, do not
append); then the `COMPLETED` task. A client that adds `append` chunks and replaces on the
others reproduces the full text exactly. Append and replace apply per `artifactId`. Tool calls
and tool results are not part of the text artifact: each arrives as its own data-part artifact
(`function_call`, `function_response`), a single `artifactUpdate` with `lastChunk: true` and no
`append` (see `approval/`).

**3. `reconnect`.** Drop the stream at the first tick. `GetTask` right after returns `WORKING`
with the text so far, `"tick 1 "` (measured in round 2; round 1 only printed the state).
`SubscribeToTask` opens with a `task` snapshot (`WORKING`, text so far), then `artifactUpdate`
events with `append: true` for the rest, then the `lastChunk: true` artifact with the full text
and no `append`, then the `COMPLETED` `task`. Final `GetTask` text equals the streamed text.
Snapshot plus deltas has no gap or overlap (the run compares the assembled text with the full
text).

**3a. `inflight`** (round 2, the crash window of spec 8.2). The stream drops mid-task and the
same message (same `messageId`, no `contextId`) is sent again while the task still runs.
*Run A*: the replay is not an error. It is a live stream that opens with a `task` snapshot of
the same task and context in state `WORKING` with the text so far, then `artifact+append x7`,
`artifact+last`, and the `COMPLETED` task, so it is `SubscribeToTask` behavior. The final task
has one history message and one artifact, and the text is one run of eight ticks, so no second
model turn is visible (request count not read). `SubscribeToTask` on the task after it has
completed returns one `task` `COMPLETED` snapshot. *Run B*: dropping the replay after its first
event and calling `SubscribeToTask` gives the same snapshot, deltas, `lastChunk` and `COMPLETED`.
Spec 8.2 says the replay returns a snapshot that Agent Kourier follows with `SubscribeToTask`. That
holds for a completed task (one `COMPLETED` snapshot); for a running task the replay is itself
the live stream. Either way the client rule (append on `append: true`, otherwise replace)
gives the full text.

**4. `busy`.** With a task `WORKING`, both `SendMessage` and `SendStreamingMessage` return
HTTP 200 and JSON-RPC error `-32004`, message `resource conflict`, `ErrorInfo` reason
`UNSUPPORTED_OPERATION` (a single event over SSE for the streaming form). `CancelTask` returns
the task as `CANCELED`. The same `-32004` also answers any send to a wedged task (finding 1).
Round 2 resent the exact params (same `messageId`) that had been rejected: after `CancelTask`,
and again after the running task completed by itself, the resend was accepted and ran as a new
task in the same context, normally to `COMPLETED` (shape `task:SUBMITTED x2,
statusUpdate:WORKING, artifact, artifact+append x7, artifact+last, task:COMPLETED`). So a
rejected `messageId` is not remembered, and Agent Kourier's queue can resend it unchanged.
`-32004` carries no reason that marks it retryable; the code and message are the signal. (The
round 1 remark about `KAGENT_SEND_NOT_ACCEPTED` here was unsourced and is removed; that string
in the WP-01 report is a NetworkPolicy failure, unrelated to a busy session.)

**5. `askuser`** (extension header on). The agent's `ask_user` tool call ends the stream with
`INPUT_REQUIRED`; the status message has `extensions: [<uri>]`, a text part (the question), and
`metadata["https://kagent.dev/extensions/hitl/v1"]` =
`{"type":"ask_user_request","id":"adk-…","questions":[{"question","choices":[…],"multiple":bool}]}`.
The answer is a user message with the same `taskId` and `contextId`, `extensions`, and
`metadata[uri] = {"type":"ask_user_response","id":<request id>,"answers":[{"answer":["…"]}]}`,
one entry per question in order. Results, each on a fresh pause (see `askuser/`), each asserted:

| Reply | Result |
|---|---|
| Correct | `COMPLETED` |
| Plain text, no payload | `COMPLETED` as a new user turn; question dropped |
| Empty `answers` | error `-32603` "ask_user decision is missing approval correlation or answers", mid-stream; task stays `WORKING` |
| Wrong request `id` | same error, same stuck task, `WORKING` at 3 s and at 20 s |
| Answer not in `choices` | `COMPLETED` (accepted) |
| 1 answer for 2 questions | `COMPLETED` (accepted) |
| Two questions, multi-select plus free text | `COMPLETED` |

A wedged task (empty answers, or wrong id): `WORKING` after 20 s; a good answer, a plain-text
message with the `taskId`, and a new turn without it each get `-32004`; `CancelTask` gives
`CANCELED` and `GetTask` agrees.

**6. `nested`.** The parent delegates to a shared sub-agent (`transfer_to_agent`) that asks
the user. The request is the same flat `ask_user_request`; the sub-agent's name appears only in
the earlier `transfer_to_agent` function-call artifact. The `nested` object exists in kagent's
types (*source*: `go/api/a2a/hitl.go`: `subagent_name`, `task_id`, `context_id`, `tools` on both
request kinds) and is for remote A2A tools. alpha5's AgentTemplate cannot configure one:
`agentRef` (Dedicated) is rejected at compilation (*source*: `a2a-subagents.md`; I did not try
it). So the nested shape is **not captured**; WP-26 should code it from the source types.

**7. `approval`.** An MCP tool with `requireApproval: true` (a stdlib Python server, `support.py`)
pauses with `{"type":"tool_approval_request","hint":"Please approve or reject the tool call
add_numbers() …","tools":[{"id","call_id","name","args"}]}`. The reply is
`{"type":"tool_approval_response","approvals":[{"id","approved","rejection_reason"?}]}`. Approve:
the tool runs (`output: "8"`). Reject: the tool result is `tool "add_numbers" call is rejected`;
the `rejection_reason` text did not reach the model. Empty `approvals`: handled as plain user
text (the tool is not run). Unknown `id`: `-32603` "tool approval response is missing id …"
mid-stream, task `WORKING` 3 s later. With the extensions header off (round 2), the pause is
`INPUT_REQUIRED` with ADK's hint as text and no structured payload. Note the `hint` is ADK
boilerplate, not the documented sentence.

**8. Q1.** `spike/a2a/sdkprobe` (its own module, so the repo takes no dependency) uses a2a-go
v2.6.0 (kagent vendors v2.5.0): resolve the card, `a2aclient.NewFromCard`, `SendMessage`. The
returned `a2a.Task` carries the request in `Status.Message.Metadata[uri]` as a `map[string]any`;
a reply built with `Message.Extensions` and `Message.Metadata` completed the task. Round 2 ran
the round trip three times and recorded the `A2A-Extensions` header on the wire (a
`RoundTripper`; `Authorization` is never printed; `q1-sdk/output.txt`):

| Client | Header on the wire | Payload | Result |
|---|---|---|---|
| `a2aext.NewActivator(hitl)`, card lists the URI | `https://kagent.dev/extensions/hitl/v1`, on both requests | present | answer completes the task |
| no activator, same card | none | absent | `INPUT_REQUIRED`, no payload |
| activator, card with `capabilities.extensions` emptied | none | absent | `INPUT_REQUIRED`, no payload |

So the activator is what adds the header, and only when the card lists the URI (measured; the
activator source, `a2aext/activator.go`, does the same: *source*). What the SDK does **not**
provide is typed HITL payloads: Agent Kourier defines its own structs and decodes the map. Two SDK
traps: `agentcard.Resolve` fetches a base URL with a path verbatim, so pass the full
`…/.well-known/agent-card.json` URL; and the card advertises the in-cluster URL
(`http://kagent-controller.kagent.svc:8083/agents/…`), so a client outside the cluster must
override it. The card also needs a bearer token (HTTP 401 without one: seen with curl during round 2, no fixture; run.sh's readiness wait relies on it). Answer: **use a2a-go; no
raw JSON-RPC for HITL.**

**9. Q3 (partial).** The cluster runs `controller.auth.userIdClaim: email`. A session made by the
Dex identity is continued by a token with the same `email` and a different `sub`, and refused
(`-31403` "permission denied", `ErrorInfo` reason `UNAUTHORIZED`, for both send and `GetTask`)
for a different `email`, for a different email and sub, and for a `sub`-only token. So the
owner is the `email` claim in this configuration (measured). That the `sub`-only token is
refused *because* a missing `email` falls back to `sub`, a different owner string, is
*inferred* from the refusal, not observed. With the chart default (`userIdClaim` empty) the
owner would be `sub` (*source*: `oidc-proxy-authentication.md`; not measured). Caveat: the
"other" identities were unsigned forged tokens, because the controller never verifies them
(WP-01), so this shows claim comparison only. Each identity call's `meta.json` records the
claims it used (`identity.claims`, and `tokenKind` `signed` or `unsigned`; never the token), so
the stub can replay `-31403` per identity. Dex's `sub` is an opaque base64 blob; an email
owner reads better in logs. WP-03 should repeat this with a second real identity behind the
verifying front door.

## Not covered

Tool approvals with several tools in one pause; `nested` (above); a wedged task after more than
20 s, after a restart, or recovered by anything but `CancelTask`; a wedge from an unknown
approval id past 3 s; whether a reply needs the extensions header when the pause was sent
with it; the model server's request count for a replayed message (only the task's artifacts
and history were inspected); `SubscribeToTask` rate after a task ends; behavior across a
controller restart; timeouts on a paused task; a same-`messageId` replay that carries a
`contextId` while the task runs (the crash window for a thread reply; WP-03 measures it).

## Reproduce

`make e2e-full-up`, then `spike/a2a/run.sh` (about 2 minutes; extra arguments go to the spike,
for example `-case session,busy`, and skip the Q1 probe). It applies `spike/a2a/manifests.yaml` (a
second mockllm scripted in `mocks.json`, the slow model and MCP server in `support.py`, four
agents), waits for Dex and the agent card, port-forwards, rewrites the fixtures, then runs the
SDK probe. The exit status is non-zero if any check fails.

No Dex (for example against the stub agent): `go run ./spike/a2a -base <url> -no-auth -out
<dir>`, or `-token <jwt>` to send a given bearer token. The identity case needs a token with an
`email` claim; without one it is reported as SKIP (and listed at the end), not ok, so a stub that
lacks `-31403` support cannot pass unnoticed. Each case wipes its `-out` directory, so `-token`
and `-no-auth` require an explicit `-out` (a scratch directory); the default holds the recorded
fixtures.

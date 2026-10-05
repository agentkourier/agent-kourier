# DEMO-1: the `a2a` dialect against an agent in a plain pod

Run 2026-10-05 in the fast tier (kind, courier from this branch, the stub agent in an ordinary
Deployment with `-require-auth`). It answers two questions for the public demo, whose reference
agent is a config-driven agent in a plain Deployment that courier reaches through the generic
`a2a` dialect, with no kagent, Substrate or Dex:

1. Does the `a2a` dialect work against such an agent for the text scenarios? Yes: S1, S4 and S5.
2. What happens when the agent no longer knows a thread's `contextId`, as after a pod restart with
   no volume? Does the demo agent need a persistent volume?

Every claim below is marked **measured** (in the tier, with the test named), **code** (read from
the code, with the reference) or **not determined**. The stub is not a real agent: where a result
depends on how the stub behaves, it says so.

## 1. The path works for the text scenarios (measured)

The fast tier now has three `a2a` AgentBackends (`hack/kind/fast/courier/a2a/agentbackend-a2a.yaml`),
each the stub's exact agent URL, for example
`http://stubagent.e2e.svc.cluster.local:8080/agents/kagent/scenario-gated`, and Bindings on them
(`binding-a2a-*.yaml`) that send the static token of Secret `courier-agent-token`. There are three
backends, not one, because the dialect uses the URL exactly and one URL is one agent: S1 and S4 use
`scenario-gated`, S5 uses `test-agent`, and the context measurement below uses `scenario-memory`.

| Scenario | Test | Result |
| --- | --- | --- |
| S1 | `TestS1_AReplyStreamsIntoANewThreadOverTheA2ADialect` | passes |
| S4 | `TestS4_AReplySentWhileATaskRunsIsQueuedAndAnsweredAfterItOverTheA2ADialect` | passes |
| S5 | `TestS5_AReplyPostedBeforeACrashIsNotPostedAgainOverTheA2ADialect` | passes |

The scenario bodies are the existing ones (`s1`, `s4`, `s5` helpers), run on channels of their own.
`test/kindmanifests` (`TestA2ABindingsReachTheStubAgentByItsExactURL`) pins the dialect, the exact
URL and the token reference of each channel, so a Binding that quietly stayed on `kagent-v1` would
fail `make test`, not pass the scenarios by reaching the same stub the old way.

Why the stub works as a generic A2A agent without a change (read from code, then confirmed by the
passing scenarios): its card has `capabilities.streaming: true`, which the pool requires
(`internal/agent/pool.go`), and lists the kagent HITL extension without `required`, so
`ConnectEndpoint` (`internal/dialect/kagentv1/connect.go`) does not refuse it. The dialect
overwrites the card's interface URL with the configured one. The stub answers 401 without a bearer
token, so a thread getting a reply shows the token was sent. The token is an unsigned JWT with an
email claim, which is what the stub (like kagent with `userIdClaim: email`) wants; courier sends the
Secret's value as it is and does not parse it.

Not covered: S3 (a question). The stub's questions are kagent's structured ones, and the generic
dialect accepts only text questions (`internal/dialect/generic/generic.go`, `ParseInteraction`).
Nothing here says how a real plain-pod agent asks a question.

## 2. A thread whose `contextId` the agent no longer knows

Courier stores one `contextId` per thread, learned from the agent's first reply, and sends it on
every later turn (`internal/session/runner.go`, `t.sess.ContextID`). What happens when the agent
does not recognise it depends on how the agent reacts. There are two ways.

### (a) The agent accepts the unknown `contextId` and starts a fresh context, with no error

**Measured in the tier** with the stub, by `TestA2A_APlainPodThatRestartedAnswersAThreadFromAFreshContext`.
The test runs a thread on `scenario-memory`, which says which turn its context has had and quotes
the earlier ones. Two turns by two users give "Message 2" quoting the first, which shows the dialect
keeps the context. The test then deletes the stub's pod (it keeps its sessions in memory, as an agent
without a volume does) and waits for the replacement to be Ready and for the old pod to be gone, so that the third turn does not meet a pod that is still shutting down. The third turn in the same thread
produced this thread (as the fake Slack shows it; the fake is not Slack, and the text shown is the
renderer's):

```
UCTXA: <@UFAKEBOT> alpha-...
bot:   Message 1. Earlier: none.
UCTXB: bravo-...
bot:   Message 2. Earlier: "[Slack: Scenario UCTXA (UCTXA)]\n<@UFAKEBOT> alpha-...".
UCTXA: charlie-...
bot:   Message 1. Earlier: none.
```

So courier delivered the turn, the agent answered, and **the thread shows an ordinary reply from an
agent that has silently forgotten the conversation**. Courier posts no notice (the test asserts the
thread holds exactly three replies of the bot for three seconds after the third). The reason is in
the code: courier raises its "no longer has this conversation" notice only when the agent answers in
a *different* `contextId` than it sent (`runner.go:683-686`, `NoticeSessionGone`; text in
`internal/render/live/sink.go`, "The agent no longer has this conversation, so it has lost its
memory of it. Your next message starts a fresh one."). The stub adopts the `contextId` it is given
(`test/stubagent/agent.go`, `session`; the stub's own comment marks that guess UNCONFIRMED against
kagent), so the ids match and courier has nothing to compare. An agent that opens its new context
under the id it was given looks, to courier, exactly like one that remembers.

Variant (a'), an agent that accepts the turn but answers in a *new* `contextId`: **code**, with
**unit test** `TestReplacedContextIsAdoptedAndReported` (`internal/session/outcome_test.go`). Courier
adopts the new id and posts the notice above once. Not measured in the tier, because the stub cannot
be made to do it without a change to the stub.

Not measured: which of (a) or (a') a real ADK `llmagent` served over A2A does for an unknown
`contextId`. That depends on its session service. It should be measured against the reference
agent in DEMO-2 and not assumed from the stub.

### (b) The agent returns an error

**Not measured in the tier.** The stub has no path that answers a send naming an unknown `contextId`
with an error, and adding one is a change to the stub, which this package does not make. Everything
below is **code**, read from the files named and from existing unit tests, and none of it was run
against an agent that does this. It has two parts, because it depends on how the error arrives.

**An error that arrives as a JSON-RPC error on the send** (the case this section assumes, for
example `-32602` invalid params, or `-32603` internal error, on a send that is not an answer):

- It arrives before any event, so `Agent.send` takes the `!dropped` branch (`internal/agent/agent.go:127-132`,
  `dropped` is false for an agent's own answer) and `sendError`/`typed` (`internal/agent/errors.go:118`,
  `:189`). `typed` gives a special type only to `-32004` (busy), the unauthorized error (forbidden),
  HTTP 401/403 and, for answers only, `-32603`. Any other code stays untyped.
- An untyped error reaches `Manager.unsent` (`runner.go:1048`), which has no case for it and returns
  `ended, err` (`runner.go:1091`) **without posting a notice and without marking the turn
  delivered**. It also clears neither `m.attempted` (set at `:480`) nor `m.fresh`. `Manager.run` logs
  `session turn failed` at Error and retires the runner (`runner.go:188-206`). Unit tests:
  `TestRealFailureStillLogsAtError` (`retrylog_test.go`), whose comment says "the turn stays queued, and
  the runner gives up on this wake", and the "a failed send" case of
  `TestAgentErrorTextIsRedactedAndCappedInTurnFailureLogs` (`errorlog_test.go`), which sends a `-32603`
  as a first send's error and finds the Error line. Neither asserts that no notice is posted: that part
  is read from `unsent`, which has no call that posts one for this error.
- The turn stays queued. It is tried again when another message arrives in the thread (`Submit` calls
  `wake`, `intake.go:82`) or when courier restarts (`Recover` starts the runner, `recover.go:85`). The
  oldest queued turn goes first, so every later message queues behind it.
- **Each retry is a lookup and then a send** (`runner.go:475-509`). The turn is a resend, because
  `prev.id == r.ID` (`:479`; `:480` recorded it on the failed try) or, after a courier restart, because
  `m.fresh` is empty. A resend into an existing context first calls `FindTask`, a `ListTasks` on the
  thread's `contextId` (`runner.go:489`, `internal/agent/find.go`):
  - If the lookup **errors** (plausible for an agent that rejects an unknown `contextId`, or has no
    task listing), the flow is `Manager.retry` (`runner.go:493`, `:1120-1133`) and nothing is sent: the
    runner stays in its backoff loop (`:197`), posts `NoticeRetrying` ("I can't reach the agent right
    now, and I'm trying again.") on the second failure in a row (`:1129`), and after `SendGiveUp`
    (15 minutes by default, `session.go:297`) calls `fail` (`:1126`, `:1146`). `fail` posts `NoticeFailed`
    ("This request failed, and I won't try it again.", `:1153`), marks the turn delivered (`:1156`) and
    lets the queue go on.
  - If the lookup **returns an empty list** (what the stub does for an unknown `contextId`), the turn is
    sent again, gets the same error, and ends in `unsent` as above: silent, still queued, until the next
    wake.
  - If the lookup finds a task holding the turn, the turn is followed and not sent (`:498`). That does
    not fit an agent that rejected the send, so it is not expected here.
- So the Slack thread shows one of two things, depending on that lookup: **nothing from courier** (the
  empty list), or **a retry notice and then, after about 15 minutes, a failure notice** (the lookup
  errors). Which one a real agent produces was not determined. Whether the "waiting" reaction
  (hourglass) stays on the message was not determined either. In the silent case the only trace is
  courier's Error log line, which the user does not see.

**An error that arrives as an HTTP status on the send** (a 4xx or 5xx from a proxy or from the agent's
server, not a JSON-RPC error) does not take this path. It is not an agent answer, so `dropped` is true
and `Agent.send` returns `beforeAnyEvent` (`agent.go:134`, `errors.go:168-175`): a 4xx is `ErrNotSent`,
which `unsent` retries (`runner.go:1083`, `m.retry`), and anything else is `ErrOutcomeUnknown`, which
`unsent` records and retries (`:1078-1082`), the next try starting with the `FindTask` lookup above. Both end in the retry notice and, after
`SendGiveUp`, the failure notice. A 401 or 403 is `ErrCredentialsRejected` and fails the turn at once
with a notice (`:1050-1052`). Not measured.

**Against the spec.** Spec 8.2, "Thread lifetime": "If the backend reports that the session no longer
exists, courier says so in the thread, clears the stored context, and the next message in the thread
starts a fresh context." It names the signals courier takes for that: "`TaskNotFound` for a recorded
task, or a first response whose `contextId` differs from the stored one" (marked UNCONFIRMED for
kagent). An error on the send that names an unknown `contextId` is a report from the backend that
courier does not recognise as either, so in the silent case path (b) is a gap against the spec, not
only a product question: the spec says the thread is told, and here it may not be. This is the
follow-up below, not a fix.

### What was not measured

- Any real agent's reaction to an unknown `contextId`: only the stub's, which is its author's guess.
- The error path (b) in the tier. It is read from code and unit tests only.
- The variant (a') in the tier.
- Whether Slack (not the fake) renders any of this differently.
- A pod restart's effect on a turn running at the time it happens: the test restarts the pod between
  turns, with no task in flight.
- Courier's own behavior on a connection that a restart cuts: the test waits until the old pod is gone
  before the third turn, so the turn goes to the new pod, and a restart while courier has a turn in
  flight or a stream open is not covered.

## 3. Conclusion

A demo agent in a plain Deployment does not need a persistent volume for courier to work: the
`a2a` dialect, the exact URL and a static token carry S1, S4 and S5, and courier keeps nothing about
the conversation but the `contextId`. But **without a volume the agent loses its conversations
whenever its pod restarts, and what a user then sees depends on the agent, not on courier**. If the
agent accepts the old `contextId` and starts afresh, which is what the stub does (measured), the user
sees a normal answer from an agent that has forgotten the thread, with no warning, so a follow-up such
as "and what about the second one?" is answered without its context. If the agent answers the send
with an error, the user sees one of two things, both read from the code and neither measured: nothing
from courier, if the agent's task listing returns an empty list, or a "trying again" notice and then,
after about 15 minutes, a failure notice, if the listing errors. Which a real agent produces was not
determined, and the silent case is a gap against the spec's rule that the thread is told. A demo whose
script includes a follow-up in a thread should therefore give the agent a volume for its session store
(a SQLite file, as `examples/ax` has) or avoid restarting the pod during the demo, and DEMO-2 should
measure what its own agent does with an unknown `contextId`, and with a task listing for it, before
the demo relies on either reading.

## Follow-ups (not done here)

1. **A JSON-RPC error on a send that courier does not recognise can leave the thread silent** (path
   (b)), when the retry's `FindTask` returns an empty list: `Manager.unsent` ends the turn with no
   notice and leaves it queued. Spec 8.2 says the thread is told when the backend reports that the
   session no longer exists, so this is a gap against the spec as well as a product question.
   `Manager.unsent` should end such a turn with a notice, or bound its retries, as the typed errors do.
   It changes `internal/session`, which this package does not touch; it needs a decision on which
   JSON-RPC codes are permanent, and whether an error on a send that names a `contextId` is the
   session-gone signal of 8.2. A test for it belongs under `internal/session`. First, which of the two
   outcomes in (b) a real agent gives should be measured.
2. **An agent that forgets and keeps the id is invisible to courier** (path (a)). The only signal a
   generic agent gives is its text. Whether courier should say anything is a product decision, and
   with the generic dialect there is nothing to compare.
3. Measure the reference agent (DEMO-2) with this test's pod restart, as the first check of whether
   it needs a volume.

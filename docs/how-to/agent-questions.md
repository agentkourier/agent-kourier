# Let the agent ask questions

For a team whose agent needs a decision from a person mid-task: at the end, the agent's question appears in the
thread, a channel member answers it there, and the same agent task carries on with the answer.

## Turn it on

In the Binding:

```yaml
spec:
  interactions:
    askUser: true
    timeout: 30m # (1)!
```

1. How long a question waits. The default is 30 minutes.

Without `askUser: true`, Agent Kourier cancels the agent's task and says so in the thread.

## What people see

=== "kagent 1.0"

    The `kagent-v1` dialect reads kagent's structured questions. A question with choices is shown as radio buttons, or
    checkboxes where the agent allows several (a select or multi-select above 10 choices, and a numbered list with a
    text box for the numbers above 100). A question with no choices is a text input. Every prompt has a **Submit**
    button. A question raised by a sub-agent is labelled with that sub-agent.

=== "Generic A2A"

    The `a2a` dialect shows an agent's text-only `input-required` pause as one text question, which you can
    answer with **Submit** or with a typed reply in the thread; its prompt says so. A pause with a part that is
    not text cannot be used, so the task is cancelled and the thread is told. In a Binding with
    `chat.threadReplies: mention`, that reply needs no mention.

## How an answer is taken

- Only a member of the channel can answer, and never a bot. An incomplete answer is turned away with a message only
  the person sees; nothing reaches the agent.
- The first complete answer wins. The prompt is updated to show who answered and when, and its controls are removed.
- The answer goes to the agent under the Binding's identity. The audit log records who answered, when and the
  answer's shape, never its text.
- A reply that is not an answer waits until the question is answered, times out or is cancelled, and is then sent as
  the next turn.
- When the timeout passes, Agent Kourier cancels the agent's paused task, closes the prompt and tells the thread.

## Tool approvals

Tool approvals are available for a Binding that uses the `kagent-v1` dialect. On the `a2a` dialect no approval prompt
is posted and nothing is approved, whatever `toolApprovals` says: kagent 1.0.0-alpha5, reached on that dialect, pauses
for an approval with text alone, and Agent Kourier handles that pause as a question whose reply is sent as plain text
(see [Connect any A2A agent](connect-a2a-agent.md#what-works-on-this-dialect)). Approvals are disabled by default.
Enable them with a separate policy from questions:

```yaml
spec:
  interactions:
    askUser: true
    toolApprovals: true
    approverGroups: [S0123456789] # optional Slack user group IDs
    timeout: 30m
```

`approverGroups` is optional; a nonempty list requires `toolApprovals: true`. When it is omitted or empty, any non-bot member of
the Binding's channel may decide an approval; the config loader warns and names the Binding. When groups are listed, a
press must come from a member of at least one listed group, a member of the channel, and a non-bot user. Groups gate tool
approvals only, so they do not restrict ask-user answers. The list accepts at most 10 Slack user group IDs, and rejects
empty or duplicate entries. Membership is read on each press in the order the groups are listed and is not cached. A
permanent group-read failure, such as an unknown group or missing Slack permission, ends that press with a fixed
ephemeral message, makes no decision and writes no audit row, and leaves the approval pending. The error log names the
Binding and interaction. A timeout, server error or rate limit keeps the press for retry; the first group-read error
stops the check even if a later group would contain the person, while a member found in an earlier group may decide.

When approvals are enabled, a one-tool prompt shows the tool name, its redacted arguments, and **Approve** and **Deny**.
A prompt with several tools shows each tool and its arguments with an Approve/Deny choice, plus **Submit**, **Approve
all**, and **Deny all**. The prompt refuses a tool name or argument that cannot be shown exactly, contains an unsupported
value, or does not fit in one message; Agent Kourier does not truncate an approval prompt. Secrets are replaced with
`[redacted]` before the prompt is stored and shown.

The first complete, authorized decision wins. An incomplete press is rejected with an ephemeral message and does not reach
the agent. Agent Kourier audits the decision before sending it under the Binding's service identity, then replaces the
prompt with an ending that shows each tool's Approved or Denied verdict and removes the controls. The audit records the
Binding, Slack user, time, counts and cleaned tool names; it never records arguments or rejection text. A denial uses the
dialect's fixed reason.

At the deadline (30 minutes by default), the sweeper audits `timed_out` before denying every tool through the kagent
dialect, then updates the posted prompt with a timed-out ending and queues a fixed notice. Recovery reuses that timeout audit; if it is missing, recovery writes it
before retrying the denial. A denial that the agent does not take causes the task to be cancelled on a later sweep.
If a person's recorded decision wins a race with the sweep after its timeout audit, the sweep appends a `superseded`
audit for that timeout. A failure to write the compensation is logged.

A permanently abandoned or oversized approval prompt is refused as unusable. Agent Kourier queues the fixed refusal
notice and closes the interaction before cancelling the task, so a restart can retry the cancellation without reporting
another timeout. A transient posting failure remains retryable.

A pinned kagent 1.0.0-alpha5 runtime check emitted one two-tool pause. Approving `add_numbers` and denying
`check_status` completed the task, and only `add_numbers` executed. The broker's end-to-end suite verifies this with fake Slack
and pinned kagent 1.0.0-alpha5.

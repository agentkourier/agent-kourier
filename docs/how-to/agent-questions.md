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

Without `askUser: true`, Agent Kourier refuses the agent's question and says so in the thread.

## What people see

=== "kagent 1.0"

    The `kagent-v1` dialect reads kagent's structured questions. Each question is shown with its choices as a select
    (a multi-select where the agent allows several), or as a text input when there are no choices, and a **Submit**
    button. A question raised by a sub-agent is labelled with that sub-agent.

=== "Generic A2A"

    The `a2a` dialect shows an agent's `input-required` pause as a question in text. A single question with no
    choices also takes a typed reply in the thread, and its prompt says so. In a Binding with
    `chat.threadReplies: mention`, that reply needs no mention.

## How an answer is taken

- Only a member of the channel can answer, and never a bot. An incomplete answer is turned away with a message only
  the person sees; nothing reaches the agent.
- The first complete answer wins. The prompt is updated to show who answered and when, and its controls are removed.
- The answer goes to the agent under the Binding's identity. The audit log records who answered, when and the
  answer's shape, never its text.
- A reply that is not an answer waits until the question is answered, times out or is cancelled, and is then sent as
  the next turn.
- When the timeout passes, the question is cancelled and the thread is told.

## Tool approvals

`interactions.toolApprovals` is reserved for approving an agent's tool calls from Slack, which is not built yet. Keep
it `false`. If an agent pauses for a tool approval anyway, Agent Kourier denies it, says so in the thread, and counts
it, because the Binding was declared to point at a read-only agent.

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
    answer with **Submit** or with a typed reply in the thread; its prompt says so. A pause with structured parts
    cannot be used, so the task is cancelled and the thread is told. In a Binding with
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

Approving an agent's tool calls from Slack is not built yet, and a Binding has no setting for it. If an agent pauses
for a tool approval, Agent Kourier denies it, says so in the thread, and counts it, because the Binding is meant to
point at a read-only agent.

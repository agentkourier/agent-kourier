# Threads and sessions

This page explains how a chat thread becomes an agent session, and what happens to it as people reply, the agent
works, Agent Kourier restarts, and time passes.

## A thread is a session

Agent Kourier maps each thread to one A2A context. The conversation key is the connection, the channel and the
thread's root message; it maps to the agent's `contextId`. Every turn in the thread is sent into that context, so the
agent sees the whole conversation and nothing is re-explained.

```mermaid
sequenceDiagram
    participant P as Person
    participant C as Channel thread
    participant K as Agent Kourier
    participant A as Agent
    P->>C: mention
    C->>K: message
    K->>K: store the session and queue the turn
    K->>A: send (new context)
    A-->>K: stream
    K-->>C: one message, edited as text arrives
    P->>C: reply in the thread
    C->>K: message
    K->>A: send (same context)
    A-->>K: stream
    K-->>C: answer in the same thread
```

The session row is written before the first call to the agent, and the context is filled in from the agent's first
response. A turn's message ID comes from the chat message itself, so the two events Slack delivers for one mention, and
a redelivery of either, are one turn.

## Who the agent hears from

Every turn runs as the Binding's service identity, whoever typed it. Agent Kourier puts the person's name and ID, behind a label that names the chat platform (`Slack:`), at the
start of the text they wrote. That prefix is informational, never authorization, and it is load-bearing: in kagent's
measured behaviour the agent's own earlier replies reach the model as user messages, so the prefix is what tells a person's message from the
agent's own output.

## Replies while the agent works

An agent runs one task per session at a time. A reply that arrives while a task runs is queued and sent when the task
ends, in order, one per turn. A reply while the agent waits on a question is held the
same way, unless the question takes typed replies, in which case the first reply is its answer.

By default, Agent Kourier adds an hourglass reaction to each accepted message. It stays while the message waits or the
agent processes it, and is removed when the turn ends or the agent asks a question and waits for an answer. A typed answer
gets its own reaction. Set `chat.acknowledgment.enabled: false` on the Binding to disable it, or set
`chat.acknowledgment.emoji` to another bare Slack reaction name such as `eyes`.

With `chat.threadReplies: mention`, only replies that mention Agent Kourier are turns, except a typed answer to the agent's pending question. The
others are ignored, and the next mentioned turn carries them to the agent as context, in a block marked as untrusted data.

## Output

With `output: live`, the answer streams into one message as the agent writes it. A dropped stream is reconciled by
asking the agent for the task and subscribing to it again. With `output: final`, the answer is posted once, when the turn
ends; nothing else is shown until then but the acknowledgment reaction, and `chat.progress` has no effect.

What a live reply shows of the agent's work depends on where the thread is and on `chat.progress`.

- **A chat thread** (a message someone sent, so Slack shows the stream to that person) shows a step card for each tool
  call, in the order the calls were made. The card begins when the call arrives and finishes when its result does. Only the
  `kagent-v1` dialect reports tool calls.
- **An alert thread** has no one to show a stream to, so one message is updated instead. A line of small print names the
  latest tool call: `Calling name(arguments)` while it is under way, `Ran name(arguments)` once its result has arrived,
  and `name(arguments) failed` if the result reports a failure. A call that has its result is shown only while the message
  has no text yet, and the text replaces the line.

`chat.progress` adds a sign of life for the seconds when the agent says nothing, which is when a model is deciding its next
step. It is one of three values:

| `progress` | Chat thread | Alert thread |
|---|---|---|
| `steps` | A card for each tool call. | The line for the latest tool call. |
| `steps-and-activity` (the default) | The same, and when 3 seconds pass with no new text and no tool call under way, a card titled `Working…` begins. The next tool call takes that card over, so it carries the tool's name and arguments from then on; text instead retitles it `Wrote the answer`. | The same, and `Working…` as a line of small print once 3 seconds pass with nothing new, in place of the line of a call that returned, until the next text or tool call. |
| `activity` | One card for the whole wait, begun at the first tool call or result, or after 3 seconds, and only before any text. Its title counts the tool calls that have a result, such as `Working… 12 steps, 3 failed`. At the first text it becomes `Worked`, with `Took 3 min 26 s, 16 steps (7 failed)`. | One line, `Working… (2 min, 12 steps, 3 failed)`, until the first text. |

The `activity` mode never shows a tool's name or arguments in the reply's cards and lines; a tool approval prompt, with
`interactions.toolApprovals`, still names the tool and shows its arguments. No mode shows the agent's reasoning: Agent Kourier does not read
it, and the `Working…` title says nothing about what the agent is doing. The 3 second delay is not configurable. The
`Working…` card or line is written once for a gap, and no other write follows the clock. A gap longer than the delay costs
at most two writes more than `steps` spends in a chat thread (the card's begin and, when no tool call takes it over, its
end), and one more message update in an alert thread. The `activity` card is written when a result changes a count, at most
once in a throttle window, and its line's elapsed minutes are refreshed only when an event makes a write.

In an alert thread, a turn that is silent for 3 seconds now posts `Working…` as its message. If the turn then ends with
nothing to show, or fails, that message is left saying `(no output)`; under `steps` such a turn posted nothing. A chat
thread whose stream was started for a card, and whose platform then refuses cards, is stopped with the same `(no output)`
when the turn ends empty, and is not left open.

A `Working…` card that no tool call took over ends under a title that says how, never as a tick on `Working…`. If text came
while it was open it is `Wrote the answer`. If the turn ends first, it is `Finished` for a completed task, `Waiting for your
answer` when the agent asks a question, and `Stopped`, shown as an error, when the task failed, was cancelled or was
rejected. The `activity` card ends the same way, as `Worked` for text or a completed task, with the same `Waiting for your
answer` and `Stopped`. A card that was never sent when a turn ends with no text is not sent at all.

A Binding that sets no `progress` gets `steps-and-activity`, so a Binding written before the field existed now shows
`Working…` in a gap. Set `progress: steps` to keep the cards and lines alone.

On Slack, an alert thread in `steps-and-activity` or `activity` also gets Slack's assistant status, an `is working…` row
with a spinner at the bottom of the thread, from the moment the turn starts, before the thread has any reply. Agent Kourier
sets it again after each write it makes anyway, because a reply in the thread takes it off, and clears it when the turn
ends, whether the turn finished, failed, was cancelled or stopped to ask a question. It adds no write of its own on a timer.
Slack shows it only to people who have Slack open when it is set, so it adds to the `Working…` line and does not replace
it. A chat thread never sets it. It needs the optional `assistant:write` scope; without it Slack may refuse the call,
and Agent Kourier then logs that once and goes on without the status
([Configure Slack](../how-to/configure-slack.md#what-the-app-is-allowed-to-do)).

On the generic `a2a` dialect there are no tool events: `steps` shows nothing until text, `steps-and-activity` shows a
`Working…` card in each gap, and `activity` shows its card with no step count. When the platform has no step cards, or Slack
refuses them for a stream (the stream is then plain text), no mode shows a card.

## Restarts

Agent Kourier persists sessions, queued replies, pending questions and intended posts. After a restart it finds the
tasks that were running, asks the agent for their state, and renders their output under the turn that started them, so
a half-written message is replaced with the whole answer and the thread finishes once.

## Lifetime

A thread keeps its session for `session.threadTTL`, 90 days by default, after its last activity, and a reply at any
point in that window resumes the same context. The agent platform suspends and resumes idle sessions on its own;
Agent Kourier has no idle timeout.

After the TTL, the thread has expired: a reply that the Binding would take gets a notice that the conversation has expired and that a new mention
starts a new one. 90 days after expiry the session is deleted with everything that belongs to it, and the thread
becomes one Agent Kourier never owned. If the agent reports that a session no longer exists, Agent Kourier says so in
the thread, and the next message starts a fresh context.

## Alerts

An alert thread is a session like any other, started by a trigger instead of a person. Repeats of the same alert attach
to the thread as notes, its resolve closes it with a note, and people continue the investigation by replying. The
first turn names no person; it is filed under the system user. See [Chat triggers](../reference/chat-triggers.md).

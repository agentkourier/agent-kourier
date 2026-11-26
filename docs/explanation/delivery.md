# Delivery guarantees

This page explains what Agent Kourier promises about messages coming in and posts going out, how it keeps those
promises across a crash, and the windows it accepts.

## Coming in: at least once, acknowledged only when durable

Slack expects an acknowledgement within about 3 seconds (documented for HTTP delivery and for interactions), so no
agent call happens while a message is being received. The adapter writes the turn locally, by its message ID, and only
then acknowledges.

If the write fails, the event is not acknowledged, and Slack delivers it again. Because the write is keyed by the
message, a redelivery is the same turn.

In Socket Mode the acknowledgement is a message on the WebSocket. In HTTP mode it is the answer 200, and an event that
is not taken within 2 seconds, or cannot be taken, is answered 503 ([Limits](../reference/limits.md#run-the-broker)).
Slack treats that answer as a failed delivery (Slack's documentation; not checked here).

The events of one channel are handled one at a time and in the order they arrive, and different channels at the same
time. A Binding whose regular expressions are slow to try therefore holds up its own channel's events first. It does not
keep the CPU for the others: with many busy channels, the acknowledgments of healthy ones slow too.

The broker holds a bounded number of events (see
[Limits](../reference/limits.md#start-investigations-triggers-rates-and-probes)). An event that arrives past the bound
is left unacknowledged, as if its write had failed, and Slack may send it again.

Slack's retries are finite (not verified here), so a shed event can be lost. One that comes back after a later event of
its channel was taken is handled after it. Order within a thread therefore holds only while the bounds do, and the
metric `agentkourier_slack_events_shed_total` says when they did not.

## Going out: an outbox, so a crash does not repeat a post

Slack's `chat.postMessage` has no idempotency key, so a crash between posting and recording the result would otherwise
post twice. Agent Kourier prevents that with an outbox:

1. Before posting, it writes an outbox row whose ID is derived from the event and the message's role (root, reply,
   digest, notice).
2. Every post carries Slack message metadata holding that ID.
3. On recovery, a row with no recorded result is reconciled first: Agent Kourier reads the recent history of the
   channel or thread, with metadata, and adopts a matching message instead of posting again.

A queued post, such as a notice, is looked up the same way before it is posted. The exception is a post that the
leader queued itself and has not tried to post yet: nothing can have posted it, so it is posted at once, with no lookup.

Once an attempt has begun, the row is looked up before the next. A post that failed or timed out, and may have landed
all the same, is therefore adopted if the lookup finds it rather than sent again. On Microsoft Teams a lookup can miss
a post, which is then sent and may show twice: see
[Finding a post again on Microsoft Teams](../reference/platforms.md#finding-a-post-again-on-microsoft-teams).

Which rows are untried is kept in memory, so after a restart or a takeover every row queued before it is looked up first.

Edits are naturally idempotent. Reactions also go through the outbox, and adding a reaction that is already there
counts as delivered. The agent's questions are posts too, and go through the outbox under the interaction's ID.

## The one window

A streamed message is created when the stream starts, and its metadata can be attached only when it stops. Agent
Kourier records the stream's message right after the start returns. A crash inside that window cannot be reconciled
from history, and is accepted: the partial stream stays, and the recovered turn posts the whole answer as a new
message.

## Turns that may run twice

Before resending a turn whose task it has not recorded, Agent Kourier asks the agent for the context's tasks and looks
for one holding the turn's message ID. If it finds one, it follows that task. If not, it sends the turn.

An A2A 0.3 agent cannot list its tasks, so that question cannot be asked: a resend treats the turn as absent, and the
turn may run twice. That is harmless for a read-only agent and not for one that changes things.

An agent that can list its tasks can still fail to answer. Each task in a page of the list carries its history, and a
page of more than 4 MiB is not read. Agent Kourier asks for that page again at half the size, down to three tasks, and
for the pages after it at the size that worked.

The walk as a whole has five minutes and 200 pages. If an earlier page held the turn, that is the answer, and the task
is followed.

If none did and the list still cannot be read in full, Agent Kourier does not know whether the agent has the turn and
cannot find out, so it does not send it. The turn fails. The thread is told that Agent Kourier could not tell, that the
agent may be running it, and that posting the message again runs it.

The failure is logged, audited as a `find_task` call that failed with `lookup_incomplete`, and counted as the turn
outcome `lookup_incomplete` (see [Troubleshoot](../how-to/troubleshoot.md#the-thread-says-agent-kourier-could-not-tell-if-the-agent-already-started-this-message)).

The difference from an agent that cannot list is on purpose. That agent never could be asked, and the turn is sent.
This one could be asked and its answer is lost, so a turn that may have run is not run again by Agent Kourier.

A reply that Agent Kourier queued itself and that the agent never took (it was refused as busy, or held behind a task)
is not such a turn, and is sent.

## Audit writes

A record that authorizes an action is written first, and the action fails if the write fails: an answer to a question is
recorded before it is sent. A record of something already done is logged and counted but never blocks the follow-up.

These are what the audit log holds for the chat path:

- agent calls
- the decisions on a question (such as answered, denied or timed out)
- a pause that could not be used, and its cancel
- a vote on a message
- a trigger's start that was refused
- a mention of a declared person that a reply queued
- a stale task that was ended

A message that arrives is not recorded as such.

## With two replicas

With Postgres, one replica leads and does all the work. A takeover runs the same recovery as a restart, so the
promises above hold across a failover.

What a failover adds for delivery:

- A deposed leader's writes are refused, to the store by the epoch and to the chat by its own clock, so it cannot
  finish a stream it had open.
- Its sends, answers and cancels to the agent are refused by the same clock, so it cannot send a reply that was queued.
- Each Slack call or agent send already in flight when a pause began can still happen once more.

The election, the fencing and the failover are explained in [High availability and failover](high-availability.md).

The store, the outbox and the session state are described field by field in [Stored data](../reference/stored-data.md).

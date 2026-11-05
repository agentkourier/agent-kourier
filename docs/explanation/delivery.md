# Delivery guarantees

This page explains what Agent Kourier promises about messages coming in and posts going out, how it keeps those
promises across a crash, and the windows it accepts.

## Coming in: at least once, acknowledged only when durable

Slack expects an acknowledgement within about 3 seconds (documented for HTTP delivery and for interactions), so no agent call happens while a message is being received. The
adapter writes the turn locally, by its message ID, and only then acknowledges. If the write fails, the event is not
acknowledged, and Slack delivers it again. Because the write is keyed by the message, a redelivery is the same turn.

The events of one channel are handled one at a time and in the order they arrive, and different channels at the same
time, so a Binding whose regular expressions are slow to try delays its own channel's events and no other's. The
broker holds a bounded number of events (see [Limits](../reference/limits.md#start-investigations-triggers-rates-and-probes)).
An event that arrives past the bound is left unacknowledged too, as if its write had failed, and Slack delivers it
again.

## Going out: an outbox, so a crash does not repeat a post

Slack's `chat.postMessage` has no idempotency key, so a crash between posting and recording the result would otherwise
post twice. Agent Kourier prevents that with an outbox:

1. Before posting, it writes an outbox row whose ID is derived from the event and the message's role (root, reply,
   digest, notice).
2. Every post carries Slack message metadata holding that ID.
3. On recovery, a row with no recorded result is reconciled first: Agent Kourier reads the recent history of the
   channel or thread, with metadata, and adopts a matching message instead of posting again.

Edits are naturally idempotent. Reactions also go through the outbox; adding a reaction that is already there counts
as delivered. The agent's questions are posts too, and go through the outbox under the interaction's ID.

## The one window

A streamed message is created when the stream starts, and its metadata can be attached only when it stops. Agent
Kourier records the stream's message right after the start returns. A crash inside that window cannot be reconciled
from history, and is accepted: the partial stream stays, and the recovered turn posts the whole answer as a new
message.

## Turns that may run twice

Before resending a turn whose task it has not recorded, Agent Kourier asks the agent for the context's tasks and looks
for one holding the turn's message ID. Found, it follows that task; absent, it sends. An A2A 0.3 agent cannot list its
tasks, so that question cannot be asked: a resend treats the turn as absent, and the turn may run twice. That is
harmless for a read-only agent and not for one that changes things.

An agent that can list its tasks can still fail to answer. Each task in a page of the list carries its history, and a page of more than 4 MiB is not read:
Agent Kourier asks for that page again at half the size, down to three tasks, and for the pages after it at the size that worked. The walk as a whole has five minutes and 200 pages. If an earlier page held the
turn, that is the answer, and the task is followed. If none did and the list still cannot be read in full, Agent Kourier does not know whether the agent has the turn
and cannot find out, so it does not send it. The turn fails: the thread is told that Agent Kourier could not tell, that the agent may be running it, and that
posting the message again runs it. The failure is logged, audited as a `find_task` call that failed with `lookup_incomplete`, and counted as the turn
outcome `lookup_incomplete` (see [Troubleshoot](../how-to/troubleshoot.md#the-thread-says-agent-kourier-could-not-tell-if-the-agent-already-started-this-message)). The difference from an agent that
cannot list is on purpose: that agent never could be asked, and the turn is sent, and this one could be asked and its answer is lost, so a turn that may have run is not run again by Agent Kourier. A reply
that Agent Kourier queued itself and that the agent never took (it was refused as busy, or held behind a task) is not such a turn, and is sent.

## Audit writes

A record that authorizes an action is written first, and the action fails if the write fails: an answer to a question is
recorded before it is sent. A record of something already done is logged and counted but never blocks the follow-up.
These are what the audit log holds for the chat path: agent calls, the decisions on a question (such as answered, denied
or timed out), a pause that could not be used and its cancel, a vote on a message, a trigger's start that was
refused, and a stale task that was ended. A message that arrives is not recorded as such.

## With two replicas

With Postgres, one replica leads and does all the work, and a takeover runs the same recovery as a restart, so the
promises above hold across a failover. What it adds for delivery: a deposed leader's writes are refused, to the store
by the epoch and to the chat by its own clock, so it cannot finish a stream it had open; its sends, answers and
cancels to the agent are refused by the same clock, so it cannot send a reply that was queued; and each Slack call or
agent send already in flight when a pause began can still happen once more. The election, the fencing and the failover are
explained in [High availability and failover](high-availability.md).

The store, the outbox and the session state are described field by field in [Stored data](../reference/stored-data.md).

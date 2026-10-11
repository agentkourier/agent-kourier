# Delivery guarantees

This page explains what Agent Kourier promises about messages coming in and posts going out, how it keeps those
promises across a crash, and the windows it accepts.

## Coming in: at least once, acknowledged only when durable

Slack expects an acknowledgement within 3 seconds, so no agent call happens while a message is being received. The
adapter writes the turn locally, by its message ID, and only then acknowledges. If the write fails, the event is not
acknowledged, and Slack delivers it again. Because the write is keyed by the message, a redelivery is the same turn.

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

## Audit writes

A record that authorizes an action is written first, and the action fails if the write fails: an answer to a question is
recorded before it is sent. A record of something already done is logged and counted but never blocks the follow-up.
These are what the audit log holds for the chat path: agent calls, the decisions on a question (such as answered, denied
or timed out), a pause that could not be used and its cancel, a vote on a message, a trigger's start that was
refused, and a stale task that was ended. A message that arrives is not recorded as such.

## With two replicas

With Postgres, one replica leads and does all the work, and a takeover runs the same recovery as a restart, so the
promises above hold across a failover. What it adds for delivery: a deposed leader's writes are refused, to the store
by the epoch and to the chat by its own clock, so it cannot finish a stream it had open; and each Slack call or agent
send already in flight when a pause began can still happen once more. The election, the fencing and the failover are
explained in [High availability and failover](high-availability.md).

The store, the outbox and the session state are described field by field in [Stored data](../reference/stored-data.md).

# Delivery guarantees

This page explains what Agent Kourier promises about messages coming in and posts going out, how it keeps those
promises across a crash, and the one window it accepts.

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

A record that authorizes an action is written first, and the action fails if the write fails: an inbound event before
it is queued, and an answer to a question before it is sent. A record of something already done is logged and counted
but never blocks the follow-up.

## With two replicas

With Postgres, one replica leads and does all the work. Every write checks the leader's epoch, and a leader that loses
its lease, or meets a stale epoch, exits at once. A leader that was paused past its renew deadline refuses every write
to the chat and exits, so a deposed leader cannot finish a stream it had open. A Slack call or agent send already in
flight when the pause began can still happen once more.

The store, the outbox and the session state are described field by field in [Stored data](../reference/stored-data.md).

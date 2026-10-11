# Glossary

This page defines the terms the documentation uses, each with one meaning.

A2A
:   The Agent2Agent protocol. Agent Kourier speaks A2A 1.0 over JSON-RPC, and A2A 0.3 to an agent whose card is a 0.3
    card, and needs an agent that streams.

AgentBackend
:   The resource that names an A2A endpoint and the dialect to speak to it. Owned by the platform team.

Binding
:   The resource that connects one agent to one channel, with the service identity every turn runs as. Owned by an
    application team.

Binding identity
:   The one service identity, a bearer token or a user ID, that every turn of a Binding runs as, chat or alert. The
    person who wrote a message is carried as attribution, never as the identity.

Channel
:   A place in a chat platform where people post, named by its ID. One channel takes at most one Binding per
    connection.

ChatConnection
:   The resource that connects Agent Kourier to one chat platform workspace, with the Secret that holds its tokens.
    Owned by the platform team.

Context
:   The A2A `contextId` that a session maps to. Agent Kourier resumes the same context for every turn of a thread.

Cooldown
:   For a `botMessage` trigger with `thread.by: key`: the window after an investigation starts in which another fire of
    the same key adds a note and starts no turn.

Dialect
:   The per-backend part of the A2A client: URL layout, card location, identity header and how a pause is read. `a2a`
    is generic; `kagent-v1` adds kagent's human-in-the-loop extension.

Digest line
:   One line at the top level of a channel that counts the alerts a trigger's limits stopped in a window.

Front door
:   A proxy in front of an agent runtime that verifies the Binding's token and strips the headers that select a user.
    Required in front of kagent.

Interaction
:   A pause in which the agent waits for a person: a question (ask-user), or a tool approval, which is not built yet.

Investigation
:   A session that a trigger or an alert started rather than a person.

Leader
:   With Postgres, the one replica that does Agent Kourier's work, chosen by a Kubernetes Lease. Its writes are fenced
    by an epoch in the database.

Outbox
:   The durable table of posts Agent Kourier means to make. A post is recorded before it is sent, so a crash between
    sending and recording is found and not posted twice.

Platform
:   A chat platform behind the `chat.Platform` interface. Slack is the one platform in v1.

Post
:   A message Agent Kourier writes into a channel or thread.

Preset
:   A named set of `botMessage` trigger defaults for a known sender. v1 has one, `alertmanager`.

Redaction
:   Taking secrets out of text before it is shown, stored, logged or sent to an agent.

Reply queue
:   The durable queue of thread replies that wait for the agent's active task to end.

Session
:   The conversation between a thread and an agent context, stored by Agent Kourier. A thread is a session.

Thread
:   A message and its replies. Agent Kourier owns a thread it started, or one it was mentioned in.

Trigger
:   A rule in a Binding's `chat.triggers` that decides what starts a turn: `mention`, or `botMessage` for another bot's
    posts.

Turn
:   One message to the agent and the task that answers it. A session's turns run one at a time, in order.

Untrusted block
:   Text from people or other bots that reaches an agent, framed as data inside a tagged block that says it is never
    instructions.

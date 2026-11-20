# Glossary

This page defines the terms the documentation uses, each with one meaning.

A2A
:   The Agent2Agent protocol. Agent Kourier speaks A2A 1.0 over JSON-RPC, and A2A 0.3 to an agent whose card is a 0.3
    card, and needs an agent that streams.

Accepted
:   The condition of a Binding's status that says the broker took the Binding as written. With custom resources it is
    `False` with a reason such as `InvalidSpec` or `ChannelConflict` when it did not
    ([Binding status](binding-status.md)).

Actor
:   Who a turn is from, as the agent sees it: the `agentkourier.dev/actor` metadata (`id`, `platform`, and `name` and
    `email` when the chat platform knows them) and the prefix at the start of the text. It is information for the agent,
    never authorization ([Agent contract](agent-contract.md)).

Adaptive Card
:   Microsoft Teams' card format. On Teams, questions, tool approvals and the useful and not-useful buttons are Adaptive
    Cards (version 1.5) that a member of the channel's team presses ([What each platform supports](platforms.md)).

Agent Substrate
:   The runtime kagent 1.0 runs every agent session on. It needs the Kubernetes pod-certificate APIs, which most clusters
    do not serve ([Connect a kagent agent](../how-to/connect-kagent.md)).

AgentBackend
:   The resource that names an A2A endpoint and the dialect to speak to it. Owned by the platform team.

Binding
:   The resource that connects one agent to one channel, with the service identity every turn runs as. Owned by an
    application team.

Binding identity
:   The one service identity, a bearer token or a user ID, that every turn of a Binding runs as, chat or alert. The
    person who wrote a message is carried as attribution, never as the identity.

Binding verification
:   The leader's repeated check that a Binding can be served: that its agent's card can be fetched, that the bot is
    in its channel and, for a Binding whose tool approvals name approver groups, that each group can be read. It reports through metrics and log lines and, with custom resources, in the Binding's status, and changes
    nothing else. See [Metrics](metrics.md#binding-verification) and [Binding status](binding-status.md).

Bot Connector
:   Microsoft's service through which a Teams bot posts, replies and edits. A Microsoft Teams connection calls it with a token that
    Microsoft Entra issues for the connection's client secret.

Channel
:   A place in a chat platform where people post, named by its ID. One channel takes at most one Binding per
    connection. On a webhook connection it is the value the connection reads from each event, such as `acme/widgets` or
    `OPS`: the Binding whose `chat.channel` is that value takes the event.

Chat listener
:   The broker's port for the requests that chat platforms and services send it, apart from the metrics port, set by
    `AGENTKOURIER_CHAT_LISTEN` (the chart's `chatListener`). It takes `POST /chat/<platform>/<namespace>/<name>`; only the
    leader runs connections, so a standby answers 503. Slack in HTTP mode, Teams in `http` mode or with a `notificationURL`,
    and every webhook connection need it.

ChatConnection
:   The resource that connects Agent Kourier to one chat platform workspace or one service's webhooks, with the Secret
    that holds its credentials.
    The platform team owns the broker's own; a team may own one in a namespace the platform admits.

Config route
:   Where the broker reads its resources from, chosen when it starts: `files` (usually a ConfigMap, all or nothing) or `crds`
    (custom resources, one object at a time), which is the chart's default. See [Choose a config route](../explanation/config-routes.md).

Context
:   The A2A `contextId` that a session maps to. Agent Kourier resumes the same context for every turn of a thread.

Cooldown
:   For a `botMessage` trigger with `thread.by: key`: the window after an investigation starts in which another fire of
    the same key adds a note and starts no turn.

Delivery
:   One request that a service sends to a webhook connection. Its delivery ID is the service's own ID of it, read from the
    header that `spec.webhook.delivery.header` names, such as `X-GitHub-Delivery`; each verified delivery's INFO line,
    `webhook: delivery answered`, shows it.

Dialect
:   The per-backend part of the A2A client: URL layout, card location, identity header and how a pause is read. Both
    ask for kagent's human-in-the-loop extension when the agent card lists it; `kagent-v1` reads a pause only through
    it, and `a2a` reads it through the extension or as a text question.

Digest line
:   One line at the top level of a channel that counts the messages that limits stopped from starting an investigation
    in a window: a trigger's, or the fixed ones on a bot's mention.

Epoch
:   With Postgres, the leadership number in the database. A replica that takes the Lease increments it, and every write
    transaction checks it, so a deposed leader's writes fail. This is the fencing of a leader.

Failover
:   The standby taking the Lease after the leader stops, is lost or is paused past its renew deadline, and running the
    same recovery as a restart.

Front door
:   A proxy in front of an agent runtime that verifies the Binding's token and strips the headers that select a user.
    Required in front of kagent.

Identity (webhook)
:   A webhook connection's `spec.webhook.identity`: the agent's own account on the service, as the service names it, such as
    `pr-reviewer[bot]`. An event that account caused is dropped, so the agent does not answer itself. Not the Binding
    identity, which is who the agent's calls run as.

Interaction
:   A pause in which the agent waits for a person: a question, when the Binding enables
    `interactions.askUser`, or a tool approval, when it enables `interactions.toolApprovals`. Both default to off, and a
    pause the Binding does not allow is refused and its task cancelled. Structured pauses use kagent's human-in-the-loop extension.

Investigation
:   A session that a `botMessage` or `personMessage` trigger started, such as an alert's, rather than a mention.

Kind full tier
:   The local kind cluster that `make e2e-full-up` builds from a checkout of the repository, with kagent 1.0, Agent
    Substrate, a front door and an OIDC issuer (Dex) in it. The `sre-triage` recipe's agent runs on it.

Lease
:   The Kubernetes Lease, `<fullname>-leader`, that the replicas on Postgres hold to elect the leader. The leader renews
    it; a standby takes it a lease duration after the last renewal it saw.

Leader
:   With Postgres, the one replica that does Agent Kourier's work, chosen by a Kubernetes Lease. Its writes are fenced
    by an epoch in the database.

Message field
:   One of `text`, `title`, `link`, `body`, `footer` and `fallback`: what a trigger's `match`, `exclude` and `extract`
    read. `text` is the message's top-level text. The others are the heading, the heading's URL, the main text, the
    small print and the plain-text summary of the message's structured part (Slack: its first legacy attachment).

Microsoft Graph notifications and polling
:   How a Teams connection reads other apps' posts, such as an alert's card, which Teams does not send a bot: Graph's change
    notifications at `spec.teams.notificationURL` when it is set, or a poll of each bound channel every 10 seconds when it
    is not.

Microsoft Teams connection
:   A ChatConnection with `spec.platform: teams` (a Teams connection, for short): `spec.teams` names the bot's Microsoft Entra tenant and
    application, its mode (`http` or `socket`) and an optional `notificationURL`, and its Secret holds `clientSecret`.
    Experimental.

Outbox
:   The durable table of posts Agent Kourier means to make. A post is recorded before it is sent, so a crash between
    sending and recording is found by a tag that the post carries and not posted twice. On Microsoft Teams a post whose tag cannot
    be looked for is posted again, and may show twice.

Output
:   A Binding's `chat.output`, what Agent Kourier posts of the agent's work: `live` (the default), the reply as it is written;
    `final`, the finished reply alone; or `none`, nothing, the only output a webhook connection takes.

Platform
:   A chat platform behind the `chat.Platform` interface, which a ChatConnection's `spec.platform` names. The API takes `slack`,
    `teams` and `webhook`, and the broker runs all three ([What each platform supports](platforms.md)).

Post
:   A message Agent Kourier writes into a channel or thread.

Redaction
:   Taking secrets out of text before it is shown, stored, logged or sent to an agent.

Reply queue
:   The durable queue of thread replies that wait for the agent's active task to end.

RSC permission
:   A resource-specific consent permission, which a Teams app's manifest asks for and its install in a team grants for that team
    alone. A Teams connection reads channel messages through Microsoft Graph under `ChannelMessage.Read.Group`.

Sender
:   Who posted a message a `botMessage` trigger reads, named by `from.id` and `from.kind`. A message matches only when
    both are equal to the sender's.

Session
:   The conversation between a thread and an agent context, stored by Agent Kourier. A thread is a session.

Socket Mode (Slack)
:   The default mode of a Slack connection: the broker takes Slack's events over a WebSocket that it opens with the app-level
    token (`appToken`), so it needs no public endpoint.

Socket mode (Teams)
:   `spec.teams.mode: socket`: the leader opens an outbound WebSocket to Microsoft Teams in each region and takes the bot's
    activities over it, with no public endpoint. Microsoft calls it a public preview; what Teams sends while no socket is
    ready is not delivered later.

Standby
:   With Postgres, a replica that does not hold the Lease. It is Ready, serves health and metrics, does no work and waits
    to take the Lease.

Thread
:   A message and its replies. Agent Kourier owns a thread it started, or one it was mentioned in. On a webhook connection,
    the prefix and value of the field that `spec.webhook.thread` names, such as `pr-42`: every event with it is one session.

Trigger
:   A rule in a Binding's `chat.triggers` that decides what starts a turn: `mention`, `botMessage` for another bot's
    posts, `personMessage` for the messages people post at the top level of the channel, or `webhookEvent` for an event of
    a webhook connection, the only type such a connection's Bindings take.

Turn
:   One message to the agent and the task that answers it. A session's turns run one at a time, in order.

Untrusted block
:   Text from people or other bots that reaches an agent, framed as data inside a tagged block that says it is never
    instructions.

Webhook connection
:   A ChatConnection with `spec.platform: webhook`: it takes one service's signed JSON webhooks at
    `POST /chat/webhook/<namespace>/<name>` on the chat listener, verifies each on its raw bytes, maps it onto an event by JSON
    Pointers, and posts nothing. Experimental ([Start agents from webhooks](../how-to/receive-webhooks.md)).

Workflows webhook
:   A Microsoft Teams flow that posts what it is sent to one channel. Alertmanager's `msteamsv2` receiver posts its alerts
    through one. It is not the `webhook` platform.

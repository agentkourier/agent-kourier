# What each platform supports

A Binding names its chat platform through its ChatConnection, whose `spec.platform` says which: `slack`, or `web`, whose
`spec.web.profile` names the integration it speaks, `kourier-v1` or `github`. This page lists, for Slack and for each web profile,
the shape of the chat IDs a Binding names and the features of a Binding that it serves.

A web connection runs on the broker's chat listener: its sender sends each request to `POST /chat/web/<namespace>/<name>` on the
port that `AGENTKOURIER_CHAT_LISTEN` opens (the chart's `chatListener`), which only the replica that leads serves. A broker with no
chat listener does not run one: as the broker's own connection it stops the start (exit code 2), and a team's connection is not
started. On the files route the directory holds exactly one ChatConnection, the broker's own, so such a broker runs one connection,
Slack or web; to run several side by side, use custom resources, where the others are team connections.

A Binding is checked against its platform when the broker reads it with its ChatConnection: the API server and the broker's load
check only each ID's length and characters (see [Chat IDs](#chat-ids)). A Binding that names an ID of another shape than its platform's,
such as the Slack channel's name `general`, or asks for a feature that its platform does not serve, is refused then, as a Binding that
fails validation is:

- with custom resources its `Accepted` condition is `False` with the reason `InvalidPlatformID` or `UnsupportedOnPlatform`
  ([Binding status](binding-status.md#accepted)). A Binding that had a good generation keeps serving it, and the message says which
  (`serving generation 1`), so an edit with a mistyped ID does not take; a new Binding is not served. The other Bindings keep running;
- on the files route the whole config fails, with the same message: at start the broker exits with code 2, and on a reload it keeps
  its last good config.

`agent-kourier dry-run`, which reads only Slack, holds the Binding to Slack's shapes and features too, and refuses it with the same
problem and fix.

The message names the field, the value, the platform, the ChatConnection and what to write instead, and links this page, for example:

```text
spec.chat.channel: "octo-org/hello-world" is not a channel ID on Slack, the platform of ChatConnection agent-kourier-system/slack-main: use the channel's ID, such as C0123456789 (G… for an older private channel), not its name (see https://agentkourier.dev/reference/platforms/)
```

A Binding that its ChatConnection does not admit (in another namespace that `allowedNamespaces` does not list) is told
`NamespaceNotAllowed` and nothing of the platform.

## Chat IDs

| Field | Slack | Web, `kourier-v1` profile | Web, `github` profile |
|---|---|---|---|
| `chat.channel` | A channel ID: `C`, or `G` for an older private channel, then capitals and digits, at most 32 bytes, such as `C0123456789`. Not the channel's name. | The channel's ID as the sender sends it: letters, digits, `.`, `_` and `-`, starting with a letter or a digit, at most 64 bytes, such as `ch-1`. | A repository, `owner/name`, at most 140 bytes, such as `octo-org/hello-world`. |
| A `botMessage` trigger's `from.id` | A bot ID: `B`, then capitals and digits, at most 32 bytes, such as `B0123456789`. Not the bot's name. | The sender's ID as the sender sends it: letters, digits, `.`, `_` and `-`, starting with a letter or a digit, at most 64 bytes. | Refused: `UnsupportedOnPlatform`. The profile hands on no bot's message, so remove the `botMessage` trigger. |
| A `chat.mentions` entry's `user` | A user ID: `U` or `W`, then 2 to 20 capitals and digits, such as `U0123456789`. | The person's ID, of the sender's shape, at most 64 bytes. | Refused: `UnsupportedOnPlatform`. Agent Kourier posts nothing on the profile, so it notifies nobody; remove `chat.mentions`. |
| An entry of `interactions.approverGroups` | A user group ID: `S`, then capitals and digits, at most 32 bytes, such as `S0123456789`. | A group ID, of the sender's shape, at most 64 bytes. | Refused: `UnsupportedOnPlatform`. The profile asks for no tool approvals; remove `approverGroups`. |

A repository's owner is at most 39 letters and digits, with single hyphens between them, so it neither starts nor ends with a hyphen,
and its name is 1 to 100 letters, digits, `.`, `_` and `-`. The hyphens do not count toward the 39: the broker bounds the length with
the cap alone, 140 bytes. A `kourier-v1` ID has no `:` and no `/`. Since tool approvals are not served on either web profile (see
[Features](#features)), a `kourier-v1` Binding's approver groups are refused with `toolApprovals` anyway.

A ChatConnection of the `github` profile names the agent in `spec.web.identity`: the GitHub login the agent posts as, a user's such as
`pr-reviewer` or a GitHub App's such as `pr-reviewer[bot]`, at most 44 bytes with the `[bot]`, of the owner's shape above. The profile
starts a turn for a comment that mentions it (`@pr-reviewer`), and for no event that it sent. It is required there, and a
`kourier-v1` connection refuses it. The API server and the load check it, with the profile's shape.

What the API server and the load take of a Binding's IDs is wider: any ID of printable ASCII with no space and no `:`, up to the field's
cap. So a channel's name such as `general`, a bot's name such as `alertmanager`, or another platform's ID is taken when it is applied,
and refused by the broker, as above. A value with a space, a tab or another control character, a `:`, or a character outside ASCII is
refused when it is applied: no platform's ID has one. The length caps of each field are in
[Limits](limits.md#write-a-binding-an-agentbackend-or-a-chatconnection).

## Features

| Feature | Slack | Web, `kourier-v1` profile | Web, `github` profile |
|---|---|---|---|
| What Agent Kourier posts of the agent's work: `chat.output` | `live` or `final`; left out, `live`. `none` is refused: `UnsupportedOnPlatform`. | `live` or `final`; left out, `live`. `none` is refused: `UnsupportedOnPlatform`. | `none` alone: the agent posts on GitHub itself, and Agent Kourier posts nothing. Left out, `none`. `live` and `final` are refused: `UnsupportedOnPlatform`. |
| What starts a turn: `chat.triggers` | Every trigger. Left out, `[{type: mention}]`. | Every trigger. Left out, `[{type: mention}]`. A `botMessage` trigger matches nothing: the profile hands on people's messages alone. | A list must hold a `mention` trigger: every event the profile hands on is a mention, in the thread of its pull request. A list without one is refused: `UnsupportedOnPlatform`. Left out, `[{type: mention}]`. A `personMessage` trigger beside it matches nothing, and a `botMessage` trigger is refused (see [Chat IDs](#chat-ids)). |
| Questions to a person: `interactions.askUser` | Works: Agent Kourier asks in the thread with buttons. | Refused: `UnsupportedOnPlatform`. Set it to `false`. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| Tool approvals: `interactions.toolApprovals`, and `interactions.approverGroups`, which requires it | Works: Agent Kourier asks in the thread with buttons. | Refused: `UnsupportedOnPlatform`. Set `toolApprovals` to `false` and remove `approverGroups`. | Refused: `UnsupportedOnPlatform`. Set `toolApprovals` to `false` and remove `approverGroups`. |
| The useful and not-useful buttons: `feedback.enabled` | Works. | Refused: `UnsupportedOnPlatform`. Set it to `false`. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| The acknowledgment reaction: `chat.acknowledgment` | Works. With no `emoji` the reaction is `hourglass_flowing_sand`. | Taken, and nothing is shown: the profile has no reactions. With custom resources, a Binding that names an `emoji` has the condition `AcknowledgmentShown` `False` ([Binding status](binding-status.md#acknowledgmentshown)). | Taken, and nothing is shown: the profile has no reactions. With custom resources, a Binding that names an `emoji` has the condition `AcknowledgmentShown` `False` ([Binding status](binding-status.md#acknowledgmentshown)). |
| A thread whose session has ended: past `session.threadTTL`, or its Binding gone | A reply gets a notice in the thread instead of a turn, that the conversation has expired or that its Binding is gone ([Threads and sessions](../explanation/threads-and-sessions.md#lifetime)); an unmentioned reply under `chat.threadReplies: mention` gets none. The thread's session is not replaced. | As on Slack: the notice is posted as a reply. | No notice can be posted, so an event that starts a turn starts a fresh session in the pull request's thread, with nothing said and none of the old session's context. While the old session records a task or holds a reply not yet delivered, or when the event's body is one the broker took before, the event is dropped instead. |

The API server does not fill in `output`: the broker fills in the platform's default when it reads the Binding with its
ChatConnection. A Binding stored by an earlier version, whose API server filled in `live`, keeps it, and on Slack it is served as
before.

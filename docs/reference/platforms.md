# What each platform supports

A Binding names its chat platform through its ChatConnection, whose `spec.platform` says which: `slack`, or `web`, whose
`spec.web.profile` names the integration it speaks, `kourier-v1` or `github`. This page lists, for Slack and for each web profile,
the shape of the chat IDs a Binding names and the features of a Binding that it serves.

No broker runs a web connection yet. The API server takes a web ChatConnection and its Bindings, and the broker checks those Bindings
against the profile as this page says, but it does not start the connection: as the broker's own connection it stops the broker's
start, and a team's connection is not started.

A Binding is checked against its platform when the broker reads it with its ChatConnection: the API server and the broker's load
check only that each ID has the shape of some platform's ID. A Binding that names an ID of another shape, or asks for a feature that its
platform does not serve, is refused then, as a Binding that fails validation is:

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
| `chat.channel` | A channel ID: `C`, or `G` for an older private channel, then capitals and digits, at most 32 bytes, such as `C0123456789`. Not the channel's name. | The channel's ID as the sender sends it: letters, digits, `.`, `_` and `-`, starting with a letter or a digit and with at least one digit, at most 64 bytes, such as `ch-1`. | A repository, `owner/name`, at most 140 bytes, such as `octo-org/hello-world`. |
| A `botMessage` trigger's `from.id` | A bot ID: `B`, then capitals and digits, at most 32 bytes, such as `B0123456789`. Not the bot's name. | The sender's ID as the sender sends it: letters, digits, `.`, `_` and `-`, starting with a letter or a digit, at most 64 bytes. | A login, at most 44 bytes, such as `dependabot[bot]` for an app. |
| A `chat.mentions` entry's `user` | A user ID: `U` or `W`, then 2 to 20 capitals and digits, such as `U0123456789`. | The person's ID, of the sender's shape, at most 64 bytes. | A login, such as `octocat`. |
| An entry of `interactions.approverGroups` | A user group ID: `S`, then capitals and digits, at most 32 bytes, such as `S0123456789`. | A group ID, of the channel's shape, at most 64 bytes. | A team, `org/team-slug`, such as `octo-org/sre`. |

A repository's owner and a login are at most 39 letters and digits, with single hyphens between them, so they neither start nor end
with a hyphen, and a login may end in `[bot]`; a repository's name and a team's slug are 1 to 100 letters, digits, `.`, `_` and `-`.
The hyphens do not count toward the 39: the broker bounds the length with the caps alone, 44 bytes for a login (`[bot]` included) and
140 for a repository or a team, so a login such as `a-b-c-…` of 43 bytes is taken. A `kourier-v1` ID has no `:`
and no `/`. Since tool approvals are not served on either web profile (see [Features](#features)), a web Binding's approver groups are
refused with `toolApprovals` anyway.

What the API server and the load take is wider: for each field, the shape of any of the three. A value that has none of them, such as
the channel name `general`, which has no digit, is refused when it is applied. An ID of one platform can have the shape of another's: a
bot's name such as `alertmanager` is a login and a `kourier-v1` sender, and `C0123456789` is a `kourier-v1` channel. Such an ID is
refused by the broker, as above, and not when it is applied. The length caps of each field are in
[Limits](limits.md#write-a-binding-an-agentbackend-or-a-chatconnection).

## Features

| Feature | Slack | Web, `kourier-v1` profile | Web, `github` profile |
|---|---|---|---|
| What Agent Kourier posts of the agent's work: `chat.output` | `live` or `final`; left out, `live`. `none` is refused: `UnsupportedOnPlatform`. | `live` or `final`; left out, `live`. `none` is refused: `UnsupportedOnPlatform`. | `none` alone: the agent posts on GitHub itself, and Agent Kourier posts nothing. Left out, `none`. `live` and `final` are refused: `UnsupportedOnPlatform`. |
| What starts a turn: `chat.triggers` | Every trigger. Left out, `[{type: mention}]`. | Every trigger. Left out, `[{type: mention}]`. A `botMessage` trigger matches nothing: the profile hands on people's messages alone. | A list must hold a `mention` trigger: every event the profile hands on is a mention, in the thread of its pull request. A list without one is refused: `UnsupportedOnPlatform`. Left out, `[{type: mention}]`. A `botMessage` or `personMessage` trigger beside it matches nothing. |
| Questions to a person: `interactions.askUser` | Works: Agent Kourier asks in the thread with buttons. | Refused: `UnsupportedOnPlatform`. Set it to `false`. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| Tool approvals: `interactions.toolApprovals`, and `interactions.approverGroups`, which requires it | Works: Agent Kourier asks in the thread with buttons. | Refused: `UnsupportedOnPlatform`. Set `toolApprovals` to `false` and remove `approverGroups`. | Refused: `UnsupportedOnPlatform`. Set `toolApprovals` to `false` and remove `approverGroups`. |
| The useful and not-useful buttons: `feedback.enabled` | Works. | Refused: `UnsupportedOnPlatform`. Set it to `false`. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| The acknowledgment reaction: `chat.acknowledgment` | Works. With no `emoji` the reaction is `hourglass_flowing_sand`. | Taken. No broker runs the profile yet, so nothing is shown. | Taken. No broker runs the profile yet, so nothing is shown. |

The API server does not fill in `output`: the broker fills in the platform's default when it reads the Binding with its
ChatConnection. A Binding stored by an earlier version, whose API server filled in `live`, keeps it, and on Slack it is served as
before.

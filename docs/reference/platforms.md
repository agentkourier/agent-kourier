# What each platform supports

A Binding names its chat platform through its ChatConnection, whose `spec.platform` says which. This page lists, for each
platform, the shape of the chat IDs a Binding names and the features of a Binding that the platform serves. Slack is the only
platform today.

A Binding is checked against its platform when the broker resolves it, which is when it knows the ChatConnection: the API server and
the broker's load check only that each ID has the shape of some platform's ID. A Binding that names an ID of another shape, or asks
for a feature that its platform does not serve, is rejected then:

- with custom resources it is not served, and its `Accepted` condition is `False` with the reason `InvalidPlatformID` or
  `UnsupportedOnPlatform` ([Binding status](binding-status.md#accepted)); the other Bindings keep running;
- on the files route the whole config fails, at start or on a reload, with the same message.

`agent-kourier dry-run` loads the config and does not resolve it, so it does not run these checks.

The message names the field, the value, the platform, the ChatConnection and what to write instead, and links this page, for example:

```text
spec.chat.channel: "octo-org/hello-world" is not a channel ID on Slack, the platform of ChatConnection agent-kourier-system/slack-main: use the channel's ID, such as C0123456789 (G… for an older private channel), not its name (see https://agentkourier.dev/reference/platforms/)
```

A Binding that its ChatConnection does not admit (in another namespace that `allowedNamespaces` does not list) is told
`NamespaceNotAllowed` and nothing of the platform.

## Chat IDs

| Field | Slack |
|---|---|
| `chat.channel` | A channel ID: `C`, or `G` for an older private channel, then capitals and digits, at most 32 bytes, such as `C0123456789`. Not the channel's name. |
| A `botMessage` trigger's `from.id` | A bot ID: `B`, then capitals and digits, at most 32 bytes, such as `B0123456789`. Not the bot's name. |
| A `chat.mentions` entry's `user` | A user ID: `U` or `W`, then 2 to 20 capitals and digits, such as `U0123456789`. |
| An entry of `interactions.approverGroups` | A user group ID: `S`, then capitals and digits, at most 32 bytes, such as `S0123456789`. |

What the API server and the load take is wider: for each field, Slack's shape or the shape of a GitHub ID (a repository `owner/name` as
a channel, a login such as `octocat` or `dependabot[bot]` as a sender or a user, and a team `org/team-slug` as an approver group), which
no platform served today takes. A value that has neither shape, such as the channel name `general`, is refused when it is applied.
A Slack ID of the wrong kind can have a login's shape, a bot's name such as `alertmanager` as a sender or a user group ID as a user,
and so it is refused at resolution, not when it is applied. The length caps of each field are in [Limits](limits.md#write-a-binding-an-agentbackend-or-a-chatconnection).

## Features

| Feature | Slack | On a platform without it |
|---|---|---|
| Tool approvals: `interactions.toolApprovals`, and `interactions.approverGroups`, which requires it | Works: Agent Kourier asks in the thread with buttons. | Refused: `UnsupportedOnPlatform`. Set `toolApprovals` to `false` and remove `approverGroups`. |
| The acknowledgment reaction: `chat.acknowledgment` | Works. With no `emoji` the reaction is `hourglass_flowing_sand`. | Served, and no reaction is shown. A Binding whose acknowledgment is enabled and names an `emoji` has the condition `AcknowledgmentShown`, `False` with the reason `NoReactions` ([Binding status](binding-status.md#acknowledgmentshown)). |

# What each platform supports

A Binding names its chat platform through its ChatConnection, whose `spec.platform` says which. This page lists, for each
platform, the shape of the chat IDs a Binding names and the features of a Binding that the platform serves. Slack is the only
platform today.

The API server and the broker's load hold each chat ID of a Binding to Slack's shape, the only platform's (see [Chat IDs](#chat-ids)):
a Binding that names a channel's name such as `general`, or a bot's name such as `alertmanager` as a sender, is refused when it is
applied: the API server refuses it on `kubectl apply`, `--dry-run=server` included, and on any other client that writes it. On the
files route the load refuses it with the same rule, and so does `agent-kourier dry-run`:

```text
spec.chat.channel: invalid channel "general": must be a channel ID such as C0123456789, not a name
```

The broker checks the Binding against its platform once more when it reads the Binding with its ChatConnection. That check refuses a
chat ID that does not have the shape of the platform's IDs (`InvalidPlatformID`) and a feature that the platform does not serve
(`UnsupportedOnPlatform`); with Slack, which serves every feature in [Features](#features) and whose shapes the schema already holds,
it refuses nothing that was accepted. A Binding it refuses is refused as a Binding that fails validation is:

- with custom resources its `Accepted` condition is `False` with that reason ([Binding status](binding-status.md#accepted)). A Binding
  that had a good generation keeps serving it, and the message says which (`serving generation 1`); a new Binding is not served. The
  other Bindings keep running;
- on the files route the whole config fails: at start the broker exits with code 2, and on a reload it keeps its last good config.

Its message names the field, the value, the platform, the ChatConnection and what to write instead, and links this page.

A Binding that its ChatConnection does not admit (in another namespace that `allowedNamespaces` does not list) is told
`NamespaceNotAllowed` and nothing of the platform.

## Chat IDs

| Field | Slack |
|---|---|
| `chat.channel` | A channel ID: `C`, or `G` for an older private channel, then capitals and digits, at most 32 bytes, such as `C0123456789`. Not the channel's name. |
| A `botMessage` trigger's `from.id` | A bot ID: `B`, then capitals and digits, at most 32 bytes, such as `B0123456789`. Not the bot's name. |
| A `chat.mentions` entry's `user` | A user ID: `U` or `W`, then 2 to 20 capitals and digits, such as `U0123456789`. |
| An entry of `interactions.approverGroups` | A user group ID: `S`, then capitals and digits, at most 32 bytes, such as `S0123456789`. |

The API server and the load refuse any other value, with the field's pattern and length cap (the caps are in
[Limits](limits.md#write-a-binding-an-agentbackend-or-a-chatconnection)).

## Features

| Feature | Slack | On a platform without it |
|---|---|---|
| Questions to a person: `interactions.askUser` | Works: Agent Kourier asks in the thread with buttons. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| Tool approvals: `interactions.toolApprovals`, and `interactions.approverGroups`, which requires it | Works: Agent Kourier asks in the thread with buttons. | Refused: `UnsupportedOnPlatform`. Set `toolApprovals` to `false` and remove `approverGroups`. |
| The useful and not-useful buttons: `feedback.enabled` | Works. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| The acknowledgment reaction: `chat.acknowledgment` | Works. With no `emoji` the reaction is `hourglass_flowing_sand`. | Served, and no reaction is shown. A Binding whose acknowledgment is enabled and names an `emoji` has the condition `AcknowledgmentShown`, `False` with the reason `NoReactions` ([Binding status](binding-status.md#acknowledgmentshown)). |

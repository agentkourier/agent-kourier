# What each platform supports

A Binding names its platform through its ChatConnection, whose `spec.platform` says which. This page lists, for each
platform, the shape of the chat IDs a Binding names, the trigger types and outputs it takes, and the features of a Binding that
the platform serves. The platforms are `slack` and `webhook`. The API server and the load take a `webhook` ChatConnection and its
Bindings, but this release's broker does not run one: as the broker's own connection it stops the start with a wiring error, and a
team's connection is not started, with a log line that the broker has no adapter for its platform.

The API server and the broker's load hold each chat ID of a Binding to Slack's shape, but the channel of a Binding whose triggers
are all `webhookEvent`, which they hold to a webhook channel's (see [Chat IDs](#chat-ids)). A Binding that names a channel's name
such as `general`, or a bot's name such as `alertmanager` as a sender, is refused when it is applied: the API server refuses it on
`kubectl apply`, `--dry-run=server` included, and on any other client that writes it. On the files route the load refuses it, and
so does `agent-kourier dry-run`:

```text
spec.chat.channel: invalid channel "general": must be a channel ID such as C0123456789, not a name
```

The API server's message for the channel says the same, and adds what a Binding of `webhookEvent` triggers takes instead.

The broker checks the Binding against its platform once more when it reads the Binding with its ChatConnection. That check refuses a
chat ID that does not have the shape of the platform's IDs (`InvalidPlatformID`), and a feature, trigger type or output that the
platform does not serve (`UnsupportedOnPlatform`). A Binding of `webhookEvent` triggers with the channel `acme/widgets`, for
example, passes the API server and is refused on a Slack connection. With Slack, which serves every feature in [Features](#features),
a Binding of Slack's triggers and IDs is not refused. A Binding it refuses is refused as a Binding that fails validation is:

- with custom resources its `Accepted` condition is `False` with that reason ([Binding status](binding-status.md#accepted)). A Binding
  that had a good generation keeps serving it, and the message says which (`serving generation 1`); a new Binding is not served. The
  other Bindings keep running;
- on the files route the whole config fails: at start the broker exits with code 2, and on a reload it keeps its last good config.

Its message names the field, the value, the platform, the ChatConnection and what to write instead, and links this page. When a
Binding has several such problems, the reason is that of the first, in the order of the fields.

A Binding that its ChatConnection does not admit (in another namespace that `allowedNamespaces` does not list) is told
`NamespaceNotAllowed` and nothing of the platform.

## Chat IDs

| Field | Slack | `webhook` |
|---|---|---|
| `chat.channel` | A channel ID: `C`, or `G` for an older private channel, then capitals and digits, at most 32 bytes, such as `C0123456789`. Not the channel's name. | The channel as the connection reads it from an event: letters, digits, `.`, `_` and `-`, starting with a letter or a digit, with `/` between parts, at most 140 bytes, such as `acme/widgets` or `OPS`. |
| A `botMessage` trigger's `from.id` | A bot ID: `B`, then capitals and digits, at most 32 bytes, such as `B0123456789`. Not the bot's name. | None: the platform takes no `botMessage` trigger. |
| A `chat.mentions` entry's `user` | A user ID: `U` or `W`, then 2 to 20 capitals and digits, such as `U0123456789`. | None: `chat.mentions` is refused, `UnsupportedOnPlatform`. |
| An entry of `interactions.approverGroups` | A user group ID: `S`, then capitals and digits, at most 32 bytes, such as `S0123456789`. | None: `interactions.approverGroups` is refused, `UnsupportedOnPlatform`. |

The API server and the load refuse any other value, with the field's pattern and length cap, and for `chat.channel` the rule
that chooses Slack's shape or the webhook channel's by the Binding's triggers (the caps are in
[Limits](limits.md#write-a-binding-an-agentbackend-or-a-chatconnection)).

## Triggers and output

| Field | Slack | `webhook` |
|---|---|---|
| `chat.triggers[].type` | `mention`, `botMessage` and `personMessage`. A `webhookEvent` trigger is refused, `UnsupportedOnPlatform`. | `webhookEvent` alone. Any other type is refused, `UnsupportedOnPlatform`, and so is a Binding that lists no triggers, which has `[{type: mention}]`. |
| A `webhookEvent` trigger's `match` and `exclude` keys, and the `.Fields` its `promptTemplate` reads | None. | Names of the connection's `spec.webhook.fields`; the template reads `.Channel` and the connection's `id` fields alone. A name the connection lacks, or a `text` field the template reads, is refused, `UnsupportedOnPlatform`; a template that reads `.Key`, `.State`, `.Link` or `.Source` is refused by the broker's validation (with custom resources, `InvalidSpec`), and not by the API server. |
| `chat.output` | `live` (the default) or `final`. `none` is refused: by the API server and the load on a Binding of Slack's triggers, and `UnsupportedOnPlatform` on one of `webhookEvent` triggers. | `none` alone: write it, since the default is `live`. `live` and `final` are refused, `UnsupportedOnPlatform`. |

## Features

| Feature | Slack | `webhook` | On a platform without it |
|---|---|---|---|
| Questions to a person: `interactions.askUser` | Works: Agent Kourier asks in the thread with buttons. | Not served. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| Tool approvals: `interactions.toolApprovals`, and `interactions.approverGroups`, which requires it | Works: Agent Kourier asks in the thread with buttons. | Not served. | Refused: `UnsupportedOnPlatform`. Set `toolApprovals` to `false` and remove `approverGroups`. |
| The useful and not-useful buttons: `feedback.enabled` | Works. | Not served. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| The acknowledgment reaction: `chat.acknowledgment` | Works. With no `emoji` the reaction is `hourglass_flowing_sand`. | The broker does not run the platform yet. | Served, and no reaction is shown. A Binding whose acknowledgment is enabled and names an `emoji` has the condition `AcknowledgmentShown`, `False` with the reason `NoReactions` ([Binding status](binding-status.md#acknowledgmentshown)). |

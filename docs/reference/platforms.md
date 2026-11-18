# What each platform supports

A Binding names its chat platform through its ChatConnection, whose `spec.platform` says which: `slack`, or `teams`, Microsoft
Teams. This page lists, for each platform, the shape of the chat IDs a Binding names and the features of a Binding that the platform
serves.

The broker runs a teams connection ([ChatConnection](chatconnection.md)) in `http` mode, on its chat listener
(`AGENTKOURIER_CHAT_LISTEN`): only the leader takes Teams' requests, at `POST /chat/teams/<namespace>/<name>`, and it posts, replies
and edits through the Bot Connector with a token that Microsoft Entra issues for the connection's client secret. A broker with no
chat listener does not run one: as its own connection it stops the broker's start with a wiring error that says to set
`AGENTKOURIER_CHAT_LISTEN`, and a team's connection is not started. `socket` mode is not supported yet: the API server takes it, and
the broker does not start the connection. As the broker's own connection it stops the start with a wiring error that says Socket
mode is not supported yet; a team's is not started, and the broker logs `Socket mode is not supported yet for Microsoft Teams`. A
team's connection that runs in `http` mode and is edited to `socket` is stopped, and the broker logs why. A Teams bot is its
Microsoft Entra app, so two teams connections with one `spec.teams.appID` are one app: the broker serves the older, as it serves the
older of two connections of one Slack app, and the younger's Bindings say `ConnectionNotServed`.

What Microsoft Teams sends again was measured on a test tenant in November 2026: a message answered 503 at once is sent once more,
about 0.6 seconds later; an activity answered 200, or one whose request Teams held until it gave up, is not sent again. Other
answers, and an invoke answered 503, were not measured. So a message that arrives while the connection is not taking them, or that
its channel has no room for, is lost unless there is room when it comes again, and an activity that the broker took and then could
not hand on is lost. The adapter counts the first in `agentkourier_teams_activities_shed_total` and the second in
`agentkourier_teams_activities_lost_total` ([Metrics](metrics.md)). A post goes to
the service URL that the channel's last activity named, which the broker keeps in its store, so a restart or a change of leader does not
lose it ([Stored data](stored-data.md)); for a channel that no activity has come from, or whose record cannot be read, it goes to
Microsoft's global route, `https://smba.trafficmanager.net/teams/`.

A message on Microsoft Teams shows the agent's reply in Teams' markdown, with what the agent, a person or an alert wrote shown as
text: no `<at>` in it becomes a mention, and no HTML or markdown link in it is acted on (a bare URL still links). A mention that Agent Kourier makes names a person by
their Microsoft Entra object ID and shows as `@someone`, since the broker does not read people's names on Teams yet. There are no live
progress streams, ephemeral messages or reactions, and nothing is pressed: buttons are shown as their labels.

A Binding is checked against its platform when the broker reads it with its ChatConnection: the API server and the broker's load
check only that each ID has the shape of some platform's ID and is within the field's cap, which is the largest of the platforms'. A
Binding that names an ID of another platform's shape, or an ID of its platform's shape that is longer than its platform's cap, or asks
for a feature that its platform does not serve, is refused then, as a Binding that fails validation is:

- with custom resources its `Accepted` condition is `False` with the reason `InvalidPlatformID` or `UnsupportedOnPlatform`
  ([Binding status](binding-status.md#accepted)). A Binding that had a good generation keeps serving it, and the message says which
  (`serving generation 1`), so an edit with an ID of the wrong platform does not take; a new Binding is not served. The other
  Bindings keep running;
- on the files route the whole config fails, with the same message: at start the broker exits with code 2, and on a reload it keeps
  its last good config.

`agent-kourier dry-run`, which reads only Slack, holds the Binding to Slack's shapes too, and refuses a Microsoft Teams ID with the
same problem and fix.

The message names the field, the value, the platform, the ChatConnection and what to write instead, and links this page, for example:

```text
spec.chat.channel: "19:a1b2c3d4e5f6@thread.tacv2" is not a channel ID on Slack, the platform of ChatConnection agent-kourier-system/slack-main: use the channel's ID, such as C0123456789 (G… for an older private channel), not its name (see https://agentkourier.dev/reference/platforms/)
```

A Binding that its ChatConnection does not admit (in another namespace that `allowedNamespaces` does not list) is told
`NamespaceNotAllowed` and nothing of the platform.

## Chat IDs

| Field | Slack | Microsoft Teams |
|---|---|---|
| `chat.channel` | A channel ID: `C`, or `G` for an older private channel, then capitals and digits, at most 32 bytes, such as `C0123456789`. Not the channel's name. | A channel ID: `19:`, then letters, digits, `.`, `_` and `-`, then `@thread.tacv2` or `@thread.skype`, at most 140 bytes, such as `19:a1b2c3d4e5f6@thread.tacv2`. Not the channel's name. |
| A `botMessage` trigger's `from.id` | A bot ID: `B`, then capitals and digits, at most 32 bytes, such as `B0123456789`. Not the bot's name. | The Microsoft Entra application ID of the app that posts, a GUID in lowercase hex, such as `11111111-2222-3333-4444-555555555555`. |
| A `chat.mentions` entry's `user` | A user ID: `U` or `W`, then 2 to 20 capitals and digits, such as `U0123456789`. | The person's Microsoft Entra object ID, a GUID in lowercase hex. |
| An entry of `interactions.approverGroups` | A user group ID: `S`, then capitals and digits, at most 32 bytes, such as `S0123456789`. | None: `approverGroups` is refused (see [Features](#features)). |

A GUID is taken in lowercase hex alone. The broker compares chat IDs byte for byte, so a GUID in capitals would load and then match
no message; the API server and the load refuse it, and the message says to write it in lowercase.

What the API server and the load take is wider: for each field, the shape of either platform's. A value that has neither, such as the
channel name `general`, is refused when it is applied, and so is a value past the field's cap: 140 bytes for a channel, 36 for a
sender and a mention's user, 32 for an approver group. A Microsoft Teams channel on a Slack connection, a Slack channel on a teams
connection, and a Slack channel or bot ID longer than Slack's 32 bytes (up to 140 for a channel and 36 for a sender) are refused by the
broker with `InvalidPlatformID`, as above, and not when they are applied. A Slack user ID cannot pass Slack's cap, since its pattern
bounds it at 21, and neither can an approver group, whose field's cap is Slack's 32. The length caps of each field are in
[Limits](limits.md#write-a-binding-an-agentbackend-or-a-chatconnection).

## Features

| Feature | Slack | Microsoft Teams | On a platform without it |
|---|---|---|---|
| Questions to a person: `interactions.askUser` | Works: Agent Kourier asks in the thread with buttons. | Taken, not answered yet: the question is shown as text, with a line that says it cannot be answered in Microsoft Teams yet, and it times out as an unanswered question does. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| Tool approvals: `interactions.toolApprovals`, and `interactions.approverGroups`, which requires it | Works: Agent Kourier asks in the thread with buttons. | `toolApprovals` is taken, and no approval is asked yet: a turn that asks for one is refused as a pause that cannot be shown, and its task is cancelled. `approverGroups` is refused: `UnsupportedOnPlatform`. Remove it. | Refused: `UnsupportedOnPlatform`. Set `toolApprovals` to `false` and remove `approverGroups`. |
| The useful and not-useful buttons: `feedback.enabled` | Works. | Taken, not pressed yet: the buttons are shown as their labels. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| Alert triggers: a `botMessage` trigger in `chat.triggers` | Works. | Taken, in either `spec.teams.mode` and with or without `spec.teams.notificationURL`, and not served yet: Microsoft Teams does not send a bot the posts of another app (measured on a test tenant in November 2026), and the broker does not read them through Microsoft Graph yet, so no alert starts a turn. | Refused: `UnsupportedOnPlatform`. Remove the trigger. |
| The acknowledgment reaction: `chat.acknowledgment` | Works. With no `emoji` the reaction is `hourglass_flowing_sand`. | Taken, and no reaction is shown, as on a platform without it. | Served, and no reaction is shown. A Binding whose acknowledgment is enabled and names an `emoji` has the condition `AcknowledgmentShown`, `False` with the reason `NoReactions` ([Binding status](binding-status.md#acknowledgmentshown)). |

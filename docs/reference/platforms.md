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
text: no `<at>` in it becomes a mention, and no HTML or markdown link in it is acted on (a bare URL still links). A mention that Agent Kourier makes in a message names a person by
their Microsoft Entra object ID and shows as `@someone`; only the card that ends a question or an approval names its decider
([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)). There are no live
progress streams, ephemeral messages or reactions. Questions, tool approvals and the useful and not-useful buttons are Adaptive
Cards, which a person presses ([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)).

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

## What Microsoft Teams shows while the agent works

Teams has no streams, so a live reply (`chat.output: live`), a mention's as an alert's, is one message that Agent Kourier posts
and then edits, at most once every 2 seconds ([Limits](limits.md#what-a-thread-shows)). Its first post shows the agent's text so far,
with a status line for the latest tool call, such as `Calling get_pods(ns="prod")`, or `Working…` in a gap. Each edit after it shows
the text so far and then, under it, the tool calls as a list in a quote, one line a call: `Calling name(arguments)` while it runs,
`Ran name(arguments)` once its result came, `name(arguments) failed` when the result says so, and `name(arguments) ended with no
result` for one with no result when the turn ended. The list holds the latest 20 steps, its `Working…` lines among them, under a
line such as `5 earlier steps are not shown.` With `chat.progress: steps-and-activity`, the default, a gap with nothing new
also adds a line, `Working…`, which the next tool call takes over, or which ends as `Wrote the answer`, `Wrote an update`, `Finished`, `Waiting for your answer` or `Stopped`.
The final edit keeps the list under the answer, as a record of what the agent did, and, when `feedback.enabled` is set, a card of
the useful and not-useful buttons under it. With `chat.progress: activity` there is no list: the message shows
one `Working…` line that counts the steps. An edit that Teams' budget for a message cannot hold with its list is sent again
without it, and the reply lists no steps from then on.

In a channel's thread, Teams' web client shows the list as a quote, with a bar at its left, and a bullet before each step
(seen in a live check, November 2026). Agent Kourier sets no busy mark on Teams: in the same check Teams showed no typing indicator
for the bot in a channel's thread. A reply that waits on the agent shows its latest line, `Working…` or a step, until the next edit.

Teams drops every span in braces from a message's prose: in a live check (November 2026, a channel thread, the web client)
`rate(http_requests_total{code=~"5.."}[5m])` showed as `rate(http_requests_total[5m])`. So every line that Agent Kourier writes
on Teams, other than the agent's reply, sends `{` and `}` as the character references `&#123;` and `&#125;`, and the agent's
reply sends them with a backslash, `\{` and `\}`, in its prose. Teams showed both as braces in the same check. In the reply's
code, inline or in a block, braces are sent as the agent wrote them, which Teams shows raw.

## Buttons on Microsoft Teams

A question or a tool approval is a message of one Adaptive Card, version 1.5. A question's card has its heading, the sub-agent
that asked if one did, each question with a text box or its choices (radio buttons or checkboxes up to 10 choices, a menu past
them), the time it closes, and Submit. An approval of one tool shows its name and arguments, with Approve and Deny; of several, each
tool numbered with its arguments and a choice of Approve or Deny under them, then Submit, Approve all and Deny all. When the question
or approval ends, Agent Kourier replaces the card with one that has no controls: who answered or cancelled it, or that nobody
answered in time, then what was asked with each answer, or each tool with `Approved` or `Denied` and its arguments. With
`feedback.enabled` set, the useful and not-useful buttons are a card of buttons under the reply. When Agent Kourier replaces a
card, Teams marks the message `Edited`.

The ending names who decided with a mention, which Teams styles and which notifies the person. Teams shows the text that Agent
Kourier writes in the mention, not the person's name, so when it edits the card the broker reads the name from the Bot Connector's
list of the members of the channel's team, or uses the name it read of the person in the last hour, and writes it there: the ending
reads, for example, `Alex Example answered.` It writes only the name's letters, marks, digits, `.`, `-` and `'`, and at most 64 units
of it ([Limits](limits.md)). A name that cannot be read, because the list cannot be read or no longer lists the person, is written
`someone`, and the mention still names the person. That Teams shows a name written so was not seen on a test tenant: the live check
saw the text `someone`.

In a card, what the agent, a person or an alert wrote is the text of a `TextRun` or the title of a choice, which Teams shows as the
characters they hold: `<at>`, markdown, braces and links in them show as written, and none becomes a mention or a link. So Agent
Kourier does not escape them. Arguments shown in monospace keep their braces and their line breaks. A tool whose name or arguments are longer than their cap is refused and never shown cut, and so is a card over the
budget of a message: the pause is refused as one that cannot be shown. An ending's texts are cut until its card fits
([Limits](limits.md#what-a-thread-shows)).

Teams sends a press to the broker in a request. On a test tenant on 18 November 2026 (web client, a channel thread), timed from the
press's arrival at the server to its answer, Teams showed the person who pressed an answer given at 12.5 seconds, and showed them an
error, "Something went wrong. Please try again.", for one given at 13 seconds or later, although Teams' service held the request
until about 14 seconds. Agent Kourier records the press and waits up to 6 seconds in the request for the worker that decides
recorded presses to decide it, inside a deadline of 8 seconds for the request ([Limits](limits.md)), then answers with a line for
Teams to show the person who pressed. A press decided in that wait is told what it came to: that it was recorded, or was already, or
why it was refused (not a member of the channel, not an approver, a question that is gone or that someone else answered first, an
answer that is not valid). A press not decided in the wait, or whose decision is to be tried again (a read of the members that
failed, say), is told that it was received (and, for a question's or an approval's card, that the card changes for everyone when it
is decided); the worker decides it after the answer, once, and since Teams has no ephemeral messages, a refusal then is told nothing
more and its card does not change. So is a press that waits when the broker stops: it is answered that it was received, and decided
by the worker. A press whose recording failed is told that it may not have been recorded and to press again, and one that names no
button of Agent Kourier's, or has no presser or no message, that it cannot be taken. Teams shows the line to the person who pressed,
as a banner under the card, and clears the card's inputs. The card changes for everyone by its own update when the question is
decided, not by this answer. In the same check the banner showed whether that update came before the answer or after it, and a press
whose answer came too late for the client, which showed the error, was still recorded and decided, and its card replaced.

The worker decides a press as it does on Slack: the person who pressed must be a member of the channel and no bot. On Microsoft
Teams the broker reads the Bot Connector's list of the members of the channel's conversation, which Microsoft documents as the
team's members for a team's channel, at every press, and finds the person by their Microsoft Entra object ID; someone the list does not hold is refused as
not a member. Whether the person is a bot it reads from the account that list gave, or from the lists of the members of the teams
it has heard from since it started: an account whose Teams ID begins `28:` is a bot's. A person whom no list holds, a read that
fails, a read that is not done within 10 seconds, and one that would need more than 50 pages of the lists ([Limits](limits.md)) are no
answer: the press is tried again, with a line at WARN for
each try, until the worker gives up on it, an hour after it was recorded, and logs that at ERROR. The list's paging, the fields of its
answer and its errors are what Microsoft documents: no list was read on a test tenant.

A Binding's `ChannelJoined` condition on Teams reads the Bot Connector's list of the channels of the channel's team: it is `True`
when the list names the channel, and `False` with the reason `ChannelNotFound` when it does not. That list says nothing of an
archive, so the reason is never `ChannelArchived` on Teams: whether the list leaves out an archived channel, or the channels of an
archived team, is not known.

What is written here of how Teams shows cards and answers presses was seen on a test tenant, in the web client, in a channel thread,
in November 2026. In the same check, an edit that sent no text left the message's earlier text in place. Every edit that Agent
Kourier makes on Teams sends the text a message is to show: a reply's edits always hold text, and a question's card, and the card
that replaces it, have none to leave. Whether an edit that sends no card takes away a card the message had was not seen: if it does not,
a card of buttons would stay, and could still be pressed, on a message that an edit meant to take it off. A reply gets its buttons
on its last edit, and no later edit of Agent Kourier's is meant to take them off.

## Earlier replies on Microsoft Teams

A mentioned turn carries the replies that people posted in its thread since the previous turn, as on Slack ([Limits](limits.md#what-a-thread-shows)).
On Microsoft Teams the broker reads them from Microsoft Graph, `GET /teams/{team}/channels/{channel}/messages/{thread}/replies`, with a
token for Microsoft Graph that Microsoft Entra issues to the connection's app for the same client secret. The app may read them under
the resource-specific permission `ChannelMessage.Read.Group`, which its manifest must ask for and which the team grants when the app
is installed in it. On a test tenant in November 2026, Graph issued that token and answered a read of a channel's messages under that
permission alone, with no tenant-wide consent.

Graph names a team by its Microsoft Entra group. The broker learns it from the activities that Teams sends of the channel, and keeps
it in its store ([Stored data](stored-data.md)). A channel none of whose activities has named its team cannot be read yet: the turn
goes with the line that says the earlier replies could not be read, and the broker logs why at WARN.

Graph takes no bound on a thread's replies but the page size, so the broker reads every page, 50 replies a page and at most 20 pages,
and keeps the replies of the window itself. A thread with more replies than that cannot be read, however few the window holds, and
the turn goes with the same line ([Limits](limits.md#what-a-thread-shows)). The broker orders the replies by their message IDs, which on
the test tenant were each post's time in milliseconds; Microsoft calls them opaque. A reply's HTML is read as text: its tags are
dropped, a paragraph, a list item or a line break begins a line, and its character references are decoded. A post of another app, such
as an alert's card that Workflows posted, is read as a bot's message, whose body is the text of its card; a person's mention leaves
bots' messages out of the replies it carries, as on Slack. A system event and a deleted message are left out.

A 403 of Graph, which an app gets that may not read the team's channel messages, is logged with the fix: the manifest must ask for
`ChannelMessage.Read.Group`, and the app must be installed in the team. A 429 that asks for at most 5 seconds is waited out once; any
other refusal, and a read that runs out of time, sends the turn with the line. The replies endpoint, its paging, the order of its
replies and its errors are Microsoft's documentation: none of them was measured.

## Finding a post again on Microsoft Teams

The outbox records each post before it sends it, and before it posts one that it queued or tried before, a post that was sent and not
recorded before a crash among them, it asks the platform for the post by its tag. On Slack the tag is in the message's metadata. On
Microsoft Teams it is in a part of the post that people do not see: an empty `<span>` whose `title` is the tag's carrier, in a paragraph
of its own at the end of a text, or the `id` of the first element of a card's body when the card is the whole post (a question's or an
approval's card, and the card that ends one). A reply with buttons, such as the useful and not-useful buttons, carries it in its text. An edit of a post
does not carry the tag: the outbox looks for a post only until it has recorded where the post landed, and nothing edits a post before
then. The broker looks for it through
Microsoft Graph, with the same token, permission and bounds as a read of a thread's replies ([Limits](limits.md#what-the-broker-keeps-and-logs)): the
thread's replies, or, for a post that opens a thread, such as a trigger's digest line, the channel's messages, of which it reads the
first 200 that Graph gives. Only a post of the connection's own app counts, so a person or another app that writes the same span
does not keep a post from being sent, and the agent cannot write one: no `<` of its text reaches Teams as HTML.

On a test tenant in November 2026, with the web client, the span came back through Graph at the end of a reply and of a post that
opened a thread, and so did the `id` of the two kinds of card element that the broker puts first on a card, the `TextBlock` and the
`RichTextBlock`. A reply was in Graph at the first look, 1.4 seconds after it was posted. The client showed no carrier, and the span's paragraph adds a
little space under the text. The span was tried only at the end of a text. Teams' mobile and desktop clients, screen readers, Teams'
search and notifications, and the order in which Graph lists a channel's messages were not checked.

A reply with buttons is sent as one message of text and a card, which Teams answers with no ID and shows as two messages: the text,
with the span, and a second message that holds only the buttons, with no tag. The broker does not learn either one's ID from the post,
so it looks the reply up, finds the text by its tag and records it; the message of buttons is one that the broker never knows the ID
of, and a later edit of the reply does not reach it.

A post that the pages read do not hold, a channel whose team the broker does not know yet, a read that runs out of time after Graph
gave a page, a page that Graph names that is not Graph's or that the read already took, an answer longer than 8 MiB, and a refusal of
Graph that no retry mends (a 400, 401, 403 or 404, or Microsoft Entra's refusal of the token for Graph) are taken as not found: the
broker logs why at WARN and sends the post, which may then show twice. A 429, a 5xx, an answer that is not JSON and a lookup that runs
out of time before any page came back are tried again later.

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
| Questions to a person: `interactions.askUser` | Works: Agent Kourier asks in the thread with buttons. | Works: the question is a card that a member of the channel's team presses ([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)). | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| Tool approvals: `interactions.toolApprovals`, and `interactions.approverGroups`, which requires it | Works: Agent Kourier asks in the thread with buttons. | `toolApprovals` works: the approval is a card that a member of the channel's team presses ([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)). `approverGroups` is refused: `UnsupportedOnPlatform`. Remove it. | Refused: `UnsupportedOnPlatform`. Set `toolApprovals` to `false` and remove `approverGroups`. |
| The useful and not-useful buttons: `feedback.enabled` | Works. | Works: the buttons are a card under the reply, and a vote of a member of the channel's team is counted ([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)). | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| Alert triggers: a `botMessage` trigger in `chat.triggers` | Works. | Taken, in either `spec.teams.mode` and with or without `spec.teams.notificationURL`, and not served yet: Microsoft Teams does not send a bot the posts of another app (measured on a test tenant in November 2026), and the broker does not watch a channel through Microsoft Graph yet, so no alert starts a turn. It reads Graph only for a thread's replies, for a mention's context ([Earlier replies on Microsoft Teams](#earlier-replies-on-microsoft-teams)), and to find its own posts again ([Finding a post again on Microsoft Teams](#finding-a-post-again-on-microsoft-teams)). | Refused: `UnsupportedOnPlatform`. Remove the trigger. |
| The earlier replies a mentioned turn carries: `chat.threadReplies: mention`, the thread of a `personMessage` trigger, and a bot's mention | Works. | Works for a person's mention, read through Microsoft Graph ([Earlier replies on Microsoft Teams](#earlier-replies-on-microsoft-teams)). Teams sends a bot no other app's posts, so no bot's mention reaches it. | Served: the turn carries the mentioned message alone. |
| The acknowledgment reaction: `chat.acknowledgment` | Works. With no `emoji` the reaction is `hourglass_flowing_sand`. | Taken, and no reaction is shown, as on a platform without it. | Served, and no reaction is shown. A Binding whose acknowledgment is enabled and names an `emoji` has the condition `AcknowledgmentShown`, `False` with the reason `NoReactions` ([Binding status](binding-status.md#acknowledgmentshown)). |

# What each platform supports

A Binding names its chat platform through its ChatConnection, whose `spec.platform` says which: `slack`; `teams`, Microsoft Teams;
or `webhook`. This page lists, for each platform, the shape of the chat IDs a Binding names, the trigger types and outputs it takes,
and the features of a Binding that the platform serves. The broker runs Slack and Microsoft Teams connections. The API server and the
load take a `webhook` ChatConnection and its Bindings, but this release's broker does not run one: as the broker's own connection it
stops the start with a wiring error, and a team's connection is not started, with a log line that the broker has no adapter for its
platform.

The broker runs a teams connection ([ChatConnection](chatconnection.md)) in `http` mode, the default, on its chat listener
(`AGENTKOURIER_CHAT_LISTEN`): only the leader takes Teams' requests, at `POST /chat/teams/<namespace>/<name>`, and it posts, replies
and edits through the Bot Connector with a token that Microsoft Entra issues for the connection's client secret. A broker with no
chat listener does not run one: as its own connection it stops the broker's start with a wiring error that says to set
`AGENTKOURIER_CHAT_LISTEN`, and a team's connection is not started. A Teams bot is its
Microsoft Entra app, so two teams connections with one `spec.teams.appID` are one app: the broker serves the older, as it serves the
older of two connections of one Slack app, and the younger's Bindings say `ConnectionNotServed`.

In `socket` mode (`spec.teams.mode: socket`) the connection needs no chat listener and no public endpoint: the leader opens an
outbound WebSocket to Microsoft Teams in each of its regions, `amer`, `emea` and `apac`, and takes the activities that Teams sends over
them as it takes the requests of `http` mode, answering each over its socket. Microsoft calls Socket mode a public preview and
recommends an HTTPS endpoint in production; its documentation has the bot's delivery method set to Socket mode first. A region that
refuses the bot's socket answers 403, which the broker logs and tries again every 5 minutes, give or take a fifth, while the other
regions serve (on a test tenant in November 2026, `apac` answered 403, "Socket mode is not enabled for this bot.", while `amer` and
`emea` worked). Only the leader opens sockets,
because Teams sends a region's activities to the newest socket of the bot and nothing to the older; the sockets close when the
leader's run ends, and a standby opens none. Each socket is replaced before its token expires, every 9 minutes with the 10-minute
life measured, by one that takes over its region before the old one closes. A rejected client secret, or a 401 to the call that opens
a socket, stops the connection's run as in `http` mode; if every region answers 403 and none has a socket, the run ends and the broker
runs it again after a wait. A team's connection that is edited from one mode to the other is restarted in the new mode; the broker's
own connection takes the edit when the broker restarts.

**What Teams sends while no socket is ready is lost.** Teams does not queue an activity for a bot with no socket, nor send it to an
HTTPS endpoint: a message or a mention sent while the connection has no ready socket in any region (while the broker starts,
between a socket that ended and the next, while leadership moves, or while the broker is down) never reaches the broker (measured on a
test tenant in November 2026). The broker logs each such interval after the first with its start and end, and observes it in
`agentkourier_teams_socket_gap_seconds`; `agentkourier_teams_socket_ready` says which regions have a socket, and
`agentkourier_chat_connected` is 1 while one does ([Metrics](metrics.md)). The bounds and waits are on [Limits](limits.md#run-the-broker).

What Microsoft Teams sends again was measured on a test tenant in November 2026, in `http` mode (over a socket it was not): a message answered 503 at once is sent once more,
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
their Microsoft Entra object ID and shows the person's name, which the broker reads from the members of the channel's team as it
does for the decider that ends a question or an approval ([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)); a name it
cannot read shows as `@someone`. There are no live
progress streams, ephemeral messages or reactions. Questions, tool approvals and the useful and not-useful buttons are Adaptive
Cards, which a person presses ([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)).

The API server and the broker's load check only that each chat ID of a Binding has the shape of a chat platform's ID, Slack's or
Microsoft Teams', and is within the field's cap, which is the largest of the two platforms'; the channel of a Binding whose triggers
are all `webhookEvent` they hold to a webhook channel's instead (see [Chat IDs](#chat-ids)). A Binding that names a channel's name
such as `general`, or a bot's name such as `alertmanager` as a sender, is refused when it is applied: the API server refuses it on
`kubectl apply`, `--dry-run=server` included, and on any other client that writes it. On the files route the load refuses it, and
so does `agent-kourier dry-run`:

```text
spec.chat.channel: invalid channel "general": must be a channel ID such as C0123456789 (Slack) or 19:…@thread.tacv2 (Microsoft Teams), not a name
```

The API server refuses it too, with a message that also says what a Binding of `webhookEvent` triggers takes.

The broker checks the Binding against its platform once more when it reads the Binding with its ChatConnection. That check refuses a
chat ID that does not have the shape of the platform's IDs, or has it and is longer than the platform's cap (`InvalidPlatformID`),
and a feature, trigger type or output that the platform does not serve (`UnsupportedOnPlatform`). A Microsoft Teams channel on a
Slack connection, a Slack channel on a teams connection, and a Binding of `webhookEvent` triggers with the channel `acme/widgets` on
either, for example, pass the API server and are refused then. With Slack, which serves every feature in [Features](#features), a
Binding of Slack's triggers and IDs is not refused. A Binding it refuses is refused as a Binding that fails validation is:

- with custom resources its `Accepted` condition is `False` with that reason ([Binding status](binding-status.md#accepted)). A Binding
  that had a good generation keeps serving it, and the message says which (`serving generation 1`), so an edit with an ID of the
  wrong platform does not take; a new Binding is not served. The other Bindings keep running;
- on the files route the whole config fails, with the same message: at start the broker exits with code 2, and on a reload it keeps
  its last good config.

`agent-kourier dry-run`, which reads only Slack, holds the Binding to Slack's shapes too, and refuses a Microsoft Teams ID with the
same problem and fix.

Its message names the field, the value, the platform, the ChatConnection and what to write instead, and links this page, for
example:

```text
spec.chat.channel: "19:a1b2c3d4e5f6@thread.tacv2" is not a channel ID on Slack, the platform of ChatConnection agent-kourier-system/slack-main: use the channel's ID, such as C0123456789 (G… for an older private channel), not its name (see https://agentkourier.dev/reference/platforms/)
```

When a Binding has several such problems, the reason is that of the first, in the order of the fields.

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
The final edit keeps the list under the answer, as a record of what the agent did, and, when `feedback.enabled` is set, posts the
useful and not-useful buttons after it, as a card of their own ([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)). With `chat.progress: activity` there is no list: the message shows
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
`feedback.enabled` set, the useful and not-useful buttons are a card of their own, a message that Agent Kourier posts in the thread
right after the reply's text, or, for a reply that it edits as the agent works, after the final edit. When Agent Kourier replaces a
card, Teams marks the message `Edited`.

A reply's text and its buttons are two posts because Teams did not take them as one: on a test tenant in November 2026, Teams answered
one message of text and a card with no ID, and showed it as two messages, the text and a card of the buttons alone, so the broker could
not learn either one's ID. Posted apart, each is a message of its own, and the text is the message that the broker records as the reply.
A press of the buttons is taken as a press of that text, which the card names. Each button of the card carries, beside the text's ID, an
HMAC-SHA256 of the connection, the channel, the thread, the text's ID and the button's value, under a key that the broker derives from
the connection's client secret and neither logs nor stores. A press is taken only when it carries the MAC that the broker computes for
it, so every replica takes it, and so does the broker after a restart; a press whose data names another message, another value or
another thread is refused, and the person who pressed is told that Agent Kourier cannot take it. When the client secret is rotated, the
presses of every card of buttons posted before are refused the same way: a vote on an older reply can no longer be cast. What the person
who pressed is told shows under the card they pressed, and no edit follows a vote.

If the post of the buttons fails for this try only (a rate limit, a 5xx, a call that ran out of time), the broker waits as the answer
asks, if that is at most 5 seconds, and tries once more ([Limits](limits.md)). If that fails too, or Teams refuses the post for good, the
text stands, and the broker logs it at WARN; the next edit of the reply that has the buttons posts them, and a reply that is posted and
never edited after keeps its text without them. A text whose ID is longer than 64 bytes or holds a `:` gets no buttons. The broker
remembers which buttons it posted under which text, so that an edit with the same buttons does not post them again, and an edit with
other buttons edits their card; if that card is gone (someone deleted it), the broker posts the buttons anew, and if that post fails
for this try only, the edit fails and is tried again. It forgets the oldest past
4096, and remembers nothing across a restart or between replicas, where such an edit posts them a second time ([Limits](limits.md)).

On a test tenant in November 2026, with the web client, a reply that opened a thread was posted this way: Teams gave the text and the
card each its own ID, put the card in as the text's reply, and showed the text and then the card. A press of the card came with the
card's ID as the message pressed, in the text's thread, and with the card's data whole (the button, its value, the text's ID and the
MAC); the broker took it as a press of the text, and refused the same press with another text's ID. The answer to the press showed as
a banner under the card. The client showed the reply under the text only after the page was reloaded. Not checked: a reply posted
into a thread that was already open, an edit that posts or edits the card, a rate limit on the card's post, and Teams' mobile and
desktop clients.

The ending names who decided with a mention, which Teams styles and which notifies the person. Teams shows the text that Agent
Kourier writes in the mention, not the person's name, so when it edits the card, and when it posts or edits a message that mentions
a person, the broker reads the name from the Bot Connector's
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
that replaces it, have none to leave. A reply's edits send its text alone: its buttons are a message of their own, which an edit
of the reply posts if it has not been posted, and edits only when the buttons change. A reply gets its buttons on its last edit, and
no later edit of Agent Kourier's is meant to take them off; an edit with none leaves the card of buttons as it is.

## Alerts on Microsoft Teams

Microsoft Teams sends a bot no post of another app, in either mode, so an alert's card that Workflows or any other app posts in a channel
never comes to the broker as an activity (measured on a test tenant in November 2026). The broker reads such posts from Microsoft Graph
instead, in either mode: by Graph's notifications when the ChatConnection has a `spec.teams.notificationURL`, and by polling Graph when it
has none ([Polling](#polling-without-a-notificationurl), below). The `notificationURL` is the public HTTPS URL at which Microsoft Graph reaches the
connection's route on the leader's chat listener, `https://<host>/chat/teams/<namespace>/<name>`. With the chart that is the chat
Service, which selects the leader alone, behind your ingress. The URL needs the chat listener in either mode: a broker with none refuses the
connection with a wiring error that says to set `AGENTKOURIER_CHAT_LISTEN`. In `socket` mode the route takes Microsoft Graph's requests alone,
and answers anything else 400. A connection without a `notificationURL` needs no public route for its alerts.

**Subscriptions.** While it leads, the broker keeps a Microsoft Graph subscription for each channel that a Binding of the connection names,
to the channel's messages (`/teams/{group}/channels/{channel}/messages`, for new and edited messages, with no message content in the
notification), with the `notificationURL` as both its notification URL and its lifecycle URL. The app makes it under the resource-specific
permission `ChannelMessage.Read.Group`, which reads a thread's replies too ([Earlier replies on Microsoft Teams](#earlier-replies-on-microsoft-teams));
on a test tenant in November 2026, Graph made one under that permission alone. The broker compares the subscriptions with the Bindings
every minute, and within 1 s of a change of the channels that the Bindings name, once any comparison in progress ends: a channel that a Binding names gets one, and one
whose channel no Binding names any longer is deleted. Graph names a team by its Microsoft Entra group, which the broker learns from an
activity of the channel: a channel whose group it does not know yet waits, which it logs once at WARN with the fix (install the app in the
channel's team and mention the bot in the channel), and is subscribed as soon as an activity has told it. A subscription is made to expire 24 hours later and renewed once it would expire within 12 hours,
or within half the lifetime that Graph granted when it made or last renewed it, if Graph granted less than it was asked.
Making or renewing one took about 8 seconds on a test tenant, while Microsoft Graph sent its validation request to the URL, which the broker
answers with the token that Graph sent. At the start of each run the leader lists the app's subscriptions, even with no channel bound,
keeps those to the connection's URL that it can, renewing any near its expiry, and deletes the others to that URL; a subscription to any
other URL, another connection's or another broker's, is left as it is. When the leader stops, its subscriptions stay in Graph, which goes on
sending to the URL, and the next leader takes them over. A call that fails is made again at the next comparison. If the listing at the
start of a run fails, nothing is subscribed until a listing succeeds. If a listing after Graph refused a make with 409 fails, the
subscriptions the leader holds are still renewed, and no subscription is made until a listing succeeds. The bounds are on
[Limits](limits.md#run-the-broker).

**Changing the `notificationURL`.** Microsoft Graph lets an app have one subscription to a channel's messages, and refuses a second with
409 (Microsoft's documentation). A subscription that the app made for the old URL, or that another broker of the same app holds for its
own URL, therefore holds the channel: the broker cannot subscribe it for the new URL, and leaves the other subscription as it is, since it
cannot tell whether another broker still reads it. It logs that once at WARN for each channel, with the other subscription's URL (its scheme, host and path, never its query) and
expiry and the fix, counts the channel as `failing`, and tries again every 10 minutes, or when that subscription expires if sooner. Until
it expires, up to 24 hours after its broker last renewed it, or until someone deletes it, no alert of that channel reaches the connection.
To move without that gap, delete the old subscriptions in Microsoft Graph (`DELETE /subscriptions/{id}`, as the app) once nothing serves
the old URL.

**Notifications.** A notification carries a `clientState`, which the broker derives from the connection's client secret, so every replica
and every restart has the same. A notification whose `clientState` is not the connection's is dropped, and the subscription it names is
made anew, which also mends a subscription made before the client secret was rotated. A notification names a message and holds nothing of
it, so the broker answers Microsoft Graph 202 at once, then reads the message from Graph on its channel's lane, in order with the
channel's activities:

- a post of another app, such as an alert's card that Workflows posted, is a bot's message from the app's Microsoft Entra application ID
  (`358f0194-6b0e-4dd3-af35-c24fe8a9ec87` for Workflows, measured), whose body is the text of its cards: each `TextBlock`, and each fact of a
  `FactSet` as a line `title: value`. It starts a turn when a `botMessage` trigger of the channel names that ID, as on Slack
  ([Triggers](chat-triggers.md)); one that mentions Agent Kourier is a bot's mention;
- a person's post is the person's, as if Teams had sent it. A post that comes both as an activity and through Graph, such as a mention of
  the bot, is handed on once, whichever comes first;
- Agent Kourier's own posts, edits and deletions, system events and deleted messages are dropped: an edit of an alert's card does not
  start a turn again. An edit or a deletion is dropped without a read, and so is a post that the replica that leads made (a reply, or the
  card of its buttons), which the broker remembers in memory for at least 10 minutes; one it no longer remembers, or another replica's,
  is read and then dropped.

Microsoft Graph's lifecycle notifications come to the same URL: `reauthorizationRequired` renews the subscription at once,
`subscriptionRemoved` makes it anew, and `missed`, which says that Graph did not deliver some notifications of the channel, is logged at
WARN and starts a catch-up read of the channel ([Catch-up reads](#catch-up-reads)).

**What is lost.** A notification that comes while the connection does not run, before it has listed its subscriptions, or when its
channel's lane is full is answered 503, and Microsoft Graph sends it again (for up to 4 hours, by Microsoft's documentation; not measured).
A message that cannot be read from Graph in its tries is dropped: its notification was answered 202, and Graph does not send it again.
`agentkourier_teams_graph_notifications_total` counts each notification by what came of it, and `agentkourier_teams_graph_channels` the bound
channels by the state of their subscription ([Metrics](metrics.md)).

### Polling without a notificationURL

A connection with no `spec.teams.notificationURL` polls Microsoft Graph for the new messages of each channel that a Binding of the
connection names, in either `spec.teams.mode`, so `botMessage` triggers work on every Microsoft Teams connection. The leader alone polls,
while the connection runs, under the same resource-specific permission and with the same Microsoft Entra group as the subscriptions: a
channel whose group the broker does not know yet waits, logged once at WARN with the same fix and counted as `waiting_group`, and is read
at the next poll round once an activity has told the group: at once, or as soon as a round in progress ends.

- **New posts.** Each channel is read every 10 seconds through Graph's delta of its messages (`…/messages/delta`, then the `deltaLink` it
  gives), so a new post reaches the broker within about one interval after Graph shows it. A post is new when it was made at most 30 seconds
  before the channel's last read began; an edit, a reaction or another change of an older post, which the delta gives again, is not handed on.
- **Replies.** The delta does not give a reply in an existing thread (measured on a test tenant in November 2026). At each poll the
  broker reads the replies (`…/messages/{id}/replies`, the newest 50) of the 3 newest threads of each channel that it has seen begin
  within the last hour, and of the threads it read at the poll before, so a thread that a new one pushes out of the 3 has its replies
  read once more: at most 6 reads. It hands on each reply that no read of the thread gave before and that was made at most 30 seconds
  before the newest reply it read there, so a reply that Graph shows late, within those 30 seconds, is still handed on. **A reply in an
  older thread, or in a thread past the newest 3, is not read by polling.** A person's mention there still comes as an activity; another
  app's reply there does not reach the broker. A thread whose replies Graph refuses to read (a 400, 403 or 404) is no longer read; the
  channel is polled on.
- **What it hands on.** Each message goes the way of a notified message (above): another app's post is a bot's message when a `botMessage`
  trigger names its app, a person's post is the person's, Agent Kourier's own posts, system events and deleted messages are dropped, and a
  post that also came as an activity is handed on once, whichever comes first. What a read gives goes on its channel's lane, and the
  read's `deltaLink` is kept only after that, so a read whose lane had no room is made again.
- **What it holds.** Where each channel's delta stands, the `deltaLink` of its last read whose messages the lane has handed on and when
  that poll began, is kept in the broker's store ([Stored data](stored-data.md)), written at most once a minute while the `deltaLink`
  changes and every 10 minutes while it does not ([Limits](limits.md#run-the-broker)). The replies read of each thread are kept in
  memory. A run's first poll of a channel reads its stored position and resumes the delta from it, so a post made while no broker polled
  (a restart, a change of leader) is handed on if it was made no earlier than 30 seconds before the stored poll began. A run reads the
  channel anew when it has no stored position, when the position is more than an hour old, or when its time is more than 30 seconds
  ahead of the broker's clock (the last two logged at WARN): its first reads page through Graph's delta to a fresh `deltaLink`, at most 5
  pages at each poll and 500 in all, and hand on nothing from those pages, which hold the channel's history. Until the channel has its
  `deltaLink`, each of its polls makes a catch-up read ([Catch-up reads](#catch-up-reads)), the first of what was posted in the 10
  minutes before, so a channel with a long history hands on its new posts while it is read to its `deltaLink`. A channel read anew hands
  on nothing older than the 10 minutes, and a resumed one nothing posted before its stored poll, at most an hour back, so an old alert
  does not fire a trigger. A channel whose delta names no `deltaLink` within 500 pages counts as `failing`, logged once at WARN, and is
  read by its catch-up read alone, each interval, until the run ends (a restart or a change of leader) or no Binding names it and one
  names it again, when it is read to its `deltaLink` anew. A channel that no Binding names any longer is forgotten, and its stored
  position deleted. The first round of each run deletes too the stored positions of the connection's channels that no Binding names
  then, so a Binding removed while no broker polled, or one whose delete failed, leaves none. A Binding that names the channel again
  reads it anew, and what was posted meanwhile is not handed on. A delete that fails is logged once at WARN and made again a minute
  later, and each minute while it fails, and the polls go on. A channel that no Binding named at the run's first round, or whose delete
  failed, is read anew when a Binding names it in the run. A Binding removed and added back while no broker ran is not seen: the
  channel resumes from its stored position, as one that stayed bound does.
- **Errors.** Graph's 429 pauses every read of the connection for its `Retry-After`, since Graph throttles an app in a tenant, and ends the
  channel's poll. A 403 (the app may not read the channel's messages: install or update the app in the team), a 401, a 404 or a 400 of
  a channel's delta or catch-up read ends the channel's poll and counts the channel as `failing`, logged once at WARN. Another failure is
  logged at DEBUG, and the rest of the poll is made: a delta read that fails still leaves the channel's replies and its due catch-up read
  to be read. A channel whose poll fails is read again after 10 seconds, twice as long after each further failure, up to 5 minutes; the
  other channels go on. A `deltaLink` that Graph no longer takes, the stored one included (410, or a 400 that asks for a new sync, by
  Microsoft's documentation; not measured), is logged at WARN, and the channel is read to a new `deltaLink` as at a start, with a
  catch-up read of what was posted since the last poll, within the 10 minutes. A stored position that cannot be read fails the channel's
  poll, which is made again after the same backoff. A write of a position that fails is logged once at WARN, until one succeeds, and
  tried again a minute later; the channel is polled on. A write that the store refuses because the broker no longer leads stops the
  connection's polls; on Postgres the process then exits, as on any write a replaced leader makes.
- **Cost.** A connection makes at most 4 reads a second, one at a time, at least 250 milliseconds apart: 240 reads a minute, which no
  setting changes. Once it has its `deltaLink`, a channel costs one delta read and 3 reads of replies each 10 seconds; at a poll that
  finds new threads, 4 to 6 reads of replies (6 when three or more threads begin): 24 to 42 reads a minute. While it is read to its
  `deltaLink`, a poll of it makes at most 5 pages, 3 reads of replies and a catch-up read, 9 reads. [Limits](limits.md#run-the-broker)
  has the budget.
- **How many channels fit.** A connection makes at most 4 reads a second, 40 in 10 seconds. A poll of a channel is one read of its
  delta (one page), and one read of the replies of each of the channel's 3 newest threads that began within the last hour, with those
  of the threads it read at the poll before. So one connection polls every 10 seconds up to 40 bound channels in which no thread
  began within the last hour (1 read each), 10 in which three did and none begins now (4 reads), 8 when one begins in each at every
  poll (5 reads: the 3 newest and the one it pushed out), and 5 when three or more do (7 reads). Past that, nothing is skipped and no read is dropped: the reads wait their
  turn, so each channel's polls come later than every 10 seconds, the more so the more channels are bound, and a new post is found
  that much later. A delta read still finds every new post. Replies are where a long interval costs: a poll reads the replies of a
  channel's 3 newest threads, so when more threads begin between two polls, the replies of the older ones are not read. For more
  channels, give the connection a `notificationURL`: its notifications cost no poll. On a test tenant, 22 reads at 5 seconds
  apart drew no 429; the throttle of a channel's messages is not measured.

### Catch-up reads

A catch-up read is one read of a channel's 20 newest threads with their replies (`…/messages?$top=20&$expand=replies`), of which the
messages made since a given time are handed on as a poll's are, through the same memory of what was handed on. It looks back 10 minutes
at most, the window in which that memory holds a message, so nothing that was handed on already in this run is handed on again; a
catch-up read that fails is made at the channel's next poll, looking back 10 minutes from then. A reply in a thread past the 20 newest is
not recovered, nor anything posted earlier than the look-back.

- **At the start of a run**, on a connection that polls: what was posted in the 10 minutes before, whether the channel's delta resumes
  from its stored position or not. The resumed delta gives the new threads posted while no broker polled, but not the replies in threads
  that began earlier, and the threads whose replies a poll reads are held in memory: this read gives those replies, within the 10
  minutes, and the threads to read the replies of. That covers a change of leader and a restart, in `socket` mode the activities that came
  while no socket was connected included.
- **While a channel has no `deltaLink`** (its first reads in a run, a new sync after Graph refused its `deltaLink`, or a channel past the 500
  pages): at each poll, what was posted since the last catch-up read of the channel began, less 30 seconds.
- **When a gap of the sockets ends** (`socket` mode): with no `notificationURL`, every channel is read at once, its delta and its recent
  threads' replies as at a poll, and a catch-up read back to the gap's start, at most 10 minutes. With a `notificationURL` nothing more is
  read: Graph notified the posts of the gap all the same (measured on a test tenant in November 2026), mentions included.
- **On Graph's `missed`** (a `notificationURL`): what was posted in the channel in the 10 minutes before.

**After a restart.** What a run handed on is held in memory, so a post that a catch-up read, or a delta resumed from the stored
position, gives again after a restart is handed on again: the stored position is the last one written, a minute or more behind what
the run had handed on (a write is made at a poll, at most once a minute, once the lane has handed on the read; longer while writes
fail). The broker keeps in its store what such a post did, and drops it there: a trigger that threads by key finds the thread the message
already opened; a trigger that starts a turn of its own finds that turn, which is named by the message, queued already, and starts no
second investigation; a start that a trigger's limits stopped is not counted or told again; and a trigger's note is one row whatever asks
for it. The intake's metrics count the post again: `agentkourier_trigger_matched_total` and `agentkourier_trigger_decisions_total`, and,
for a turn, `agentkourier_intake_total` with `source="reply_queue",disposition="duplicate"`.

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

The thread's first message is the exception. Teams sends a bot no other app's posts, so an alert's card reaches a trigger only through
Microsoft Graph ([Alerts on Microsoft Teams](#alerts-on-microsoft-teams)); when no trigger takes it, a person who mentions Agent Kourier in
the card's thread starts a session there. That session's first turn carries the message that opened the thread,
whoever posted it, Workflows or a person, whatever `chat.threadReplies` says; it carries no reply of the thread. Agent Kourier's own post,
a blank one and one that was already a turn are not carried. The broker reads the first message alone, with one request,
`GET /teams/{team}/channels/{channel}/messages/{thread}`, and none of the replies, so a thread with too many replies still gives it,
and the turn carries it with no line. A first message that Graph says is gone is not carried; a read that fails carries the line in
its place. No later person's turn carries it again: it reads at most what came after
the turn before it. On Slack an alert's post reaches its `botMessage` trigger, which starts the session with the post, and a person's
first mention in a thread carries the person's message alone.

A 403 of Graph, which an app gets that may not read the team's channel messages, is logged with the fix: the manifest must ask for
`ChannelMessage.Read.Group`, and the app must be installed in the team. A 429 that asks for at most 5 seconds is waited out once; any
other refusal, and a read that runs out of time, sends the turn with the line. The replies endpoint, its paging, the order of its
replies and its errors are Microsoft's documentation: none of them was measured.

## Finding a post again on Microsoft Teams

The outbox records each post before it sends it, and before it posts one that it tried before, or that was queued before this leader
started, a post that was sent and not recorded before a crash among them, it asks the platform for the post by its tag. A post that this
leader queued and has not tried is sent with no lookup. On Slack the tag is in the message's metadata. On
Microsoft Teams it is in a part of the post that people do not see: an empty `<span>` whose `title` is the tag's carrier, in a paragraph
of its own at the end of a text, or the `id` of the first element of a card's body when the card is the whole post (a question's or an
approval's card, and the card that ends one). A reply with buttons, such as the useful and not-useful buttons, carries it in its text,
and the card of its buttons, a post of its own, carries none. An edit of a post
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

A reply with buttons is posted as its text and then a card of its buttons, two messages that each get their own ID (seen on a test
tenant in November 2026, for a reply that opened a thread). Teams answered one message of text and a card with no ID in November
2026, on a test tenant, and showed it as two messages, the text with the span and a card of the buttons with no tag. If the broker stops between the two posts, the outbox's lookup finds the text by its tag and
records it, and the buttons are posted only if the reply is edited with them after.

A post that the pages read do not hold, a channel whose team the broker does not know yet, a read that runs out of time after Graph
gave a page, a page that Graph names that is not Graph's or that the read already took, an answer longer than 8 MiB, and a refusal of
Graph that no retry mends (a 400, 401, 403 or 404, or Microsoft Entra's refusal of the token for Graph) are taken as not found: the
broker logs why at WARN and sends the post, which may then show twice. A 429, a 5xx, an answer that is not JSON and a lookup that runs
out of time before any page came back are tried again later.

## Chat IDs

| Field | Slack | Microsoft Teams | `webhook` |
|---|---|---|---|
| `chat.channel` | A channel ID: `C`, or `G` for an older private channel, then capitals and digits, at most 32 bytes, such as `C0123456789`. Not the channel's name. | A channel ID: `19:`, then letters, digits, `.`, `_` and `-`, then `@thread.tacv2` or `@thread.skype`, at most 140 bytes, such as `19:a1b2c3d4e5f6@thread.tacv2`. Not the channel's name. | The channel as the connection reads it from an event: letters, digits, `.`, `_` and `-`, starting with a letter or a digit, with `/` between parts, at most 140 bytes, such as `acme/widgets` or `OPS`. |
| A `botMessage` trigger's `from.id` | A bot ID: `B`, then capitals and digits, at most 32 bytes, such as `B0123456789`. Not the bot's name. | The Microsoft Entra application ID of the app that posts, a GUID in lowercase hex, such as `11111111-2222-3333-4444-555555555555`. | None: the platform takes no `botMessage` trigger. |
| A `chat.mentions` entry's `user` | A user ID: `U` or `W`, then 2 to 20 capitals and digits, such as `U0123456789`. | The person's Microsoft Entra object ID, a GUID in lowercase hex. | None: `chat.mentions` is refused, `UnsupportedOnPlatform`. |
| An entry of `interactions.approverGroups` | A user group ID: `S`, then capitals and digits, at most 32 bytes, such as `S0123456789`. | None: `approverGroups` is refused, `UnsupportedOnPlatform` (see [Features](#features)). | None: `interactions.approverGroups` is refused, `UnsupportedOnPlatform`. |

A GUID is taken in lowercase hex alone. The broker compares chat IDs byte for byte, so a GUID in capitals would load and then match
no message; the API server and the load refuse it, and the message says to write it in lowercase.

What the API server and the load take is wider: for each field, the shape of either chat platform's, Slack's or Microsoft Teams',
and for `chat.channel` the rule that takes a webhook channel instead on a Binding whose triggers are all `webhookEvent`. A value that
has none of them, such as the channel name `general`, is refused when it is applied, and so is a value past the field's cap: 140
bytes for a channel, 36 for a sender and a mention's user, 32 for an approver group. A Microsoft Teams channel on a Slack connection,
a Slack channel on a teams connection, and a Slack channel or bot ID longer than Slack's 32 bytes (up to 140 for a channel and 36 for
a sender) are refused by the broker with `InvalidPlatformID`, as above, and not when they are applied. A Slack user ID cannot pass
Slack's cap, since its pattern bounds it at 21, and neither can an approver group, whose field's cap is Slack's 32. The length caps of
each field are in [Limits](limits.md#write-a-binding-an-agentbackend-or-a-chatconnection).

## Triggers and output

| Field | Slack | Microsoft Teams | `webhook` |
|---|---|---|---|
| `chat.triggers[].type` | `mention`, `botMessage` and `personMessage`. A `webhookEvent` trigger is refused, `UnsupportedOnPlatform`. | As on Slack. | `webhookEvent` alone. A Binding with a trigger of any other type, or one that lists no triggers and so has `[{type: mention}]`, is refused. With a webhook channel such as `acme/widgets` or `OPS`, the API server refuses it when it is applied and the load refuses it as invalid (with custom resources, `InvalidSpec`), since only a Binding whose triggers are all `webhookEvent` takes that channel. With a channel of Slack's shape, which a webhook channel's shape also takes, it passes both, and the broker refuses it on the webhook connection, `UnsupportedOnPlatform`. With a Microsoft Teams channel it passes both too, and the broker refuses its channel first, which is not a webhook channel: `InvalidPlatformID`. |
| A `webhookEvent` trigger's `match` and `exclude` keys, and the `.Fields` its `promptTemplate` reads | None. | None. | Names of the connection's `spec.webhook.fields`; the template reads `.Channel` and the connection's `id` fields alone. A name the connection lacks, or a `text` field the template reads, is refused, `UnsupportedOnPlatform`; a template that reads `.Key`, `.State`, `.Link` or `.Source` is refused by the broker's validation (with custom resources, `InvalidSpec`), and not by the API server. |
| `chat.output` | `live` (the default) or `final`. `none` is refused: by the API server and the load on a Binding of Slack's triggers, and `UnsupportedOnPlatform` on one of `webhookEvent` triggers. | As on Slack. | `none` alone: write it, since the default is `live`. `live` and `final` are refused, `UnsupportedOnPlatform`. |

## Features

| Feature | Slack | Microsoft Teams | `webhook` | On a platform without it |
|---|---|---|---|---|
| Questions to a person: `interactions.askUser` | Works: Agent Kourier asks in the thread with buttons. | Works: the question is a card that a member of the channel's team presses ([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)). | Not served. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| Tool approvals: `interactions.toolApprovals`, and `interactions.approverGroups`, which requires it | Works: Agent Kourier asks in the thread with buttons. | `toolApprovals` works: the approval is a card that a member of the channel's team presses ([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)). `approverGroups` is refused: `UnsupportedOnPlatform`. Remove it. | Not served. | Refused: `UnsupportedOnPlatform`. Set `toolApprovals` to `false` and remove `approverGroups`. |
| The useful and not-useful buttons: `feedback.enabled` | Works. | Works: the buttons are a card of their own, posted after the reply, and a vote of a member of the channel's team is counted ([Buttons on Microsoft Teams](#buttons-on-microsoft-teams)). | Not served. | Refused: `UnsupportedOnPlatform`. Set it to `false`. |
| Alert triggers: a `botMessage` trigger in `chat.triggers` | Works. | Works, in either `spec.teams.mode`: Microsoft Teams does not send a bot the posts of another app (measured on a test tenant in November 2026), so the broker reads them through Microsoft Graph, by its notifications of the bound channels on a connection with a `spec.teams.notificationURL`, and by polling them on one without ([Alerts on Microsoft Teams](#alerts-on-microsoft-teams)). | Not served: the platform takes `webhookEvent` triggers alone ([Triggers and output](#triggers-and-output)). | Refused: `UnsupportedOnPlatform`. Use a trigger type that the platform takes. |
| The earlier replies a mentioned turn carries: `chat.threadReplies: mention`, the thread of a `personMessage` trigger, and a bot's mention | Works. | Works for a person's mention, read through Microsoft Graph ([Earlier replies on Microsoft Teams](#earlier-replies-on-microsoft-teams)). The first turn of a session that a person's mention starts in a thread also carries the thread's first message, such as an alert's card, whatever `chat.threadReplies` says. A bot's mention reaches it through Microsoft Graph, by its notifications or by polling ([Alerts on Microsoft Teams](#alerts-on-microsoft-teams)), since Teams sends a bot no other app's posts. | The broker does not run the platform yet. | Served: the turn carries the mentioned message alone. |
| The acknowledgment reaction: `chat.acknowledgment` | Works. With no `emoji` the reaction is `hourglass_flowing_sand`. | Taken, and no reaction is shown, as on a platform without it. | The broker does not run the platform yet. | Served, and no reaction is shown. A Binding whose acknowledgment is enabled and names an `emoji` has the condition `AcknowledgmentShown`, `False` with the reason `NoReactions` ([Binding status](binding-status.md#acknowledgmentshown)). |

# Slack user-token and Slackbot reminder probe (2026-11-08)

What Slack sends when an app posts with a person's user token, and when Slackbot posts a reminder or a custom response.
The adapter takes a plain message with a `user`, text and no `bot_id` (not Agent Kourier's own) as a person's, which a
`personMessage` trigger may then take, and drops Slackbot (`USLACKBOT`) on both the `message` and the `app_mention`
event. Neither assumption about these senders had been measured.

## Setup

- The sandbox workspace, in a public channel that the mentioned app (Agent Kourier, a Socket Mode app) is a member of.
- Three Socket Mode listeners on the mentioned app, connected at once, recorded every envelope raw and acknowledged it.
  Each `hello` frame counts the connections at the moment it is sent (1, 1 and 2 here, as the three came up). No Agent
  Kourier broker was connected to the app (no kind tier was up), and every case's events arrived once, at one of the
  three.
- **User token:** a new app made for this probe from a manifest with one user scope, `chat:write`, no bot scope, no
  bot user, installed by a person of the workspace. `auth.test` on its `xoxp-` token returns the person's user ID and
  no `bot_id`. The app posted with `chat.postMessage` (default `as_user` for a user token), `chat.scheduleMessage` and
  `chat.meMessage`.
- **Reminders:** the same person typed `/remind #channel "… @Agent Kourier" in 1 minute` (the mention picked from the
  composer's autocomplete) and a second `/remind` with no mention, in the web client.
- **Custom response:** a workspace admin added a Slackbot automated message (Customize workspace, Automated Messages)
  whose reply held Agent Kourier's raw token `<@U_KOURIER>`, triggered it once by typing in the web client and once
  through the user-token app, then removed it.
- Each recorded payload was then passed through the adapter's own `decode` (and `fromAnotherWorkspace`).
  `TestDecodeTheShapesSlackSent` in `internal/chat/slack` holds `decode` to them.

IDs are redacted: `U_PERSON` (the person), `T_SANDBOX` (the workspace), `C_CHAN`, `A_PROBE` and `B_PROBE` (the
user-token app and the bot ID Slack gave it), `U_KOURIER` (the mentioned app's bot user). `USLACKBOT` and `B01` are
Slack's own and are as received. `bot_profile.icons` is cut.

## Results

| # | Sender, route | Events | `user` | `bot_id` | `app_id` | `bot_profile` | `subtype` | `decode` |
|---|---|---|---|---|---|---|---|---|
| U1 | user token, `chat.postMessage`, top level | `message` | `U_PERSON` | `B_PROBE` | `A_PROBE` | yes | none | a bot's post |
| U2 | the same, with `<@U_KOURIER>` in `text` | `message`, `app_mention` | `U_PERSON` | `B_PROBE` on both | on both | on both | none | a bot's post; a bot's mention |
| U3 | the same, a reply in U1's thread | `message` | `U_PERSON` | `B_PROBE` | `A_PROBE` | yes | none (`thread_ts`, `parent_user_id`) | a bot's post |
| U4 | the same, a section block | `message` | `U_PERSON` | `B_PROBE` | `A_PROBE` | yes | none | a bot's post |
| U5 | the same with `as_user: false` and `username` | refused, `invalid_arguments` | | | | | | |
| U6 | user token, `chat.scheduleMessage`, with `<@U_KOURIER>` | `message`, `app_mention` at the scheduled time | `U_PERSON` | `B_PROBE` on both | on both | on both | none | a bot's post; a bot's mention |
| U7 | user token, `chat.meMessage` | `message` | `U_PERSON` | `B_PROBE` | `A_PROBE` | **no** | `me_message` | dropped (subtype) |
| R0 | the `/remind` command itself, both reminders | `message` | `U_PERSON` | none | none | no | `reminder_add` | dropped (subtype) |
| R1 | Slackbot's reminder with the mention | `message`, `app_mention` | `USLACKBOT` | **`B01`** on both | none | no | none | a bot's post; a bot's mention |
| R2 | Slackbot's reminder with no mention | `message` | `USLACKBOT` | **`B01`** | none | no | none | a bot's post |
| S1 | Slackbot's custom response, after a person's post and after a user-token post | `message` | `USLACKBOT` | **none** | none | no | `slackbot_response` | dropped (subtype) |
| P1 | the person typing in the web client (S1's trigger), for contrast | `message` | `U_PERSON` | none | none | no | none (`client_msg_id`) | a person's message |

No event of any case carried `user_team` or `source_team`. The user-token posts and the reminders carried `team`
(`T_SANDBOX`); the `reminder_add` events did not.

### The user-token post (U2)

```json
{
  "type": "message",
  "channel": "C_CHAN", "channel_type": "channel",
  "user": "U_PERSON",
  "bot_id": "B_PROBE",
  "app_id": "A_PROBE",
  "bot_profile": {"id": "B_PROBE", "app_id": "A_PROBE", "name": "Kourier User-Token Probe", "team_id": "T_SANDBOX", "deleted": false, "icons": "…", "updated": 1794161711},
  "team": "T_SANDBOX",
  "text": "probe U2: user-token top-level post mentioning <@U_KOURIER>",
  "blocks": [{"type": "rich_text", "elements": [{"type": "rich_text_section", "elements": [{"type": "text", "text": "probe U2: user-token top-level post mentioning "}, {"type": "user", "user_id": "U_KOURIER"}]}]}],
  "ts": "1794161752.908249", "event_ts": "1794161752.908249"
}
```

Its `app_mention` holds the same fields, with `type: app_mention` and no `channel_type`. The `chat.postMessage`
response's `message` already holds `app_id`, `bot_id` and `bot_profile`, so Slack stores them with the message.

The web client shows these posts under the person's name and avatar with no APP label, unlike a bot token's post: a
reader sees the person, while the event says app.

### Slackbot's reminder (R1)

```json
{
  "type": "message",
  "channel": "C_CHAN", "channel_type": "channel",
  "user": "USLACKBOT",
  "bot_id": "B01",
  "team": "T_SANDBOX",
  "text": "Reminder: probe R1 check backups <@U_KOURIER|agent_kourier> ",
  "blocks": [{"type": "rich_text", "elements": [{"type": "rich_text_section", "elements": [{"type": "text", "text": "Reminder: probe R1 check backups "}, {"type": "user", "user_id": "U_KOURIER"}]}]}],
  "metadata": {"event_type": "slack_system.reminders.posted_in_channel", "event_payload": {"reminder_id": "Rm…"}},
  "ts": "1794161936.615329", "event_ts": "1794161936.615329"
}
```

Its `app_mention` holds the same fields, with `type: app_mention` and no `channel_type`. There is no `app_id`, no
`bot_profile` and no subtype.

### Slackbot's custom response (S1)

```json
{
  "type": "message", "subtype": "slackbot_response",
  "channel": "C_CHAN", "channel_type": "channel",
  "user": "USLACKBOT",
  "text": "kourierprobe pong &lt;@U_KOURIER&gt;",
  "ts": "1794164740.152319", "event_ts": "1794164740.152319"
}
```

No `bot_id`, `team` or blocks. Slack escaped the token typed into the response, so it is text, the client shows it
literally, and no `app_mention` came. The client shows the response under the workspace's name and icon. Slackbot
answered the user-token app's post as it answered the person's.

### The `/remind` command (R0)

```json
{
  "type": "message", "subtype": "reminder_add",
  "channel": "C_CHAN", "channel_type": "channel",
  "user": "U_PERSON",
  "text": " set up a reminder “probe R1 check backups @Agent Kourier ” in this channel at 1:18PM today, Eastern Standard Time.",
  "ts": "1794161846.547439", "event_ts": "1794161846.547439"
}
```

The mention is plain text here, with no token, and Slack sent no `app_mention` for it.

## What this means for the code

- **A granular app posting with a user token is a bot to the adapter, not a person.** Slack gives the app a bot ID
  even with no bot scope, and sends it, with `app_id` and `bot_profile`, on the `message` and the `app_mention`, for an
  immediate and a scheduled post, at the top level and in a thread. `decode` takes the bot branch before it looks at
  the user: the post is a bot's message that only a `botMessage` trigger naming `B_PROBE` takes, and its mention is a
  bot's mention, held to the fixed bot-mention limits and dropped in a channel shared with another workspace. A
  `personMessage` trigger never sees it, and Agent Kourier's own posts are told apart by bot ID as before.
- **Slackbot's reminders are a bot's posts too, under the bot ID `B01`.** The `USLACKBOT` check in `self.person` is
  never reached for them: `decode` sends a message or `app_mention` with a `bot_id` down the bot branch first, both
  before and after the `personMessage` change. So that change did not disable scheduled reminder mentions:
  - A reminder that mentions Agent Kourier starts a bot's-mention turn on a Binding with a mention trigger (an empty
    `chat.triggers` is one), unless `chat.botMentions: none` or a `botMessage` trigger names `B01`, held to the fixed
    limits of a bot's mention. It did the same before the change.
  - A reminder with no mention is a bot's message from `B01`, which a `botMessage` trigger with `from: B01` takes
    (the pattern `^B[A-Z0-9]+$` accepts it).
- **What is wrong today:** the triggers reference says a Slackbot reminder is ignored and that no trigger can name
  Slackbot; the spec's `personMessage` decision, the comment on `slackbotUser` and `TestDecodeSlackbotIsNoPerson` say
  Slackbot posts with no bot ID. Reminders carry one. A custom response has none, but its subtype
  (`slackbot_response`) drops it before the `USLACKBOT` check matters. That check is a defence for a plain Slackbot post
  with no bot ID, which this probe did not see.
- `chat.meMessage` with a user token arrives as subtype `me_message` with a bot ID and no `bot_profile`, and is dropped
  like any subtype that is not kept.

## Not probed

- A channel shared with another workspace, so whether `user_team` or `source_team` names another workspace's person is
  still unmeasured. Neither field appeared on any event here. The sandbox invited a second workspace to a shared
  channel, but that workspace is on the free plan, and Slack asks a free workspace to start a paid trial before it can
  accept, so the probe stopped there.
- A classic app, a legacy test token, or a tool that posts with a Slack client session token (`xoxc-`, as browser
  automation does). A client session's post would look like the person typing it, and nothing in the event could tell
  them apart.
- Slackbot's other posts: a reminder in a thread or in a private channel, and a reminder to oneself (in the Slackbot
  direct message).
- `files.upload` with a user token (the probe app had no `files:write`). A `file_share` with a `bot_id` is dropped by
  `decode` either way.
- Whether `B01` is Slackbot's bot ID in every workspace or only in this one. The second workspace was at its free
  plan's app limit, so no app could read its events without removing one, and the shared channel that would have carried
  its Slackbot's reminder was not accepted.
- Enterprise Grid, and mobile clients' rendering of a user-token post.

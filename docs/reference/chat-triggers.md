# Chat triggers

This page lists what a message in a bound channel does, the fields of a `botMessage` trigger and of a `personMessage`
trigger, and the decisions, notes and limits of a trigger's threads. The fields' types are in the
[Binding reference](binding.md#trigger).

## What a message does

These are the defaults, which a Binding with no `chat.triggers` gets.

| Message | What happens |
|---|---|
| A mention in a bound channel, not in a thread | A new thread and a new session. |
| A reply in a thread Agent Kourier owns | Continues that session. With `chat.threadReplies: mention`, only a reply that mentions Agent Kourier does; the rest are ignored, except a typed answer to a pending question that takes text replies. A thread that [a bot's mention](#a-bots-mention) or [a person's message](#a-persons-message) began takes only mentioned replies, whatever the setting. |
| A mention inside a thread Agent Kourier does not own, in a bound channel | A new session anchored to that thread, which Agent Kourier owns from then on. |
| A reply without a mention in any other thread | Ignored. |
| Any message Agent Kourier posted, including the edit events of its own posts | Ignored, always. |
| A message from any other bot or app | Ignored, unless a `botMessage` trigger names its bot ID. |
| A person's message that is not in a thread and does not mention Agent Kourier | Ignored, unless a `personMessage` trigger takes it. See [A person's message](#a-persons-message). |
| A message Slackbot posts, such as a reminder | Ignored: Slackbot is no person, and no trigger can name it. |
| A mention of Agent Kourier by any other bot or app | A turn in the thread that holds it, within fixed limits, unless the Binding says otherwise. See [A bot's mention](#a-bots-mention). |
| An edit or deletion of any message, or another non-message subtype | Ignored. Plain messages, `thread_broadcast` and `file_share` (its text only) count as messages. |
| A direct message | Not served yet. |

## The trigger list

- An empty `chat.triggers` is `[{type: mention}]`, the table above.
- A list that names triggers is the whole list. A Binding that lists only `botMessage` triggers starts no session from a
  mention, in or out of a thread.
- Triggers are tried in order, and the first that matches handles the message. A trigger's index in the list names it
  in logs and metrics.
- v1 has three types: `mention`, `botMessage` and `personMessage`. A `mention` trigger takes no other field.
- Whether a reply in an owned thread needs a mention is `chat.threadReplies`, not a trigger.

## A bot's mention

Any bot or incoming webhook that can post in a bound channel can start a turn by mentioning Agent Kourier, because Slack cannot tell an
agent from any other bot. This is on by default for a Binding with a `mention` trigger, including one with no `chat.triggers`.
Set `chat.botMentions: none` to turn it off. A Binding whose list leaves out the `mention` trigger takes no bot's mention either.

| | |
|---|---|
| Where the turn goes | The thread that holds the mention, or a new thread for a mention outside one. A new session if Agent Kourier does not own the thread. |
| Limits | Fixed, and across every bot on the Binding: 3 turns in any 30 minutes, 1 at a time, 20 in any 24 hours. They count mentions, including those in a thread Agent Kourier already owns. "1 at a time" lasts while the mention's own turn is queued or running, including while it waits for a person's answer: a later turn in its thread does not hold it. A mention whose session takes no turn does not count. A mention in an alert's thread does not count as that alert running under the trigger's `maxConcurrent`. |
| The prompt | Fixed, with the post and the thread so far as untrusted data. The post is its text, its first attachment's fields and, if that attachment has no text, what its section, context, header and rich text blocks say past its text. The thread is what opened it and every author's messages since, except Agent Kourier's own and what were already turns, at most 50: what opened it stays, with the newest. A bot's message in the thread shows the same parts as the post, and its broadcasts and file shares (their text and blocks) count. A later mention gets what came since the previous bot's mention, so a reply sent to the agent as context on a person's turn can come again. |
| Replies in the thread | A session that a bot's mention began takes a person's reply as a turn only if it mentions Agent Kourier, whatever `chat.threadReplies` says, and the final message says so. A thread Agent Kourier already owned keeps its Binding's rule. |
| Rating buttons | As on an answer to a person: only with `feedback.enabled`, and counted as an answer. |
| A limit is reached | No turn and nothing in the thread. Counted, audited, and in the digest line of its window. |

These are never taken as a mention:

- Agent Kourier's own posts.
- An edit that adds a mention.
- A bot that a `botMessage` trigger of the Binding names. That trigger reads its posts.
- Any bot's mention in a channel shared with another workspace.

A session past its `threadTTL`, or whose Binding has left the config, takes no turn from a bot, with no note, and the mention uses none of the limits. A bot's turn never answers a
pending question; it stays a turn of its own, queued until the pause ends, and the question still times out at its deadline. [Let another agent start yours](../how-to/any-bot-trigger.md#let-another-agent-start-yours) has what
this does not bound.

## A person's message

A `personMessage` trigger starts a turn on each message a person posts at the top level of a bound channel, with no
mention. Use it where people report things in the channel and an agent should read every report, such as an incident
channel. It has no fields of its own: it takes `match`, `exclude`, `promptTemplate` and `limits` as a `botMessage`
trigger does, and is bounded the same way. Add it beside a `mention` trigger, so that people can still ask the agent
directly:

```yaml
spec:
  chat:
    channel: C0123456789
    triggers:
      - type: mention
      - type: personMessage
        exclude:
          text: '^(?i:thanks|thank you|ok)\b'
        limits:
          rateLimit: {maxRuns: 6, per: 30m, maxConcurrent: 2}
          maxRunsPerDay: 50
        promptTemplate: |
          A person reported something in {{ .Channel }}: {{ .Link }}.
          Investigate it with your read-only tools and reply in this thread.
```

| | |
|---|---|
| Fields | `match` and `exclude` read the message's `text` and no other field, because a person's message has no structured part. They read the raw text: they filter noise and control no access, and a zero-width character inside a word gets past an `exclude`. `promptTemplate` and `limits` are required as for a `botMessage` trigger (the limits are described [below](#limits-and-the-digest-line)). `from`, `extract` and `thread` are refused: there is no sender to name, nothing to extract a key from, and each message starts a thread of its own. |
| Where the turn goes | The thread of the message, which the agent's answer goes into. |
| Whose turn it is | The person's who wrote the message: the agent sees their name, ID and email in front of the prompt, as for a mention, the audit log names them, the answer is streamed to them, and the final message has rating buttons only with `feedback.enabled`, counted as an answer. |
| Replies in the thread | A reply is a turn only if it mentions Agent Kourier, whatever `chat.threadReplies` says, and the final message says so: people discuss a report under it. A mentioned reply carries the replies before it, which did not mention Agent Kourier, as untrusted data. No limit counts a mentioned reply, as none counts a reply in any thread Agent Kourier owns or a person's mention: the trigger's limits count the start alone. The thread's turns run one at a time. |
| What it takes | A message of a person with text that is not in a thread, or is the root of one: a plain message, or a file share with its text. |
| What it never takes | Agent Kourier's own posts, a bot's message (a `botMessage` trigger reads those), Slackbot's, a reply in a thread, even one also sent to the channel, an edit or deletion, and a direct message. |
| A mention | Where the Binding has a `mention` trigger, a top-level message that mentions Agent Kourier is that trigger's, whatever the order of the list, and uses no place in the `personMessage` trigger's limits. Where it has none, the mention is a message like any other. A mention counts in the message's text or where Slack registers one in its blocks. |
| Redelivery | A message that Slack delivers more than once is one start. A message no trigger takes is counted as unmatched each time it is delivered. A message that a limit stopped, or whose start was not made, is one decision too: a second delivery starts nothing, even once there is capacity again. Where the Binding has a `mention` trigger and Slack also sends a message that a limit stopped as a mention of Agent Kourier, that mention is the `mention` trigger's. |
| Limits | The trigger's own, and they count starts: each top-level message the trigger takes, not the mentioned replies in its thread. A start holds its place in `maxConcurrent` while its own turn is queued or running, including while it waits for a person's answer; a later turn in its thread does not hold it. |
| A limit is reached | No turn of the trigger, and nothing from it in the thread; if the message mentions Agent Kourier and the Binding has a `mention` trigger, the mention is answered (see Redelivery). Counted, audited under the person who posted the message (the message's ID, the trigger and the limit, never its text), and in the digest line of its window. |
| The prompt | The template's output, then the whole message in the untrusted block, as for a `botMessage` trigger. The template has no extract to read, so `.Fields` is empty and `.State` and `.Key` are empty; `.Source` is `personMessage`. |
| Who can start it | Anyone who can post in the channel, held to the trigger's limits, which bound starts and not the turns that follow in their threads. A message from a person of another workspace in a channel shared with it counts like any other, as a mention from that person does. A message that an app posts with a person's token is that person's when Slack sends it with no bot ID. |
| Slack | The app needs the `channels:history` scope, and `groups:history` for a private channel, and the events `message.channels` and `message.groups`. The [app manifest](../how-to/configure-slack.md) has them already, because replies in a thread need them too. |

`agent-kourier dry-run` replays a `personMessage` trigger as the intake runs it: a top-level message it takes is `matched`, one it
turns away `unmatched` or `excluded` with the reason, and a mention that the `mention` trigger takes, or a reply in a thread, `person`.

## `botMessage` fields

A `botMessage` trigger works for any bot, named by its sender ID, and is written in full: `match`, `extract`, `thread`,
`limits` and `promptTemplate` are all in the Binding. A worked trigger for Alertmanager's Slack receiver, with a comment
on each regex, is in [Investigate Alertmanager alerts](../how-to/investigate-alertmanager-alerts.md).

| Field | Meaning |
|---|---|
| `from.id` | The one sender whose messages the trigger accepts, by the platform's ID for it (Slack: the bot ID, `B...`), never by display name. Agent Kourier's own messages are never handed on, even if a trigger names its ID; that mistake is logged once, at warn. |
| `from.kind` | What kind of sender `from.id` names. `bot` is the only value, and the default. A message matches only when its sender's ID and kind both equal `from.id` and `from.kind`; on Slack every bot sender is of kind `bot`. |
| `match` | Message field to regex. Every entry must match. |
| `exclude` | Message field to regex. No entry may match. |
| `extract` | Named values taken from a field by a regex whose named group has the entry's name. A message in which an extract does not match does not match the trigger. |
| `thread.by` | `key`: messages with the same extracted `key` share one thread, anchored to the first. |
| `thread.cooldown` | 1m to 168h. No new turn for a key inside the window, which runs from the message that started the turn. Required with `thread.by`. |
| `thread.closeWhen` | Extracted field to value, for example `{state: RESOLVED}`. A matching message posts a note and starts no turn. |
| `thread.maxIdle` | At least twice the cooldown, at most 720h; unset, the larger of 24h and twice the cooldown. An open thread with no message of its key for this long is abandoned, and the next fire starts a new investigation. It must be longer than the sender's repeat interval. |
| `promptTemplate` | A Go template over the trigger's view of the message (below). Parsed and run once at load against a placeholder view, so a mistyped field rejects the Binding. It may use text, `if`, `with`, the fields below and the functions `and`, `eq`, `ge`, `gt`, `index`, `le`, `len`, `lt`, `ne`, `not`, `or` and `slice`; a loop, a variable, a defined template or any other function rejects the Binding. At most 8192 bytes. |
| `limits.rateLimit` | Required. `maxRuns` starts in any `per` (10s to 24h), and `maxConcurrent` investigations at once; all three are set. |
| `limits.maxRunsPerDay` | Required. Starts in any 24 hours. An explicit `0` is no cap. |

A `botMessage` trigger without `limits.rateLimit` or without `limits.maxRunsPerDay` does not load: the Binding is
rejected with a message that names the missing field. Leaving `maxRunsPerDay` out is not the same as `0`.

**Message fields** a regex can read: `text`, `title`, `link`, `fallback`, `footer` and `body`. `text` is the message's
top-level text, outside any attachment or card. `title`, `link`, `body`, `footer` and `fallback` are the heading, the
heading's URL, the main text, the small print and the plain-text summary of the message's structured part. Each platform
adapter flattens its native message into them. The Slack adapter reads the message's first legacy attachment: its
`title` is `title`, its `title_link` is `link`, its `text` is `body`, its `footer` is `footer` and its `fallback` is
`fallback`. All are raw, as delivered. Later attachments are not read. A field the message lacks is empty.

**Regexes** are RE2 (linear time), at most 512 bytes, and compile at load, as do the named groups and the template. A
Binding that fails any of them is rejected. A regex is also held to the size of the program it compiles to: at most
2000 instructions each, and 5000 for all the regexes of a Binding (see [Limits](limits.md#write-a-binding-an-agentbackend-or-a-chatconnection)),
because the time to try a regex on a message grows with that size and the message's length. A regex reads the first
16 KiB of its field and nothing past it. The check ends at the first regex it refuses.

## What a matched message does to its thread

For a trigger with `thread.by: key`. A *close* is a message whose extracted fields all equal `closeWhen`; any other
match is a *fire*.

| Message | The key has | Decision | What happens |
|---|---|---|---|
| fire | no thread, a closed one past its cooldown, one whose turn never started and is over a minute old, or an open one idle past `maxIdle` | `started` | A new investigation in a thread anchored to this message. The cooldown begins once its turn is taken. |
| fire | an open thread that is not idle past `maxIdle`, or a closed one inside its cooldown | `repeat` | "Fired again at `<time>`." in that thread, which reopens. No turn. |
| fire | this very message as its thread (a redelivery) | `dedup` | Nothing, if its turn started. |
| fire, key empty | | `empty_key` | An investigation of its own, with no cooldown and no note. |
| close | an open thread whose turn started | `closed` | "Resolved at `<time>`." in that thread, which closes. No turn. |
| close | no thread, a closed one, or one whose turn never started | `close_unknown` | Ignored and counted. |
| fire | would be `started`, and a limit is reached | `limited` | No turn and nothing in the alert's thread. Counted, and in the window's digest line. |
| fire | the first prompt failed its check or could not be rendered | `not_started` | "Not investigated: ..." in the thread, an audit entry, and a metric. No cooldown. |

A trigger with no `thread` has none of this: every match starts a turn. The notes are fixed text and a time, never any
of the message.

## Limits and the digest line

Only a start is limited: a repeat, a close and a note never are. Rate and daily counts are sliding windows that survive
a restart. Concurrency counts the trigger's investigations, started in the last 24 hours, that have an active task (running, or paused for a person)
or a queued turn. For a `personMessage` trigger that is the start's own turn alone. When several limits are reached, the daily cap is reported first, then the rate, then the
concurrency.

Once for each window and trigger, one line at the top level of the channel counts the starts a limit stopped, for
example "7 messages not investigated in the last 10m (rate limit)." A window opens with the first stopped start
and lasts the trigger's `rateLimit.per`. The line holds a count, a length
and a limit, never any text of an alert.

## The first turn's prompt

The template sees:

| Field | Value |
|---|---|
| `.Channel` | The channel the message arrived in. |
| `.State` | The extracted `state`, empty if none. |
| `.Key` | The extracted `key`. |
| `.Fields` | Every extracted value, by name. |
| `.Link` | The platform's permalink to the triggering message (Slack: the message permalink), built by Agent Kourier, never a field of the message. Empty when the platform makes none or cannot make one. |
| `.Source` | The type of the trigger: `botMessage`, or `personMessage`. |

`.Fields`, `.State` and `.Key` come out of the sender's message, so each value the template reads is checked before it
reaches the instruction part: the alert-name shape (ASCII letters, digits and `_.-:`, at most 128 bytes) for fields,
state, channel and source; the same plus `/` for the key; and a plain `http` or `https` URL of at most 256 bytes for
the link. A value that fails refuses the turn (`not_started`). A key that holds spaces, such as the
Alertmanager example's, fails this check, so a template should read `.Fields` entries, not `.Key`.

After the rendered template, the whole message follows, normalized and with its secrets redacted, inside a block
labelled as untrusted data.

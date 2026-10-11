# Chat triggers

This page lists what a message in a bound channel does, the fields of a `botMessage` trigger, the `alertmanager`
preset, and the decisions, notes and limits of a trigger's threads. The fields' types are in the
[Binding reference](binding.md#trigger).

## What a message does

These are the defaults, which a Binding with no `chat.triggers` gets.

| Message | What happens |
|---|---|
| A mention in a bound channel, not in a thread | A new thread and a new session. |
| A reply in a thread Agent Kourier owns | Continues that session. With `chat.threadReplies: mention`, only a reply that mentions Agent Kourier does; the rest are ignored, except a typed answer to a pending question that takes text replies. |
| A mention inside a thread Agent Kourier does not own, in a bound channel | A new session anchored to that thread, which Agent Kourier owns from then on. |
| A reply without a mention in any other thread | Ignored. |
| Any message Agent Kourier posted, including the edit events of its own posts | Ignored, always. |
| A message from any other bot or app | Ignored, unless a `botMessage` trigger names its bot ID. |
| An edit or deletion of any message, or another non-message subtype | Ignored. Plain messages, `thread_broadcast` and `file_share` (its text only) count as messages. |
| A direct message | Not served yet. |

## The trigger list

- An empty `chat.triggers` is `[{type: mention}]`, the table above.
- A list that names triggers is the whole list. A Binding that lists only `botMessage` triggers starts no session from a
  mention, in or out of a thread.
- Triggers are tried in order, and the first that matches handles the message. A trigger's index in the list names it
  in logs and metrics.
- v1 has two types: `mention` and `botMessage`. A `mention` trigger takes no other field.
- Whether a reply in an owned thread needs a mention is `chat.threadReplies`, not a trigger.

## `botMessage` fields

| Field | Meaning |
|---|---|
| `from.botId` | The one bot whose messages the trigger accepts, by Slack bot ID (`B...`), never by display name. Agent Kourier's own messages are never handed on, even if a trigger names its bot ID; that mistake is logged once, at warn. |
| `match` | Message field to regex. Every entry must match. |
| `exclude` | Message field to regex. No entry may match. |
| `extract` | Named values taken from a field by a regex whose named group has the entry's name. A message in which an extract does not match does not match the trigger. |
| `thread.by` | `key`: messages with the same extracted `key` share one thread, anchored to the first. |
| `thread.cooldown` | 1m to 168h. No new turn for a key inside the window, which runs from the message that started the turn. Required with `thread.by`. |
| `thread.closeWhen` | Extracted field to value, for example `{state: RESOLVED}`. A matching message posts a note and starts no turn. |
| `thread.maxIdle` | At least twice the cooldown, at most 720h; unset, the larger of 24h and twice the cooldown. An open thread with no message of its key for this long is abandoned, and the next fire starts a new investigation. It must be longer than the sender's repeat interval. |
| `promptTemplate` | A Go template over the trigger's view of the message (below). Parsed and run once at load against a placeholder view, so a mistyped field rejects the Binding. |
| `limits.rateLimit` | `maxRuns` starts in any `per` (10s to 24h), and `maxConcurrent` investigations at once. |
| `limits.maxRunsPerDay` | Starts in any 24 hours. An explicit `0` is no cap. |
| `preset` | `alertmanager` fills in `match`, `extract`, `thread`, `limits` and `promptTemplate` (below). |

A trigger with neither a preset nor `limits` is unlimited.

**Message fields** a regex can read: `text`, `title`, `titleLink`, `fallback`, `footer` and `attachmentText`. The Slack
adapter flattens the message's first legacy attachment into the last five; `text` is the message's own text. All are
raw, as delivered. Later attachments are not read.

**Regexes** are RE2 (linear time), at most 512 bytes, and compile at load, as do the named groups and the template. A
Binding that fails any of them is rejected.

## The `alertmanager` preset

For Alertmanager's Slack receiver. A Binding's own entries override the preset's: a `match`, `exclude` or `extract`
entry replaces the preset's entry of that name; `thread.by`, `thread.cooldown`, `thread.closeWhen` and
`promptTemplate` replace the preset's; each `limits` field the Binding sets replaces the preset's, and the others stay.
An empty value means unset, so a Binding cannot switch a preset entry off; to drop a `match` entry, override it with
`'.*'`.

| Part | Preset value |
|---|---|
| `match.title` | `FIRING (<n>) - ` or `RESOLVED - ` at the start, optionally behind a bracketed tag of up to 32 bytes such as `[kind] ` |
| `match.fallback` | `[FIRING]`, `[FIRING:<n>]` or `[RESOLVED]` at the start: Alertmanager's default fallback |
| `extract.state` | `FIRING` or `RESOLVED`, from the title |
| `extract.alertname` | The rest of the title after ` - ` |
| `extract.key` | The group label values from the fallback, without the count |
| `thread` | `by: key`, `cooldown: 4h`, `closeWhen: {state: RESOLVED}` |
| `limits` | 5 starts in any 10 minutes, 2 at once, 40 a day |
| `promptTemplate` | Asks the agent to investigate the alert with its read-only tools and report the likely cause, the evidence it checked, and the next steps a person should take |

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
a restart. Concurrency counts the trigger's investigations that have an active task (running, or paused for a person)
or a queued turn. When several limits are reached, the daily cap is reported first, then the rate, then the
concurrency.

Once for each window and trigger, one line at the top level of the channel counts the starts a limit stopped, for
example "7 alert messages not investigated in the last 10m (rate limit)." A window opens with the first stopped start
and lasts the trigger's `rateLimit.per` (an hour for a trigger with only a daily cap). The line holds a count, a length
and a limit, never any text of an alert.

## The first turn's prompt

The template sees:

| Field | Value |
|---|---|
| `.Channel` | The channel the message arrived in. |
| `.State` | The extracted `state`, empty if none. |
| `.Key` | The extracted `key`. |
| `.Fields` | Every extracted value, by name. |
| `.Link` | The Slack permalink of the triggering message, built by Agent Kourier, never a field of the message. |
| `.Source` | The preset's source name. |

`.Fields`, `.State` and `.Key` come out of the sender's message, so each value the template reads is checked before it
reaches the instruction part: the alert-name shape (ASCII letters, digits and `_.-:`, at most 128 bytes) for fields,
state, channel and source; the same plus `/` for the key; and a plain `http` or `https` URL of at most 256 bytes for
the link. A value that fails refuses the turn (`not_started`). The preset's key holds spaces, so a template should
read `.Fields` entries, not `.Key`.

After the rendered template, the whole message follows, normalized and with its secrets redacted, inside a block
labelled as untrusted data.

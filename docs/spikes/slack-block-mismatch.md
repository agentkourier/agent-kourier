# Slack `block_mismatch` probe (2026-11-03)

Which `chat.update` calls Slack refuses with `block_mismatch`, and why `internal/render/live` stops trying one. Slack's page for
the method documents the code as "Rich-text blocks cannot be replaced with non-rich-text blocks" (docs.slack.dev, read 2026-11-03).
It says no more than that, and the table is what it means in practice.

A probe in a sandbox workspace made a fresh message for each cell, with a bot token, then called `chat.update` with `text` and the
blocks named. A cell is Slack's answer, and the blocks are as `conversations.replies` and the update's response showed them.

| Message, and the blocks Slack stores | `markdown` or `rich_text`, with or without others | `context`, `section` or `task_card` alone | no `blocks`, or `blocks: []` |
|---|---|---|---|
| post, `markdown`: `rich_text` | ok | **block_mismatch** | ok |
| post, `rich_text`: `rich_text` | ok | **block_mismatch** | ok |
| stream of text, stopped: `rich_text` | ok | **block_mismatch** | ok |
| post, `markdown` and `context`: `rich_text`, `context` | ok | ok, and the text is gone | ok |
| stream of text and a card, stopped: `rich_text`, `task_card` | ok | ok, and the text is gone | ok |
| stream of text, stopped with `context`: `rich_text`, `context` | ok | ok, and the text is gone | ok |
| post, `section` or `context` | ok | ok | ok |
| stream of a card alone, or empty, stopped | ok | ok | ok |

A post with `text` and no blocks reads back as `rich_text` too, and a `task_card` alone still passed on it (the one cell tried).

So the rule that fits: the update is refused when every block the message holds is rich text and the update has blocks and none is
text. `markdown` counts as text because Slack stores it as `rich_text`. A message of `rich_text` and anything else has no such
protection, which means a card alone replaces its text with no error.

The refusal does not pass with time. Five updates of a `task_card` alone, 2.1 s apart, on a `(no output)` post that was all
`rich_text`, were all `block_mismatch`. An update of a `task_card` and a `markdown` block then passed, and after that a `task_card`
alone passed too, and left only the card. So the code is permanent for that call on that message, not for the message.

Not measured: what the web client shows at each step (only the API's answers were read), and whether the rule changes with the age
of a message (the longest gap tried was about 15 s, and the two threads seen failing in production had messages hours old).

## In the renderer

The real renderer, Slack adapter and SQLite store, against the sandbox, on a recovered turn whose earlier attempt left a stopped stream
of text. Attempt 1 had no event and 15 s, attempt 2 a tool call and 8 s, attempt 3 text and a final.

| Attempt | Before the fix | After it |
|---|---|---|
| 1 | 5 `block_mismatch`, 2 s apart, then `live: giving up on a message` | 1 `block_mismatch` |
| 2 | 4 `block_mismatch`, 2 s apart | 1 `block_mismatch` |
| 3 | passes | passes |

A later run, measured the same way, after the writer stopped writing `(no output)` as text:

| Sequence | Updates | Message afterwards |
|---|---|---|
| Alert thread, cut 5 s in, after its `Working…` | passes | `(no output)`, one `section` |
| Next attempt's `Working…` on it, 6 s later | passes, none refused | one `task_card` |
| That attempt's answer | passes | `task_card`, `rich_text` |
| Stopped stream of text, then a final with nothing | 1 `block_mismatch` | the same text, one `rich_text` |
| The same, then a final with a tool's card | 1 `block_mismatch` | the same text, one `rich_text` |

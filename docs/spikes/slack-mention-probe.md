# Slack mention and link probe (2026-11-01)

Why a mention is a message of its own, and why Agent Kourier adds no link code. `internal/chat/message.go`,
`internal/chat/slack/layout_test.go` and `internal/chat/slack/platform_test.go` cite this table.

A probe posted to a sandbox channel with a bot token, as the adapter sends (`parse: none`, no unfurl, on posts and updates), and
mentioned a person whose session could be read. "Registers" means the message shows in that person's Activity > Mentions in the web
client. A push or desktop notification itself was not observed, nor was a mention of someone who is not a member of the channel, nor mobile.

| # | Delivery | Blocks | `<@U…>` in top-level `text` | Registers |
|---|---|---|---|---|
| A, F | post | context: mrkdwn token and plain_text words | no | yes |
| B | post | section: mrkdwn token | no | yes |
| C | post | rich_text with a `user` element | no | no |
| C3 | post | rich_text with a `user` element | yes | yes |
| D | post | markdown with the token | no | no |
| D2 | post | markdown with the token | yes | yes |
| D3 | post | markdown with no token | yes | yes |
| E | post | section: plain_text | yes | no |
| T1 | post | none | yes | yes |
| N1 | post | **markdown, then context with the token** | no | **no** |
| G | update, at once and 45 s later | a section replaced by context with the token | no | yes |
| H2 | update, 30 s later | rich_text kept, context with the token added | no | no |
| K2 | update of a stopped stream | rich_text kept, context with the token added | no | no |
| I | stream chunk with the token, at once and 45 s in | Slack stores rich_text, and a `text` with the token | Slack's | yes |
| J | stream stop with context with the token, at once and 45 s in | Slack stores rich_text and context, and a `text` with the token | Slack's | yes |

The reading that fits, which Slack does not document: with a rich_text or markdown block in a message, mentions come from top-level
`text` alone. With only section and context blocks, they come from the blocks' mrkdwn. So `chat.Mention` notifies in a message of its
own (A) and not beside a `chat.Body` (N1).

Replacing a markdown message or a stopped stream by chat.update with blocks that drop the original (a section in place of its body)
was refused with `block_mismatch`, which is why H2 and K2 keep it.

A mention of a user ID that does not exist posted `ok` with no error, so a stale ID in a Binding is not reported by Slack.

## Through the adapter

The same probe then posted through the adapter's own client and block layout: a reply body holding `@name`, `@here`, `<!channel>` and a raw
`<@U…>`, then a message of `chat.Mention` parts. The second message registered. The reply did not, and its `@here`, `<!channel>` and `<@U…>`
showed as literal text.

## URLs

Read back through `conversations.replies` and seen in the web client.

| In a markdown block or a stream | Slack stores | Shown as |
|---|---|---|
| A workspace message URL, with or without `?thread_ts=…&cid=…`, and with `&amp;` as the adapter writes it | `message_mention` | Slack's own reference to the message |
| Another path on the workspace host | `link` | its own address |
| Any other host | `link` | its own address |

In a plain_text object a URL stays text, with no link. `@name` and `@here` in a markdown block stay text.

## What a second app receives (2026-11-03)

Why a bot's mention is read from `app_mention` and from nothing else. A second app posted to a sandbox channel with a bot token, as the
adapter sends (`parse: none`, no unfurl), and a Socket Mode listener recorded what the mentioned app received. Each case ran three times, in a
thread except where it says "top level". The incoming webhook posted to another channel the mentioned app is in.

An Agent Kourier was connected as the mentioned app throughout, and Slack gives each event to one connection, so five listeners shared events with
it and took 59 of about 76. So "yes" means seen at least once, and "none" means no `app_mention` was seen in three tries while their `message` events were.

| # | Blocks | `<@U…>` in top-level `text` | `app_mention` | `text` of both events holds the token |
|---|---|---|---|---|
| A | context: mrkdwn token and plain_text words (`render.MentionMessage`) | no | yes | no |
| A, top level | the same | no | yes | no |
| B | none | yes | yes | yes |
| B, top level | none | yes | yes | yes |
| C | context with the token | yes | yes | yes |
| D | markdown with no token | yes | yes | yes |
| E | markdown with the token | no | none | no |
| F | section: mrkdwn token | no | yes | no |
| G | rich_text with a `user` element | no | none | no |
| H | context with another user's token | no | none | no |
| J | none, and no mention | no | none | no |
| I | a post with no mention, then chat.update to a context block with the token | no | yes, with `edited` set | no |
| W1 | incoming webhook, no blocks | yes | yes, subtype `bot_message` | yes |
| W2 | incoming webhook, context with the token | no | yes, subtype `bot_message` | no |

`app_mention` fires exactly where a mention registers for a person in the first table (rows E and G match D and C, and A and F match A and B), so it is the
signal to read: it says what `mentionedIn`, which reads top-level `text`, cannot for rows A, F, I and W2. Nothing has to read blocks, and `render.MentionMessage`
need not change.

What the events hold:

- An app's post: both events carry `bot_id`, `user` (the app's bot user), `app_id`, `bot_profile`, `blocks`, and `thread_ts` in a thread. Top-level `text` is whatever the
  post's `text` was, which for `render.MentionMessage` is its fallback, so none of the context block's words are in it.
- A webhook's post: `bot_id` and subtype `bot_message` on both events, with no `user` and no `app_id`. So a filter on the app's `user` would miss a webhook.
- A post with no blocks, or a markdown block, comes back with Slack's own rich_text block.
- Both events of one post carry the same `ts`. Nothing here shows a fixed order between them, so none may be assumed.
- An edit that adds a mention arrives as `message_changed` and as `app_mention` with `edited` set, for a bot's post as well.

Not probed: a Slack Connect channel (a bot of another workspace), Enterprise Grid, and an app that mentions itself.

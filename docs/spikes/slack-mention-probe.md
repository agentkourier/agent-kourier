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
was refused with `block_mismatch`, which is why H2 and K2 keep it. `slack-block-mismatch.md` has the whole rule.

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

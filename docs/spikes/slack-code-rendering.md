# Slack code rendering probe (2026-10-04)

Why the Slack adapter writes code raw and escapes prose. The comments in `internal/chat/slack/escape.go`,
`mdtext.go`, `approval.go` and their tests cite the rows of this table by number.

A probe sent markdown the way the adapter does (a stream in chunks mode, `inertOptions`, and
`markdown` blocks for some cases) to the sandbox channel and read back the `rich_text` Slack parsed
it into. Streams and `markdown` blocks behaved the same in every case sent both ways.

| # | Sent | Slack parsed |
|---|---|---|
| 01 | fence with `\[\]`, `&amp;`, `&lt;c&gt;`, `\\n` | code, all shown literally: `\[\]`, `&amp;`, `&lt;c&gt;`, `\\n` |
| 02 | fence with raw `[]`, `\n` | code: `[]`, `\n` |
| 03 | inline code `a\[0\] &lt;b&gt;` | code `a[0] &lt;b&gt;`: backslash before `[`/`]` removed, entities literal |
| 21 | inline code `2&gt;&amp;1` | code `2&gt;&amp;1`, literal |
| 26 | inline code `a\\b` | code `a\\b`: a doubled backslash is not collapsed |
| 54 | inline code `a\[0] b\\c [d]` | code `a[0] b\\c [d]` |
| 04 | inline code containing `[t](https://example.com)` | code, no link |
| 19, 20 | raw `a & b <c> 2>&1 <@U…> <https://example.com\|masked>` in a fence, and in inline code | code, all literal: no user, no link |
| 05 | prose `[t](https://example.com)` | a link with text `t` (why prose escapes) |
| 06 | prose `\[t\](https://example.com)` | literal brackets, and the URL autolinked as itself |
| 18 | prose `&amp; &lt;c&gt; 2&gt;&amp;1` | `& <c> 2>&1`: entities decoded in prose |
| 27 | prose raw `<@U…> <https://example.com\|masked>` | a user mention and a masked link (why prose escapes) |
| 07, 25 | fence indented under a list item or bullet | not code: list text |
| 39 | fence on the list marker line, `- ```` | not code |
| 22 | list item, then a fence at column 0 | the list, code, then a new list |
| 08, 09 | fence at top level indented 1 or 3 spaces | code |
| 10 | ```` ```bash ```` | code; the info string is not shown |
| 11 | fence never closed | code to the end of the message |
| 12 | fence split across three stream chunks (even inside the closing run) | parsed as one document: code, then prose |
| 13 | `~~~` fence | code |
| 23 | 4-space indented block after a blank line | code |
| 24 | `> ```` in a blockquote | code |
| 30, 31, 38 | column-0 fence after a quote line, after an unclosed inline span, after a quote's fence | code each time (a quote's fence ends with the quote) |
| 32, 33, 36, 37 | inside a fence: `foo ``` bar`, `> ````, `- ````, `1. ```` | not a closer |
| 40, 41, 44 | inside a fence: tab-indented ```` ``` ````, 4-space-indented ```` ``` ````, ```` ``` tail ```` | not a closer |
| 14 | inline `` ` `` never closed | not code: a link forms after it |
| 15 | inline span across a line break | code |
| 16, 17 | double-backtick span; ```` ```x``` ```` mid-line | code spans |
| 34, 35, 42, 43 | `\`\`\`` and `\~\~\~` in prose, also after `> ` | literal characters; no fence, no span; links after them still form |
| 50 | `` \` `` stray, then a span with raw `[t](url) <@U…> 2>&1 <pod>` | literal backtick, then code with all of it literal |
| 51 | `\\` then a span | a backslash, then the span |
| 52 | span whose content ends with `\` | the span closes at the backtick (code `a\`) |
| 53, 55 | `` \` `` pairs | never pair into a span |

Net: Slack follows CommonMark for fences, closers and code spans in every case tried, honours
backslash escapes of `` ` ``, `~`, `[`, `]` in prose, and shows code literally (no entity
decoding, no backslash escapes in fences; it does drop a backslash before `[`/`]` in a span).


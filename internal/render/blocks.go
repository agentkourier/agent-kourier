package render

import "github.com/agentkourier/agent-kourier/internal/agent"

// These builders are how courier makes the parts of a message from agent output. Each takes the
// flavour from the part it builds, so a caller cannot pair text with the wrong escaping: a markdown
// block is in the markdown flavour, a mrkdwn context line and a message's top-level text are in the
// text flavour (which is for those only, and never for a markdown block, markdown_text or a
// plain_text object). They return plain values, not distinct string types, which would leak into slack.Content
// and the outbox payload. Anything agent-written goes into a message through them and nothing else.

// MaxFallback is the length, in UTF-16 units, of the top-level text a message carries beside its
// blocks, which Slack shows where it cannot show them, such as in a notification.
const MaxFallback = 1000

// NoOutput is what a message says when its turn ended with nothing to show.
const NoOutput = "(no output)"

// MarkdownText is the text of a markdown block, or of a stream's markdown_text, for a turn: Body in
// the markdown flavour, at most MaxBlock.
func MarkdownText(events []agent.TextEvent) string { return Body(events, MaxBlock, FlavourMarkdown) }

// MarkdownBlock is the markdown block of a turn's text. ok is false if there is nothing to show.
func MarkdownBlock(events []agent.TextEvent) (block map[string]any, ok bool) {
	text := MarkdownText(events)
	if text == "" {
		return nil, false
	}
	return map[string]any{"type": "markdown", "text": text}, true
}

// NoOutputBlock is the markdown block that says NoOutput.
func NoOutputBlock() map[string]any { return map[string]any{"type": "markdown", "text": NoOutput} }

// Message is a message for a turn that has not ended: its top-level text, which Slack shows where
// it cannot show the blocks, such as in a notification, and its blocks. The text is Body in the text
// flavour, at most MaxFallback, or, with none, the status line of the tool call under way. The blocks
// are the markdown block of the text, if any, and, while tool is not nil, a mrkdwn context block of
// the status line, built once. The text is for a message's top-level text field only: never put it in
// a markdown block, a stream's markdown_text or a plain_text object, whose flavours differ.
func Message(events []agent.TextEvent, tool *agent.ToolCallEvent) (text string, blocks []map[string]any) {
	text = Body(events, MaxFallback, FlavourText)
	if block, ok := MarkdownBlock(events); ok {
		blocks = append(blocks, block)
	}
	if tool != nil {
		line := ToolLine(tool.Name, tool.Args, FlavourText) // for mrkdwn: "[text](url)" is no link there
		blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": line}}})
		if text == "" {
			text = line
		}
	}
	return text, blocks
}

// NoticeText is the text of a notice: fixed wording that courier wrote, and, if there is one, a
// short detail, which is untrusted like everything else and is escaped and capped. It is for a
// message's top-level text only.
func NoticeText(fixed, detail string) string {
	if detail == "" {
		return fixed
	}
	return fixed + " " + Text(detail, maxNoticeDetail)
}

const maxNoticeDetail = 200 // units of a notice's detail

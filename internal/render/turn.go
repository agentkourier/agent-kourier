package render

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/redact"
)

// These builders make the messages of a turn in the chat-neutral model (package chat) from agent output. What
// they keep is what no platform differs on: the secrets are redacted out of a tool's arguments, the text that is
// shown is bounded, and the words courier writes itself are fixed. What they do not do is anything for a
// platform's markup: the text they return is literal, and each adapter makes it inert for its own platform, cleans
// it, and cuts it to its own limits (spec 8.4, R2). So nothing here escapes, and nothing here may be escaped twice.

// MaxFallback is the length, in UTF-16 units, of the plain text a turn's message carries beside its parts, which a
// platform shows where it cannot show them, such as in a notification.
const MaxFallback = 1000

// NoOutput is what a message says when its turn ended with nothing to show.
const NoOutput = "(no output)"

const (
	maxToolName = 64  // units of a tool name in a status line
	maxToolArgs = 160 // units of the argument summary in a status line

	maxNoticeDetail = 200 // units of a notice's detail
)

// Body returns the text of a turn: each artifact's latest text, in the order the artifacts first appeared, one
// blank line between them, cleaned of control and hidden characters and cut to at most limit UTF-16 units, the cut
// marked with chat.Truncated. events are the TextEvents of the turn in arrival order. A TextEvent holds an
// artifact's whole text so far, so a later one replaces an earlier one and keeps its place. An artifact with
// nothing to show once cleaned, such as white space and zero-width characters, is left out.
func Body(events []agent.TextEvent, limit int) string {
	var order []a2a.ArtifactID
	latest := map[a2a.ArtifactID]string{}
	for _, e := range events {
		if _, seen := latest[e.ArtifactID]; !seen {
			order = append(order, e.ArtifactID)
		}
		latest[e.ArtifactID] = e.Text
	}
	var parts []string
	for _, id := range order {
		if text := chat.Clean(latest[id], false); strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	return chat.Truncate(strings.Join(parts, "\n\n"), limit)
}

// Tool returns the status line of a tool call: the tool's name and a summary of its arguments, sorted by key, as
// "name=json, ...". Secrets are taken out first, before anything is shown or cut (redactArgs and redact.Text):
// values by the name they sit under, in maps and in "name=value" text, URL credentials, private keys, tokens and AWS
// keys by shape. Then name and summary are each put on one line and capped, so the line is short whatever the agent
// sent.
func Tool(name string, args map[string]any) chat.ToolLine {
	summary := toolSummary(redactArgs(args).(map[string]any))
	return chat.ToolLine{
		Name:    chat.Truncate(chat.Clean(name, true), maxToolName),
		Summary: chat.Truncate(chat.Clean(summary, true), maxToolArgs),
	}
}

// ToolText is the status line as plain text, "Calling name(summary)", for a message that has nothing but it to say.
func ToolText(t chat.ToolLine) string { return "Calling " + t.Name + "(" + t.Summary + ")" }

// toolSummary is the arguments, already redacted one by one, as "name=json, ...", redacted again as a whole for
// what only shows there. It is cut to redact.MaxBytes at a word first: each argument was bounded, but a call may
// have any number of them, and the line shows a few hundred characters.
func toolSummary(shown map[string]any) string {
	keys := make([]string, 0, len(shown))
	for k := range shown {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + jsonValue(shown[k])
	}
	return redact.Text(strings.Join(parts, ", "))
}

// jsonValue is v as compact JSON, with "<", ">" and "&" left as they are.
func jsonValue(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return "?"
	}
	return strings.TrimSpace(b.String())
}

// Turn is the message of a turn that has not ended, or has: the text so far as its body, and, while tool is not nil,
// the status line of the call under way. The text is bounded at what the platform shows in a body (caps), and its
// plain fallback at MaxFallback or, with no text, is the status line. ok is false if there is nothing to show yet.
func Turn(events []agent.TextEvent, tool *agent.ToolCallEvent, caps chat.Caps) (m chat.Message, ok bool) {
	body := Body(events, caps.Limit(chat.WhereBody))
	m.Fallback = Body(events, MaxFallback)
	if body != "" {
		m.Parts = append(m.Parts, chat.Body{Markdown: body})
	}
	if tool != nil {
		line := Tool(tool.Name, tool.Args)
		m.Parts = append(m.Parts, line)
		if m.Fallback == "" {
			m.Fallback = ToolText(line)
		}
	}
	return m, len(m.Parts) > 0
}

// Nothing is the message of a turn that ended with nothing to show: it is there so that a tool status line is not
// left standing as if the agent were still at work.
func Nothing() chat.Message {
	return chat.Message{Fallback: NoOutput, Parts: []chat.Part{chat.Body{Markdown: NoOutput}}}
}

// Notice is the message of a notice: fixed wording that courier wrote, and, if there is one, a short detail, which
// is untrusted like everything else and is put on one line and capped.
func Notice(fixed, detail string) chat.Message {
	text := fixed
	if detail != "" {
		text += " " + chat.Truncate(chat.Clean(detail, true), maxNoticeDetail)
	}
	return chat.Message{Fallback: text}
}

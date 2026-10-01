// Package render turns agent and alert output into text for Slack. Everything it takes in is
// attacker-influenced (spec 8.4, R2), so its functions make text inert before it can reach a
// message: no mention, broadcast, link or hidden character survives, and the text fits Slack's
// limits. They are pure: no Slack client, no I/O. Courier's interaction prompts (WP-33) use them
// too.
package render

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// Slack's limits, in characters. UNCONFIRMED(WP-04): the 12000 of a markdown block or a stream
// append and the 40000 of a message are Slack's documented numbers, not measured.
const (
	MaxBlock   = 12000
	MaxMessage = 40000

	// Truncated ends text that was cut at a limit. v1 truncates; it does not continue in a
	// further message. It holds nothing that needs escaping, in any style.
	Truncated = "… (truncated)"

	maxToolName = 64  // runes of a tool name in a status line
	maxToolArgs = 160 // runes of the argument summary in a status line
)

// Text returns s as inert Slack text of at most limit characters: control and format characters
// removed, "&", "<" and ">" escaped, and a cut marked with Truncated. Newlines and tabs stay.
// "<!channel>", "<@U123>", "<https://x|y>" and the like come out as plain text.
//
// Zero-width, bidi-override and other format characters are deleted before escaping, so none can
// split "<!channel>" apart or reorder what the reader sees. That also breaks a zero-width-joined
// emoji into its parts, which is the price of deleting them all.
func Text(s string, limit int) string { return render(s, limit, false, true) }

// Markdown is Text for a markdown block, which also treats "[text](url)" and "![alt](url)" as a
// link and an image whose label the author chooses. It escapes "\", "[" and "]" with a backslash
// too, so none of those forms survives. A bare URL still becomes a link showing its own address.
func Markdown(s string, limit int) string { return render(s, limit, true, true) }

// Body returns the text of a turn: each artifact's latest text, in the order the artifacts first
// appeared, one blank line between them, as Text of at most limit characters. events are the
// TextEvents of the turn in arrival order. A TextEvent holds an artifact's whole text so far, so
// a later one replaces an earlier one and keeps its place. An artifact with only white space is left out.
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
		if strings.TrimSpace(latest[id]) != "" {
			parts = append(parts, latest[id])
		}
	}
	return Text(strings.Join(parts, "\n\n"), limit)
}

// ToolLine returns the status line for a tool call: the tool's name and a summary of its
// arguments, sorted by key, as "Calling name(key=value, ...)". Name and summary are each inert,
// on one line, and capped, so the line is short whatever the agent sent.
func ToolLine(name string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + jsonValue(args[k])
	}
	return "Calling " + render(name, maxToolName, false, false) +
		"(" + render(strings.Join(parts, ", "), maxToolArgs, false, false) + ")"
}

// jsonValue is v as compact JSON, with "<", ">" and "&" left as they are for render to escape.
func jsonValue(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return "?"
	}
	return strings.TrimSpace(b.String())
}

// render cleans s, escapes it and cuts it to limit characters. The cut is made on the escaped
// text, one character at a time, so it never splits a rune or an entity such as "&amp;".
func render(s string, limit int, markdown, multiline bool) string {
	s = clean(s, multiline)
	var out strings.Builder
	total := 0
	for _, r := range s {
		u := escape(r, markdown)
		out.WriteString(u)
		total += utf8.RuneCountInString(u)
	}
	if total <= limit {
		return out.String()
	}
	marker := utf8.RuneCountInString(Truncated)
	if limit <= marker {
		return string([]rune(Truncated)[:max(limit, 0)])
	}
	out.Reset()
	used := 0
	for _, r := range s {
		u := escape(r, markdown)
		if used+utf8.RuneCountInString(u) > limit-marker {
			break
		}
		out.WriteString(u)
		used += utf8.RuneCountInString(u)
	}
	return out.String() + Truncated
}

// escape returns r as it may appear in Slack text. Slack's mrkdwn and message text use "&amp;",
// "&lt;" and "&gt;" and nothing else; a markdown block also reads backslash escapes.
func escape(r rune, markdown bool) string {
	switch r {
	case '&':
		return "&amp;"
	case '<':
		return "&lt;"
	case '>':
		return "&gt;"
	case '\\', '[', ']':
		if markdown {
			return `\` + string(r)
		}
	}
	return string(r)
}

// clean removes what cannot be shown or can hide text: invalid UTF-8 becomes U+FFFD, format
// characters (category Cf: zero-width space and joiners, bidi marks, overrides and isolates, the
// BOM, the soft hyphen, tag characters) and control characters (Cc) are deleted, and carriage
// returns and the Unicode line and paragraph separators become newlines. With multiline false
// every newline and tab becomes a space, for a one-line status. It is the same character classes
// as the audit writer's clean, which also drops them, except that newlines are kept.
func clean(s string, multiline bool) string {
	s = strings.ToValidUTF8(s, "\ufffd")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029':
			if multiline {
				return '\n'
			}
			return ' '
		case r == '\t':
			if multiline {
				return r
			}
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// Package render turns agent and alert output into text for Slack. Everything it takes in is
// attacker-influenced (spec 8.4, R2), so its functions make text inert before it can reach a
// message: no mention, broadcast, link or hidden character survives, and the text fits Slack's
// limits. They are pure: no Slack client, no I/O. Courier's interaction prompts (WP-33) use them
// too.
//
// The contract has two halves, and this package keeps only one. Its output is inert only when it is
// posted with no parse, no link_names and no unfurling, which the Slack client's inertOptions (in
// internal/chat/slack) puts on every post, update and stream; with link_names on, a plain "@here"
// would still broadcast, and an unfurl would make Slack fetch a URL the agent wrote. So plain
// "@here", "#general" and bare URLs pass through here as text, on purpose. And text is rendered
// once, at the last step before it is sent: rendering rendered text escapes its entities again,
// and nothing here unescapes.
//
// Threat model of the redaction in tool lines (redact.go): it stops accidental exposure, a
// credential in an argument that an honest agent passed to a tool. It does not stop a determined
// agent. Letters that only look like Latin ones (a Cyrillic "a" in "password") are not matched, and
// the agent can write a secret into its own reply, which is body text and is never redacted.
package render

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/redact"
)

// Slack's limits, in characters. UNCONFIRMED(WP-04): the 12000 of a markdown block or a stream
// append and the 40000 of a message are Slack's documented numbers, not measured, and so is the
// unit: whether Slack counts runes or UTF-16 units. Every cap here counts UTF-16 units, which is
// never fewer than runes, so the text fits either way.
const (
	MaxBlock   = 12000
	MaxMessage = 40000

	// Truncated ends text that was cut at a limit. v1 truncates; it does not continue in a
	// further message. It holds nothing that needs escaping, in any style.
	Truncated = redact.Truncated

	maxToolName = 64  // units of a tool name in a status line
	maxToolArgs = 160 // units of the argument summary in a status line
)

// Flavour is the kind of Slack text to render.
type Flavour int

// The flavours.
const (
	// FlavourText is message text and mrkdwn: Text.
	FlavourText Flavour = iota
	// FlavourMarkdown is a markdown block or a stream's markdown_text: Markdown.
	FlavourMarkdown
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
//
// UNCONFIRMED(WP-04): that Slack honours CommonMark backslash escapes in a markdown block and in
// markdown_text, and shows "&lt;" there as the literal text; the safety of this flavour rests on it.
func Markdown(s string, limit int) string { return render(s, limit, true, true) }

// Body returns the text of a turn: each artifact's latest text, in the order the artifacts first
// appeared, one blank line between them, rendered as flavour and at most limit units long. events
// are the TextEvents of the turn in arrival order. A TextEvent holds an artifact's whole text so
// far, so a later one replaces an earlier one and keeps its place. An artifact with nothing to
// show once cleaned, such as white space and zero-width characters, is left out.
func Body(events []agent.TextEvent, limit int, flavour Flavour) string {
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
		if text := clean(latest[id], true); strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	return render(strings.Join(parts, "\n\n"), limit, flavour == FlavourMarkdown, true)
}

// ToolLine returns the status line for a tool call: the tool's name and a summary of its
// arguments, sorted by key, as "Calling name(key=value, ...)". Secrets are taken out first, before
// anything is shown or cut (redactArgs and redact.Text): values by the name they sit under, in
// maps and in "name=value" text, URL credentials, private keys, tokens and AWS keys by shape.
// Then name and summary are each made inert, on one line, and capped, so the line is short
// whatever the agent sent.
// flavour is the text it will sit in: for FlavourMarkdown, "[text](url)" in a name or an argument
// is escaped too, and does not become a link.
func ToolLine(name string, args map[string]any, flavour Flavour) string {
	summary := toolSummary(redactArgs(args).(map[string]any))
	md := flavour == FlavourMarkdown
	return "Calling " + render(name, maxToolName, md, false) + "(" + render(summary, maxToolArgs, md, false) + ")"
}

// toolSummary is the arguments, already redacted one by one, as "name=json, ...", redacted again as
// a whole for what only shows there. It is cut to maxRedactBytes at a word first: each argument was
// bounded, but a call may have any number of them, and the line shows a few hundred characters.
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

// render cleans s, escapes it and cuts it to limit UTF-16 units. The cut is made on the escaped
// text, one character at a time, so it never splits a rune or an entity such as "&amp;".
func render(s string, limit int, markdown, multiline bool) string {
	s = clean(s, multiline)
	var out strings.Builder
	total := 0
	for _, r := range s {
		u := escape(r, markdown)
		out.WriteString(u)
		total += units(u)
	}
	if total <= limit {
		return out.String()
	}
	if limit <= units(Truncated) {
		return string([]rune(Truncated)[:max(limit, 0)])
	}
	out.Reset()
	used := 0
	for _, r := range s {
		u := escape(r, markdown)
		if used+units(u) > limit-units(Truncated) {
			break
		}
		out.WriteString(u)
		used += units(u)
	}
	return out.String() + Truncated
}

// units is the length of s in UTF-16 units: a rune outside the Basic Multilingual Plane takes two.
func units(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
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
// BOM, the soft hyphen, tag characters), control characters (Cc), private use characters (Co) and
// the two blank fillers Hangul (U+3164) and Braille (U+2800) are deleted, and carriage
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
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r) || r == '\u3164' || r == '\u2800':
			return -1
		}
		return r
	}, s)
}

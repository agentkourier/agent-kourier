// Package render turns agent output into messages in the chat-neutral model (package chat): the text of a turn,
// the status line of a tool call, a notice. Everything it takes in is attacker-influenced (spec 8.4, R2), so what
// it keeps is what no platform differs on: secrets are redacted out of a tool's arguments (redact.go), what is shown
// is bounded, and the words courier writes are fixed. The text it returns is literal. It does nothing for a
// platform's markup: each adapter (internal/chat/slack) makes text inert for its platform, cleans it and cuts it to
// its limits, and must pass chattest.Conformance to show it does. They are pure: no platform, no I/O.
//
// Threat model of the redaction in tool lines: it stops accidental exposure, a credential in an argument that an
// honest agent passed to a tool. It does not stop a determined agent. Letters that only look like Latin ones (a
// Cyrillic "a" in "password") are not matched, and the agent can write a secret into its own reply, which is body
// text and is never redacted.
//
// What is left of Slack's own text and Block Kit here (text.go, prompt.go, replacement.go) serves the interaction
// prompts until they are ported too (WP-37 S4), and then goes.
package render

import (
	"strings"
	"unicode"

	"github.com/agentkourier/agent-kourier/internal/redact"
)

// Truncated ends text that was cut at a limit. v1 truncates; it does not continue in a further message. It holds
// nothing that needs escaping, in any style.
const Truncated = redact.Truncated

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

package chat

import (
	"strings"

	"github.com/agentkourier/agent-kourier/internal/redact"
)

// Truncated ends text that Truncate cut. It holds nothing an adapter needs to escape, on any platform.
const Truncated = "… (truncated)"

// Clean returns s without the characters a reader cannot see and a platform may act on: the invisible
// characters of redact.Normalize, which owns the set, so that display and redaction cannot disagree on it.
// They are control characters, format characters (zero-width, joiners, bidirectional overrides and isolates,
// the soft hyphen, the invisible tag characters), private-use and surrogate code points, the two blank fillers
// (Hangul U+3164 and Braille U+2800), the variation selectors and invalid UTF-8. Every kind of line break (CR,
// CRLF, NEL, the Unicode line and paragraph separators) becomes "\n". They are deleted before an adapter
// escapes anything, so none can split "<!channel>" apart, or reorder what the reader sees. A zero-width-joined
// emoji falls apart into its parts, which is the price of deleting them all.
//
// If line is true the result is one line: any white space, a line break among it, becomes a single space,
// and the ends are trimmed. That is all Clean adds to the normalization.
func Clean(s string, line bool) string {
	out := redact.Normalize(s)
	if line {
		out = strings.Join(strings.Fields(out), " ")
	}
	return out
}

// Units is the length of s in UTF-16 units: a rune outside the Basic Multilingual Plane takes two. Limits
// are counted in them because a platform may count them so, and they are never fewer than runes, so text
// that fits by them fits either way.
func Units(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// Truncate returns s if it is at most limit units long, and otherwise its longest prefix that leaves room
// for Truncated, followed by Truncated. It never splits a rune. A limit too small for Truncated gets the
// start of Truncated alone.
func Truncate(s string, limit int) string {
	if Units(s) <= limit {
		return s
	}
	if limit <= Units(Truncated) {
		return string([]rune(Truncated)[:max(limit, 0)])
	}
	used := 0
	var b strings.Builder
	for _, r := range s {
		u := 1
		if r > 0xFFFF {
			u = 2
		}
		if used+u > limit-Units(Truncated) {
			break
		}
		b.WriteRune(r)
		used += u
	}
	return b.String() + Truncated
}

// Literal returns s as the text a person sees for a literal field at place w: cleaned (on one line for a
// Line place) and cut to the platform's limit. It is the reference an adapter's output is checked
// against, and what an adapter that shows plain text uses as it is.
func Literal(s string, w Where, c Caps) string {
	return Truncate(Clean(s, w.Line()), c.Limit(w))
}

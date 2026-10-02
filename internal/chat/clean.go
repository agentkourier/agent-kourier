package chat

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Truncated ends text that Truncate cut. It holds nothing an adapter needs to escape, on any platform.
const Truncated = "… (truncated)"

// Clean returns s without the characters a reader cannot see and a platform may act on: control
// characters, and format characters (zero-width, joiners, bidirectional overrides and isolates, the soft
// hyphen, the invisible tag characters), private-use and surrogate code points and invalid UTF-8. Every
// kind of line break (CR, CRLF, NEL, the Unicode line and paragraph separators) becomes "\n". They are
// deleted before an adapter escapes anything, so none can split "<!channel>" apart, or reorder what the
// reader sees. A zero-width-joined emoji falls apart into its parts, which is the price of deleting them
// all.
//
// If line is true the result is one line: any white space, a line break among it, becomes a single space,
// and the ends are trimmed.
func Clean(s string, line bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == utf8.RuneError && n == 1:
			continue
		case r == '\r':
			if i < len(s) && s[i] == '\n' {
				i++
			}
			b.WriteByte('\n')
		case r == '\n' || r == '\u0085' || r == '\u2028' || r == '\u2029':
			b.WriteByte('\n')
		case r == '\t':
			b.WriteByte('\t')
		case unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs):
			continue
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
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

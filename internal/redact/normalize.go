package redact

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Normalize returns s as redaction reads it, and as a reader sees it: with the invisible characters deleted and
// every kind of line break written "\n". It is the one deletion set of courier (chat.Clean deletes the same
// set for display), so what a pattern matches and what a person reads cannot disagree.
//
// The invisible characters are control characters (Cc) except the line breaks and tab, format characters (Cf:
// zero-width space and joiners, bidirectional controls, the soft hyphen, the tag characters), private-use (Co) and
// surrogate (Cs) code points, invalid UTF-8, and the rest of the Unicode Default_Ignorable_Code_Point property that
// is shown as nothing: the blank fillers (U+3164, U+2800, the Hangul fillers U+115F, U+1160 and U+FFA0), the combining
// grapheme joiner U+034F, the Khmer inherent vowels U+17B4 and U+17B5, every variation selector (U+180B to U+180D and
// U+180F, which are Mongolian's, U+FE00 to U+FE0F, U+E0100 to U+E01EF) and the reserved code points that the property
// holds for characters yet to come (U+2065, U+FFF0 to U+FFF8, and U+E0000 to U+E0FFF, the tag and selector block).
// An emoji keeps its glyph and loses only its presentation hint, and a zero-width-joined emoji falls apart into its
// parts, which is the price of deleting them all.
//
// The line breaks are CR, CRLF, NEL (U+0085) and the Unicode line and paragraph separators (U+2028, U+2029),
// each of which becomes "\n". Tab and every kind of white space stay. A literal escape, the text that %q prints
// for an invisible character (a backslash and "x07", "u200b"), is text and stays too: the patterns of
// redaction read it as the gap it stands for.
func Normalize(s string) string {
	if !changes(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == '\r':
			if i < len(s) && s[i] == '\n' {
				i++
			}
			b.WriteByte('\n')
		case isLineBreak(r):
			b.WriteByte('\n')
		case r == utf8.RuneError && n == 1, deleted(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// changes reports whether Normalize would change s: a string with nothing to delete or rewrite is returned as it is.
func changes(s string) bool {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		if r == '\r' || (r != '\n' && isLineBreak(r)) || (r == utf8.RuneError && n == 1) || deleted(r) {
			return true
		}
	}
	return false
}

// isLineBreak reports whether r ends a line: LF, NEL and the Unicode separators. CR is not among them: it is
// the one that takes a following LF with it (CRLF is one break), so Normalize and changes read it first.
func isLineBreak(r rune) bool {
	return r == '\n' || r == '\U00000085' || r == '\U00002028' || r == '\U00002029'
}

// deleted reports whether r is in the deletion set. A line break is not: it is rewritten, not deleted. CR
// is a control character, so a caller must have read it as a line break before it asks.
func deleted(r rune) bool {
	switch {
	case r == '\n' || r == '\t' || isLineBreak(r):
		return false
	case r == '\U00003164' || r == '\U00002800' || r == '\U0000115f' || r == '\U00001160' || r == '\U0000ffa0': // blank fillers: shown as nothing, in none of the categories below
		return true
	case r == '\U0000034f' || r == '\U000017b4' || r == '\U000017b5': // the combining grapheme joiner and Khmer's inherent vowels: Mn, shown as nothing
		return true
	case r >= '\U0000180b' && r <= '\U0000180d', r == '\U0000180f', r >= '\U0000fe00' && r <= '\U0000fe0f': // variation selectors
		return true
	case r == '\U00002065', r >= '\U0000fff0' && r <= '\U0000fff8', r >= '\U000e0000' && r <= '\U000e0fff': // reserved, ignorable by default (the last range holds the selectors U+E0100 to U+E01EF too)
		return true
	}
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs)
}

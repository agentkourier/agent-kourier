package redact

import (
	"strings"
	"unicode/utf8"
)

// The gates of the costly patterns. A gate is a condition that every text a pattern matches meets, cheap to test, and it is
// read from the string the pattern is about to read: where it is false the pattern cannot match, its ReplaceAll would return
// the string as it is, and rawSecrets and matchFree skip it. A gate may be true where its pattern finds nothing, which costs
// only the work it would have saved; it is never false where its pattern matches (TestGatesAreNecessary and
// FuzzGatesAreNecessary hold each one to its pattern, and TestGatesChangeNothing holds rawSteps and freeSteps, the gates in
// the order they run, to the same steps with every gate open).
//
// The patterns of the token layer spell their words with a gap between any two letters (see spell and pair), and some are
// case-insensitive, under which Go's regexp matches U+212A KELVIN SIGN as "k" and U+017F LATIN SMALL LETTER LONG S as "s",
// and folds the gap's escapes too, so that "\T" and "\X0A" are escapes between the letters of such a word. Their gates are
// true for a text that holds a gap in either case, or U+212A or U+017F (see holdsGap); in any other text the words are
// written in ASCII letters, whole, and the gate looks for them in any ASCII case.

// escStarts are the characters that follow the backslash of an esc, in lower case: a case-insensitive pattern reads an esc
// in either case ("\t" and "\T", "\x0a" and "\X0A"), and the form with a capital U is the long one.
const escStarts = "abfnrtv0xu"

// holdsGap reports whether s holds anything that can stand between the letters of a word of the token layer, or for one of
// them: a character of deletable, invalid UTF-8 (which a pattern reads as U+FFFD, which deletable holds), a backslash and a
// character that can start an esc, in either case ("\t", "\T", "\x00", "\u200b", "\U000e0001"; inside (?i:...) the escapes
// fold, TestHoldsGapTakesEveryGapAndFold generates them all), or U+212A or U+017F, which are "k" and "s" to a
// case-insensitive pattern and the only characters outside ASCII that are an ASCII letter to one (TestDeletableIsTheDeletionSet
// holds holdsGap to deletable over every code point, and TestHoldsGapTakesEveryGapAndFold to unicode.SimpleFold). It takes a
// backslash and one of those characters, a gap only when an esc follows, as one all the same: what it holds more than the
// patterns do makes a gate true more often, never false where a pattern matches.
func holdsGap(s string) bool {
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c < ' ' && c != '\t' && c != '\n' && c != '\r', c == 0x7f:
				return true
			case c == '\\' && i+1 < len(s) && strings.IndexByte(escStarts, lowerASCII(s[i+1])) >= 0:
				return true
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError || r == '\u212a' || r == '\u017f' || deleted(r) {
			return true
		}
		i += n
	}
	return false
}

// regap returns out, what a pattern made of s, and whether it holds a gap: gap, what holdsGap said of s, if the pattern left
// s as it was, and holdsGap of out if it changed it.
func regap(s, out string, gap bool) (string, bool) {
	if out == s {
		return s, gap
	}
	return out, holdsGap(out)
}

// holdsFold reports whether s holds any of words, each of lower-case ASCII, in any ASCII case.
func holdsFold(s string, words ...string) bool {
	for _, w := range words {
		if containsFold(s, w) {
			return true
		}
	}
	return false
}

// containsFold reports whether s holds w, which is lower-case ASCII, in any ASCII case.
func containsFold(s, w string) bool {
	for i := 0; i+len(w) <= len(s); i++ {
		j := 0
		for j < len(w) && lowerASCII(s[i+j]) == w[j] {
			j++
		}
		if j == len(w) {
			return true
		}
	}
	return false
}

// lowerASCII returns c in lower case if it is an ASCII capital, and c otherwise.
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// The gates of the token layer. Each takes the text and what holdsGap says of it, and is true when the text holds a gap or one
// of the words that every match of its pattern holds, in ASCII and lower case. Where a word is case-sensitive in the pattern
// ("xox", "eyJ", "sk-"), the gate takes it in any case, which it holds more often.

// secretGate is secretPattern's: a Slack token holds "xox" or "xapp-", a Bearer credential "bearer" and a JWT "eyJ".
func secretGate(s string, gap bool) bool { return gap || holdsFold(s, "xox", "xapp-", "bearer", "eyj") }

// skGate is skProject's and skBare's: a key holds "sk-".
func skGate(s string, gap bool) bool { return gap || holdsFold(s, "sk-") }

// oauthGate is oauthPattern's: each of its field names ends in "token" or "secret".
func oauthGate(s string, gap bool) bool { return gap || holdsFold(s, "token", "secret") }

// basicGate is basicPattern's and gluedBasicPattern's: both hold the scheme, "basic".
func basicGate(s string, gap bool) bool { return gap || holdsFold(s, "basic") }

// The gates of matchFree's own patterns, which take no gap: the text they read is normalized, and their words are plain.

// urlGate is urlUserinfo's and urlSpaced's: "://" and an "@" after it.
func urlGate(s string) bool {
	i := strings.Index(s, "://")
	return i >= 0 && strings.IndexByte(s[i:], '@') >= 0
}

// userinfoGate is plainUserinfo's: a ":" and an "@" after it.
func userinfoGate(s string) bool {
	i := strings.IndexByte(s, ':')
	return i >= 0 && strings.IndexByte(s[i:], '@') >= 0
}

// envGate is envPair's: the words "name" and "value", in any case (neither has a letter that a case-insensitive pattern
// matches outside ASCII).
func envGate(s string) bool { return containsFold(s, "name") && containsFold(s, "value") }

// userFlagGate is userFlag's: each of its flags, -u, --user and --proxy-user, holds "-u", in any case.
func userFlagGate(s string) bool { return containsFold(s, "-u") }

// flagGate is secretFlag's: a "-" at the start of s or after white space, and, past a second "-" if there is one, a letter.
// The pattern's letter is [a-z] under (?i), which U+212A and U+017F match too, so a byte outside ASCII counts as one.
func flagGate(s string) bool {
	for i := strings.IndexByte(s, '-'); i >= 0; {
		if r, _ := utf8.DecodeLastRuneInString(s[:i]); i == 0 || isSpace(r) {
			j := i + 1
			if j < len(s) && s[j] == '-' {
				j++
			}
			if j < len(s) && (lowerASCII(s[j]) >= 'a' && lowerASCII(s[j]) <= 'z' || s[j] >= utf8.RuneSelf) {
				return true
			}
		}
		k := strings.IndexByte(s[i+1:], '-')
		if k < 0 {
			break
		}
		i += 1 + k
	}
	return false
}

// keyLen is how long a run of bareRun is that matchFree takes as an AWS secret access key.
const keyLen = 40

// keyRunGate is the gate of bareRun's replacement, which takes only a run of exactly keyLen: s holds a run of that many
// characters of bareRun's class with none of them on either side.
func keyRunGate(s string) bool {
	n := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '/' || c == '+' || c == '=' {
			n++
			continue
		}
		if n == keyLen {
			return true
		}
		n = 0
	}
	return n == keyLen
}

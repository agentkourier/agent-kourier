package github

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Mentions says whether text mentions handle, a login as a mention writes it (an app's without its [bot]): "@" and the handle, in any
// case, as GitHub matches logins without regard to case (unverified), and on word boundaries:
//
//   - the "@" starts the text, or follows a character that is not a letter, a digit, '_', '-', '.' or '/', so that an email address
//     (someone@pr-reviewer.example) or a path (/@pr-reviewer) is not a mention;
//   - the handle ends the text, or is followed by a character that is not a letter, a digit, '_', '-' or '/', so that a longer login
//     (@pr-reviewer-2) or a team (@pr-reviewer/sre) is not a mention of it. A full stop, a comma, a quote or an app's "[bot]" may follow.
//
// It reads the text as written: a mention inside a code span or a code block counts here, although GitHub does not notify the
// account of one there (unverified).
func Mentions(text, handle string) bool {
	if handle == "" {
		return false
	}
	for i := strings.IndexByte(text, '@'); i >= 0; {
		end := i + 1 + len(handle)
		if end <= len(text) && equalFoldASCII(text[i+1:end], handle) && boundaryBefore(text[:i]) && boundaryAfter(text[end:]) {
			return true
		}
		next := strings.IndexByte(text[i+1:], '@')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return false
}

// boundaryBefore says whether the text before an "@" lets it start a mention.
func boundaryBefore(before string) bool {
	if before == "" {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(before)
	return !wordRune(r) && r != '.' && r != '/'
}

// boundaryAfter says whether the text after a handle ends the mention there.
func boundaryAfter(after string) bool {
	if after == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(after)
	return !wordRune(r) && r != '/'
}

// wordRune says whether r continues a word, or a login: a letter, a digit, '_' or '-'.
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' }

// equalFoldASCII says whether a and b are equal but for the case of ASCII letters. A login is ASCII, and folding other characters
// (the Kelvin sign is a K to strings.EqualFold) would match what GitHub does not.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

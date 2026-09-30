package store

import (
	"regexp"
	"strings"
)

const (
	redacted = "[redacted]"

	// space is any Unicode white space: what unicode.IsSpace accepts, which is
	// the ASCII set plus vertical tab, NEL (U+0085) and category Z. Go's \s is
	// ASCII only, so NBSP and the em space would slip past it.
	space = `\t-\r \x{85}\p{Z}`
	// format is category Cf: zero-width space and joiners, BOM, soft hyphen,
	// word joiner. They render as nothing, so an attacker can hide one between
	// a scheme and its credential, or inside a prefix.
	format = `\p{Cf}`
	// gap separates a scheme from its credential.
	gap = `[` + space + format + `]`
	// credential is a run of non-space characters. It may contain format
	// characters, but cannot start with one, so a scheme followed by nothing
	// but a format character is left alone.
	credential = `[^` + space + format + `][^` + space + `]*`
)

// spell returns a pattern for s that tolerates format characters between its
// letters ("xo<ZWSP>xb-").
func spell(s string) string {
	return strings.Join(strings.Split(s, ""), format+"*")
}

var (
	// secretPattern matches Slack tokens, bearer credentials and JWTs. Bearer
	// takes a gap and any credential, or, glued to the scheme, a token-shaped
	// run of at least 8 characters so "bearers" and "bearer's" survive.
	// Compiled once and never modified.
	secretPattern = regexp.MustCompile(
		spell("xox") + format + `*[a-z]` + format + `*-[^` + space + `]+` +
			`|` + spell("xapp-") + `[^` + space + `]+` +
			`|(?i:` + spell("bearer") + `)(?:` + gap + `+` + credential + `|[\w~+/=.-]{8,})` +
			`|` + spell("eyJ") + `[\w\p{Cf}-]+\.[\w\p{Cf}-]+\.[\w\p{Cf}-]*`)

	// basicPattern matches "Basic" and a base64 credential of at least 8
	// characters (user:pass encodes to that or longer). Basic is a common
	// English word, so redactBasic also spares plain words.
	basicPattern = regexp.MustCompile(`(?i:` + spell("basic") + `)` + gap + `+([A-Za-z0-9+/]{8,}={0,2})`)

	// plainWord is a word with at most a leading capital: prose, not base64.
	plainWord = regexp.MustCompile(`^[A-Z]?[a-z]+$`)
)

// RedactSecrets replaces anything in s that looks like a Slack token, a Bearer
// or Basic credential, or a JWT with "[redacted]". Schemes and credentials may
// be separated by any Unicode white space or format character. It is a
// backstop for free-text audit fields, not a license to put secrets in them.
func RedactSecrets(s string) string {
	s = secretPattern.ReplaceAllString(s, redacted)
	return basicPattern.ReplaceAllStringFunc(s, func(m string) string {
		if plainWord.MatchString(basicPattern.FindStringSubmatch(m)[1]) {
			return m
		}
		return redacted
	})
}

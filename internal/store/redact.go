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
	// blank is what may pad a field: white space, format and control (Cc)
	// characters, so NUL, ESC and the C1 set cannot split a scheme from its
	// credential either.
	blank = space + format + `\p{Cc}`
	// sep separates a scheme from its credential: any run of blank characters,
	// colons ("Bearer: X"), or a percent-encoded space ("Bearer%20X").
	sep = `(?:[` + blank + `:]|%20)+`
	// credential is a run of non-space characters. It may contain format and
	// control characters, but cannot start with a separator character, so a
	// scheme followed by nothing but padding is left alone.
	credential = `[^` + blank + `:][^` + space + `]*`
	// value is the value of an OAuth field: it stops at white space, a quote,
	// or a query, list or object delimiter.
	value = `[^` + space + `"',&;}]+`
)

// spell returns a pattern for s that tolerates format characters between its
// letters ("xo<ZWSP>xb-").
func spell(s string) string {
	return strings.Join(strings.Split(s, ""), format+"*")
}

var (
	// secretPattern matches Slack tokens, bearer credentials and JWTs. Bearer
	// takes a separator and any credential, or, glued to the scheme, a run of
	// at least 7 token characters starting with a letter or digit, so
	// "bearers", "bearer's" and identifiers like "bearer-gateway" survive.
	// Compiled once and never modified.
	secretPattern = regexp.MustCompile(
		spell("xox") + format + `*[a-z]` + format + `*-[^` + space + `]+` +
			`|` + spell("xapp-") + `[^` + space + `]+` +
			`|(?i:` + spell("bearer") + `)(?:` + sep + credential + `|[A-Za-z0-9][\w~+/=.\p{Cf}-]{6,})` +
			`|` + spell("eyJ") + `[\w\p{Cf}-]+\.[\w\p{Cf}-]+\.[\w\p{Cf}-]*`)

	// oauthPattern matches an OAuth token or secret field, as key=value or as
	// JSON "key":"value". The first group is the key and its punctuation, kept;
	// the second is the value, redacted.
	oauthPattern = regexp.MustCompile(`((?i:` +
		spell("access_token") + `|` + spell("refresh_token") + `|` + spell("id_token") + `|` + spell("client_secret") +
		`)["']?[` + blank + `]*[:=][` + blank + `]*["']?)(` + value + `)`)

	// basicPattern matches "Basic" and a base64 credential of at least 8
	// characters (user:pass encodes to that or longer). Basic is a common
	// English word, so RedactSecrets also spares plain words.
	basicPattern = regexp.MustCompile(`(?i:` + spell("basic") + `)` + sep + `([A-Za-z0-9+/]{8,}={0,2})`)

	// plainWord is a word with at most a leading capital: prose, not base64.
	plainWord = regexp.MustCompile(`^[A-Z]?[a-z]+$`)
)

// RedactSecrets replaces anything in s that looks like a Slack token, a Bearer
// or Basic credential, a JWT, or the value of an OAuth access_token,
// refresh_token, id_token or client_secret field with "[redacted]". Schemes
// and credentials may be separated by any Unicode white space, format or
// control character, a colon, or "%20". It is a backstop for free-text audit
// fields, not a license to put secrets in them.
//
// Known limits, both deliberate: a Basic credential under 8 base64 characters
// survives (it would collide with ordinary words), and a scheme name written in
// fullwidth or other compatibility forms (U+FF22 for B) is not matched, because
// that would need NFKC normalization first.
func RedactSecrets(s string) string {
	s = secretPattern.ReplaceAllString(s, redacted)
	s = oauthPattern.ReplaceAllString(s, "${1}"+redacted)
	return basicPattern.ReplaceAllStringFunc(s, func(m string) string {
		if plainWord.MatchString(basicPattern.FindStringSubmatch(m)[1]) {
			return m
		}
		return redacted
	})
}

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
	// stop ends a credential that may sit inside JSON, a list or a header: a
	// quote, comma, semicolon, closing brace, or the backslash of an escaped
	// quote (what %q prints inside a string).
	stop = `"',;}\\`
	// credential is a run of non-space characters up to a stop. It may contain
	// format and control characters and start with a (possibly escaped) quote,
	// but cannot start with a separator or stop character, so a scheme followed
	// by nothing but padding, or by the end of a JSON string, is left alone.
	credential = `\\*["']?[^` + blank + `:` + stop + `][^` + space + stop + `]*`
	// value is the value of an OAuth field: it stops at white space, a stop,
	// or a query delimiter.
	value = `[^` + space + stop + `&]+`
)

// spell returns a pattern for s that tolerates format characters between its
// letters ("xo<ZWSP>xb-").
func spell(s string) string {
	return strings.Join(strings.Split(s, ""), format+"*")
}

// pair returns a pattern for a two-word field name: the words joined by an
// optional underscore or hyphen (access_token, access-token, accessToken),
// with format characters tolerated anywhere. Callers match it case-insensitively.
func pair(a, b string) string {
	return spell(a) + format + `*[_-]?` + format + `*` + spell(b)
}

var (
	// secretPattern matches Slack tokens, bearer credentials and JWTs. Bearer
	// takes a separator and a credential, or, glued to the scheme, a run of
	// at least 7 token characters starting with a letter or digit, so
	// "bearers", "bearer's" and identifiers like "bearer-gateway" survive.
	// Slack tokens and Bearer credentials end at a stop, so compact JSON keeps
	// its next field. Compiled once and never modified.
	secretPattern = regexp.MustCompile(
		spell("xox") + format + `*[a-z]` + format + `*-[^` + space + stop + `]+` +
			`|` + spell("xapp-") + `[^` + space + stop + `]+` +
			`|(?i:` + spell("bearer") + `)(?:` + sep + credential + `|[A-Za-z0-9][\w~+/=.\p{Cf}-]{6,})` +
			`|` + spell("eyJ") + `[\w\p{Cf}-]+\.[\w\p{Cf}-]+\.[\w\p{Cf}-]*`)

	// oauthPattern matches an OAuth token or secret field, as key=value or as
	// JSON "key":"value", plain or with the quotes escaped (\"key\":\"value\").
	// The first group is the key and its punctuation, kept; the second is the
	// value, redacted. After a colon any padding is allowed. After an equals
	// sign it is allowed only when the sign is padded on both sides
	// ("key = value"), so "key= next" reads as an empty value, not as "next".
	oauthPattern = regexp.MustCompile(`((?i:` +
		pair("access", "token") + `|` + pair("refresh", "token") + `|` + pair("id", "token") + `|` + pair("client", "secret") +
		`)\\*["']?(?:[` + blank + `]*:[` + blank + `]*|[` + blank + `]*=|[` + blank + `]+=[` + blank + `]+)\\*["']?)(` + value + `)`)

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
// control character, a colon, or "%20". Field names match in any case, with an
// underscore, a hyphen or nothing between the words (accessToken), and in JSON
// with escaped quotes, as %q prints a token-endpoint body. A Bearer or Slack
// credential, and a field value, end at a quote, comma, semicolon, closing
// brace or backslash, so compact JSON keeps the fields after it; a credential
// that itself contains one of those is cut there. "key= next" is an empty
// value and leaves "next" alone. It is a backstop for free-text audit fields,
// not a license to put secrets in them.
//
// Known limits, all deliberate; TestRedactSecretsKnownLimits pins them:
//   - A value that is a JSON object or array ({"access_token":{...}}) is not
//     redacted past its first character.
//   - A quote escaped inside a value ("ab\"cd") ends the value at the backslash.
//   - JSON that is URL-encoded (%22access_token%22%3A) or HTML-encoded
//     (&quot;access_token&quot;) is not matched.
//   - password fields are not matched: the name is too common to redact by key.
//   - An equals sign padded only after it (client_secret= "x") is read as an
//     empty value followed by text, so the text survives.
//   - A Basic credential under 8 base64 characters survives (it would collide
//     with ordinary words).
//   - A scheme or key written in fullwidth or other compatibility forms (U+FF22
//     for B) is not matched, because that would need NFKC normalization first.
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

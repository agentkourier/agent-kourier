package render

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/agentkourier/agent-kourier/internal/store"
)

const redacted = "[redacted]"

// secretWords are the words that mark a name as one whose value is never shown. A name is split
// into words first, so "apiKey", "api_key" and "x-api-key" match "key" while "monkey" and "author"
// do not.
var secretWords = map[string]bool{
	"pass": true, "password": true, "passwd": true, "pwd": true, "secret": true, "token": true, "key": true,
	"apikey": true, "auth": true, "authorization": true, "credential": true, "credentials": true,
	"cookie": true, "session": true, "jwt": true, "bearer": true, "signature": true, "dsn": true,
}

// secretSuffixes catch a word that runs a secret word on to a prefix, as env var names do:
// PGPASSWORD, MYSECRET. "key" is not among them, or "monkey" would match.
var secretSuffixes = []string{"password", "passwd", "secret", "token", "apikey", "credential", "credentials"}

// isSecretName reports whether any word of name is a secret word, or ends in one of the
// long ones.
func isSecretName(name string) bool {
	for _, w := range nameWords(name) {
		if secretWords[w] || slicesHasSuffix(w, secretSuffixes) {
			return true
		}
	}
	return false
}

func slicesHasSuffix(w string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(w, s) {
			return true
		}
	}
	return false
}

// nameWords splits name into lower-case words at anything that is not a letter or digit, at a
// lower-to-upper step ("apiKey"), at the end of an acronym ("APIKey" is API, Key) and between
// letters and digits.
func nameWords(name string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if i > 0 && len(cur) > 0 {
			p := rs[i-1]
			switch {
			case unicode.IsLower(p) && unicode.IsUpper(r),
				unicode.IsUpper(p) && unicode.IsUpper(r) && i+1 < len(rs) && unicode.IsLower(rs[i+1]),
				unicode.IsDigit(p) != unicode.IsDigit(r):
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

// redactArgs returns v with secret values replaced, in nested maps and lists too: the value of a
// key that isSecretName, and the "value" of a map whose "name" or "key" names a secret, which is
// how Kubernetes writes an env var ({"name": "DB_PASSWORD", "value": "..."}).
func redactArgs(v any) any {
	switch t := v.(type) {
	case map[string]any:
		pair := false
		for k, x := range t {
			if s, ok := x.(string); ok && (strings.EqualFold(k, "name") || strings.EqualFold(k, "key")) && isSecretName(s) {
				pair = true
			}
		}
		out := make(map[string]any, len(t))
		for k, x := range t {
			if isSecretName(k) || (pair && strings.EqualFold(k, "value")) {
				out[k] = redacted
			} else {
				out[k] = redactArgs(x)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = redactArgs(x)
		}
		return out
	}
	return v
}

var (
	// urlUserinfo matches the credentials in "scheme://user:pass@host": everything up to the last "@"
	// of the word, so an "@" or a "/" in the password does not end it early. A path with an "@" in it
	// is redacted too, which loses a little and leaks nothing.
	urlUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^\s"]*@`)
	// plainUserinfo is "user:pass@host" with no scheme, up to the last "@" of the word. The
	// password may not start with "/", which leaves "scheme://" to urlUserinfo.
	plainUserinfo = regexp.MustCompile(`[A-Za-z0-9_.\-]+:[^\s"'/][^\s"']*@`)
	// pemKey matches a private key from its BEGIN line to the end of the text, END line or not.
	pemKey = regexp.MustCompile(`-----BEGIN[A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----[\s\S]*`)
	// pair matches "name=value", "name: value" and, inside JSON, name":"value" (with the quotes
	// escaped when the JSON is itself in a string). The value is a quoted string, or a run up to
	// white space, after an optional scheme word ("Token abc"). Groups: the name, the separator, the value.
	pair = regexp.MustCompile(`(?i)([a-z0-9_.\-]+)((?:\\?["'])?\s*[=:]\s*)` +
		`((?:(?:bearer|basic|token|digest)[ \t]+)?(?:\\"(?:[^\\"]|\\[^"])*\\"|"(?:[^"\\]|\\.)*"|'[^']*'|[^\s"']\S*))`)
	// nextPair is a cookie-style continuation: "; name=value".
	nextPair = regexp.MustCompile(`^[ \t]*[A-Za-z0-9_.\-]+=\S*`)
	// bareRun is a run of the characters of an AWS secret access key.
	bareRun = regexp.MustCompile(`[A-Za-z0-9/+=]+`)
)

// redactSummary takes secrets out of display text: URL credentials, then what store.RedactSecrets
// knows for a token, a private key, the value of any "name=value" whose name isSecretName, and
// an AWS secret access key by its shape. It runs before the text is cut, so no prefix of a
// secret is shown.
func redactSummary(s string) string {
	s = urlUserinfo.ReplaceAllString(s, "${1}"+redacted+"@")
	s = plainUserinfo.ReplaceAllString(s, redacted+"@")
	s = store.RedactSecrets(s)
	s = pemKey.ReplaceAllString(s, redacted)
	s = redactPairs(s)
	return bareRun.ReplaceAllStringFunc(s, func(run string) string {
		if len(run) == 40 && strings.Trim(run, "0123456789abcdefABCDEF") != "" { // not a SHA-1
			return redacted
		}
		return run
	})
}

// redactPairs replaces the value of each pair whose name is a secret name. A pair whose name is not
// is skipped, but its value is searched too, so "x=password=y" gives nothing away.
func redactPairs(s string) string {
	var out strings.Builder
	pos := 0
	for pos < len(s) {
		loc := pair.FindStringSubmatchIndex(s[pos:])
		if loc == nil {
			break
		}
		name, valStart, end := s[pos+loc[2]:pos+loc[3]], pos+loc[6], pos+loc[1]
		out.WriteString(s[pos:valStart])
		if !isSecretName(name) {
			pos = valStart
			continue
		}
		for s[end-1] == ';' && nextPair.MatchString(s[end:]) { // a cookie's other pairs
			end += len(nextPair.FindString(s[end:]))
		}
		out.WriteString(redacted)
		pos = end
	}
	out.WriteString(s[min(pos, len(s)):])
	return out.String()
}

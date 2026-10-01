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

// secretJoined are names that are secret as a whole, however they are cased or divided:
// "connectionString" and "connection_string" are no secret word apiece.
var secretJoined = map[string]bool{"connectionstring": true, "sessionid": true}

// isSecretName reports whether any word of name is a secret word, or ends in one of the
// long ones, or the name as a whole is one of secretJoined.
func isSecretName(name string) bool {
	words := nameWords(name)
	if secretJoined[strings.Join(words, "")] {
		return true
	}
	for _, w := range words {
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

// maxRedactBytes bounds each key and string that is redacted. Longer text is cut at the last white
// space within it (see boundWords), so that the work is bounded whatever the agent sends, and
// ToolLine shows at most a few hundred characters of it anyway.
const maxRedactBytes = 4 << 10

// boundWords returns s cut to at most maxRedactBytes at a white-space boundary and marked with
// Truncated, or s if it is short enough. Every word is whole or gone, so no part of a secret that
// straddles the limit survives; a run longer than the limit with no white space goes entirely.
func boundWords(s string) string {
	if len(s) <= maxRedactBytes {
		return s
	}
	return s[:max(strings.LastIndexFunc(s[:maxRedactBytes+1], unicode.IsSpace), 0)] + Truncated
}

// redactArgs returns v with secret values replaced, in nested maps and lists too: the value of a
// key that isSecretName, the "value" of a map whose "name" or "key" names a secret (which is how
// Kubernetes writes an env var, {"name": "DB_PASSWORD", "value": "..."}), the second of a
// two-element list whose first is a secret name (a header as ["X-Api-Key", "..."]), and what
// redactSummary finds in each string. Keys and strings are cleaned first, so that zero-width and
// control characters cannot split a name, and bounded (see maxRedactBytes). A string is redacted
// as it is, with its own line breaks, and not as the JSON that shows it: there a newline is a
// backslash and an "n", which would join the next name. Letters that only look like Latin ones
// (Cyrillic "a" in "password") are not handled.
func redactArgs(v any) any {
	switch t := v.(type) {
	case string:
		return redactSummary(boundWords(clean(t, true)))
	case map[string]any:
		pair := false
		for k, x := range t {
			if s, ok := x.(string); ok && (strings.EqualFold(k, "name") || strings.EqualFold(k, "key")) && isSecretName(clean(s, false)) {
				pair = true
			}
		}
		out := make(map[string]any, len(t))
		for k, x := range t {
			k = boundWords(clean(k, false))
			if isSecretName(k) || (pair && strings.EqualFold(k, "value")) {
				out[k] = redacted
			} else {
				out[k] = redactArgs(x)
			}
		}
		return out
	case []any:
		if name, ok := firstString(t); ok && len(t) == 2 && isSecretName(clean(name, false)) {
			return []any{redactArgs(name), redacted}
		}
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = redactArgs(x)
		}
		return out
	}
	return v
}

func firstString(l []any) (string, bool) {
	if len(l) == 0 {
		return "", false
	}
	s, ok := l[0].(string)
	return s, ok
}

var (
	// urlUserinfo matches the credentials in "scheme://user:pass@host": everything up to the last "@"
	// of the word, so an "@" or a "/" in the password does not end it early. A path with an "@" in it
	// is redacted too, which loses a little and leaks nothing.
	urlUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^\s"]*@`)
	// urlSpaced is the same for a password that holds a space ("https://u:pa ss@h"): userinfo with a
	// colon, up to the last "@" of the same quoted value or line, so that it cannot reach the next
	// argument. It takes more than the userinfo when text after a "host:port" holds an "@", which
	// loses a little and leaks nothing.
	urlSpaced = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^\s"/@]*:[^"\n]*@`)
	// plainUserinfo is "user:pass@host" with no scheme, up to the last "@" of the word. The
	// password may not start with "/", which leaves "scheme://" to urlUserinfo.
	plainUserinfo = regexp.MustCompile(`[A-Za-z0-9_.\-]+:[^\s"'/][^\s"']*@`)
	// pemKey matches a private key from its BEGIN line to the end of the text, END line or not.
	pemKey = regexp.MustCompile(`-----BEGIN[A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----[\s\S]*`)
	// pair finds "name=value", "name: value", "name => value" and, inside JSON, name":"value"
	// (with the quotes escaped when the JSON is itself in a string), by the name and its
	// separator only. The value is found by valueEnd, so that a long run that is no secret is
	// scanned once. Group 1 is the name; the match ends where the value starts.
	pair = regexp.MustCompile(`(?i)([a-z0-9_.\-]+)(?:\\?["'])?\s*(?:=>|[=:])\s*`)
	// secretFlag finds a command-line flag and the white space before its value: "--token abc".
	// Group 1 is the flag's name.
	secretFlag = regexp.MustCompile(`(?i)(?:^|[ \t\n])--?([a-z][a-z0-9_.\-]*)[ \t]+`)
	// userFlag is a flag that takes "user:password": curl's -u and --user.
	userFlag = regexp.MustCompile(`(?i)((?:^|[ \t\n])(?:-u|--user|--proxy-user)[ \t]+[^\s:]*:)\S+`)
	// schemeWord is an authorization scheme before a value: "Token abc".
	schemeWord = regexp.MustCompile(`(?i)^(?:bearer|basic|token|digest)[ \t]+`)
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
	s = urlSpaced.ReplaceAllString(s, "${1}"+redacted+"@")
	s = plainUserinfo.ReplaceAllString(s, redacted+"@")
	s = store.RedactSecrets(s)
	s = pemKey.ReplaceAllString(s, redacted)
	s = redactNamed(s, pair, true)
	s = redactNamed(s, secretFlag, false)
	s = userFlag.ReplaceAllString(s, "${1}"+redacted)
	return bareRun.ReplaceAllStringFunc(s, func(run string) string {
		if len(run) == 40 && strings.Trim(run, "0123456789abcdefABCDEF") != "" { // not a SHA-1
			return redacted
		}
		return run
	})
}

// redactNamed replaces the value after each match of re, a name and what separates it from its
// value, whose name isSecretName. A match whose name is not is skipped, but the search goes on
// from the end of the match, so "x=password=y" gives nothing away. The text is read once: matches
// come from one pass, and a value is scanned only for a secret name. With cookies, the other
// pairs of a "; name=value" list go with it; without, the value may not be another flag.
func redactNamed(s string, re *regexp.Regexp, cookies bool) string {
	var out strings.Builder
	pos := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		valStart := loc[1]
		if loc[0] < pos || valStart >= len(s) || !isSecretName(s[loc[2]:loc[3]]) || (!cookies && s[valStart] == '-') {
			continue // inside a value already taken, no value, no secret, or a flag
		}
		end := valueEnd(s, valStart)
		for cookies && end > valStart && s[end-1] == ';' && nextPair.MatchString(s[end:]) { // a cookie's other pairs
			end += len(nextPair.FindString(s[end:]))
		}
		out.WriteString(s[pos:valStart])
		out.WriteString(redacted)
		pos = end
	}
	out.WriteString(s[pos:])
	return out.String()
}

// valueEnd returns where the value that starts at i ends: after a scheme word if there is one, a
// quoted string (JSON's, the same with the quotes escaped, or single-quoted), or a run up to white
// space. A quote that never closes takes the rest of the text, so that a secret is not left out of
// a malformed one.
func valueEnd(s string, i int) int {
	if loc := schemeWord.FindStringIndex(s[i:]); loc != nil {
		i += loc[1]
	}
	switch {
	case strings.HasPrefix(s[i:], redacted): // taken out already, by the pass over the string itself
		return i + len(redacted)
	case strings.HasPrefix(s[i:], `\"`):
		for j := i + 2; j < len(s); j++ {
			if s[j] == '"' {
				return j + 1
			}
			if s[j] == '\\' {
				if j++; j < len(s) && s[j] == '"' {
					return j + 1
				}
			}
		}
		return len(s)
	case strings.HasPrefix(s[i:], `"`):
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case '"':
				return j + 1
			}
		}
		return len(s)
	case strings.HasPrefix(s[i:], `'`):
		if k := strings.IndexByte(s[i+1:], '\''); k >= 0 {
			return i + 1 + k + 1
		}
		return len(s)
	}
	for i < len(s) && !strings.ContainsRune(" \t\n\f\r", rune(s[i])) {
		i++
	}
	return i
}

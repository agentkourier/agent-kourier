package redact

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Redacted is what takes the place of a secret value.
const Redacted = redacted

// Truncated ends text that Bound cut. It holds nothing that needs escaping, in any style.
const Truncated = "… (truncated)"

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

// SecretName reports whether any word of name is a secret word, or ends in one of the long ones,
// or the name as a whole is one of secretJoined. It normalizes name first, so that an invisible
// character cannot split "password".
func SecretName(name string) bool {
	words := nameWords(Normalize(name))
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

// MaxBytes bounds each key and string that is redacted. Longer text is cut at the last white
// space within it (see Bound), and ToolLine shows at most a few hundred characters of it anyway.
// The work is bounded whatever the agent sends because text over rawCap is cut before any pattern
// reads it (see capRaw).
const MaxBytes = 4 << 10

// rawCap is how much of a text the token layer reads as it came: four times MaxBytes, which Bound shows at most a
// quarter of, so what is cut off is never shown. A text longer than that is cut at a character, and what is kept
// is marked with Truncated.
const rawCap = 4 * MaxBytes

// capRaw returns s cut to rawCap bytes at a character boundary, and whether it was cut.
func capRaw(s string) (string, bool) {
	if len(s) <= rawCap {
		return s, false
	}
	cut := rawCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// Bound returns s cut to at most MaxBytes at a white-space boundary and marked with
// Truncated, or s if it is short enough. Every word is whole or gone, so no part of a secret that
// straddles the limit survives; a run longer than the limit with no white space goes entirely.
func Bound(s string) string {
	if len(s) <= MaxBytes {
		return s
	}
	return s[:max(strings.LastIndexFunc(s[:MaxBytes+1], unicode.IsSpace), 0)] + Truncated
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
	// namedPair finds "name=value", "name: value", "name => value" and, inside JSON, name":"value"
	// (with the quotes escaped when the JSON is itself in a string), by the name and its
	// separator only. The value is found by valueEnd, so that a long run that is no secret is
	// scanned once. Group 1 is the name; the match ends where the value starts.
	namedPair = regexp.MustCompile(`(?i)([a-z0-9_.\-]+)(?:\\?["'])?\s*(?:=>|[=:])\s*`)
	// secretFlag finds a command-line flag and the white space before its value: "--token abc".
	// Group 1 is the flag's name.
	secretFlag = regexp.MustCompile(`(?i)(?:^|[ \t\n])--?([a-z][a-z0-9_.\-]*)[ \t]+`)
	// userFlag is a flag that takes "user:password": curl's -u and --user.
	// The value follows after white space or "=", or, for -u, directly: "-uadmin:pw".
	userFlag = regexp.MustCompile(`(?i)((?:^|[ \t\n])(?:-u[ \t]*|--user[ \t=]+|--proxy-user[ \t=]+)[^\s:]*:)\S+`)
	// schemeWord is an authorization scheme before a value: "Token abc".
	schemeWord = regexp.MustCompile(`(?i)^(?:bearer|basic|token|digest)[ \t]+`)
	// nextPair is a cookie-style continuation: "; name=value".
	nextPair = regexp.MustCompile(`^[ \t]*[A-Za-z0-9_.\-]+=\S*`)
	// bareRun is a run of the characters of an AWS secret access key.
	bareRun = regexp.MustCompile(`[A-Za-z0-9/+=]+`)
)

// FreeText takes secrets out of free text: a command line, a connection string, a log line, an error, an
// agent's question, a thread reply. It returns normalized text (see Normalize): the invisible characters are
// deleted and the line breaks written "\n" first, so that none can split a name or a scheme, and no caller can
// get the order of cleaning and redacting wrong. It then cuts the text to MaxBytes at a white-space boundary (see
// Bound), and redacts URL credentials, what secrets knows for a token, a private key, the value of any
// "name=value" or "--flag value" whose name is a SecretName, a "-u user:password" flag, and an AWS secret access
// key by its shape. The order is normalize, bound, match, and it runs before any cut the caller makes, so no
// prefix of a secret is shown. Line breaks are kept: they end a value, and the caller decides what to do with
// them. IdentifierText is the entry for a short value that must be one line.
//
// The token layer also runs once on the text as it came, before it is normalized: deleting an invisible
// character can glue a word to a scheme ("token", a zero-width space, "Bearer") or a credential to it, and the
// glued rules, which need a word boundary and a credential that decodes, refuse what the gap hid. Its patterns
// tolerate the gaps, so they take these in the raw text.
func FreeText(s string) string {
	s, cut := capRaw(s)
	s = Normalize(secrets(s))
	if cut { // the end of the kept text may be a secret the cut went through, and invisible padding may have brought it forward
		s = dropTail(s) + Truncated
	}
	return matchFree(Bound(s))
}

// FreeTextCut is FreeText for a caller that shows only the start of the text, such as the alert fields of a prompt.
// Where FreeText cuts at white space, so that an unbroken run longer than MaxBytes goes whole, FreeTextCut cuts at
// a character boundary at limit bytes, so that a field with no white space in it (Japanese, a long link) keeps its
// start, and marks the cut with Truncated. The cut comes after normalizing and before matching, as Bound's does.
//
// A secret that straddles the cut is cut with it, and what is left of it may match no rule (a URL password without
// its "@", a bare key short of its 40 characters). The matched text can also be much shorter than the cut text, so
// what the caller shows of it may reach back to the end of the cut. So when the text was cut, the last run of the
// matched text goes before the mark: back to the last white space in its final tailDrop bytes, or if there is none
// the final tailDrop bytes. A secret that holds white space and straddles the cut (a URL password with a space in
// it) loses only its last word to this, and shows the rest of itself, if no other rule has taken it.
func FreeTextCut(s string, limit int) string {
	s = Normalize(secrets(s))
	if len(s) <= limit {
		return matchFree(s)
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return dropTail(matchFree(s[:cut])) + Truncated
}

// tailDrop is how much of the end of a cut text dropTail takes at most.
const tailDrop = 1 << 10

// dropTail returns s without its last run: cut back to the last white space in its final tailDrop bytes, or, if
// there is none, without those bytes, at a character boundary (and with nothing left of a text that is shorter).
func dropTail(s string) string {
	from := max(len(s)-tailDrop, 0)
	for from < len(s) && !utf8.RuneStart(s[from]) {
		from++
	}
	if i := strings.LastIndexFunc(s[from:], unicode.IsSpace); i >= 0 {
		return s[:from+i]
	}
	return s[:from]
}

// matchFree is the matching half of FreeText, on text that is normalized and cut.
func matchFree(s string) string {
	s = urlUserinfo.ReplaceAllString(s, "${1}"+redacted+"@")
	s = urlSpaced.ReplaceAllString(s, "${1}"+redacted+"@")
	s = plainUserinfo.ReplaceAllString(s, redacted+"@")
	s = secrets(s)
	s = pemKey.ReplaceAllString(s, redacted)
	s = redactNamed(s, namedPair, true)
	s = redactNamed(s, secretFlag, false)
	s = userFlag.ReplaceAllString(s, "${1}"+redacted)
	return bareRun.ReplaceAllStringFunc(s, func(run string) string {
		if len(run) == 40 && strings.Trim(run, "0123456789abcdefABCDEF") != "" { // not a SHA-1
			return redacted
		}
		return run
	})
}

// IdentifierText takes secrets out of an identifier: a short value that is shown or stored on one line, such as
// an audit field, a user's or a Binding's name. It returns normalized text (see
// Normalize) with every run of white space, a line break among it, written as one space and the ends trimmed,
// and redacts only what has a shape of its own: a Slack token, a Bearer or Basic credential, a JWT, an OAuth
// field. It does not read an identifier as a "name=value" or a flag: "secret:ns/name" is the identity of a
// Secret, and a task ID may be 40 characters long. It does not bound the text, but for the work of reading it
// (a text over rawCap is cut and marked); the caller caps an identifier after it is redacted, so that no
// token is cut short of its pattern. Like FreeText, it runs the token layer on
// the text as it came as well, before the invisible characters are deleted.
func IdentifierText(s string) string {
	s, cut := capRaw(s)
	s = secrets(strings.Join(strings.Fields(Normalize(secrets(s))), " "))
	if cut {
		s += Truncated
	}
	return s
}

// redactNamed replaces the value after each match of re, a name and what separates it from its
// value, whose name is a SecretName. A match whose name is not is skipped, but the search goes on
// from the end of the match, so "x=password=y" gives nothing away. The text is read once: matches
// come from one pass, and a value is scanned only for a secret name. With cookies, the other
// pairs of a "; name=value" list go with it; without, the value may not be another flag.
func redactNamed(s string, re *regexp.Regexp, cookies bool) string {
	var out strings.Builder
	pos := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		valStart := loc[1]
		if loc[0] < pos || valStart >= len(s) || !SecretName(s[loc[2]:loc[3]]) || (!cookies && s[valStart] == '-') {
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

// endsValue reports whether a value could end at i: at the end of the text or before a delimiter. The
// marker of a value taken out is accepted only so, so an agent cannot stop redaction by writing it:
// "password=[redacted]hunter2" is all value.
func endsValue(s string, i int) bool {
	return i >= len(s) || strings.ContainsRune(" \t\n\f\r\"',;}]\\", rune(s[i]))
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
	case strings.HasPrefix(s[i:], redacted) && endsValue(s, i+len(redacted)):
		return i + len(redacted) // taken out already, by the pass over the string itself
	case strings.HasPrefix(s[i:], `\"`): // JSON inside a JSON string: its quote is a backslash and a quote
		for j := i + 2; j < len(s); j++ {
			switch s[j] {
			case '\\': // a run of backslashes: one before a quote closes, more escape it
				k := j
				for k < len(s) && s[k] == '\\' {
					k++
				}
				if k < len(s) && s[k] == '"' && k-j == 1 {
					return k + 1
				}
				j = k // past the run, and the quote it escapes
			case '"':
				return j + 1
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

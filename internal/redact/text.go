package redact

import (
	"math"
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
func capRaw(s string) (string, bool) { return capAt(s, rawCap) }

// capAt returns s cut to n bytes at a character boundary, and whether it was cut.
func capAt(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	return s[:runeBoundary(s, n)], true
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

// hspace is the white space that does not end a line, as a class body: tab and every Unicode space (a no-break space
// among them, which Go's \s does not match). The patterns of matchFree read text that Normalize has made so that
// "\n" is the only line break in it.
const hspace = `\t\p{Z}`

// continuations are the marks that continue a command on the next line when one ends a line: the backslash of a POSIX shell,
// the backtick of PowerShell and the caret of cmd, as a class body.
const continuations = "\\\\`^"

var (
	// urlUserinfo matches the credentials in "scheme://user:pass@host": everything up to the last "@"
	// of the word, so an "@" or a "/" in the password does not end it early. A path with an "@" in it
	// is redacted too, which loses a little and leaks nothing.
	urlUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^` + space + `"]*@`)
	// urlSpaced is the same for a password that holds a space ("https://u:pa ss@h"): userinfo with a
	// colon, up to the last "@" of the same quoted value or line, so that it cannot reach the next
	// argument. It takes more than the userinfo when text after a "host:port" holds an "@", which
	// loses a little and leaks nothing.
	urlSpaced = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^` + space + `"/@]*:[^"\n]*@`)
	// plainUserinfo is "user:pass@host" with no scheme, up to the last "@" of the word. The
	// password may not start with "/", which leaves "scheme://" to urlUserinfo.
	plainUserinfo = regexp.MustCompile(`[A-Za-z0-9_.\-]+:[^` + space + `"'/][^` + space + `"']*@`)
	// pemKey matches a private key from its BEGIN line to the end of the text, END line or not.
	pemKey = regexp.MustCompile(`-----BEGIN[A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----[\s\S]*`)
	// namedPair finds "name=value", "name: value", "name => value" and, inside JSON, name":"value"
	// (with the quotes escaped when the JSON is itself in a string), by the name and its
	// separator only, with white space of any width, a no-break space among it, around the separator, and Markdown
	// emphasis ("**Password:** x", "**Password**: x", "*Token:* x": up to three of "*", "_" or "~" after the name, and
	// after the separator if white space follows them, so that a value that begins with one is whole). The value is
	// found by valueEnd, so that a long run that is no secret is scanned once. Group 1 is the name; the match ends
	// where the value starts.
	namedPair = regexp.MustCompile(`(?i)([a-z0-9_.\-]+)(?:\\?["'])?[*_~]{0,3}[` + space + `]*(?:=>|[=:])(?:[*_~]{1,3}[` + space + `]+|[` + space + `]*)`)
	// secretFlag finds a command-line flag and the white space before its value: "--token abc". That white space holds
	// no line break: a value on the next line is the flag's only after a trailing backslash (see continued). Group 1 is
	// the flag's name.
	secretFlag = regexp.MustCompile(`(?i)(?:^|[` + space + `])--?([a-z][a-z0-9_.\-]*)[` + hspace + `]+`)
	// userFlag is a flag that takes "user:password": curl's -u and --user. The value follows after white space or
	// "=", or, for -u, directly: "-uadmin:pw". It may begin after a continuation mark (see continuations) and its line
	// break.
	userFlag = regexp.MustCompile(`(?i)((?:^|[` + space + `])(?:-u[` + hspace + `]*|--user[` + hspace + `=]+|--proxy-user[` + hspace + `=]+)` +
		`(?:[` + continuations + `][` + hspace + `]*(?:\n[` + hspace + `]*)?)?[^\s:]*:)[^` + space + `]+`)
	// envPair finds a Kubernetes env entry, "name: DB_PASSWORD" and its "value:" on a later line, past up to eight lines of the
	// entry (comments, blank lines and other keys, "description: x"; a line that begins a list entry, "- name: HOST", or
	// an object, "{" or "}" of JSON, is the next entry's and ends the match), or after a comma on the same line: the entry's name says the value is a secret.
	// The entry may be JSON that is itself in a string (name\":\"DB_PASSWORD\"). Group 1 is the entry's name, and the match
	// ends where the value starts. The "name" is not the end of a longer word ("hostname:"). The lines are lazy, so the
	// first "value:" is the entry's.
	envPair = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_.\-])\\?["']?name\\?["']?[` + hspace + `]*:[` + hspace + `]*\\?["']?([^\s"',}\\]+)\\?["']?(?:[` + hspace + `]+#[^\n]*)?` +
		`(?:[` + hspace + `]*,?[` + hspace + `]*\n(?:[` + hspace + `]*(?:#[^\n]*|[^-#{}` + space + `][^\n]*)?\n){0,8}?|[` + hspace + `]*,)[` + hspace + `]*\\?["']?value\\?["']?[` + hspace + `]*:[` + hspace + `]*`)
	// blockHeader is what starts a YAML block scalar, whose lines are the value: "|" and ">", with the chomping and
	// indentation indicators that may follow ("|-", ">+", "|2").
	blockHeader = regexp.MustCompile(`^[|>](?:[1-9][+-]?|[+-][1-9]?)?$`)
	// schemeWord is an authorization scheme before a value: "Token abc".
	schemeWord = regexp.MustCompile(`(?i)^(?:bearer|basic|token|digest)[` + hspace + `]+`)
	// listMarker is the first word of a list item: "-", "*", "+" or a number and a full stop or a bracket.
	listMarker = regexp.MustCompile(`^(?:[-*+]|[0-9]+[.)])$`)
	// nextPair is a cookie-style continuation: "; name=value".
	nextPair = regexp.MustCompile(`^[` + hspace + `]*[A-Za-z0-9_.\-]+=[^` + space + `]*`)
	// bareRun is a run of the characters of an AWS secret access key.
	bareRun = regexp.MustCompile(`[A-Za-z0-9/+=]+`)
)

// FreeText takes secrets out of free text: a command line, a connection string, a log line, an error, an
// agent's question, a thread reply. It returns normalized text (see Normalize): the invisible characters are
// deleted and the line breaks written "\n" first, so that none can split a name or a scheme, and no caller can
// get the order of cleaning and redacting wrong. It then cuts the text to MaxBytes at a white-space boundary (see
// Bound), and redacts URL credentials, what secrets knows for a token, a private key, the value of any
// "name=value" or "--flag value" whose name is a SecretName (when that value is itself a key, the key's own value, as in
// "authorization:\n  credentials: x"), a "-u user:password" flag, a Kubernetes env entry's value (a "name: DB_PASSWORD"
// and its "value:", past comment lines), a YAML block scalar under such a name (with a tag or an anchor before its
// header, or without), a value that a trailing continuation mark (a backslash, a backtick, a caret) continues on the next
// line, and an AWS secret access key by its shape. The order is normalize, bound, match, and it runs before any cut the
// caller makes, so no prefix of a secret is shown. Line breaks are kept: they end a value, and the caller decides what to
// do with them. FreeTextLine is the entry for free text that is shown on one line, and IdentifierText for a short value
// that must be one line and is no free text.
//
// Known limits, all deliberate (TestFreeTextKnownLimits pins them): the rules read a value as a run up to white space, and
// a few shapes of text have none. A URL's password that a line break cuts is taken by FreeTextLine only if no pair on the
// next line takes the "@" first ("https://admin:hunter2\npassword=x@host": the lines are read before they are joined, and
// that is the second read's only way to see the "@"). A list ("password:\n- hunter2") or a mapping of keys that are no
// secret names ("secret:\n  db: hunter2") under a secret name, a fenced block after a name ("password:\n\n```\nx\n```": the
// fence is the value), Terraform's separate "name = ..." and "value = ..." lines, a tag with its value on the next line
// ("password: !!str\n  x"; FreeTextLine leaves it too), a plain scalar that goes on over lines, a short flag
// ("-p hunter2") and a name with no separator ("passwd hunter2") leave the value. A secret written where a nested key
// would be, on the line under a secret name ("token:\n  SECRET : x", "Password:\n    'SECRET': rotated"), is shown by
// FreeText as the key it looks like (FreeTextLine, which joins the lines, takes the unquoted one and shows the quoted one). After a blank line a value is taken
// unless the line reads as prose, a plain word first and more words after it: "password:\n\nletmein now" leaves the word
// in FreeText (FreeTextLine takes it), and "Token:\n\nExpired", one word, is taken. And text that reads as a name and a
// value only once its lines are joined is taken by FreeTextLine as one ("Retry with --token\nThe deploy": "The");
// FreeText keeps the lines and does not. A Markdown legend of a secret word ("**Key**: blue means prod") is taken as a name
// and its value, which loses a little.
//
// The token layer also runs once on the text as it came, before it is normalized: deleting an invisible
// character can glue a word to a scheme ("token", a zero-width space, "Bearer") or a credential to it, and the
// glued rules, which need a word boundary and a credential that decodes, refuse what the gap hid. Its patterns
// tolerate the gaps, so they take these in the raw text.
func FreeText(s string) string { text, _ := freeText(s, false); return text }

// FreeTextWhole is FreeText that also says whether none of s was cut: whole is false when the text was bounded (Bound), or
// when it was longer than the token layer reads (capRaw), and the returned text then ends in a mark of the cut, or in
// what a rule made of that mark. It is the one answer to "was it cut", for a caller that must not show part of a text as
// if it were all of it. The cut cannot be told from the length of s, because the token layer runs first and
// lengthens text ("access_token=x" becomes "access_token=[redacted]"), nor from the end of the result, because a rule
// that takes a value takes the mark with it. FreeText is FreeTextWhole without the answer.
func FreeTextWhole(s string) (text string, whole bool) { return freeText(s, false) }

// FreeTextLine is FreeText for free text that is shown on one line, such as a title, a link or an author's name and ID, and for
// the free text that an audit row keeps on one line: it returns what FreeText does with every run of white space, a
// line break among it, written as one space and the ends trimmed. The text is read twice, as FreeText reads it, with
// its lines, and then joined, so that what a line break hides from a rule is not hidden from it: a flag and its value,
// a user and its password, or a URL's password that the break cuts, is taken as it would be on one line. Redacting
// before joining leaks those, and joining first loses the lines a YAML block scalar needs, so the text is matched both ways.
func FreeTextLine(s string) string { text, _ := freeText(s, true); return text }

func freeText(s string, line bool) (text string, whole bool) {
	s, cut := capRaw(s)
	s = Normalize(rawSecrets(s))
	if cut { // the end of the kept text may be a secret the cut went through, and invisible padding may have brought it forward
		s = dropTail(s) + Truncated
	}
	return match(Bound(s), line), !cut && len(s) <= MaxBytes
}

// FreeTextCut is FreeText for a caller that shows only the start of the text, such as the alert fields of a prompt.
// Where FreeText cuts at white space, so that an unbroken run longer than MaxBytes goes whole, FreeTextCut cuts at
// a character boundary at limit bytes, so that a field with no white space in it (Japanese, a long link) keeps its
// start, and marks the cut with Truncated. The cut comes after normalizing and before matching, as Bound's does. The
// patterns read the text as it came only up to cutRawFactor times limit, at a character, so the work is bounded
// whatever is sent: a text cut there is cut and marked like one cut at limit.
//
// A secret that straddles the cut is cut with it, and what is left of it may match no rule (a URL password without
// its "@", a bare key short of its 40 characters). The matched text can also be much shorter than the cut text, so
// what the caller shows of it may reach back to the end of the cut. So when the text was cut, the last run of the
// matched text goes before the mark: back to the last white space in its final tailDrop bytes, or if there is none
// the final tailDrop bytes. A secret that holds white space and straddles the cut (a URL password with a space in
// it) loses only its last word to this, and shows the rest of itself, if no other rule has taken it.
func FreeTextCut(s string, limit int) string { return freeTextCut(s, limit, false) }

// FreeTextCutLine is FreeTextCut for a field that is shown on one line, as FreeTextLine is FreeText: the text is read
// through its first limit bytes with its lines, and joined and read again.
func FreeTextCutLine(s string, limit int) string { return freeTextCut(s, limit, true) }

// cutRawFactor is how many times limit bytes of a text FreeTextCut reads as it came: the text left after deleting the
// invisible characters is usually the shorter, and a window's worth of it is what the caller can show.
const cutRawFactor = 4

func freeTextCut(s string, limit int, line bool) string {
	limit = max(limit, 0)
	s, cut := capAt(s, rawLimit(limit))
	s = Normalize(rawSecrets(s))
	if len(s) > limit {
		s, cut = s[:runeBoundary(s, limit)], true
	}
	s = match(s, line)
	if cut {
		return dropTail(s) + Truncated
	}
	return s
}

// rawLimit is cutRawFactor times limit, without overflowing.
func rawLimit(limit int) int {
	if limit > math.MaxInt/cutRawFactor {
		return math.MaxInt
	}
	return limit * cutRawFactor
}

// runeBoundary returns the largest index of s that is at most i and starts a character; i must be less than len(s).
func runeBoundary(s string, i int) int {
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
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

// match is matchFree on text that is normalized and cut, and, for text shown on one line, matchFree again on the text
// with its lines joined.
func match(s string, line bool) string {
	s = matchFree(s)
	if line {
		s = matchFree(joinLines(s))
	}
	return s
}

// joinLines writes every run of white space of s, a line break among it, as one space, and trims the ends.
func joinLines(s string) string { return strings.Join(strings.Fields(s), " ") }

// matchFree is the matching half of FreeText, on text that is normalized and cut.
func matchFree(s string) string {
	s = urlUserinfo.ReplaceAllString(s, "${1}"+redacted+"@")
	s = urlSpaced.ReplaceAllString(s, "${1}"+redacted+"@")
	s = plainUserinfo.ReplaceAllString(s, redacted+"@")
	s = secrets(s)
	s = pemKey.ReplaceAllString(s, redacted)
	s = redactNamed(s, namedPair, pairValue)
	s = redactNamed(s, envPair, entryValue)
	s = redactNamed(s, secretFlag, flagValue)
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
// field, a token with a prefix of its own (GitHub's, GitLab's, OpenAI's, Google's, Stripe's), an Azure SAS signature.
// It does not read an identifier as a "name=value" or a flag: "secret:ns/name" is the identity of a
// Secret, and a task ID may be 40 characters long. It does not bound the text, but for the work of reading it
// (a text over rawCap is cut and marked); the caller caps an identifier after it is redacted, so that no
// token is cut short of its pattern. Like FreeText, it runs the token layer on
// the text as it came as well, before the invisible characters are deleted.
func IdentifierText(s string) string {
	s, cut := capRaw(s)
	s = secrets(joinLines(Normalize(rawSecrets(s))))
	if cut {
		s += Truncated
	}
	return s
}

// valueRule is how redactNamed reads what follows a name.
type valueRule int

const (
	// pairValue follows "name=value" and "name: value": a cookie's other pairs go with the value, and a block scalar
	// is the value with all its lines.
	pairValue valueRule = iota
	// flagValue follows "--flag value": a value that is another flag is no value.
	flagValue
	// entryValue follows the "value:" of a Kubernetes env entry: read as a pair is, but it may begin with a dash.
	entryValue
)

// redactNamed replaces the value after each match of re, a name and what separates it from its
// value, whose name is a SecretName. A match whose name is not is skipped, and the search goes on from the end of its
// name, not of the match: a match that reaches over what follows its name (an env entry's lines up to its "value:") must
// not hide a later name, and "x=password=y" gives nothing away. A value that was taken is not read again, and a value is
// scanned only for a secret name (what a skipped match reached over is read again from its name on, a few lines at most). The value ends at white space or at the end of its quotes,
// and see valueRule, startOfPair, continued, tagged and blockScalarEnd for the rest.
func redactNamed(s string, re *regexp.Regexp, rule valueRule) string {
	var out strings.Builder
	pos, from := 0, 0
	for from < len(s) {
		loc := re.FindStringSubmatchIndex(s[from:])
		if loc == nil {
			break
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += from
			}
		}
		valStart := loc[1]
		if !SecretName(s[loc[2]:loc[3]]) {
			from = loc[3]
			continue
		}
		from = loc[1]
		if valStart >= len(s) || (rule == flagValue && s[valStart] == '-') {
			continue // no value, or a flag
		}
		through := 0
		if rule == pairValue {
			var skip bool
			if valStart, through, skip = startOfPair(s, loc[0], valStart); skip {
				continue
			}
		}
		end := valueEnd(s, valStart)
		if end == valStart { // white space where the value should start: a line break after a flag's own, for one
			continue
		}
		end = max(continued(s, valStart, end, rule), through)
		if rule != flagValue {
			header, headerEnd := tagged(s, valStart, end)
			end = max(end, blockScalarEnd(s, header, headerEnd))
		}
		for rule != flagValue && end > valStart && s[end-1] == ';' && nextPair.MatchString(s[end:]) { // a cookie's other pairs
			end += len(nextPair.FindString(s[end:]))
		}
		out.WriteString(s[pos:valStart])
		out.WriteString(redacted)
		pos, from = end, end
	}
	out.WriteString(s[pos:])
	return out.String()
}

// valueHolders are the keys that hold the value of the key above them, the secret itself: "apiKey:", then "value:" and
// the secret on the next line. Any other nested key is a label or a field of its own, and a secret key's value is read
// from the nested key that is a secret name.
var valueHolders = map[string]bool{"value": true}

// startOfPair says where the value of the pair that begins at pairStart and whose value starts at start (the white
// space after its separator is behind start, and may hold line breaks) is to be read from, how far it reaches at least
// (through; 0 for as far as its first run), and whether the match is to be skipped. The value of a pair is read as a run
// up to white space, so when it is itself a key, "credentials:" in "authorization:\n  credentials: x", the run is the key
// and the secret that follows it would be shown, and the nested match, which the search would find next, skipped as
// inside a value already taken. So:
//
//   - the match is skipped when what the value starts with (past a "{", a "[", a quote, or white space) is a key (see
//     nestedKey) that is a SecretName and has something after it: the search goes on to that key, which takes its own value;
//   - when it is a valueHolder ("value:"), the value starts after it;
//   - when it is another key, on a line of its own under the name ("secret:\n  name: db"), the match is skipped too: that
//     is a field of the object under the name, not its value, unless it is a compact flow mapping, one run, which the name
//     takes whole ("secret:\n  {\"db\":\"x\"}");
//   - when the line under the name begins with a secret pair that nestedKey does not read ("password : x", "$env:APIKEY =
//     x"), the value is that whole pair, its value too: it is what the name above takes, and nothing of it is shown;
//   - otherwise the value is read from start, as it always was: "password: abc:def", "password: key=x" and
//     "password: Password123" and a line of "=======" under it are all value.
//
// A value that starts after a blank line is no value either unless it is indented more than the name's line, or its line
// does not read as prose, which is a plain word first (or a list item of one): "Token:\n\nPlease rotate it" is a name and a
// new paragraph, "password:\n\n  hunter2" a key and a value of YAML, and "Password:\n\nhunter2x" a label and a secret on a
// line of its own, which is shown as it would be without the blank line. A one-word line is a value ("Token:\n\nExpired"
// is taken: a secret may be a plain word).
func startOfPair(s string, pairStart, start int) (valueStart, through int, skip bool) {
	gap := s[pairStart:start]
	if strings.Count(gap, "\n") >= 2 && lineIndent(s, start) <= lineIndent(s, pairStart) && !oneWordLine(s, start) && proseLine(s, start) {
		return start, 0, true
	}
	crossed := strings.Contains(gap, "\n")
	for range 4 { // a value holder under a value holder is no deeper than a few levels
		name, valueAt, ok := nestedKey(s, start, crossed)
		if !ok && crossed { // not a key: a secret pair that nestedKey does not read is taken whole
			return start, secretPairThrough(s, start), false
		}
		if !ok || valueAt >= len(s) { // not a key, or nothing after it: the value is as it is
			return start, 0, false
		}
		switch {
		case SecretName(name):
			return start, 0, true
		case valueHolders[strings.ToLower(name)]:
			start = valueAt
		case crossed && !compactFlow(s, start):
			return start, 0, true
		default:
			return start, 0, false
		}
	}
	return start, 0, false
}

// nestedKey reads a key at the start of the value that begins at start, past any "{", "[", quotes, backslashes and white
// space, and returns its name and where what follows it begins. It reads only what has a key's shape, because a secret
// is the value of the name above it however much it looks like a key ("password: Password123" and a line of "=====",
// "password: key=x", "password: 'a: b'", "password: s:ecret"): a quoted key, which is closed by its quote before the
// separator, with ":" or "=>" ("token": x, 'token' => x), or an unquoted one, a name and ":" and white space (YAML's ": ").
// An unquoted key is never followed by "=" or "=>" or a glued ":", and has no white space before its ":" unless it begins
// a line of its own under the name above it (ownLine: "basic_auth:\n  password : x"); a quoted key may have some
// between its closing quote and its separator anywhere.
func nestedKey(s string, start int, ownLine bool) (name string, valueStart int, ok bool) {
	i, quoted := start, false
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == '"' || r == '\'' {
			quoted = true
		} else if r != '{' && r != '[' && r != '\\' && !isSpace(r) {
			break
		}
		i += n
	}
	j := i
	for j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9' || s[j] == '_' || s[j] == '.' || s[j] == '-') {
		j++
	}
	if j == i {
		return "", 0, false
	}
	name, k := s[i:j], j
	if quoted {
		if k < len(s) && s[k] == '\\' {
			k++
		}
		if k >= len(s) || (s[k] != '"' && s[k] != '\'') {
			return "", 0, false // a quoted scalar, not a key: the quote does not close before the separator
		}
		k = skipHSpace(s, k+1)
		switch {
		case strings.HasPrefix(s[k:], "=>"):
			k += 2
		case strings.HasPrefix(s[k:], ":"):
			k++
		default:
			return "", 0, false
		}
		return name, skipSpace(s, k), true
	}
	if ownLine {
		k = skipHSpace(s, k)
	}
	if !strings.HasPrefix(s[k:], ":") || k+1 >= len(s) {
		return "", 0, false
	}
	r, _ := utf8.DecodeRuneInString(s[k+1:])
	if !isSpace(r) {
		return "", 0, false
	}
	return name, skipSpace(s, k+1), true
}

// secretPairThrough returns where the value of the pair whose name is a SecretName and begins in the first run of the
// line that starts at start ends (what valueEnd would take as the value is a run, and the pair's name is in it), or 0 if
// there is no such pair. The pair is not a key to nestedKey ("password : x", "$env:APIKEY = x", "Password123 = rotated"),
// and its name, and the value above it, are taken with it: nothing of the line is left to show a secret.
func secretPairThrough(s string, start int) int {
	end := valueEnd(s, start)
	lineEnd := len(s)
	if k := strings.IndexByte(s[start:], '\n'); k >= 0 {
		lineEnd = start + k
	}
	last := 0
	for from := start; from < lineEnd; { // the pairs whose names are in the first run, however many; the work ends with it
		m := namedPair.FindStringSubmatchIndex(s[from:lineEnd])
		if m == nil || from+m[2] >= end {
			break
		}
		if SecretName(s[from+m[2]:from+m[3]]) && from+m[1] < len(s) {
			last = valueEnd(s, from+m[1])
		}
		from += m[1]
	}
	return last
}

// compactFlow reports whether the value that starts at start is a flow collection with no white space in it ({"db":"x"}),
// or the start of one that is cut ({"db":"x"), or one that a comma or a semicolon follows: one run, which the name above
// takes whole. A run that ends before its line does and does not end in a bracket is no collection (spaced JSON, an
// env entry: {"name": "x", ...), and its first key is read as a key.
func compactFlow(s string, start int) bool {
	if s[start] != '{' && s[start] != '[' {
		return false
	}
	end := valueEnd(s, start)
	if end <= start {
		return false
	}
	if end == len(s) || s[end] == '\n' { // the run is the rest of its line: {"db":"x"}, and one that is cut, {"db":"x"
		return true
	}
	run := strings.TrimRight(s[start:end], ",;") // a flow collection in a list or a mapping, {"db":"x"},
	return strings.HasSuffix(run, "}") || strings.HasSuffix(run, "]")
}

// proseLine reports whether the line of s that begins at i reads as prose: its first word, after a list marker ("- ",
// "* ", "1. "), is a plain word with at most a leading capital, as a sentence begins and a secret ("hunter2x",
// "S3cr3t-Pa55", a token) does not.
func proseLine(s string, i int) bool {
	end := len(s)
	if k := strings.IndexByte(s[i:], '\n'); k >= 0 {
		end = i + k
	}
	f := strings.Fields(s[i:end])
	if len(f) > 1 && listMarker.MatchString(f[0]) {
		f = f[1:]
	}
	return len(f) > 0 && plainWord.MatchString(f[0])
}

// skipSpace returns the index of the first character of s at or after i that is not white space of any kind.
func skipSpace(s string, i int) int {
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if !isSpace(r) {
			break
		}
		i += n
	}
	return i
}

// oneWordLine reports whether the line of s that begins at i, less the white space that ends it, holds no white space:
// a single word, such as a secret on a line of its own, where a sentence has several.
func oneWordLine(s string, i int) bool {
	end := len(s)
	if k := strings.IndexByte(s[i:], '\n'); k >= 0 {
		end = i + k
	}
	return !strings.ContainsFunc(strings.TrimRightFunc(s[i:end], isHSpace), isHSpace)
}

// lineIndent is how many characters of white space begin the line of s that holds index i.
func lineIndent(s string, i int) int {
	n := 0
	for j := strings.LastIndexByte(s[:i], '\n') + 1; j < len(s); {
		r, size := utf8.DecodeRuneInString(s[j:])
		if !isHSpace(r) {
			break
		}
		n++
		j += size
	}
	return n
}

// isSpace reports whether r is white space that ends an unquoted value: of any width, as the space class of the
// patterns reads it, with the form feed that Normalize deletes in text it has read.
func isSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\f' || r == '\r' || r == '\u0085' || unicode.Is(unicode.Z, r)
}

// isHSpace reports whether r is white space that is not a line break: hspace as a function.
func isHSpace(r rune) bool { return r == '\t' || unicode.Is(unicode.Z, r) }

// skipHSpace returns the index of the first character of s at or after i that is not hspace.
func skipHSpace(s string, i int) int {
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if !isHSpace(r) {
			break
		}
		i += n
	}
	return i
}

// continued returns where the value that starts at start and ends at end ends once a trailing continuation mark is read
// (see continuations). A shell goes on to the next line after a backslash that ends a line, as PowerShell does after a
// backtick and cmd after a caret, so the value of "--password \" is the first word of the next line; with the lines
// joined, the mark is followed by a space instead. A flag's value is not another flag.
func continued(s string, start, end int, rule valueRule) int {
	if end-start != 1 || !strings.ContainsRune(continuations, rune(s[start])) {
		return end
	}
	i := skipHSpace(s, end)
	if i < len(s) && s[i] == '\n' {
		i = skipHSpace(s, i+1)
	}
	if i == end || i >= len(s) || s[i] == '\n' || (rule == flagValue && s[i] == '-') {
		return end
	}
	return valueEnd(s, i)
}

// tagged returns where the header of a block scalar is, or where the value ends, for a value that starts at start and
// ends at end with a tag or an anchor ("password: !!binary |", "password: &pw >-", "password: &pw hunter2",
// "password: !!str 'hunter 2'"): the header that follows them on the line, which is what blockScalarEnd reads from, or
// the end of the word or the quoted string that follows, which the value goes on to ("password: !important note" loses
// "note" too). With no tag or anchor, or one and nothing after it on the line, the start and end are returned as they are.
func tagged(s string, start, end int) (int, int) {
	hs, he := start, end
	for range 4 { // a tag and an anchor, and a little room: more is no YAML
		if s[hs] != '!' && s[hs] != '&' {
			break
		}
		i := skipHSpace(s, he)
		if i == he || i >= len(s) || s[i] == '\n' {
			break
		}
		next := valueEnd(s, i)
		if blockHeader.MatchString(s[i:next]) {
			return i, next
		}
		hs, he = i, next
	}
	if hs == start {
		return start, end
	}
	return hs, he
}

// blockScalarEnd returns where the value that starts at start and ends at end ends if it is the header of a YAML block
// scalar ("|", ">-"): at the end of the last line that is indented more than the line of the header, blank lines among
// them, and at least at the end of the header's own line (whatever follows the header there, a comment or not, goes with
// it). A header is no block scalar if it is not "|" or ">" and its indicators, and end is returned as it is. (A list
// entry's "- " is no indentation here, so a line of an entry of a sequence that is indented less than its key still
// counts: that takes a sibling key too, and loses a little.)
func blockScalarEnd(s string, start, end int) int {
	if !blockHeader.MatchString(s[start:end]) {
		return end
	}
	eol := len(s)
	if k := strings.IndexByte(s[end:], '\n'); k >= 0 {
		eol = end + k
	}
	parent := indent(s[strings.LastIndexByte(s[:start], '\n')+1:])
	last := eol
	for pos := eol + 1; pos <= len(s); {
		lineEnd := len(s)
		if k := strings.IndexByte(s[pos:], '\n'); k >= 0 {
			lineEnd = pos + k
		}
		if line := s[pos:lineEnd]; strings.TrimLeftFunc(line, isHSpace) != "" { // a blank line is the scalar's if more follow it
			if indent(line) <= parent {
				break
			}
			last = lineEnd
		}
		pos = lineEnd + 1
	}
	return last
}

// indent is how many characters of white space begin line.
func indent(line string) int {
	return utf8.RuneCountInString(line) - utf8.RuneCountInString(strings.TrimLeftFunc(line, isHSpace))
}

// endsValue reports whether a value could end at i: at the end of the text or before a delimiter. The
// marker of a value taken out is accepted only so, so an agent cannot stop redaction by writing it:
// "password=[redacted]hunter2" is all value.
func endsValue(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return isSpace(r) || strings.ContainsRune(`"',;}]\`, r)
}

// valueEnd returns where the value that starts at i ends: after a scheme word if there is one, a
// quoted string (JSON's, the same with the quotes escaped, or single-quoted), or a run up to white
// space of any width. A quote that never closes takes the rest of the text, so that a secret is not left out of
// a malformed one.
func valueEnd(s string, i int) int {
	if loc := schemeWord.FindStringIndex(s[i:]); loc != nil {
		if name, _, ok := nestedKey(s, i+loc[1], false); ok && SecretName(name) { // "Bearer token: x": the scheme alone, the key has its own value
			return i + len(strings.TrimRightFunc(s[i:i+loc[1]], isHSpace))
		}
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
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if isSpace(r) {
			break
		}
		i += n
	}
	return i
}

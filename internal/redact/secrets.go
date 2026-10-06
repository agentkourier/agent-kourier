// Package redact takes secrets out of text that is shown, stored, logged or sent to an agent: courier's one
// place that knows what a token, a credential or a secret value looks like (spec 8.4, 8.6 step 7).
//
// It has two kinds of entry point, free text (FreeText, FreeTextCut, and FreeTextLine and FreeTextCutLine for text that is
// shown on one line) and IdentifierText, and all return normalized text (see Normalize): the invisible characters
// deleted and the line breaks written "\n" before any secret is matched, so that no caller can get the order of
// cleaning and redacting wrong. The package imports no other package of courier.
package redact

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	redacted = "[redacted]"

	// space is the white space that ends a credential: tab, the line breaks (LF, CR, NEL, and the Unicode line and
	// paragraph separators of category Z) and every other Unicode space (NBSP and the em space, which Go's \s, ASCII
	// only, would miss). The vertical tab and the form feed are not here: Normalize deletes them, so they are
	// part of a credential (see deletable), not its end.
	space = `\t\n\r \x{85}\p{Z}`
	// deletable is every character Normalize deletes, as a class body (TestDeletableIsTheDeletionSet holds the two
	// equal): format, control (Cc, but not the white space and line breaks above), private use, the blank fillers,
	// the other default-ignorable characters, the variation selectors and invalid UTF-8 (which a pattern reads as
	// U+FFFD). The patterns run on the text as it came as well as on the normalized text, and in
	// the first run a character that will be deleted must be transparent: inside a credential it does not end it,
	// or the rest would be glued to the "[redacted]" where the second run cannot see it.
	deletable = `\p{Cf}\p{Co}\x{3164}\x{2800}\x{115f}\x{1160}\x{ffa0}\x{34f}\x{17b4}\x{17b5}\x{180b}-\x{180d}\x{180f}\x{fe00}-\x{fe0f}` +
		`\x{2065}\x{fff0}-\x{fff8}\x{e0000}-\x{e0fff}\x{fffd}\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x84\x86-\x9f`
	// blank is what may pad a field: white space and everything Normalize
	// deletes, so NUL, ESC and the C1 set cannot split a scheme from its
	// credential either.
	blank = space + deletable
	// esc is a literal escape sequence of the kind Go's %q, %+q and %#v print
	// for a character they cannot show: a backslash and t, x00, u00a0 or
	// U000e0001. An audit field that holds quoted text carries an invisible
	// gap as one of these, so they count as format and blank characters.
	esc = `\\(?:[abfnrtv0]|x[0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8})`
	// gap is what may hide inside a word: any number of the characters that Normalize deletes (zero-width space
	// and joiners, BOM, soft hyphen, word joiner, control characters, ...), or escapes. They render as nothing, so
	// an attacker can hide any of them, and more than one, inside a scheme or a prefix.
	gap = `(?:[` + deletable + `]|` + esc + `)*`
	// pad is one character of padding: a blank character or an escape.
	pad = `(?:[` + blank + `]|` + esc + `)`
	// sep separates a scheme from its credential: any run of blank characters
	// or escapes, colons ("Bearer: X"), or a percent-encoded space ("Bearer%20X").
	sep = `(?:[` + blank + `:]|%20|` + esc + `)+`
	// stop ends a credential that may sit inside JSON, a list or a header: a
	// quote, comma, semicolon, closing brace, or the backslash of an escaped
	// quote (what %q prints inside a string).
	stop = `"',;}\\`
	// credential is a run of non-space characters up to a stop. It may contain
	// format and control characters and start with a (possibly escaped) quote,
	// but cannot start with a separator or stop character, so a scheme followed
	// by nothing but padding, or by the end of a JSON string, is left alone.
	credential = `\\*["']?[^` + blank + `:` + stop + `][^` + space + stop + `]*`
	// jwtChar is one character of a JWT segment: base64url, a character that
	// is deleted, or an escape.
	jwtChar = `(?:[\w-]|[` + deletable + `]|` + esc + `)`
	// b64Std and b64URL are the characters of a base64 run, in the standard alphabet and in either one.
	b64Std = `[A-Za-z0-9+/]`
	b64URL = `[A-Za-z0-9+/_-]`
	// gluedRest is how many characters follow the first of a token glued to
	// "bearer": with that one, a token of seven. A word of fewer after
	// "bearer" ("bearerless", "bearerbonds") is no credential.
	gluedRest = `6`
	// value is the value of an OAuth field: it stops at white space, a stop,
	// or a query delimiter, and holds at least one character that is not deleted (a value of nothing but
	// zero-width spaces is no value: the redaction would take them and leave the real text after them).
	value = `[` + deletable + `]*[^` + space + stop + `&` + deletable + `][^` + space + stop + `&]*`

	// A token's characters after its prefix, and how many of them make a token and not a word that begins like one.
	// The lengths are those of the real tokens or a little under, so a test key a person wrote short is still taken;
	// "sk-learn", "ask-me" and "AIzawhatever" are well under.
	wordChars  = `[A-Za-z0-9_]`
	urlChars   = `[A-Za-z0-9_-]`
	alnumChars = `[A-Za-z0-9]`
)

// chars returns a pattern for a run of at least n characters of class (a character class) that no deleted character
// ends: each may be followed by any number of them, which count for nothing.
func chars(class string, n int) string {
	return `(?:` + class + `[` + deletable + `]*){` + strconv.Itoa(n) + `,}`
}

// run is chars with up to two "=" of padding after it, as base64 has.
func run(class string, n int) string {
	return chars(class, n) + `(?:=[` + deletable + `]*){0,2}`
}

// spell returns a pattern for s that tolerates format characters and escapes
// between its letters ("xo<ZWSP>xb-", `xo\u200bxb-`).
func spell(s string) string {
	return strings.Join(strings.Split(s, ""), gap)
}

// pair returns a pattern for a two-word field name: the words joined by an
// optional underscore or hyphen (access_token, access-token, accessToken),
// with format characters and escapes tolerated anywhere. Callers match it
// case-insensitively.
func pair(a, b string) string {
	return spell(a) + gap + `[_-]?` + gap + spell(b)
}

var (
	// skKey matches the sk- keys of OpenAI and Anthropic (sk-proj-, sk-ant-: they are sk- and a longer run), from
	// 20 characters after the prefix. It needs a word boundary, or it would take the end of "task-" and "disk-" and every
	// long identifier that has it, and the boundary is what a gap that Normalize deletes can hide ("word", a zero-width
	// space, "sk-"): so it is one of the patterns of the raw pass too, and the run after it tolerates the gaps.
	skKey = `\bsk-[` + deletable + `]*` + chars(urlChars, 20)

	// secretPattern matches Slack tokens, bearer credentials and JWTs. Bearer
	// takes a separator and a credential, or, glued to the scheme, a token of
	// at least gluedMin characters starting with a letter or digit, and then
	// only at a word boundary before "bearer": "flagbearer" and "x_bearer",
	// "bearers", "bearer's", a short word and identifiers like
	// "bearer-gateway" survive. Slack tokens and Bearer credentials end at a
	// stop, so compact JSON keeps its next field. Compiled once and never
	// modified.
	secretPattern = regexp.MustCompile(
		spell("xox") + gap + `[a-z]` + gap + `-[^` + space + stop + `]+` +
			`|` + spell("xapp-") + `[^` + space + stop + `]+` +
			`|(?i:` + spell("bearer") + `)` + sep + credential +
			`|\b(?i:` + spell("bearer") + `)[A-Za-z0-9][^` + space + stop + `]{` + gluedRest + `,}` +
			`|` + spell("eyJ") + jwtChar + `+\.` + jwtChar + `+\.` + jwtChar + `*` +
			`|` + skKey)

	// oauthPattern matches an OAuth token or secret field, as key=value or as
	// JSON "key":"value", plain or with the quotes escaped (\"key\":\"value\").
	// The first group is the key and its punctuation, kept; the second is the
	// value, redacted. After a colon any padding is allowed. After an equals
	// sign it is allowed only when the sign is padded on both sides
	// ("key = value"), so "key= next" reads as an empty value, not as "next".
	oauthPattern = regexp.MustCompile(`((?i:` +
		pair("access", "token") + `|` + pair("refresh", "token") + `|` + pair("id", "token") + `|` + pair("client", "secret") +
		`)\\*["']?(?:` + pad + `*:` + pad + `*|` + pad + `*=|` + pad + `+=` + pad + `+)\\*["']?)(` + value + `)`)

	// prefixPatterns match the tokens that give themselves away by a prefix, with no name before them, in the normalized
	// text only: GitHub's (ghp_, gho_, ghu_, ghs_, ghr_, github_pat_), GitLab's glpat-, Google's AIza and its 35
	// characters, and Stripe's secret and restricted keys, live and test, each from the length of the shortest real one
	// or a little under ("AIzawhatever" stays). They need no word boundary, so a gap that Normalize deletes cannot hide
	// one from the pass over the normalized text, and they are literal prefixes, which the regexp package finds fast:
	// they are not in the pass over the text as it came (see secrets). The sk- keys are skKey, in secretPattern.
	prefixPatterns = []*regexp.Regexp{
		regexp.MustCompile(`gh[pousr]_` + wordChars + `{20,}`),
		regexp.MustCompile(`github_pat_` + wordChars + `{20,}`),
		regexp.MustCompile(`glpat-` + urlChars + `{20,}`),
		regexp.MustCompile(`AIza` + urlChars + `{35,}`),
		regexp.MustCompile(`sk_(?:live|test)_` + alnumChars + `{16,}`),
		regexp.MustCompile(`rk_(?:live|test)_` + alnumChars + `{16,}`),
	}

	// sasPattern matches the signature of an Azure shared access signature, the value of the "sig" query parameter, in
	// the normalized text: a URL-encoded base64 run of at least 16 characters, so "the sig= field" and "sig=abc" stay.
	// The first group, the name and its "=", is kept; the second is redacted.
	sasPattern = regexp.MustCompile(`(sig=)([A-Za-z0-9%+/_-]{16,}(?:=){0,2})`)

	// basicPattern matches "Basic" and a base64 credential of at least 8
	// characters (user:pass encodes to that or longer). Basic is a common
	// English word, so Secrets also spares plain words.
	basicPattern = regexp.MustCompile(`(?i:` + spell("basic") + `)` + sep + `(` + run(b64Std, 8) + `)`)

	// gluedBasicPattern is a Basic credential against its scheme, with nothing between: what is left of
	// "Basic", an invisible character and a credential once normalization has deleted the character. It
	// needs a word boundary before "Basic" and a base64 run (either alphabet) of at least 12 characters
	// (user:pass is 12), and replaceGluedBasic takes it only if the run decodes to a "user:password": there
	// is no gap to tell "BasicdXNlcjpwYXNz" from "BasicAuthMiddleware" but what the run says.
	gluedBasicPattern = regexp.MustCompile(`\b(?i:` + spell("basic") + `)(` + run(b64URL, 12) + `)`)

	// plainWord is a word with at most a leading capital: prose, not base64.
	plainWord = regexp.MustCompile(`^[A-Z]?[a-z]+$`)
)

// secrets is the token layer of redaction, which FreeText and IdentifierText
// run on text they have normalized. It replaces anything in s that looks like
// a Slack token, a Bearer or Basic credential, a JWT, or the value of an OAuth
// access_token, refresh_token, id_token or client_secret field with
// "[redacted]". It is not an entry point: it matches s as it is, and the
// patterns tolerate the invisible characters only as defence in depth, since
// both entries delete them first. Schemes
// and credentials may be separated by any Unicode white space, format or
// control character, a colon, or "%20", and each of those may also appear as
// the literal escape Go's %q, %+q and %#v print for it (\t, \x00, \u00a0,
// \u200b). Field names match in any case, anywhere in a word (validtoken=x
// redacts on purpose: a left boundary would miss x_access_token), with an
// underscore, a hyphen or nothing between the words (accessToken), and in JSON
// with escaped quotes, as %q prints a token-endpoint body. A Bearer or Slack
// credential, and a field value, end at a quote, comma, semicolon, closing
// brace or backslash, so compact JSON keeps the fields after it; a credential
// that itself contains one of those is cut there. "key= next" is an empty
// value and leaves "next" alone. It is a backstop for free-text audit fields,
// not a license to put secrets in them. A Bearer credential glued to its
// scheme needs a word boundary before "bearer" and seven characters of token. A token with a prefix of its own
// (GitHub's ghp_ and its kind, GitLab's glpat-, the sk- of OpenAI and Anthropic, Google's AIza, Stripe's sk_live_ and
// its kind) is taken with no name before it, from the length of the shortest real one (a word that only begins so,
// "sk-learn" or "AIzawhatever", is kept), and so is the value of an Azure SAS signature (sig=, from 16 characters).
//
// Known limits, all deliberate; TestSecretsKnownLimits pins them:
//   - A value that is a JSON object or array ({"access_token":{...}}) is not
//     redacted past its first character.
//   - A quote escaped inside a value ("ab\"cd") ends the value at the backslash.
//   - JSON that is URL-encoded (%22access_token%22%3A) or HTML-encoded
//     (&quot;access_token&quot;) is not matched.
//   - password fields are not matched: the name is too common to redact by key.
//   - An equals sign padded only after it (client_secret= "x") is read as an
//     empty value followed by text, so the text survives.
//   - A format or control character inside a credential or value, printed by
//     %q as an escape, ends it at the backslash; the rest survives.
//   - A Basic credential under 8 base64 characters survives (it would collide
//     with ordinary words). Glued to its scheme, with no gap at all, one survives
//     that is under 12 characters, or does not decode to UTF-8 text with a colon
//     and no control character in it (a "user:password"): so "BasicAuthMiddleware"
//     is kept, and so is a glued Latin-1 credential. A gap, of any kind Normalize
//     deletes, makes it a credential for the first rule, whatever it decodes to.
//   - A Bearer credential under 7 characters survives when nothing at all
//     separates it from its scheme ("Bearerabc").
//   - A scheme or key written in fullwidth or other compatibility forms (U+FF22
//     for B) is not matched, because that would need NFKC normalization first.
func secrets(s string) string {
	s = rawSecrets(s)
	for _, re := range prefixPatterns {
		s = re.ReplaceAllString(s, redacted)
	}
	return sasPattern.ReplaceAllString(s, "${1}"+redacted)
}

// rawSecrets is secrets without the patterns that are for normalized text only (prefixPatterns, sasPattern): the
// token layer for text as it came, whose patterns tolerate the gaps that Normalize deletes.
func rawSecrets(s string) string {
	s = secretPattern.ReplaceAllString(s, redacted)
	s = oauthPattern.ReplaceAllString(s, "${1}"+redacted)
	s = basicPattern.ReplaceAllStringFunc(s, func(m string) string {
		if plainWord.MatchString(Normalize(basicPattern.FindStringSubmatch(m)[1])) {
			return m
		}
		return redacted
	})
	return gluedBasicPattern.ReplaceAllStringFunc(s, func(m string) string {
		if userPassword(Normalize(gluedBasicPattern.FindStringSubmatch(m)[1])) {
			return redacted
		}
		return m
	})
}

// userPassword reports whether run is base64, in the standard or the URL alphabet and with or without its
// padding, of UTF-8 text that holds a colon and no control character: the "user:password" a Basic credential
// carries. (Latin-1 text is not taken: the credential is a "user:password" in UTF-8, as RFC 7617 says it should be, and
// a glued Latin-1 run, with no gap to show it, would let far more words pass for one.)
func userPassword(run string) bool {
	run = strings.TrimRight(run, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		b, err := enc.DecodeString(run)
		if err == nil && bytes.IndexByte(b, ':') >= 0 && utf8.Valid(b) && !bytes.ContainsFunc(b, unicode.IsControl) {
			return true
		}
	}
	return false
}

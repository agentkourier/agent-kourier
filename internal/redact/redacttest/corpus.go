// Package redacttest is the one corpus of split-secret text that every place courier stores, logs or sends text
// is held to, and the checks that read what such a place returned. It is for tests only: it imports the testing
// package, and nothing courier runs may import it (TestRedacttestIsOnlyForTests).
//
// It imports no courier package, so any package's tests, redact's own among them, can use it.
package redacttest

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"
)

// Invisible is a character, or a run of bytes, that redaction's normalization deletes: one sample of one class
// of the deletion set, or the edge of a range inside a class.
type Invisible struct {
	Name string
	Text string
}

// Invisibles has a sample of every class of the deletion set, and both ends of each variation-selector range. A
// class that a change drops from the set has a sample here, so every test over Invisibles fails for it.
var Invisibles = []Invisible{
	{"Cc NUL", "\x00"},
	{"Cc BEL", "\x07"},
	{"Cc vertical tab", "\x0b"},
	{"Cc form feed", "\x0c"},
	{"Cc ESC", "\x1b"},
	{"Cc DEL", "\x7f"},
	{"Cc C1 start", "\U00000080"},
	{"Cc C1 CSI", "\U0000009b"},
	{"Cf zero-width space", "\U0000200b"},
	{"Cf zero-width non-joiner", "\U0000200c"},
	{"Cf zero-width joiner", "\U0000200d"},
	{"Cf left-to-right mark", "\U0000200e"},
	{"Cf right-to-left override", "\U0000202e"},
	{"Cf right-to-left isolate", "\U00002067"},
	{"Cf word joiner", "\U00002060"},
	{"Cf byte order mark", "\U0000feff"},
	{"Cf soft hyphen", "\U000000ad"},
	{"Cf Arabic letter mark", "\U0000061c"},
	{"Cf interlinear annotation", "\U0000fff9"},
	{"Cf tag language", "\U000e0001"},
	{"Cf tag letter", "\U000e0041"},
	{"Co BMP private use start", "\U0000e000"},
	{"Co BMP private use end", "\U0000f8ff"},
	{"Co plane 15", "\U000f0000"},
	{"Co plane 16", "\U0010fffd"},
	{"Cs encoded surrogate", "\xed\xa0\x80"},
	{"invalid UTF-8, a lone continuation byte", "\x80"},
	{"invalid UTF-8, a byte no sequence has", "\xff"},
	{"invalid UTF-8, an overlong slash", "\xc0\xaf"},
	{"Hangul filler", "\U00003164"},
	{"Braille blank", "\U00002800"},
	{"variation selector 1", "\U0000fe00"},
	{"variation selector 8", "\U0000fe08"},
	{"variation selector 16", "\U0000fe0f"},
	{"variation selector 17", "\U000e0100"},
	{"variation selector 100", "\U000e0150"},
	{"variation selector 256", "\U000e01ef"},
}

// LineBreak is a kind of line break, as it reaches redaction.
type LineBreak struct {
	Name string
	Text string
}

// LineBreaks has every kind of line break that normalization turns into "\n".
var LineBreaks = []LineBreak{
	{"LF", "\n"},
	{"CR", "\r"},
	{"CRLF", "\r\n"},
	{"NEL", "\U00000085"},
	{"line separator", "\U00002028"},
	{"paragraph separator", "\U00002029"},
}

// Case is one text and what must and must not come out of any place that redacts it.
type Case struct {
	Name string
	// Text is the input.
	Text string
	// Secrets are values that must not be in what comes out, however the place cuts or escapes the rest.
	Secrets []string
	// Keep is text that is no secret and that a place which only redacts must leave in.
	Keep []string
	// Free is set when only the free-text rules know the secret: the value of a pair or a flag, the password of
	// a URL. Identifier text keeps to what has a shape of its own (a token, a scheme and its credential), so
	// a place that redacts identifiers leaves these out (see Identifiers).
	Free bool
}

// value is the secret in most cases: a run no pattern of redaction knows by its own shape, so that only the
// name, the scheme or the prefix around it gives it away.
const value = "Zq9vK3mT7wRx"

// encodedBasic is the base64 of "user:password", the credential of a Basic scheme.
const encodedBasic = "dXNlcjpwYXNzd29yZA=="

// encodedLatin1 and encodedControl are the base64 of "admin:p\xe9ss" (Latin-1, not UTF-8) and of a password with a
// control character in it: text that is no UTF-8 line of a user and a password, and is one all the same.
var (
	encodedLatin1  = base64.StdEncoding.EncodeToString([]byte("admin:p\xe9ss"))
	encodedControl = base64.StdEncoding.EncodeToString([]byte("admin:pa\x07ss"))
)

// bs is the backslash that %q prints in front of an escape. It is joined to the rest in code, so that what is
// text in a test is never turned into the character it spells.
const bs = `\`

// split is a template of a secret whose giveaway is cut in two by "%s": a name, a scheme or a prefix. The text
// is the secret as an agent, a person or a tool might write it to get past a match on the plain form.
type split struct {
	name   string
	text   string
	secret string
	free   bool // see Case.Free
}

var splits = []split{
	{"a pair name", "pass%sword=" + value + " tail", value, true},
	{"a second pair name", "api%s_key: " + value + " tail", value, true},
	{"an environment name", "PGPASS%sWORD=" + value + " tail", value, true},
	{"a flag name", "tool --pass%sword " + value + " tail", value, true},
	{"a Slack bot token prefix", "xo%sxb-1234-5678-" + value + " tail", value, false},
	{"a Slack token after its prefix", "xoxb%s-1234-5678-" + value + " tail", value, false},
	{"a Slack token inside its prefix", "xox%sb-1234-5678-" + value + " tail", value, false},
	{"a Slack app token", "xa%spp-1-" + value + " tail", value, false},
	{"a Bearer scheme", "Authorization: Bear%ser " + value + " tail", value, false},
	{"a Bearer credential, glued", "Authorization: Bearer%s" + value + " tail", value, false},
	{"a Bearer credential, spaced", "Authorization: Bearer %s" + value + " tail", value, false},
	{"a JWT header", "eyJ%shbGciOiJub25lIn0.eyJjYW5hcnkiOjF9." + value + " tail", value, false},
	// Normalizing deletes the gap, and the glued rules, which need a word boundary and a credential that decodes, refuse
	// what it leaves: the token layer must take these in the text as it came.
	{"a word, an invisible and a glued Bearer", "token%sBearer" + value + " tail", value, false},
	{"a word run into Bearer, then an invisible", "authBearer%s" + value + " tail", value, false},
	{"a letter run into Bearer, then an invisible", "xBearer%s" + value + " tail", value, false},
	{"a percent-encoded colon and a glued Bearer", "%3ABearer%s" + value + " tail", value, false},
	{"a Basic credential of Latin-1 text, glued", "Authorization: Basic%s" + encodedLatin1 + " tail", encodedLatin1, false},
	{"a Basic credential with a control character in the password, glued", "Authorization: Basic%s" + encodedControl + " tail", encodedControl, false},
	{"a Basic credential of Latin-1 text, spaced", "Authorization: Basic %s" + encodedLatin1 + " tail", encodedLatin1, false},
	{"an OAuth field with only an invisible for a value, then the value", "access_token=%s " + value + " tail", value, true},
	{"an OAuth field", "access%s_token=" + value + " tail", value, false},
	{"an OAuth field in JSON", `{"client_%ssecret":"` + value + `","user":"U1"} tail`, value, false},
	{"a Basic credential", "Authorization: Bas%sic " + encodedBasic + " tail", encodedBasic, false},
	{"a Basic credential, glued", "Authorization: Basic%s" + encodedBasic + " tail", encodedBasic, false},
	{"a Basic credential, spaced", "Authorization: Basic %s" + encodedBasic + " tail", encodedBasic, false},
	{"a URL password", "https://adm:Zq9%svK3mT7wRx@host/path tail", value, true},
}

// insides are templates of a secret whose VALUE, not its name, an invisible character cuts: "%s" is inside the
// credential. A match that ended at the character would leave the rest of the value in the text, glued to the
// "[redacted]". The secret of each is what follows the cut.
var insides = []split{
	{"a Slack bot token cut in its value", "xoxb-1234-5678-Zq9v%sK3mT7wRx tail", "K3mT7wRx", false},
	{"a Slack app token cut in its value", "xapp-1-Zq9v%sK3mT7wRx tail", "K3mT7wRx", false},
	{"a spaced Bearer credential cut in its value", "Authorization: Bearer Zq9v%sK3mT7wRx tail", "K3mT7wRx", false},
	{"a glued Bearer credential cut in its value", "Authorization: BearerZq9v%sK3mT7wRx tail", "K3mT7wRx", false},
	{"a Basic credential cut in its value", "see Basic dXNlcjpw%sYXNzd29yZA== tail", "YXNzd29yZA", false},
	{"a glued Basic credential cut in its value", "see BasicdXNlcjpw%sYXNzd29yZA== tail", "YXNzd29yZA", false},
	{"a glued word and a glued Basic cut in its value", "token\x00BasicdXNlcjpw%sYXNzd29yZA== tail", "YXNzd29yZA", false},
	{"a JWT signature cut in its value", "eyJhbGciOiJub25lIn0.eyJjYW5hcnkiOjF9.sigZq9v%sK3mT7wRx tail", "K3mT7wRx", false},
	{"an OAuth field value cut in its value", "access_token=Zq9v%sK3mT7wRx tail", "K3mT7wRx", false},
}

// twoInvisibles are templates with two invisible characters, "%s" and "%t": one inside the scheme's name, the
// other before the scheme or between the scheme and its credential. Each pair of the representatives (one of each
// class of the deletion set) is run through each.
var twoInvisibles = []split{
	{"a word, an invisible, a Bearer cut inside", "token%sBea%trer" + value + " tail", value, false},
	{"a Bearer cut inside, and a gap before its token", "tokenBea%srer%t" + value + " tail", value, false},
	{"a word, an invisible, a Basic cut inside, glued", "token%sBa%tsic" + encodedBasic + " tail", encodedBasic, false},
	{"a Basic cut inside, and a gap before its credential", "Authorization: Bas%sic%t" + encodedBasic + " tail", encodedBasic, false},
	{"a Slack token's prefix cut twice", "xo%sx%tb-1234-5678-" + value + " tail", value, false},
	{"a JWT's prefix cut twice", "e%syJ%thbGciOiJub25lIn0.eyJjYW5hcnkiOjF9." + value + " tail", value, false},
}

// pairRepresentatives are the invisibles that two-invisible templates are run with: one of each class.
var pairRepresentatives = []int{0, 2, 4, 6, 8, 15, 16, 18, 21, 25, 26, 29, 30, 33, 34}

// SplitSecrets returns the corpus: every template of splits with every Invisible cut into it, the same secrets
// written as the literal escapes that %q prints, and the cases about spaces and the glued Bearer.
func SplitSecrets() []Case {
	var out []Case
	for _, s := range append(append([]split(nil), splits...), insides...) {
		for _, inv := range Invisibles {
			out = append(out, Case{
				Name:    s.name + " split by " + inv.Name,
				Text:    strings.Replace(s.text, "%s", inv.Text, 1),
				Secrets: []string{s.secret},
				Keep:    []string{"tail"},
				Free:    s.free,
			})
		}
	}
	for _, s := range twoInvisibles {
		for _, i := range pairRepresentatives {
			for _, j := range pairRepresentatives {
				text := strings.Replace(strings.Replace(s.text, "%s", Invisibles[i].Text, 1), "%t", Invisibles[j].Text, 1)
				out = append(out, Case{
					Name:    s.name + " split by " + Invisibles[i].Name + " and " + Invisibles[j].Name,
					Text:    text,
					Secrets: []string{s.secret},
					Keep:    []string{"tail"},
				})
			}
		}
	}
	out = append(out, escapes()...)
	out = append(out, spaces()...)
	return append(out, gluedBearer()...)
}

// escapes are token secrets whose giveaway is cut by what %q prints for an invisible character: text, not the
// character. The patterns of the token layer read it as the gap it stands for. The name of a pair is not among
// them: "pass\x07word=x" is no pair named password, and was never read as one.
func escapes() []Case {
	var out []Case
	for _, e := range []struct{ name, text string }{
		{"a Slack token, u200b", "xo" + bs + "u200bxb-1234-5678-" + value + " tail"},
		{"a Slack token, a selector", "xox" + bs + "ufe0fb-1234-5678-" + value + " tail"},
		{"a Slack token, a tag selector", "xox" + bs + "U000e0100b-1234-5678-" + value + " tail"},
		{"a Bearer scheme, x00", "B" + bs + "x00earer " + value + " tail"},
		{"a Bearer credential, t", "Bearer" + bs + "t" + value + " tail"},
		{"a Bearer credential, u00a0", "Bearer" + bs + "u00a0" + value + " tail"},
		{"a Bearer credential, u3164", "Bearer" + bs + "u3164" + value + " tail"},
	} {
		out = append(out, Case{Name: "escape: " + e.name, Text: e.text, Secrets: []string{value}, Keep: []string{"tail"}})
	}
	return out
}

// spaces are secrets after a white space that Go's \s does not match.
func spaces() []Case {
	var out []Case
	for _, sp := range []struct{ name, text string }{
		{"no-break space", "\U000000a0"}, {"em space", "\U00002003"}, {"ideographic space", "\U00003000"},
	} {
		for _, form := range []struct{ name, text string }{
			{"Bearer", "Authorization: Bearer%s" + value + " tail"},
			{"Bearer after a colon", "Authorization:%sBearer%s" + value + " tail"},
			{"a pair", "password:%s" + value + " tail"},
		} {
			out = append(out, Case{
				Name:    form.name + " across a " + sp.name,
				Text:    strings.ReplaceAll(form.text, "%s", sp.text),
				Secrets: []string{value},
				Keep:    []string{"tail"},
				Free:    form.name == "a pair",
			})
		}
	}
	return out
}

// gluedBearer is the one rule for a credential written against its scheme, and what it must not take.
func gluedBearer() []Case {
	return []Case{
		{Name: "glued Bearer", Text: "sent bearer" + value + " tail", Secrets: []string{value}, Keep: []string{"sent", "tail"}},
		{Name: "glued Bearer, upper case", Text: "sent BEARER" + value + " tail", Secrets: []string{value}, Keep: []string{"tail"}},
		{Name: "glued Bearer after a punctuation mark", Text: "(Bearer" + value + ") tail", Secrets: []string{value}, Keep: []string{"tail"}},
		{Name: "flagbearer is no scheme", Text: "the flagbearer" + value + " tail", Keep: []string{"flagbearer" + value, "tail"}},
		{Name: "flagbearer alone", Text: "tail, then the flagbearer", Keep: []string{"tail", "the flagbearer"}},
		{Name: "bearers", Text: "bearers of the bearer's duty tail", Keep: []string{"bearers of the bearer's duty tail"}},
		{Name: "Bearer alone", Text: "tail, then the Bearer", Keep: []string{"tail", "the Bearer"}},
		{Name: "a short glued word", Text: "bearerless bearerbonds tail", Keep: []string{"bearerless bearerbonds tail"}},
		{Name: "an identifier that starts with bearer", Text: "ns/bearer-gateway bearer_gateway.svc tail", Keep: []string{"ns/bearer-gateway bearer_gateway.svc tail"}},
	}
}

// Breaks returns a text of three lines, the first two ending in b, with a secret on the last: after a break of
// any kind, the secret is still named by what comes before it on its line.
func Breaks(b LineBreak) Case {
	return Case{
		Name:    "a pair after a " + b.Name,
		Text:    "first" + b.Text + "second" + b.Text + "token=" + value,
		Secrets: []string{value},
		Keep:    []string{"first", "second"},
		Free:    true,
	}
}

// Identifiers returns the cases of cs that identifier text redacts: the ones with no Free secret.
func Identifiers(cs []Case) []Case {
	var out []Case
	for _, c := range cs {
		if !c.Free {
			out = append(out, c)
		}
	}
	return out
}

// Check reports to t every way got, what a place returned for c.Text, is wrong: it holds a secret of c, or a
// character that normalization deletes, or a line break other than "\n". Call it for any place that stores,
// logs or sends text.
func (c Case) Check(t *testing.T, got string) {
	t.Helper()
	for _, s := range c.Secrets {
		if strings.Contains(got, s) {
			t.Errorf("%s: %q still holds the secret %q", c.Name, got, s)
		}
	}
	CheckNormalized(t, got)
}

// CheckKept is Check, and also reports each of c.Keep that got lacks: a place that redacts must take the
// secret and nothing else. Use it where the place changes no more than redaction does.
func (c Case) CheckKept(t *testing.T, got string) {
	t.Helper()
	c.Check(t, got)
	for _, k := range c.Keep {
		if !strings.Contains(got, k) {
			t.Errorf("%s: %q lost %q, which is no secret", c.Name, got, k)
		}
	}
}

// CheckNormalized reports to t any invisible character in got, any invalid UTF-8, and any line break that is not
// "\n". It is the check for a place that normalizes and does not redact (chat.Clean).
func CheckNormalized(t *testing.T, got string) {
	t.Helper()
	if !utf8.ValidString(got) {
		t.Errorf("%q is not valid UTF-8", got)
	}
	for _, inv := range Invisibles {
		if utf8.ValidString(inv.Text) && strings.Contains(got, inv.Text) {
			t.Errorf("%q still holds %s", got, inv.Name)
		}
	}
	for _, b := range LineBreaks {
		if b.Text != "\n" && b.Text != "\r\n" && strings.Contains(got, b.Text) { // CRLF is found by its CR
			t.Errorf("%q still holds a %s", got, b.Name)
		}
	}
}

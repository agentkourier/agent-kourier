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

// coreInvisibles has a sample of every class of the deletion set that the first version had, and both ends of each
// variation-selector range.
var coreInvisibles = []Invisible{
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

// ignorableInvisibles are the rest of the Unicode Default_Ignorable_Code_Point property that is shown as nothing and is in
// none of the categories above: the combining grapheme joiner, the Hangul fillers, the Khmer inherent vowels, the Mongolian
// free variation selectors and the reserved code points of the property, each range at both its ends.
var ignorableInvisibles = []Invisible{
	{"Mn combining grapheme joiner", "\U0000034f"},
	{"Lo Hangul choseong filler", "\U0000115f"},
	{"Lo Hangul jungseong filler", "\U00001160"},
	{"Mn Khmer inherent vowel Aq", "\U000017b4"},
	{"Mn Khmer inherent vowel Aa", "\U000017b5"},
	{"Mn Mongolian free variation selector 1", "\U0000180b"},
	{"Mn Mongolian free variation selector 2", "\U0000180c"},
	{"Mn Mongolian free variation selector 3", "\U0000180d"},
	{"Mn Mongolian free variation selector 4", "\U0000180f"},
	{"Lo halfwidth Hangul filler", "\U0000ffa0"},
	{"Cn reserved ignorable U+2065", "\U00002065"},
	{"Cn reserved ignorable U+FFF0", "\U0000fff0"},
	{"Cn reserved ignorable U+FFF8", "\U0000fff8"},
	{"Cn reserved tag U+E0000", "\U000e0000"},
	{"Cn reserved tag U+E0002", "\U000e0002"},
	{"Cn reserved ignorable U+E0080", "\U000e0080"},
	{"Cn reserved ignorable U+E01F0", "\U000e01f0"},
	{"Cn reserved ignorable U+E0FFF", "\U000e0fff"},
}

// Invisibles has a sample of every class of the deletion set, and both ends of each variation-selector range and of
// each run of Default_Ignorable code points. A class that a change drops from the set has a sample here, so every
// test over Invisibles fails for it. The first ones are coreInvisibles, and ignorableInvisibles follow, so an index
// into the list (pairRepresentatives, fewRepresentatives) stays where it was.
var Invisibles = append(append([]Invisible(nil), coreInvisibles...), ignorableInvisibles...)

// crossed are the invisibles that each template of splits and insides is cut with: all of coreInvisibles, and one
// sample of each run of ignorableInvisibles. The rest of the deletion set is held by every test that goes over
// Invisibles alone (Normalize, the token patterns' class, SecretName), and the corpus, which every caller runs, does not
// grow by the whole set for each template.
func crossed() []Invisible {
	out := append([]Invisible(nil), coreInvisibles...)
	for _, i := range []int{0, 1, 5, 9, 11, 13} { // 034f, 115f, 180b, ffa0, fff0, e0000
		out = append(out, ignorableInvisibles[i])
	}
	return out
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

// long is a run for the tokens that have a minimum length: 36 characters, none of which ends a token. A Google API key
// takes long[:35], its exact length, and ends no other way.
const long = value + value + value

// sasValue is the value of an Azure SAS signature as a URL carries it: base64 with its "+" and "=" percent-encoded.
const sasValue = value + "%2B" + value + "%3D"

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
	// Tokens that give themselves away by a prefix, with no name before them.
	{"a GitHub personal token", "gh%sp_" + long + " tail", long, false},
	{"an OpenAI key", "s%sk-" + long + " tail", long, false},
	{"an Azure SAS signature", "https://acct.blob.core.windows.net/c?sv=2020-08-04&si%sg=" + sasValue + " tail", sasValue, false},
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
	{"a GitHub token cut in its value", "ghp_" + long + "%sK3mT7wRx tail", "K3mT7wRx", false},
	{"an Azure SAS signature cut in its value", "https://h/c?sv=1&sig=" + long + "%sK3mT7wRx tail", "K3mT7wRx", false},
}

// splitsFew and insidesFew are the same for the other tokens with a prefix of their own (each is built from the same
// pieces, spelled with a gap between the letters and before the run), run with fewRepresentatives, so that the
// corpus, which every caller runs, does not grow by the whole deletion set for each prefix.
var splitsFew = []split{
	{"a GitHub OAuth token", "gho%s_" + long + " tail", long, false},
	{"a GitHub user token", "ghu_%s" + long + " tail", long, false},
	{"a GitHub server token", "g%shs_" + long + " tail", long, false},
	{"a GitHub refresh token", "ghr%s_" + long + " tail", long, false},
	{"a GitHub fine-grained token", "github%s_pat_" + long + "_" + long + " tail", long, false},
	{"a GitLab token", "glpat%s-" + long + " tail", long, false},
	{"an OpenAI project key", "sk-pro%sj-" + long + " tail", long, false},
	{"an Anthropic key", "sk-a%snt-api03-" + long + " tail", long, false},
	{"a Google API key", "AI%sza" + long[:35] + " tail", long[:35], false},
	{"a Stripe live secret key", "sk%s_live_" + long + " tail", long, false},
	{"a Stripe live restricted key", "rk_l%sive_" + long + " tail", long, false},
	{"a Stripe test secret key", "sk_te%sst_" + long + " tail", long, false},
	{"a Stripe test restricted key", "rk_test%s_" + long + " tail", long, false},
}

var insidesFew = []split{
	{"a fine-grained GitHub token cut in its value", "github_pat_" + long + "%sK3mT7wRx tail", "K3mT7wRx", false},
	{"a GitLab token cut in its value", "glpat-" + long + "%sK3mT7wRx tail", "K3mT7wRx", false},
	{"an OpenAI project key cut in its value", "sk-proj-" + long + "%sK3mT7wRx tail", "K3mT7wRx", false},
	{"a Google API key cut in its value", "AIza" + long[:35] + "%sK3mT7wRx tail", "K3mT7wRx", false},
	{"a Stripe key cut in its value", "sk_live_" + long + "%sK3mT7wRx tail", "K3mT7wRx", false},
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

// twoInvisiblesPrefixed are the same for the tokens with a prefix of their own and for the Azure SAS signature, which are
// built like the others (a gap between the letters and before the value). They are run with fewRepresentatives, so
// that the corpus does not grow by the square of the deletion set for every template.
var twoInvisiblesPrefixed = []split{
	{"a GitHub token's prefix cut twice", "g%shp%t_" + long + " tail", long, false},
	{"an Azure SAS signature's name cut twice", "https://h/c?sv=1&s%si%tg=" + sasValue + " tail", sasValue, false},
	{"a word, an invisible, an OpenAI key and a gap before its run", "word%ssk-%t" + long + " tail", long, false},
	{"a word, an invisible, an Azure SAS name and a gap before its value", "https://h/c?sv=1&word%ssig=%t" + sasValue + " tail", sasValue, false},
}

// pairRepresentatives are the invisibles that two-invisible templates are run with: one of each class (see Invisibles
// for the indexes).
var pairRepresentatives = []int{0, 2, 4, 6, 8, 15, 16, 18, 21, 25, 26, 29, 30, 33, 34}

// fewRepresentatives are six of them for twoInvisiblesPrefixed: a control character, a format character, a private-use
// character, a variation selector, the combining grapheme joiner and a Hangul filler.
var fewRepresentatives = []int{0, 8, 21, 33, 37, 46}

// SplitSecrets returns the corpus: every template of splits with every Invisible cut into it (the tokens with a
// prefix of their own, a few of them), the same secrets written as the literal escapes that %q prints, the cases about
// spaces and the glued Bearer, what looks like a secret and is not, and the Kubernetes and shell forms. LineSplits is
// not in it. It is for the tests of internal/redact; a caller of redaction runs SplitSecretsSample.
func SplitSecrets() []Case {
	return build(crossed(), few(), pairRepresentatives, fewRepresentatives, spaceKinds)
}

// sampleIndexes are the indexes of Invisibles that the sample cuts into every template: one of each class of the
// deletion set (a control character, a format character, a private-use one, an encoded surrogate, invalid UTF-8, a blank
// filler, a variation selector, a default-ignorable Mn and a reserved one).
var sampleIndexes = []int{0, 8, 21, 25, 26, 29, 33, 37, 47}

// sampleRepresentatives are the invisibles that the sample pairs in the templates with two.
var sampleRepresentatives = []int{0, 8, 21, 33, 37}

// SplitSecretsSample is the corpus every caller of redaction runs: every template and every form of SplitSecrets, but
// cut with one invisible of each class and with the three first kinds of space (no-break, general, ideographic) in place
// of every one, so that a caller's test, which has to check only that its text goes through the right entry, does not
// pay for the whole cross product. The full cross stays in internal/redact's own tests.
func SplitSecretsSample() []Case {
	sample := make([]Invisible, len(sampleIndexes))
	for k, i := range sampleIndexes {
		sample[k] = Invisibles[i]
	}
	return build(sample, sample, sampleRepresentatives, sampleRepresentatives, spaceKinds[:3])
}

// few returns the invisibles at fewRepresentatives.
func few() []Invisible {
	out := make([]Invisible, len(fewRepresentatives))
	for k, i := range fewRepresentatives {
		out[k] = Invisibles[i]
	}
	return out
}

// build returns the cases of the templates: splits and insides cut with invs, splitsFew and insidesFew with fewInvs, the
// templates with two invisibles paired from the indexes pairReps and fewReps, and the forms with spaces of the kinds.
func build(invs, fewInvs []Invisible, pairReps, fewReps []int, kinds []spaceKind) []Case {
	var out []Case
	out = append(out, singles(append(append([]split(nil), splits...), insides...), invs)...)
	out = append(out, singles(append(append([]split(nil), splitsFew...), insidesFew...), fewInvs)...)
	out = append(out, pairs(twoInvisibles, pairReps)...)
	out = append(out, pairs(twoInvisiblesPrefixed, fewReps)...)
	out = append(out, escapes()...)
	out = append(out, spaces(kinds)...)
	out = append(out, gluedBearer()...)
	out = append(out, keeps()...)
	return append(out, manifests()...)
}

// singles returns the cases of the templates with each of invs cut into them at their "%s".
func singles(templates []split, invs []Invisible) []Case {
	var out []Case
	for _, s := range templates {
		for _, inv := range invs {
			out = append(out, Case{
				Name:    s.name + " split by " + inv.Name,
				Text:    strings.Replace(s.text, "%s", inv.Text, 1),
				Secrets: []string{s.secret},
				Keep:    []string{"tail"},
				Free:    s.free,
			})
		}
	}
	return out
}

// pairs returns the cases of the templates with every pair of the invisibles at the indexes reps in their two places.
func pairs(templates []split, reps []int) []Case {
	var out []Case
	for _, s := range templates {
		for _, i := range reps {
			for _, j := range reps {
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
	return out
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

// spaceKind is a kind of Unicode space.
type spaceKind struct{ name, text string }

// spaceKinds are the kinds of space that a secret's neighbours are written with. The first three are the sample's.
var spaceKinds = []spaceKind{
	{"no-break space", "\U000000a0"}, {"em space", "\U00002003"}, {"ideographic space", "\U00003000"},
	{"narrow no-break space", "\U0000202f"}, {"thin space", "\U00002009"},
}

// spaces are secrets next to a white space that Go's \s does not match: before a separator, after a flag, or ending the
// value, in each width of space that a person or a tool may write.
func spaces(kinds []spaceKind) []Case {
	var out []Case
	for _, sp := range kinds {
		for _, form := range []struct {
			name, text string
			free       bool
		}{
			{"Bearer", "Authorization: Bearer%s" + value + " tail", false},
			{"Bearer after a colon", "Authorization:%sBearer%s" + value + " tail", false},
			{"a pair", "password:%s" + value + " tail", true},
			{"a pair, spaced on both sides of its equals sign", "password%s=%s" + value + " tail", true},
			{"a pair, spaced on both sides of its colon", "password%s:%s" + value + " tail", true},
			{"an environment name, spaced on both sides", "api_key%s=%s" + value + " tail", true},
			{"a JSON pair, spaced before its colon", `{"password"%s: "` + value + `"} tail`, true},
			{"a secret flag, spaced before its value", "tool --password%s" + value + " tail", true},
			{"a secret flag after a space", "tool%s--password " + value + " tail", true},
			{"a user flag after a space", "curl%s-u admin:" + value + " tail", true},
			{"a user flag, spaced before its user", "curl -u%sadmin:" + value + " tail", true},
			{"a user flag whose value the space ends", "curl -u admin:" + value + "%stail", true},
			{"a Token scheme", "Authorization: Token%s" + value + " tail", true},
			{"a long user flag, spaced before its user", "curl --user%sadmin:" + value + " tail", true},
			{"a value that the space ends", "password=" + value + "%stail", true},
			{"a cookie list whose pairs the space follows", "Cookie: sid=" + value + ";%scsrf=" + value + " tail", true},
		} {
			out = append(out, Case{
				Name:    form.name + " across a " + sp.name,
				Text:    strings.ReplaceAll(form.text, "%s", sp.text),
				Secrets: []string{value},
				Keep:    []string{"tail"},
				Free:    form.free,
			})
		}
	}
	return out
}

// keeps are texts that look like one of the rules of redaction and are not: prose that mentions a token prefix, a name
// that a prefix is only the end of, a value too short to be a key, Kubernetes and shell text with no secret in it. A
// place that only redacts must leave each whole.
func keeps() []Case {
	var out []Case
	for _, k := range []struct{ name, text string }{
		{"sk- in a library name", "pip install sk-learn tail"},
		{"sk- ending a word", "a risk-free trade, ask-me anything tail"},
		{"sk- ending a long identifier", "task-0123456789abcdefghijklmnopqrstuvwxyz1 and disk-usage-0123456789abcdefghijklmnop tail"},
		{"a sk- key one character short", "sk-" + long[:19] + " tail"},
		{"a Google key prefix with too little after it", "AIzawhatever tail"},
		{"a Google key one character short", "AIza" + long[:34] + " tail"},
		{"GitHub prefixes in prose", "ghp_ and gho_ and github_pat_ tokens are rotated tail"},
		{"a GitHub token one character short", "ghp_" + long[:19] + " tail"},
		{"a GitLab prefix in prose", "the glpat- prefix tail"},
		{"Stripe prefixes in prose", "sk_live_ and rk_test_ keys start so tail"},
		{"a Stripe name that ends in rk_live_", "network_live_connections_established tail"},
		{"sig= in prose", "the sig= field is empty tail"},
		{"sig= with a short value", "sig=abc and sig=0123456789abcde tail"},
		{"an env entry with a name that is no secret", "- name: LOG_LEVEL\n  value: debug\n- name: REGION\n  value: tail"},
		{"an env entry that takes its value from the pod", "- name: DB_PASSWORD\n  valueFrom:\n    fieldRef:\n      fieldPath: metadata.name\n  tail"},
		{"a block scalar under a name that is no secret", "description: |\n  plain prose here\nnext: tail"},
		{"a stripped block scalar under a name that is no secret", "args: |-\n  run --verbose now\nnext: tail"},
		{"a continued command with a flag that is no secret", "mysql --verbose \\\n  run tail"},
	} {
		// A text of several lines is kept whole only where lines are kept: identifier text puts them on one.
		out = append(out, Case{Name: "kept: " + k.name, Text: k.text, Keep: []string{k.text}, Free: strings.Contains(k.text, "\n")})
	}
	return out
}

// manifests are the forms of Kubernetes and shell text in which a secret's value is on a line of its own: the value
// of an env entry named by a secret, a block scalar under a secret name, and a value continued after a trailing
// backslash. Only the free-text rules know them, and each is held with every kind of line break.
func manifests() []Case {
	var out []Case
	add := func(name, text string, keep ...string) {
		out = append(out, Case{Name: name, Text: text, Secrets: []string{value}, Keep: append([]string{"tail"}, keep...), Free: true})
	}
	add("an env entry", "env:\n- name: DB_PASSWORD\n  value: "+value+"\n- name: LOG_LEVEL\n  value: tail", "LOG_LEVEL", "DB_PASSWORD")
	add("an env entry, quoted", "- name: \"API_TOKEN\"\n  value: \""+value+"\"\n- name: tail", "API_TOKEN")
	add("an env entry with a comment before its value", "- name: PGPASSWORD\n  # set by hand\n  value: "+value+"\n- name: tail")
	add("an env entry in a nested list", "    - name: client_secret\n      value: "+value+"\n    - name: tail")
	add("an env entry with a value that starts with a dash", "- name: DB_PASSWORD\n  value: -"+value+"\n- name: tail")
	add("an env entry in JSON on one line", `{"name":"DB_PASSWORD","value":"`+value+`"} tail`)
	add("an env entry in JSON on two lines", "{\n  \"name\": \"API_TOKEN\",\n  \"value\": \""+value+"\"\n}\ntail")
	add("an env entry in a flow mapping", "{name: DB_PASSWORD, value: "+value+"} tail")
	add("an env entry with a block scalar for a value", "- name: DB_PASSWORD\n  value: |\n    "+value+"\n- name: tail")
	for _, h := range []string{"|", "|-", "|+", ">", ">-", ">+", "|2", "|2-"} {
		add("a block scalar, header "+h, "password: "+h+"\n  "+value+"\n  "+value+"\nnext: tail", "next: tail")
	}
	add("a block scalar under a list entry", "- apiKey: >-\n    "+value+"\n    "+value+"\n- other: tail", "other: tail")
	add("a block scalar with a blank line in it", "password: |\n  "+value+"\n\n  "+value+"\nnext: tail", "next: tail")
	add("a block scalar with a comment after its header", "password: | # note\n  "+value+"\nnext: tail", "next: tail")
	add("a block scalar at the end of the text", "tail: 1\ntoken: |\n  "+value)
	for _, b := range LineBreaks {
		add("a block scalar, lines ended by a "+b.Name, "password: |"+b.Text+"  "+value+b.Text+"next: tail", "next: tail")
		add("a flag and its value after a trailing backslash and a "+b.Name, "mysql --password \\"+b.Text+"  "+value+" tail")
		add("a user flag continued after a "+b.Name, "curl -u \\"+b.Text+"  admin:"+value+" tail", "admin")
	}
	add("a name and its value after a trailing backslash", "PASSWORD=\\\n  "+value+" tail")
	add("a flag, a trailing backslash and a quoted value", "curl --token \\\n  '"+value+"' tail")
	add("a continued command that goes on", "mysql -u root --password \\\n  "+value+" \\\n  --verbose tail", "--verbose")
	return out
}

// LineSplits are secrets that a line break cuts from what gives them away: a flag from its value, a user from its
// password, a URL's password in two. A place that keeps its line breaks reads each line as it is and does not take them;
// a place that puts the text on one line must, so it runs these too (redact.FreeTextLine, audit's free text, the one-line
// fields of an untrusted block).
func LineSplits() []Case {
	var out []Case
	for _, b := range LineBreaks {
		out = append(out,
			Case{Name: "a secret flag and its value across a " + b.Name, Text: "deploy --password" + b.Text + value + " tail", Secrets: []string{value}, Keep: []string{"deploy", "tail"}, Free: true},
			Case{Name: "a user flag and its user across a " + b.Name, Text: "curl -u" + b.Text + "admin:" + value + " tail", Secrets: []string{value}, Keep: []string{"tail"}, Free: true},
			Case{Name: "a long user flag and its user across a " + b.Name, Text: "curl --user" + b.Text + "admin:" + value + " tail", Secrets: []string{value}, Keep: []string{"tail"}, Free: true},
			Case{Name: "a URL password across a " + b.Name, Text: "https://user:Zq9vK3m" + b.Text + "T7wRx@host/x tail", Secrets: []string{"Zq9vK3m", "T7wRx"}, Keep: []string{"host", "tail"}, Free: true},
			Case{Name: "a secret flag after a " + b.Name + " and a space, then its value", Text: "first" + b.Text + "tool --token " + b.Text + value + " tail", Secrets: []string{value}, Keep: []string{"first", "tail"}, Free: true},
		)
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

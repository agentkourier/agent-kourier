package redact

import (
	"slices"
	"strconv"
	"strings"
)

// The leak oracle's generator. An input is two or three fields in a layout, and every secret value of a field is a planted
// marker, a word that is unique in its input. What must be redacted is decided here from the shape of the field (a
// secret-named key or an OAuth key with a value, a Bearer or Basic credential whatever its key), never by calling the code
// under test. See leak_test.go for the oracle and leaklimits_test.go for the shapes that main does not redact.

// leakAtom is one field: a key, the operator after it and a value, rendered from a template whose {0} and {1} are the
// field's markers.
type leakAtom struct {
	id       string // style/class/operator/value form, for reports
	style    string // plain, json, hash, escaped, env, flag, header, cookie, userflag, entry, token or label
	class    string // secret (the key is a secret name), oauth (an OAuth field name) or decoy (no secret name)
	form     string // the value form
	op       string // what separates the key from the value, as written
	text     string // the field with {0} and {1} where the markers go
	markers  int    // how many markers the field holds
	scheme   string // Bearer or Basic when the value is that credential
	tail     bool   // the field ends on a line of a block scalar, a continuation or a value under its key: nothing can follow it on that line
	firstRun bool   // the first marker is where IdentifierText's OAuth rule reads a value: the first run after the operator
	plain    string // the text with the marker where the value form puts it, not moved by placeMarker (empty: the text)
	path     string // the rule path of the end matrix that the field stands for (see endMatrix)
	endChar  string // the internal character before the marker, for a field of the end matrix
	key      string // the key as written, for the key forms that main does not read (leakExtra)
	lineOnly bool   // the value is on a line after its name, which only the entries that join the lines read as the name's value
	shape    bool   // the secret has a shape of its own and no name needed (a token with a prefix, a URL's password), whatever the key
	identish bool   // IdentifierText reads the shape too
}

// secretKey reports whether the field's key is a secret name, which the generator knows from its own list of names.
func (a *leakAtom) secretKey() bool { return a.class != "decoy" }

// mustFree reports whether marker j of the field must not be shown by the free-text entries: the value of a secret-named
// or OAuth key, and a Bearer or Basic credential under any key.
func (a *leakAtom) mustFree(j int) bool {
	return j < a.markers && !a.lineOnly && (a.secretKey() || a.scheme != "" || a.shape)
}

// mustLine reports whether marker j of the field must not be shown by the free-text entries that join the lines (FreeTextLine
// and FreeTextCutLine): what the others must hide, and a value that a line break parts from its name (the others keep their lines,
// and keep the break between a flag and a word of a new sentence).
func (a *leakAtom) mustLine(j int) bool {
	return j < a.markers && (a.mustFree(j) || a.lineOnly)
}

// mustIdent reports whether marker j of the field must not be shown by IdentifierText, which reads only what has a shape of
// its own: a Bearer or Basic credential, and the value of an OAuth field when the value is the first run after the
// operator (an identifier is no YAML: its tags, block scalars and continuation lines are not read).
func (a *leakAtom) mustIdent(j int) bool {
	if j >= a.markers {
		return false
	}
	return a.scheme != "" || a.identish || (j == 0 && a.class == "oauth" && a.firstRun)
}

// valueForm is a way to write the value of a field. The text has {0} where the marker goes (and {1} for the second marker of
// a cookie), and none for a value of nothing but a tag.
type valueForm struct {
	id       string
	text     string
	markers  int
	scheme   string
	firstRun bool
	lineOnly bool
	variant  string // where placeMarker put the marker: "" at the start of the value, "#end" or "#mid"
	base     string // the text as written, when placeMarker moved the marker
}

var (
	vWord      = valueForm{id: "word", text: "{0}", markers: 1, firstRun: true}
	vDouble    = valueForm{id: "double", text: `"{0}"`, markers: 1, firstRun: true}
	vSingle    = valueForm{id: "single", text: `'{0}'`, markers: 1, firstRun: true}
	vSpaced    = valueForm{id: "spaced", text: `"{0} and more"`, markers: 1, firstRun: true}
	vSpaced2   = valueForm{id: "spaced2", text: `"and more {0}"`, markers: 1}
	vTag       = valueForm{id: "tag", text: "!x {0}", markers: 1}
	vTag2      = valueForm{id: "tag2", text: "!!str {0}", markers: 1}
	vBare2     = valueForm{id: "baretag2", text: "!!str"}
	vFolded    = valueForm{id: "folded", text: ">-\n    {0}", markers: 1}
	vAnchor    = valueForm{id: "anchor", text: "&a {0}", markers: 1}
	vTagAnchor = valueForm{id: "taganchor", text: "!x &a {0}", markers: 1}
	vTagQuoted = valueForm{id: "tagquoted", text: `!!str "{0}"`, markers: 1}
	vBareTag   = valueForm{id: "baretag", text: "!x"}
	vBlock     = valueForm{id: "block", text: "|\n    {0}", markers: 1}
	vTagBlock  = valueForm{id: "tagblock", text: "!!binary |\n    {0}", markers: 1}
	vAnchBlock = valueForm{id: "anchorblock", text: "&a |\n    {0}", markers: 1}
	vInline    = valueForm{id: "blockinline", text: "| {0}", markers: 1}
	vContBS    = valueForm{id: "contbackslash", text: "\\\n    {0}", markers: 1}
	vContTick  = valueForm{id: "contbacktick", text: "`\n    {0}", markers: 1}
	vContCaret = valueForm{id: "contcaret", text: "^\n    {0}", markers: 1}
	vBearer    = valueForm{id: "bearer", text: "Bearer {0}", markers: 1, scheme: "Bearer"}
	vBasic     = valueForm{id: "basic", text: "Basic {0}", markers: 1, scheme: "Basic"}
	vToken     = valueForm{id: "tokenscheme", text: "Token {0}", markers: 1}
	vEmpty     = valueForm{id: "empty"}
	vTagQ      = valueForm{id: "tagstring", text: `!"a" {0}`, markers: 1}
	vSymbols   = valueForm{id: "symbols", text: "{0}.-_/+=x9", markers: 1, firstRun: true}
	vBackslash = valueForm{id: "backslash", text: `</b>!\{0}`, markers: 1, firstRun: true}
	vBareTick  = valueForm{id: "baretick", text: "`"}
	vBareCaret = valueForm{id: "barecaret", text: "^"}
	vBareBS    = valueForm{id: "barebackslash", text: `\`}
	vBareBasic = valueForm{id: "barebasic", text: "Basic"}
	vBareToken = valueForm{id: "baretoken", text: "Token"}
	vEscaped   = valueForm{id: "escaped", text: `\"{0}\"`, markers: 1, firstRun: true}
	vLitGlued  = valueForm{id: "litglued", text: "[redacted]{0}", markers: 1, firstRun: true}
	vLitDot    = valueForm{id: "litdot", text: "[redacted].{0}", markers: 1, firstRun: true}
	vLitSpaced = valueForm{id: "litspaced", text: "[redacted] {0}", markers: 1}
	vVerbatim  = valueForm{id: "verbatim", text: "!<tag:yaml.org,2002:str> {0}", markers: 1}
	vTagStr    = valueForm{id: "tagq", text: "!'q' {0}", markers: 1}
	vChain     = valueForm{id: "chain", text: "!x !y &a {0}", markers: 1}
	vBang      = valueForm{id: "bang", text: "!"}
	vAmp       = valueForm{id: "amp", text: "&"}
	vGlueComma = valueForm{id: "gluecomma", text: "!x,"}
	vGlueSemi  = valueForm{id: "gluesemi", text: "!x;"}
	vGlueBrace = valueForm{id: "gluebrace", text: "!x}"}
	vBlock2    = valueForm{id: "block2", text: "|2\n    {0}", markers: 1}
	vHeredoc   = valueForm{id: "heredoc", text: "<<EOF\n{0}\nEOF", markers: 1}
	vNestedVal = valueForm{id: "nestedvalue", text: "\n    value: {0}", markers: 1}
	vNextLine  = valueForm{id: "nextline", text: "\n{0}", markers: 1, lineOnly: true}
)

var (
	// kvForms are all the value forms, for the key forms where every one reads (a name, a colon or an equals sign, and a value).
	kvForms = []valueForm{
		vWord, vDouble, vSingle, vSpaced, vSpaced2, vTag, vTag2, vAnchor, vTagAnchor, vTagQuoted, vBareTag, vBare2, vBlock, vFolded, vTagBlock,
		vAnchBlock, vInline, vContBS, vContTick, vContCaret, vBearer, vBasic, vToken, vEmpty, vTagQ, vSymbols, vBackslash, vBareTick, vBareCaret, vBareBS,
		vBareBasic, vBareToken, vLitGlued, vLitDot, vLitSpaced,
	}
	// moreForms are the rarer YAML forms, crossed with the colon only: a verbatim tag, a quoted tag, a chain of tags and an anchor, a
	// tag or an anchor with nothing after it, a tag glued to a delimiter, a block scalar with an indentation indicator, a value in a
	// key under the name.
	moreForms = []valueForm{vVerbatim, vTagStr, vChain, vBang, vAmp, vGlueComma, vGlueSemi, vGlueBrace, vBlock2, vNestedVal}
	// fewForms are the forms that run on into what follows them, or end a value early, for the operators and key forms that
	// kvForms are not crossed with.
	fewForms = []valueForm{vWord, vTag, vBareTag, vBlock, vContBS, vBearer}
	// decoyForms are the forms under a key that is no secret name: only a Bearer or Basic credential is required hidden there.
	decoyForms  = []valueForm{vWord, vTag, vBareTag, vBlock, vBearer, vBasic}
	jsonForms   = []valueForm{vWord, vDouble, vSingle, vTag, vTagQuoted, vBareTag, vBlock, vContBS, vBearer, vBasic, vEmpty}
	envForms    = []valueForm{vWord, vDouble, vContBS, vBearer, vEmpty}
	flagForms   = []valueForm{vWord, vDouble, vContBS, vContTick, vBearer, vEmpty, vNextLine}
	headerForms = []valueForm{vWord, vDouble, vBearer, vBasic, vToken, vEmpty}
	entryForms  = []valueForm{vWord, vDouble, vTag, vBlock}
)

// keyNames are the names of a class, which an atom draws in turn, so that the names are all in the set without multiplying it.
var keyNames = map[string][]string{
	"secret": {"password", "api_key", "Secret", "x-token", "x_password", "apiKey"},
	"oauth":  {"access_token", "id_token", "refresh_token", "client_secret", "accessToken", "client-secret"},
	"decoy":  {"note", "user", "host"},
}

var envNames = map[string][]string{
	"secret": {"DB_PASSWORD", "PGPASSWORD", "API_TOKEN"},
	"oauth":  {"ACCESS_TOKEN", "CLIENT_SECRET"},
	"decoy":  {"LOG_LEVEL", "HOME"},
}

var flagNames = map[string][]string{
	"secret": {"password", "token", "api-key"},
	"oauth":  {"access_token", "client_secret"},
	"decoy":  {"verbose", "user"},
}

var headerNames = map[string][]string{
	"secret": {"Authorization", "X-Api-Key", "Proxy-Authorization"},
	"decoy":  {"X-Request-Id", "Accept"},
}

// opForms are the value forms that a key is written with after an operator.
type opForms struct {
	op    string
	forms []valueForm
}

// leakAlphabet builds the fields. extra adds key forms that main does not read (see leakExtra); they are for proving the
// oracle against a branch that does, and main's own run leaves them out.
func leakAlphabet(extra bool) []*leakAtom {
	var out []*leakAtom
	n := 0
	pick := func(names map[string][]string, class string) string {
		l := names[class]
		n++
		return l[n%len(l)]
	}
	vn := 0
	vary := func(f valueForm) valueForm {
		vn++
		return placeMarker(f, vn)
	}
	add := func(style, class, op string, f valueForm, tmpl string) {
		out = append(out, &leakAtom{
			id: style + "/" + class + "/" + strconv.Quote(op) + "/" + f.id + f.variant, style: style, class: class, form: f.id, op: op,
			text: tmpl, markers: f.markers, scheme: f.scheme, firstRun: f.firstRun && oauthReads(style, op),
			tail: strings.Contains(f.text, "\n") || strings.Contains(op, "\n"), lineOnly: f.lineOnly, plain: plainOf(tmpl, f),
		})
	}
	for _, class := range []string{"secret", "oauth", "decoy"} {
		plain := []opForms{{": ", append(slices.Clone(kvForms), moreForms...)}, {"=", kvForms}, {" = ", append(slices.Clone(fewForms), vHeredoc)}, {":\n    ", fewForms}, {" => ", []valueForm{vWord, vAnchor, vTag, vBareTag}}}
		jsonKey := []opForms{{":", jsonForms}, {" : ", jsonForms[:2]}, {": ", jsonForms[:2]}}
		env := []opForms{{"=", envForms}, {" = ", envForms[:1]}}
		flags := []opForms{{" ", flagForms}, {"=", []valueForm{vWord, vEmpty}}, {"-", []valueForm{vWord}}}
		if class == "decoy" {
			plain = []opForms{{": ", decoyForms}, {"=", decoyForms}}
			jsonKey = []opForms{{":", []valueForm{vWord, vBearer, vBasic}}}
			env = []opForms{{"=", []valueForm{vWord, vBearer}}}
			flags = []opForms{{" ", []valueForm{vWord}}}
		}
		for _, o := range plain {
			for _, f := range o.forms {
				if strings.HasSuffix(o.op, "=") && strings.HasPrefix(f.text, ">") {
					continue // "=>-" is a hash rocket and a dash, no operator and a folded scalar
				}
				f = vary(f)
				k := pick(keyNames, class)
				add("plain", class, o.op, f, k+o.op+f.text)
			}
		}
		for _, o := range jsonKey {
			for _, f := range o.forms {
				f = vary(f)
				k := pick(keyNames, class)
				add("json", class, o.op, f, `"`+k+`"`+o.op+f.text)
			}
		}
		if class != "decoy" {
			for _, f := range []valueForm{vWord, vTag, vBareTag} {
				f = vary(f)
				k := pick(keyNames, class)
				add("hash", class, " => ", f, "'"+k+"' => "+f.text)
			}
			for _, f := range []valueForm{vEscaped, vWord} {
				f = vary(f)
				k := pick(keyNames, class)
				add("escaped", class, ":", f, `\"`+k+`\":`+f.text)
			}
		}
		for _, o := range env {
			for _, f := range o.forms {
				f = vary(f)
				k := pick(envNames, class)
				add("env", class, o.op, f, k+o.op+f.text)
			}
		}
		for _, o := range flags {
			for _, f := range o.forms {
				f = vary(f)
				k := pick(flagNames, class)
				dash, sep := "--", o.op
				if o.op == "-" {
					dash, sep = "-", " "
				}
				add("flag", class, o.op, f, dash+k+sep+f.text)
			}
		}
	}
	for _, class := range []string{"secret", "decoy"} {
		forms := headerForms
		if class == "decoy" {
			forms = []valueForm{vWord, vBearer, vBasic}
		}
		for _, f := range forms {
			f = vary(f)
			k := pick(headerNames, class)
			add("header", class, ": ", f, k+": "+f.text)
		}
	}
	for _, c := range []struct {
		text    string
		markers int
	}{
		{"Cookie: sid={0}", 1}, {"Cookie: sid={0}; csrf={1}", 2}, {"Set-Cookie: sid={0}; Path=/", 1}, {"Cookie: sid={0}; csrf={1}; theme=dark", 2},
	} {
		f := vary(valueForm{id: "pairs", text: c.text, markers: c.markers})
		out = append(out, &leakAtom{id: "cookie/secret/" + c.text + f.variant, style: "cookie", class: "secret", form: "pairs", op: ": ", text: f.text, plain: c.text, markers: c.markers})
	}
	for _, t := range []string{"-u admin:{0}", "--user=admin:{0}", "-uadmin:{0}"} {
		f := vary(valueForm{id: "userpass", text: t, markers: 1})
		out = append(out, &leakAtom{id: "userflag/secret/" + t + f.variant, style: "userflag", class: "secret", form: "userpass", op: " ", text: f.text, plain: t, markers: 1})
	}
	for _, class := range []string{"secret", "decoy"} {
		name, forms := "DB_PASSWORD", entryForms
		if class == "decoy" {
			name, forms = "HOST", []valueForm{vWord}
		}
		for _, f := range forms {
			f = vary(f)
			add("entry", class, "newline", f, "- name: "+name+"\n  value: "+f.text)
			if class == "secret" {
				add("entry", class, "json", f, `{"name":"`+name+`","value":`+jsonValue(f)+`}`)
				add("entry", class, "comma", f, "name: "+name+", value: "+f.text)
				if f.markers > 0 && !strings.Contains(f.text, "\n") {
					add("entry", class, "gap", f, "- name: "+name+"\n  description: x\n  # note\n  value: "+f.text)
				}
			}
		}
	}
	out = append(out, &leakAtom{id: "userflag/secret/nextline", style: "userflag", class: "secret", form: "nextline", op: " ", text: "-u\nadmin:{0}", markers: 1, lineOnly: true, tail: true})
	// Secrets with a shape of their own, which need no name: the length of a real token is what makes one (so a cut one is no token, and
	// the end of a text that is cut through it must go), and the password of a URL.
	for _, sh := range []struct{ id, text string }{
		{"github", "ghp_{0}ABCDEFGHIJKLMNOPQRSTUVWX"}, {"gitlab", "glpat-{0}ABCDEFGH"}, {"stripe", "sk_live_{0}ABCDEFGHIJKL"},
	} {
		out = append(out, &leakAtom{id: "token/" + sh.id, style: "token", class: "secret", form: sh.id, text: sh.text, markers: 1, shape: true, identish: true})
	}
	for _, sh := range []struct{ id, text string }{
		{"slack", "xoxb-1234-{0}"}, {"jwt", "eyJhbGciOiJIUzI1NiJ9.{0}.c2lnbmF0dXJl"}, {"openai", "sk-{0}ABCDEFGHIJ"},
	} {
		out = append(out, &leakAtom{id: "token/" + sh.id, style: "token", class: "secret", form: sh.id, text: sh.text, markers: 1, shape: true, identish: true})
	}
	out = append(out, &leakAtom{id: "token/url", style: "token", class: "secret", form: "url", text: "https://admin:{0}@db.example/app", markers: 1, shape: true})
	for _, l := range []struct{ class, text string }{{"secret", "token"}, {"secret", "Password"}, {"decoy", "note"}} {
		out = append(out, &leakAtom{id: "label/" + l.class + "/" + l.text, style: "label", class: l.class, form: "none", op: "", text: l.text})
	}
	if extra {
		out = append(out, leakExtra()...)
	}
	return out
}

// internalChars are the characters a value may hold inside, which a rule that ends a value at one would leave the rest of.
var internalChars = []string{".", "/", "+", "=", "-", "_", "~", ":", "@", "|"}

// plainOf is the text of a field built from tmpl, which holds f's text, with the marker of the value form where the form puts it.
func plainOf(tmpl string, f valueForm) string {
	if f.base == "" {
		return tmpl
	}
	return strings.Replace(tmpl, f.text, f.base, 1)
}

// placeMarker moves the marker of a value form: n % 3 picks the marker at the start of the value (as written), at its end after text
// and an internal character, or in the middle with an internal character and text after it. The character comes round with n. A
// Basic credential is base64, so it takes "+" or "/".
func placeMarker(f valueForm, n int) valueForm {
	if f.markers == 0 || f.lineOnly || !strings.Contains(f.text, "{0}") || slices.Contains([]string{"symbols", "backslash", "escaped"}, f.id) {
		return f
	}
	if n%3 != 0 {
		f.base = f.text
	}
	c := internalChars[(n/3)%len(internalChars)]
	if f.scheme == "Basic" {
		c = []string{"+", "/"}[(n/3)%2]
	}
	switch n % 3 {
	case 1:
		f.text, f.variant = strings.Replace(f.text, "{0}", "x"+c+"{0}", 1), "#end"
	case 2:
		f.text, f.variant = strings.Replace(f.text, "{0}", "{0}"+c+"z9", 1), "#mid"
	}
	return f
}

// plainText is the text of the field with the marker where its value form puts it.
func (a *leakAtom) plainText() string {
	if a.plain != "" {
		return a.plain
	}
	return a.text
}

// endPaths are the paths a value takes through the rules of redaction, each with the fields that stand for it in the end matrix: a
// name and its value, a Kubernetes env entry's value, a flag's value, a user flag's password, a cookie's value, an OAuth field's first
// word in the token layer, a Bearer credential and a Basic one.
var endPaths = []struct {
	path                   string
	style, class, op, form string
}{
	{"names", "plain", "secret", ": ", "word"}, {"names", "plain", "secret", "=", "word"}, {"names", "plain", "secret", ": ", "double"},
	{"names", "plain", "secret", "=", "single"}, {"names", "plain", "secret", ": ", "spaced"}, {"names", "json", "secret", ":", "word"},
	{"names", "header", "secret", ": ", "word"},
	{"entry", "entry", "secret", "newline", "word"}, {"entry", "entry", "secret", "comma", "word"}, {"entry", "entry", "secret", "json", "word"},
	{"flag", "flag", "secret", " ", "word"}, {"flag", "flag", "secret", "=", "word"}, {"flag", "flag", "secret", "-", "word"},
	{"userflag", "userflag", "secret", " ", "userpass"},
	{"cookie", "cookie", "secret", ": ", "pairs"},
	{"oauth", "plain", "oauth", ": ", "word"}, {"oauth", "plain", "oauth", "=", "word"}, {"oauth", "plain", "oauth", ": ", "double"},
	{"oauth", "json", "oauth", ":", "word"}, {"oauth", "env", "oauth", "=", "word"}, {"oauth", "flag", "oauth", "=", "word"},
	{"bearer", "plain", "secret", ": ", "bearer"}, {"bearer", "header", "secret", ": ", "bearer"}, {"bearer", "plain", "decoy", ": ", "bearer"},
	{"basic", "header", "secret", ": ", "basic"}, {"basic", "plain", "secret", ": ", "basic"}, {"basic", "plain", "decoy", ": ", "basic"},
}

// endPrefix is the word before the internal character in the end matrix: two or more letters and digits, like the version prefix of a
// token ("ya29.").
const endPrefix = "ya29"

// endChars are the internal characters the end matrix puts before a marker: a Basic credential is base64, so it takes two of them.
func endChars(path string) []string {
	if path == "basic" {
		return []string{"+", "/"}
	}
	return internalChars
}

// endMatrix returns, for each path of endPaths and each internal character that a value on it can hold, a field whose value is the
// marker after "x" and that character: every way a rule can cut a value short at one, on every path. A field that is not of the
// alphabet (a user flag has three forms; the cookie has two) is there for each of its forms.
func endMatrix(atoms []*leakAtom) []*leakAtom {
	var out []*leakAtom
	for _, p := range endPaths {
		seen := map[string]bool{}
		for _, a := range atoms {
			if a.style != p.style || a.class != p.class || a.op != p.op || a.form != p.form || a.lineOnly || a.markers == 0 || seen[a.plainText()] {
				continue
			}
			seen[a.plainText()] = true
			for _, c := range endChars(p.path) {
				v := *a
				v.endChar, v.path = c, p.path
				v.text = strings.Replace(a.plainText(), "{0}", endPrefix+c+"{0}", 1)
				v.id = strings.SplitN(a.id, "#", 2)[0] + "#end" + strconv.Quote(c)
				v.plain = a.plainText()
				v.tail = strings.Contains(v.text, "\n") || strings.Contains(v.op, "\n")
				out = append(out, &v)
			}
		}
	}
	return out
}

// oauthReads reports whether the OAuth rule of IdentifierText reads a key written in style with op: a name and a ":" or an "="
// (not the "=>" of a hash, and not a flag's space).
func oauthReads(style, op string) bool {
	if strings.Contains(op, "=>") {
		return false
	}
	switch style {
	case "plain", "json", "escaped", "env":
		return true
	case "flag":
		return op == "="
	}
	return false
}

// jsonValue is a form as it is written inside a JSON object: quoted, whatever the form says.
func jsonValue(f valueForm) string {
	if f.markers == 0 {
		return `""`
	}
	return `"{0}"`
}

// leakExtra are key forms that main does not read, for the proof run against a branch that does (-leak.extra): a secret
// name in Markdown emphasis, in backticks and in HTML emphasis, and Go's := (RF-1's forms).
func leakExtra() []*leakAtom {
	var out []*leakAtom
	for _, f := range []valueForm{vWord, vDouble, vTag, vBareTag, vBlock, vContBS, vBearer, vBasic, vEmpty} {
		for _, k := range []struct{ class, key, op string }{
			{"oauth", "**access_token:**", " "}, {"oauth", "**id_token**:", " "}, {"secret", "**password:**", " "}, {"secret", "`password`", ": "},
			{"secret", "`DB_PASSWORD`", "="}, {"secret", "<b>password</b>", ": "}, {"secret", "password", " := "}, {"oauth", "`access_token`", ": "},
		} {
			out = append(out, &leakAtom{
				id: "extra/" + k.class + "/" + k.key + "/" + f.id, style: "extra", class: k.class, form: f.id, op: k.op, text: k.key + k.op + f.text, key: k.key,
				markers: f.markers, scheme: f.scheme,
			})
		}
	}
	return out
}

// leakLayout joins the fields of an input.
type leakLayout struct {
	id   string
	join func(parts []string) string
}

var leakLayouts = []leakLayout{
	{"space", func(p []string) string { return strings.Join(p, " ") }},
	{"comma", func(p []string) string { return strings.Join(p, ", ") }},
	{"newline", func(p []string) string { return strings.Join(p, "\n") }},
	{"flow", func(p []string) string { return "{" + strings.Join(p, ", ") + "}" }},
	{"cell", func(p []string) string { return "| " + strings.Join(p, " | ") + " |" }},
	{"list", func(p []string) string { return "- " + strings.Join(p, "\n- ") }},
}

// leakMarker is a planted value, and what the oracle requires of it.
type leakMarker struct {
	text      string // the planted word; every window of markerWindow bytes of it is unique in the input
	atom, idx int    // which field, and which of its markers
	pos       int    // where the marker is in the text
	free      bool   // must not appear in the free-text entries
	line      bool   // must not appear in the free-text entries that join the lines
	ident     bool   // must not appear in IdentifierText
}

// leakCase is one generated input.
type leakCase struct {
	atoms   []*leakAtom
	layout  leakLayout
	text    string
	markers []leakMarker
	starts  []int // where the text of each field begins in text
	ends    []int // where it ends
	wrap    string
}

// String names the case by its fields and layout, for reports.
func (c leakCase) String() string {
	ids := make([]string, len(c.atoms))
	for i, a := range c.atoms {
		ids[i] = a.id
	}
	return c.layout.id + c.wrap + ": " + strings.Join(ids, " + ")
}

// markerLen is how long a marker is; markerWindow is how much of one the oracle takes for a leak: any markerWindow bytes of it.
const (
	markerLen    = 12
	markerWindow = 4
)

// markerWords are the planted words: markerLen letters and digits each, mixed so that none is a plain word (a Basic credential of a
// plain word is kept), made so that every window of markerWindow bytes of one is in no other and in no key, tag or filler of the
// generator (TestLeakMarkersAreUnique holds it). The first two fields of an input take the first four, the third the last two.
var markerWords = makeMarkerWords(8)

// makeMarkerWords returns n markers from a fixed pseudo-random sequence, so the same run always plants the same words.
func makeMarkerWords(n int) []string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	seen := map[string]bool{}
	var out []string
	state := uint32(20261008)
	next := func() int {
		state = state*1664525 + 1013904223
		return int(state >> 8)
	}
	for len(out) < n {
		w := make([]byte, markerLen)
		for i := range w {
			w[i] = alphabet[next()%len(alphabet)]
		}
		word, ok := string(w), true
		hasUpper, hasLower, hasDigit := false, false, false
		for _, r := range word {
			hasUpper = hasUpper || r >= 'A' && r <= 'Z'
			hasLower = hasLower || r >= 'a' && r <= 'z'
			hasDigit = hasDigit || r >= '2' && r <= '9'
		}
		for i := 0; i+markerWindow <= markerLen; i++ {
			if seen[word[i:i+markerWindow]] {
				ok = false
			}
		}
		if !ok || !hasUpper || !hasLower || !hasDigit {
			continue
		}
		for i := 0; i+markerWindow <= markerLen; i++ {
			seen[word[i:i+markerWindow]] = true
		}
		out = append(out, word)
	}
	return out
}

// markerText is the planted word of marker j of field i.
func markerText(i, j int) string { return markerWords[2*i+j] }

// windowIndex returns where the first window of word (in word's order) that is in out is, or -1.
func windowIndex(word, out string) int {
	best := -1
	for i := 0; i+markerWindow <= len(word); i++ {
		if j := strings.Index(out, word[i:i+markerWindow]); j >= 0 && (best < 0 || j < best) {
			best = j
		}
	}
	return best
}

// shownIn reports whether any window of the marker is in out.
func (m leakMarker) shownIn(out string) bool { return windowIn(m.text, out) }

// windowIn reports whether any markerWindow bytes of word are in out.
func windowIn(word, out string) bool {
	for i := 0; i+markerWindow <= len(word); i++ {
		if strings.Contains(out, word[i:i+markerWindow]) {
			return true
		}
	}
	return false
}

// newLeakCase renders atoms in layout and plants a marker in each value.
func newLeakCase(atoms []*leakAtom, layout leakLayout) leakCase {
	c := leakCase{atoms: atoms, layout: layout}
	parts := make([]string, len(atoms))
	for i, a := range atoms {
		parts[i] = strings.NewReplacer("{0}", markerText(i, 0), "{1}", markerText(i, 1)).Replace(a.text)
		for j := 0; j < a.markers; j++ {
			c.markers = append(c.markers, leakMarker{text: markerText(i, j), atom: i, idx: j, free: a.mustFree(j), line: a.mustLine(j), ident: a.mustIdent(j)})
		}
	}
	c.text = layout.join(parts)
	at := 0
	for _, part := range parts {
		i := at + strings.Index(c.text[at:], part)
		c.starts, c.ends = append(c.starts, i), append(c.ends, i+len(part))
		at = i + len(part)
	}
	for i := range c.markers {
		c.markers[i].pos = strings.Index(c.text, c.markers[i].text)
	}
	return c
}

// leakWrap is text before and after the fields of an input: prose, a delimiter, more lines.
type leakWrap struct{ id, prefix, suffix string }

var leakWraps = []leakWrap{
	{" +comma", "", ","}, {" +bar", "", " |"}, {" +prose", "", " and then the run failed."}, {" +lines", "", "\n  more"},
	{" +cell", "| ", " |"}, {" +literal", "[redacted] ", " [redacted]"}, {" +words", "The agent printed: ", ""}, {" +words+prose", "The agent printed: ", " and then the run failed."},
	leakLongWrap,
}

// leakLongWrap is a tail of prose so long that a Cut entry's raw cut (cutFactor times the limit) falls short of the text at the limits
// that matter: the path where the token layer reads a text that was cut.
var leakLongWrap = leakWrap{" +long", "", " " + strings.Repeat("and then the run failed. ", 9)}

// wrapCase returns the case with the wrap around its text.
func wrapCase(c leakCase, w leakWrap) leakCase {
	out := c
	out.text, out.wrap = w.prefix+c.text+w.suffix, w.id
	out.starts, out.ends = make([]int, len(c.starts)), make([]int, len(c.ends))
	for i := range c.starts {
		out.starts[i], out.ends[i] = c.starts[i]+len(w.prefix), c.ends[i]+len(w.prefix)
	}
	out.markers = slices.Clone(c.markers)
	for i := range out.markers {
		out.markers[i].pos += len(w.prefix)
	}
	return out
}

// wrapped returns each case once in each wrap.
func wrapped(cases []leakCase) []leakCase {
	var out []leakCase
	for _, c := range cases {
		for _, w := range leakWraps {
			out = append(out, wrapCase(c, w))
		}
	}
	return out
}

// opVariants returns the fields of plain and env style that have another way to write the operator: with no space after the colon,
// a tab after it, "==" and "?=" for "=" (the operators that take the name and the value to be glued or spaced differently).
func opVariants(atoms []*leakAtom) []*leakAtom {
	var out []*leakAtom
	for _, a := range atoms {
		if a.class == "decoy" || a.style != "plain" && a.style != "env" {
			continue
		}
		var ops []string
		switch a.op {
		case ": ":
			ops = []string{":", ":\t"}
		case "=":
			ops = []string{"==", "?="}
		case " = ":
			ops = []string{"\t=\t"}
		}
		for _, op := range ops {
			v := *a
			v.id, v.op = a.id+"@"+strconv.Quote(op), op
			v.text = strings.Replace(a.text, a.op, op, 1)
			v.firstRun = a.firstRun && oauthReads(a.style, op) && !strings.Contains(op, "?")
			out = append(out, &v)
		}
	}
	return out
}

// mapping reports whether the layouts that put fields in a flow mapping can hold the field.
func (a *leakAtom) mapping() bool {
	switch a.style {
	case "plain", "json", "hash", "escaped":
		return true
	}
	return false
}

// valid reports whether the fields make text that reads as fields in the layout: a flow mapping holds only keys and values,
// and the next field of a line cannot follow one that ends in the lines of a block scalar or a continuation.
func (l leakLayout) valid(atoms []*leakAtom) bool {
	for i, a := range atoms {
		if l.id == "flow" && !a.mapping() {
			return false
		}
		if a.tail && i < len(atoms)-1 && l.id != "newline" && l.id != "list" {
			return false
		}
	}
	return true
}

// leakSingles enumerates the inputs of one field of alphabet, in every layout.
func leakSingles(alphabet []*leakAtom) []leakCase {
	var out []leakCase
	for _, l := range leakLayouts {
		for _, a := range alphabet {
			if l.valid([]*leakAtom{a}) {
				out = append(out, newLeakCase([]*leakAtom{a}, l))
			}
		}
	}
	return out
}

// leakSinglesIn is leakSingles for the layouts with the ids given (all, if none).
func leakSinglesIn(alphabet []*leakAtom, layouts ...string) []leakCase {
	var out []leakCase
	for _, l := range leakLayouts {
		if len(layouts) > 0 && !slices.Contains(layouts, l.id) {
			continue
		}
		for _, a := range alphabet {
			if l.valid([]*leakAtom{a}) {
				out = append(out, newLeakCase([]*leakAtom{a}, l))
			}
		}
	}
	return out
}

// leakCross enumerates the inputs of a field of as and a field of bs, in either order and in every layout, and takes every
// step-th of them, from offset.
func leakCross(as, bs []*leakAtom, step, offset int) []leakCase {
	var out []leakCase
	n := 0
	for _, l := range leakLayouts {
		for _, a := range as {
			for _, b := range bs {
				for _, pair := range [][]*leakAtom{{a, b}, {b, a}} {
					if n++; (n+offset)%step == 0 && l.valid(pair) {
						out = append(out, newLeakCase(pair, l))
					}
				}
			}
		}
	}
	return out
}

// leakPairs enumerates the inputs of two fields of alphabet, in every layout, and takes every step-th of them, from offset (1
// for all), so that a sample spreads over the whole set.
func leakPairs(alphabet []*leakAtom, step, offset int) []leakCase {
	var out []leakCase
	n := 0
	for _, l := range leakLayouts {
		for _, a := range alphabet {
			for _, b := range alphabet {
				if n++; (n+offset)%step == 0 && l.valid([]*leakAtom{a, b}) {
					out = append(out, newLeakCase([]*leakAtom{a, b}, l))
				}
			}
		}
	}
	return out
}

// leakTriples is leakPairs for three fields.
func leakTriples(alphabet []*leakAtom, step, offset int) []leakCase {
	var out []leakCase
	n := 0
	for _, l := range leakLayouts {
		for _, a := range alphabet {
			for _, b := range alphabet {
				for _, c := range alphabet {
					if n++; (n+offset)%step == 0 && l.valid([]*leakAtom{a, b, c}) {
						out = append(out, newLeakCase([]*leakAtom{a, b, c}, l))
					}
				}
			}
		}
	}
	return out
}

// interplay is the alphabet of the three-field sweep: the plain key form with the value forms that run on into what follows
// them, and one of each of the other styles.
func interplay(all []*leakAtom) []*leakAtom {
	var out []*leakAtom
	for _, a := range all {
		switch {
		case a.style == "plain" && a.op == ": " && a.class != "decoy" && (a.form == "word" || a.form == "tag" || a.form == "baretag" || a.form == "block" || a.form == "contbackslash" || a.form == "basic" || a.form == "empty"):
			out = append(out, a)
		case a.style == "json" && a.op == ":" && a.class == "oauth" && (a.form == "double" || a.form == "baretag"):
			out = append(out, a)
		case a.style == "flag" && a.op == " " && a.class == "secret" && (a.form == "word" || a.form == "empty"):
			out = append(out, a)
		case a.style == "header" && a.class == "secret" && a.form == "bearer":
			out = append(out, a)
		case a.style == "entry" && a.class == "secret" && a.op == "newline" && a.form == "word":
			out = append(out, a)
		case a.style == "cookie" && a.markers == 2, a.style == "userflag" && a.text == "-u admin:{0}", a.style == "label" && a.class == "secret":
			out = append(out, a)
		}
	}
	return out
}

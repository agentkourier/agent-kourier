package redact

import (
	"slices"
	"strings"
	"testing"
)

// The known limits: the ways main leaks that the oracle knows, one row each. A row is a shape of input that an entry shows a secret of,
// pinned by examples that must still leak, so that a fix fails TestLeakLimitsStillLeak and the row goes. Its applies function says
// where main shows a marker, and it is held to that from both sides: a marker that main shows and no row explains fails the oracle,
// and so does a row that applies to a marker main hides ("limit too broad"). Later work packages may only narrow a predicate or
// delete a row, never widen one or add one without the orchestrator's sign-off. docs/reference/stored-data.md's limits paragraph points here.
//
// leakLimit is one row.
type leakLimit struct {
	id, reason string
	// examples are inputs whose secret, the word SECRETA, an entry shows.
	examples []leakExample
	// retiredBy is the work package that fixes it, and so deletes the row.
	retiredBy string
	// extra says the row is for the key forms that -leak.extra adds, which the sweep checks only with it.
	extra bool
	// applies says whether main shows marker m of case c in entry e when the entry reads limit bytes (noCut for an entry that is not
	// cut). It must be true only where main shows it: a slot where it applies and main hides the marker fails the oracle.
	applies func(s leakSlot) bool
}

// leakExample is an input and which entries show its secret: the four free-text entries (free), IdentifierText (ident: a secret
// that IdentifierText must hide and shows. It shows secrets that are no shape of its own, by design, and the table does not list
// those), or only the two free-text entries that join the lines (lines: the others keep a line break between a flag and a word,
// and are not to hide the word).
type leakExample struct {
	in                 string
	free, ident, lines bool
}

// knownLimit returns the id of the limit that explains the marker m of the case c in the entry e, or "".
func knownLimit(s leakSlot) string {
	if cutDrops(s) || !s.e.ident && !s.c.atoms[s.m.atom].lineOnly && (earlierCovers(s) || swallowCovers(s)) {
		return ""
	}
	for _, l := range leakLimits {
		if l.applies(s) {
			return l.id
		}
	}
	return ""
}

// oauthMarks are the value forms that the OAuth rule of the token layer takes the first word of for its value, and so leaves the
// secret behind: a tag, a block header, a continuation mark, an authorization scheme that is no Bearer.
var oauthMarks = []string{
	"tag", "tag2", "folded", "taganchor", "tagquoted", "tagblock", "block", "blockinline", "contbacktick", "contcaret", "tokenscheme", "basic", "tagstring",
	"verbatim", "tagq", "chain", "block2", "heredoc", "nestedvalue",
}

// unreadKeys are the key forms of leakExtra that main does not read; "**password:**" and "**id_token**:" it does.
var unreadKeys = map[string]bool{
	"**access_token:**": true, "`password`": true, "`DB_PASSWORD`": true, "<b>password</b>": true, "password": true, "`access_token`": true,
}

var leakLimits = []leakLimit{
	{
		id:        "key-forms-are-not-read",
		retiredBy: "RF-1d",
		extra:     true,
		reason: "A secret name in backticks (`password`, `DB_PASSWORD`), in HTML emphasis (<b>password</b>) or before Go's := (password := v), and an OAuth name with " +
			"the colon inside Markdown emphasis (**access_token:** v), is no name and operator to the rules: the value is shown.",
		examples: []leakExample{
			{"`password`: SECRETA", true, false, false},
			{"<b>password</b>: SECRETA", true, false, false},
			{"password := SECRETA", true, false, false},
			{"**access_token:** SECRETA", true, false, false},
		},
		applies: func(s leakSlot) bool {
			a := s.c.atoms[s.m.atom]
			return a.style == "extra" && !s.e.ident && a.scheme == "" && unreadKeys[a.key]
		},
	},
	{
		id:        "question-equals-is-no-operator",
		retiredBy: "RF-2",
		reason:    "A secret name and \"?=\" (a shell default, make's conditional assignment) is no name and operator to the rules that read names: the value is shown.",
		examples: []leakExample{
			{"password?=SECRETA", true, false, false},
			{"API_TOKEN?=\"SECRETA\"", true, false, false},
		},
		applies: func(s leakSlot) bool {
			a := s.c.atoms[s.m.atom]
			return a.op == "?=" && !s.e.ident && a.scheme == "" && !a.shape // the token layer reads a credential whatever stands before it
		},
	},
	{
		id:        "doubled-equals-hides-the-mark",
		retiredBy: "RF-2",
		reason: "After \"==\" the value is the run that begins with the second \"=\" (for an OAuth name, up to a quote, a comma or the like), so a tag, a block header, a " +
			"continuation mark or a scheme after it is no tag, no header and no mark: the value is that first run, and what follows it is shown.",
		examples: []leakExample{
			{"password==!x SECRETA", true, false, false},
			{"password==|\n  SECRETA", true, false, false},
			{"access_token==\"SECRETA\"", true, false, false},
		},
		applies: func(s leakSlot) bool {
			c, m := s.c, s.m
			a := c.atoms[m.atom]
			if a.op != "==" || s.e.ident && a.class != "oauth" || a.scheme == "Bearer" || a.scheme == "Basic" && a.class != "oauth" || a.shape {
				return false
			}
			at := strings.Index(c.text[c.starts[m.atom]:], a.op)
			if at < 0 {
				return false
			}
			v := c.starts[m.atom] + at + len(a.op) - 1 // the value is the run that begins with the second equals sign
			if a.class == "oauth" {
				return strings.ContainsAny(c.text[v:m.pos], " \t\n\"',;}&\\")
			}
			return plainEnd(c.text, v) < m.pos
		},
	},
	{
		id:        "oauth-value-is-the-mark",
		retiredBy: "RF-1o (the narrow OAuth fix)",
		reason: "The token layer reads an OAuth field's value as the first word after the separator, so a tag (!x, !!str, !\"a\"), a block header (|, >-), " +
			"a continuation mark (a backtick, a caret) or the scheme Basic or Token is taken for the value and replaced, and the real value after it " +
			"is not read again: a name's own rules see a value already taken.",
		examples: []leakExample{
			{"id_token: !x SECRETA", true, false, false},
			{"access_token: |\n  SECRETA", true, false, false},
			{"client_secret=!!str SECRETA", true, false, false},
			{"refresh_token: !!str \"SECRETA\"", true, false, false},
			{"access_token: Basic SECRETA1B2C3", true, true, false},
			{"access_token: !\"a\" SECRETA", true, false, false},
			{"id_token: `\n  SECRETA", true, false, false},
		},
		applies: func(s leakSlot) bool {
			c, m := s.c, s.m
			a := c.atoms[m.atom]
			return a.class == "oauth" && (a.style == "plain" || a.style == "json") && slices.Contains(oauthMarks, a.form) && a.op != "?=" && a.op != "==" && !tokenSwallowed(s)
		},
	},
	{
		id:        "oauth-hash-rocket",
		retiredBy: "RF-1o (the narrow OAuth fix)",
		reason:    "The OAuth rule knows \"=\" and \":\" and not \"=>\": it takes \">\" for the value of 'access_token' => x, and x is left.",
		examples: []leakExample{
			{"'access_token' => SECRETA", true, true, false},
			{"{'id_token' => !x SECRETA}", true, true, false},
		},
		applies: func(s leakSlot) bool {
			c, m := s.c, s.m
			a := c.atoms[m.atom]
			return a.class == "oauth" && (a.style == "hash" || strings.Contains(a.op, "=>")) && !tokenSwallowed(s)
		},
	},
	{
		id:        "oauth-value-stops-at-a-backslash",
		retiredBy: "RF-2",
		reason: "The OAuth rule ends a value at a quote, a comma, a semicolon, a brace or a backslash, so what follows a backslash in a value is left " +
			"(documented in secrets, with the escaped quote of TestSecretsKnownLimits).",
		examples: []leakExample{
			{"refresh_token=</b>!\\SECRETA", true, true, false},
		},
		applies: func(s leakSlot) bool {
			c, m := s.c, s.m
			a := c.atoms[m.atom]
			return a.class == "oauth" && a.form == "backslash" && !tokenSwallowed(s)
		},
	},
	{
		id:        "at-sign-ends-a-user-password",
		retiredBy: "RF-2",
		reason: "The rule for \"user:password@host\" without a scheme runs before the rule for a user flag, and takes the user and what comes before the \"@\" for the " +
			"credentials, so a password that holds an \"@\" (\"-u admin:x@v\") leaves what follows it.",
		examples: []leakExample{
			{"-u admin:x@SECRETA", true, false, false},
			{"--user=admin:pa@SECRETA", true, false, false},
		},
		applies: func(s leakSlot) bool {
			a := s.c.atoms[s.m.atom]
			return atSignEnds(a) && !s.e.ident
		},
	},
	{
		id:        "taken-out-marker-ends-the-value",
		retiredBy: "RF-2",
		reason: "A value that begins with the marker of a value taken out (the text \"[redacted]\" in the input) ends there when white space follows, as the marker of the " +
			"pass over a string does (\"password=[redacted]v\" is all value, \"password=[redacted] v\" is not): what follows the white space is shown.",
		examples: []leakExample{
			{"password: [redacted] SECRETA", true, false, false},
			{"access_token=[redacted] SECRETA", true, false, false},
		},
		applies: func(s leakSlot) bool {
			a := s.c.atoms[s.m.atom]
			return a.form == "litspaced" && a.class != "decoy" && !s.e.ident
		},
	},
	{
		id:        "heredoc-body-is-not-read",
		retiredBy: "RF-2",
		reason: "A shell or Terraform heredoc under a secret name (\"password = <<EOF\", the secret on the lines after it) has \"<<EOF\" for its value, and the lines of the " +
			"body are not read.",
		examples: []leakExample{
			{"password = <<EOF\nSECRETA\nEOF", true, false, false},
		},
		applies: func(s leakSlot) bool {
			a := s.c.atoms[s.m.atom]
			return a.class == "secret" && a.form == "heredoc" && !s.e.ident
		},
	},
	{
		id:        "block-header-under-the-name",
		retiredBy: "RF-2",
		reason:    "A block scalar's header on a line of its own under the name (\"password:\", then \"|\" indented) is read as the value, and the lines under it are not.",
		examples: []leakExample{
			{"password:\n    |\n    SECRETA", true, false, false},
		},
		applies: func(s leakSlot) bool {
			c, m, e := s.c, s.m, s.e
			a := c.atoms[m.atom]
			return !e.ident && a.class == "secret" && a.style == "plain" && a.form == "block" && strings.Contains(a.op, "\n")
		},
	},
	{
		id:        "tag-takes-the-next-key",
		retiredBy: "RF-1c",
		reason: "A secret name whose value is only a tag (\"password: !x\", \"!!str\") takes the next word for the rest of its value (tagged), and when " +
			"that word is the next field's name or a flag, the field's own value is left.",
		examples: []leakExample{
			{"password: !x token: SECRETA", true, false, false},
			{"password: !!str --password SECRETA", true, false, false},
			{"{password: !x, api_key: SECRETA}", true, false, false},
		},
		applies: func(s leakSlot) bool {
			sw, ok := swallowOf(s)
			return ok && sw.kind == "tag" && sw.leaks(s)
		},
	},
	{
		id:        "continuation-takes-the-next-key",
		retiredBy: "RF-1c",
		reason: "A secret name whose value is only a continuation mark (a backtick, a caret, a backslash) that ends its word takes the first word after it, " +
			"on the next line too, for its value (continued), and when that word is the next field's name or a flag, the field's own value is left.",
		examples: []leakExample{
			{"password: ^ token = `SECRETA`", true, false, false},
			{"password: ` --password SECRETA", true, false, false},
		},
		applies: func(s leakSlot) bool {
			sw, ok := swallowOf(s)
			return ok && sw.kind == "continuation" && sw.leaks(s)
		},
	},
	{
		id:        "scheme-takes-the-next-key",
		retiredBy: "RF-1c",
		reason: "A secret name whose value is only an authorization scheme (\"password: Basic\", \"Token\") takes the next word on its line (valueEnd), and Basic takes " +
			"a run of base64 characters that follows it over a line break (the token layer): when that word is the next field's name or a flag, the field's own value is left.",
		examples: []leakExample{
			{"password: Token --password SECRETA", true, false, false},
			{"password: Basic -u admin:SECRETA", true, false, false},
			{"password: Basic\nPGPASSWORD= SECRETA", true, false, false},
		},
		applies: func(s leakSlot) bool {
			sw, ok := swallowOf(s)
			return ok && sw.kind == "scheme" && sw.leaks(s)
		},
	},
	{
		id:        "empty-takes-the-next-run",
		retiredBy: "RF-1c",
		reason: "A secret name with no value takes the first run of what follows, on its line or the next, for its value (\"x-token:  Secret = v\" has the value \"Secret\"), " +
			"unless that run is a key the name's rules read as one (a secret name and a colon, or on the next line a pair \"name = value\", which they take whole); a flag, a user flag " +
			"or a pair whose value is a tag or a block header is not, and its value stays.",
		examples: []leakExample{
			{"token:  secret = SECRETA", true, false, false},
			{"token: secret=>&a SECRETA", true, false, false},
			{"password: --password SECRETA", true, false, false},
			{"password: -u admin:SECRETA", true, false, false},
			{"password: \napi_key=|\n    SECRETA", true, false, false},
			{"--password  token --token SECRETA", true, false, false},
		},
		applies: func(s leakSlot) bool {
			sw, ok := swallowOf(s)
			return ok && (sw.kind == "empty" || sw.kind == "flag") && sw.leaks(s)
		},
	},
	{
		id:        "block-header-takes-the-rest-of-its-line",
		retiredBy: "RF-2",
		reason: "FreeTextLine reads the lines before it joins them, and a block scalar's header (\"password: | x\", or the bar of a table cell after a name with no " +
			"value) takes the rest of its line, so a flag at the end of that line, whose value is on the next line, is gone when the lines are joined and read again.",
		examples: []leakExample{
			{"password: | x --token\nSECRETA", false, false, true},
			{"| ACCESS_TOKEN= | -u\nadmin:SECRETA |", false, false, true},
		},
		applies: func(s leakSlot) bool {
			return s.c.atoms[s.m.atom].lineOnly && s.e.line && coversAt(s, s.m.atom, s.c.starts[s.m.atom])
		},
	},
	{
		id:        "oauth-empty-takes-the-next-key",
		retiredBy: "RF-1o (the narrow OAuth fix)",
		reason: "An OAuth field with no value takes the next word, on its line or the next, for its value (the value is a run up to a quote, a comma, a semicolon, " +
			"a brace, an ampersand or a backslash), so the name of the field that follows is redacted and its own value is not read.",
		examples: []leakExample{
			{"client_secret:  api_key: SECRETA", true, true, false},
			{"access_token: \npassword: SECRETA", true, true, false},
			{"id_token: refresh_token='SECRETA'", true, true, false},
		},
		applies: func(s leakSlot) bool {
			sw, ok := swallowOf(s)
			return ok && sw.kind == "token" && sw.leaks(s)
		},
	},
}

// secretA is the secret word of the examples of leakLimits.
const secretA = "SECRETA"

// TestLeakLimitsStillLeak pins the known limits: every example still shows its secret in exactly the entries it names. A fix makes
// an example pass through, and this test fails until the example (and, when its last one goes, the limit) moves out of the table
// into a corpus case.
func TestLeakLimitsStillLeak(t *testing.T) {
	for _, l := range leakLimits {
		if l.reason == "" || len(l.examples) == 0 || l.applies == nil || l.retiredBy == "" {
			t.Errorf("limit %q needs a reason, examples, a rule and the work package that retires it", l.id)
		}
		for _, ex := range l.examples {
			if !strings.Contains(ex.in, secretA) {
				t.Errorf("%s: %q holds no %s", l.id, ex.in, secretA)
			}
			for _, e := range leakEntries {
				got := e.fn(ex.in, noCut)
				shows := strings.Contains(got, secretA)
				var fixed bool
				switch {
				case e.ident:
					fixed = ex.ident && !shows
				case ex.free:
					fixed = !shows
				case ex.lines && e.line:
					fixed = !shows
				case !ex.lines:
					fixed = shows
				}
				if fixed {
					t.Errorf("%s: %s(%q) = %q; it no longer behaves as the limit says (fixed? move it out of the table)", l.id, e.name, ex.in, got)
				}
			}
		}
	}
}

// The limits are told apart: every id is once in the table.
func TestLeakLimitIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, l := range leakLimits {
		if seen[l.id] {
			t.Errorf("limit %q is in the table twice", l.id)
		}
		seen[l.id] = true
	}
}

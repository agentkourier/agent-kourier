package redact

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// FuzzEntriesNeverPanic runs every entry point over whatever the fuzzer builds. The oracle is that none panics (the rules slice
// text by indexes they compute from matches, and a redaction that reaches over the next match once put one of them out of
// range), that what comes back is valid UTF-8 and normalized (no invisible character, no line break but "\n"), that
// FreeTextLine and IdentifierText return one line, that IdentifierText, whose rules have no line to read, is idempotent, and
// that a secret planted in the text, a pair of its own on a line of its own (see plantedFuzzSecret), does not show whatever
// the text around it is. The seeds are the inputs that broke earlier versions, a sample of the corpus and the shapes of the
// rules that main does not have yet. Run it with "go test -run '^$' -fuzz FuzzEntriesNeverPanic ./internal/redact".
func FuzzEntriesNeverPanic(f *testing.F) {
	for _, s := range []string{
		"access_token: !x access_token=y", "id_token: !!str id_token=y", `access_token: !"access_token: x"`, "access_token=!'id_token=y'",
		"access_token: !<id_token=y>", "access_token: | access_token=y", "access_token: !!binary | id_token=y", "access_token: !x | id_token=y",
		"--passwordBearerCookieaccess_token=\\\"!!binary access_token=Bearer", ":client_secret\\\"=>!<\\\"id_token\\\"::=!=>sid=",
		"**access_token:** x", "**id_token**=>** x", "~access_token~=>|\n  x", "`password`: x", "<b>password</b>: x", "password := \"x\"",
		"| password | x |", "| Key | Value |\n|---|---|\n| password | x |", "| token | password=\"a|x\" |", "| password | password: \\\na|x |",
		"Credentials: password = x", "echo $TOKEN | base64 -d | jq . |", "password: |\n  x\nnext: 1", "password: !!str &a x", "secret:\n  `token`: x",
		"Cookie: a=b; sid=a|x", "password: password=password=password=", "BAsiC", "access_token:", "password: !x", "password: ^", "\x00\xff\u200b|\r\n|", "&lt;b&gt;password&lt;/b&gt;: x",
	} {
		f.Add(s)
	}
	corpus := redacttest.SplitSecretsSample()
	for i := 0; i < len(corpus); i += 31 { // a case of every template, not the first few hundred
		f.Add(corpus[i].Text)
	}
	f.Fuzz(func(t *testing.T, s string) {
		plantedFuzzSecret(t, s)
		for name, got := range map[string]string{
			"FreeText":        FreeText(s),
			"FreeTextLine":    FreeTextLine(s),
			"FreeTextCut":     FreeTextCut(s, 64),
			"FreeTextCutLine": FreeTextCutLine(s, 64),
			"IdentifierText":  IdentifierText(s),
		} {
			if !utf8.ValidString(got) {
				t.Fatalf("%s(%q) = %q is not valid UTF-8", name, s, got)
			}
			if strings.ContainsAny(got, "\r\u0085\u2028\u2029\x00") {
				t.Fatalf("%s(%q) = %q holds a character that normalization deletes or a line break that is not \\n", name, s, got)
			}
			if (name == "FreeTextLine" || name == "FreeTextCutLine" || name == "IdentifierText") && strings.Contains(got, "\n") {
				t.Fatalf("%s(%q) = %q is not one line", name, s, got)
			}
		}
		if once := IdentifierText(s); IdentifierText(once) != once {
			t.Fatalf("IdentifierText is not idempotent: %q became %q then %q", s, once, IdentifierText(once))
		}
	})
}

// plantedFuzzSecret plants a secret of its own in s, in the middle of it and at its end, and fails if an entry shows it: a
// "password=" pair for the free-text entries and an "access_token=" pair for IdentifierText, which reads only what has a shape of
// its own, each on a line of its own after a line that holds a dot. Whatever the text before it, the pair is a name and a value and
// nothing else makes it less of one: the planted word is not shown (the cut entries are called with a limit past the plant). The dot
// line is what a rule that takes the next word of a text (a tag, a continuation mark, an OAuth field or a Basic scheme with no
// value, all of which main does take the next word for: see leaklimits_test.go) reads instead of the pair's name. That class (a
// field whose name or value a field before it took) is the generator's: leak_test.go plants a marker in every field of generated
// inputs, and the fuzz target does not look for it.
func plantedFuzzSecret(t *testing.T, s string) {
	t.Helper()
	word := markerWords[7]
	if windowIn(word, Normalize(s)) {
		return // the text holds a window of the planted word itself, which no entry is to take out
	}
	mid := len(s) / 2
	for _, in := range []struct {
		name, text string
		ident      bool
	}{
		{"password in the middle", s[:mid] + "\n.\npassword=" + word + "\n" + s[mid:], false},
		{"password at the end", s + "\n.\npassword=" + word, false},
		{"access_token in the middle", s[:mid] + "\n.\naccess_token=" + word + "\n" + s[mid:], true},
		{"access_token at the end", s + "\n.\naccess_token=" + word, true},
	} {
		for _, e := range leakEntries {
			if e.ident != in.ident {
				continue
			}
			// The cut entries show only a cut of the text; a limit past the plant shows all of it.
			if got := e.fn(in.text, len(in.text)+1); windowIn(word, got) {
				t.Fatalf("%s: %s(%q) = %q shows the planted secret", in.name, e.name, in.text, got)
			}
		}
	}
}

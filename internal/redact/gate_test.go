package redact

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// fakeRun is a run of the URL alphabet for the sk- keys, and fakeKey a run of the length of an AWS secret access key. Both are
// fake.
const (
	fakeRun = "Fake1234567890abcdefXYZ"
	fakeKey = "FAKEfakeFAKEfakeFAKEfake/+FAKEfake0123=="
)

// gateSeeds are texts that each gated pattern matches, by pattern: TestGatesTakeEveryCharacterCutIntoTheirWords cuts characters
// into them. A pattern has a seed for each word its gate looks for, and one for each way of writing it that the gate reads
// apart (glued to the scheme, at a word boundary); the cut-in test runs under -race, where each seed costs a tenth of a
// second or more. TestEveryGatedStepHasSeeds holds this map to rawSteps and freeSteps.
var gateSeeds = map[*regexp.Regexp][]string{
	secretPattern:     {"xoxb-1-fake", "xapp-1-fake", "Bearer fakecred", "auth: bearerFAKE1234 ok", "eyJhbGci.eyJzdWIi.sig"},
	skProject:         {"key sk-ant-" + fakeRun},
	skBare:            {"sk-" + fakeRun},
	oauthPattern:      {"access_token=fake", "idToken: fake", `{"client_secret":"fake"}`},
	basicPattern:      {"Basic dXNlcjpwYXNz"},
	gluedBasicPattern: {"BasicdXNlcjpwYXNzd29yZA"},
	urlUserinfo:       {"https://user:fake@host"},
	urlSpaced:         {"https://user:fa ke@host"},
	plainUserinfo:     {"user:fake@host"},
	envPair:           {"- name: DB_PASSWORD\n  value: fake"},
	secretFlag:        {"--token fake", "cmd -p fake"},
	userFlag:          {"-u admin:fake", "curl --user admin:fake", "x --proxy-user=admin:fake"},
	bareRun:           {fakeKey},
}

// gated is a step that runs behind a gate, as the tests read it: the gate as a function of the text the step reads, whether
// its pattern matches the text, its seeds, and whether it is a step of the token layer.
type gated struct {
	name  string
	gate  func(string) bool
	match func(string) bool
	seeds []string
	token bool
}

// gatedPatterns are the steps of rawSteps and freeSteps that have a gate, every one of them.
var gatedPatterns = gatedSteps()

func gatedSteps() []gated {
	var out []gated
	for i, st := range rawSteps {
		out = append(out, gated{fmt.Sprintf("rawSteps[%d] %.30s", i, st.re), func(s string) bool { return st.gate(s, holdsGap(s)) },
			stepMatch(st.re), gateSeeds[st.re], true})
	}
	for i, st := range freeSteps {
		if st.gate != nil {
			out = append(out, gated{fmt.Sprintf("freeSteps[%d] %.30s", i, st.re), st.gate, stepMatch(st.re), gateSeeds[st.re], false})
		}
	}
	return out
}

// stepMatch reports whether a step's pattern matches a text in a way that its replacement takes: for bareRun, a run of
// exactly keyLen; for every other pattern, any match.
func stepMatch(re *regexp.Regexp) func(string) bool {
	if re != bareRun {
		return re.MatchString
	}
	return func(s string) bool {
		for _, m := range re.FindAllString(s, -1) {
			if len(m) == keyLen {
				return true
			}
		}
		return false
	}
}

// Every gated step has a pattern and seeds, and every pattern with seeds has a gated step: a step added behind a gate gets
// the tests of this file, and a seed of a step that is gone does not linger.
func TestEveryGatedStepHasSeeds(t *testing.T) {
	gatedRes := map[*regexp.Regexp]bool{}
	for i, st := range rawSteps {
		if st.re == nil || st.gate == nil || len(gateSeeds[st.re]) == 0 {
			t.Errorf("rawSteps[%d] has no pattern, gate or seeds", i)
		}
		gatedRes[st.re] = true
	}
	for i, st := range freeSteps {
		if st.gate == nil {
			continue
		}
		if st.re == nil || len(gateSeeds[st.re]) == 0 {
			t.Errorf("freeSteps[%d] has a gate and no pattern or seeds", i)
		}
		gatedRes[st.re] = true
	}
	for re := range gateSeeds {
		if !gatedRes[re] {
			t.Errorf("%.40s has seeds and no gated step", re)
		}
	}
}

// checkGates fails t for each gate that is false where its pattern matches s: the gates of the token layer, and matchFree's
// too if free, for a text that matchFree reads.
func checkGates(t *testing.T, s string, free bool) {
	t.Helper()
	for _, g := range gatedPatterns {
		if (free || g.token) && !g.gate(s) && g.match(s) {
			t.Errorf("%s matches %q and its gate is false", g.name, s)
		}
	}
}

// Every gate is a condition of its pattern: no gate is false where its pattern matches, over the corpus's sample (the leak
// oracle's sample runs through the gates in TestLeakOracleSample, against its pinned leaks) and its line splits, the texts of
// the benchmark and the gates' own seeds. The gates of the token layer read each text as it came, which is how that layer
// reads it first, and all the gates read it normalized and with its lines joined, which is how matchFree reads it.
func TestGatesAreNecessary(t *testing.T) {
	var texts []string
	for _, c := range append(redacttest.SplitSecretsSample(), redacttest.LineSplits()...) {
		texts = append(texts, c.Text)
	}
	for _, b := range benchTexts {
		texts = append(texts, b.text)
	}
	for _, g := range gatedPatterns {
		texts = append(texts, g.seeds...)
	}
	seen := map[string]bool{} // the corpus cuts one secret with each of its invisibles, and normalized they are one text
	for _, s := range texts {
		checkGates(t, s, false)
		n := Normalize(s)
		for _, v := range []string{n, joinLines(n)} {
			if !seen[v] {
				seen[v] = true
				checkGates(t, v, true)
			}
		}
	}
}

// Each seed is a text its pattern matches, or cutting characters into it tests nothing.
func TestGateSeedsMatch(t *testing.T) {
	for _, g := range gatedPatterns {
		for _, s := range g.seeds {
			if !g.match(s) {
				t.Errorf("%s does not match its seed %q", g.name, s)
			}
		}
	}
}

// ungatedRaw and ungatedFree are rawSecrets and matchFree with every gate open: each step runs.
func ungatedRaw(s string) string {
	for _, st := range rawSteps {
		s = st.replace(st.re, s)
	}
	return s
}

func ungatedFree(s string) string {
	for _, st := range freeSteps {
		s = st.replace(st.re, s)
	}
	return s
}

// The gates change nothing, in the order the steps run as well: rawSecrets and matchFree make of a text what their steps make
// of it with every gate open. For rawSecrets, whose gates carry what holdsGap said from step to step, a text is the first
// seed of a pattern of the token layer and that of another, as it is and with a zero-width space between every two of its
// characters (which every pattern of the layer takes, and which leaves none of their words whole), so that the step that
// takes the first changes the text before a later gate reads the second, gap and all (see regap). For matchFree, whose gates
// read only the text, each seed and each text of the benchmark, alone and all of them together.
func TestGatesChangeNothing(t *testing.T) {
	var firsts, all []string
	for _, g := range gatedPatterns {
		all = append(all, g.seeds...)
		if g.token {
			firsts = append(firsts, g.seeds[0])
		}
	}
	for _, a := range firsts {
		for _, b := range firsts {
			for _, s := range []string{a + " " + b, a + " " + strings.Join(strings.Split(b, ""), "\u200b")} {
				if rawSecrets(s) != ungatedRaw(s) {
					t.Fatalf("rawSecrets(%q) = %q, and with every gate open %q", s, rawSecrets(s), ungatedRaw(s))
				}
			}
		}
	}
	for _, b := range benchTexts {
		all = append(all, b.text)
	}
	for _, s := range append(all, strings.Join(all, "\n"), strings.Join(all, " ")) {
		if n := Normalize(s); matchFree(n) != ungatedFree(n) {
			t.Fatalf("matchFree(%q) = %q, and with every gate open %q", n, matchFree(n), ungatedFree(n))
		}
	}
}

// cutSpread is the step of the spread of other characters that cutIns takes: one in each plane. The test runs under -race in
// every package run, and a finer spread costs seconds there; the characters a pattern reads apart from the rest are listed.
const cutSpread = 0x10001

// cutIns are what TestGatesTakeEveryCharacterCutIntoTheirWords cuts into the seeds: every ASCII character but the letters and
// digits, of which a few stand for the rest (a letter is a letter to every gate but in its case, which the test changes
// apart, and in its fold, which "k" and "s" have), every character below U+3000 that Normalize deletes, the deleted ones above it at the ends of their ranges, every space of \p{Z} (which
// ends a credential and a flag), U+212A and U+017F, a spread of the rest, invalid UTF-8, and every form of esc.
func cutIns() []string {
	var out []string
	for r := rune(0); r < 0x3000; r++ {
		if r < utf8.RuneSelf && !unicode.IsLetter(r) && !unicode.IsDigit(r) || deleted(r) {
			out = append(out, string(r))
		}
	}
	out = append(out, "a", "z", "A", "Z", "k", "K", "s", "S", "0", "9")
	for _, z := range unicode.Z.R16 {
		for r := rune(z.Lo); r <= rune(z.Hi); r += rune(z.Stride) {
			out = append(out, string(r))
		}
	}
	for r := rune(0x3000); r <= unicode.MaxRune; r += cutSpread {
		out = append(out, string(r))
	}
	out = append(out, "\u3164", "\ufe00", "\ufe0f", "\ufeff", "\ufff0", "\ufff8", "\ufffd", "\ue000", "\uf8ff", "\U000e0000",
		"\U000e0001", "\U000e0fff", "\U000f0000", "\U0010fffd", "\u212a", "\u017f", "\xff", "\x80", "\xed\xa0\x80")
	for _, e := range []string{"a", "b", "f", "n", "r", "t", "v", "0", "x00", "x7f", "u200b", "u00a0", "U000e0001",
		"A", "B", "F", "N", "R", "T", "V", "X00", "X7F", "U200B"} { // the capitals are escapes to a case-insensitive pattern
		out = append(out, `\`+e)
	}
	return out
}

// No gate is false where its pattern matches a seed with a character cut into it, or put in the place of one of its
// characters, at every place: between and in place of the letters of every word, by the separators and at the ends. That is
// where a gap, a fold and a character that ends a word go. And each ASCII letter of a seed is put in the other case. A
// pattern of matchFree reads normalized text, so what Normalize deletes is not cut into its seeds.
func TestGatesTakeEveryCharacterCutIntoTheirWords(t *testing.T) {
	all := cutIns()
	var kept []string
	for _, in := range all {
		if Normalize(in) == in {
			kept = append(kept, in)
		}
	}
	for _, g := range gatedPatterns {
		ins := kept
		if g.token {
			ins = all
		}
		for _, seed := range g.seeds {
			for i := 0; i <= len(seed); i++ {
				for _, in := range ins {
					texts := []string{seed[:i] + in + seed[i:]}
					if i < len(seed) {
						texts = append(texts, seed[:i]+in+seed[i+1:])
						if c := seed[i]; c < utf8.RuneSelf && unicode.IsLetter(rune(c)) {
							texts = append(texts, seed[:i]+string(c^0x20)+seed[i+1:])
						}
					}
					for _, s := range texts {
						if !g.gate(s) && g.match(s) {
							t.Fatalf("%s matches %q and its gate is false", g.name, s)
						}
					}
				}
			}
		}
	}
}

// holdsGap holds what a gap of the token layer is besides a character of deletable, which TestDeletableIsTheDeletionSet sweeps:
// invalid UTF-8 and every form of esc, alone and between the letters of a word, and a character that folds into ASCII. And
// plain text holds no gap, or no gate would save anything.
func TestHoldsGapTakesEveryGapAndFold(t *testing.T) {
	for _, s := range []string{"\xff", "\x80", "\xc0\xaf", "\xed\xa0\x80", "ok\xe2\x80"} {
		if !holdsGap(s) {
			t.Errorf("holdsGap(%q) is false, and a pattern reads invalid UTF-8 as U+FFFD, a gap", s)
		}
	}
	// Every esc, as a case-insensitive pattern reads it too: spell and pair put gap between the letters of the words in
	// (?i:...), where "\T" and "\X0A" are escapes. A backslash and any character, then enough hex digits for the longest form.
	foldedEsc := regexp.MustCompile(`^(?i:` + esc + `)`)
	escs := 0
	for _, c := range append([]rune{'\u212a', '\u017f'}, []rune(asciiChars())...) {
		s := `\` + string(c) + "00000000"
		if !foldedEsc.MatchString(s) {
			continue
		}
		escs++
		if !holdsGap(s) || !holdsGap("bear"+s+"er") {
			t.Errorf("holdsGap(%q) is false, and a case-insensitive pattern reads it as an esc", s)
		}
	}
	if escs != 2*len("abfnrtvxu")+1 {
		t.Errorf("%d characters start a case-insensitive esc, want the 19 of abfnrtvxu in both cases and 0", escs)
	}
	// The patterns read deletable inside (?i:...) too, and folded it is the same class, so the sweep of
	// TestDeletableIsTheDeletionSet holds for what they read there.
	plain, err := syntax.Parse(`[`+deletable+`]`, syntax.Perl)
	if err != nil {
		t.Fatal(err)
	}
	folded, err := syntax.Parse(`[`+deletable+`]`, syntax.Perl|syntax.FoldCase)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Op != syntax.OpCharClass || folded.Op != syntax.OpCharClass || !slices.Equal(plain.Rune, folded.Rune) {
		t.Errorf("deletable folded is another class: %v, not %v", folded, plain)
	}
	// Every character outside ASCII that a case-insensitive pattern reads as an ASCII letter (U+212A and U+017F, as
	// unicode.SimpleFold has it, on which Go's regexp folds) holds one too. Folding is an orbit, so these are the orbits of
	// ASCII, and a fold added to Unicode cannot keep a gate shut that should open.
	for c := rune(0); c < utf8.RuneSelf; c++ {
		for f := unicode.SimpleFold(c); f != c; f = unicode.SimpleFold(f) {
			if f >= utf8.RuneSelf && !holdsGap("a"+string(f)+"b") {
				t.Errorf("holdsGap(%q) is false, and a case-insensitive pattern reads it as %q", string(f), c)
			}
		}
	}
	// And a gate does some good: plain text holds no gap.
	for _, b := range benchTexts {
		if holdsGap(b.text) {
			t.Errorf("holdsGap(%q) is true", b.text)
		}
	}
}

// An escape that only a case-insensitive pattern reads ("\T", "\X0A" between the letters of a word in (?i:...)) does not shut a
// gate: the five texts leaked through the gates when holdsGap took only the escapes in lower case.
func TestGatesTakeFoldedEscapes(t *testing.T) {
	for _, c := range []struct{ text, secret string }{
		{`access_tok\Ten=fakevalue123`, "fakevalue123"},
		{`{"client_sec\Rret":"fakevalue123"}`, "fakevalue123"},
		{`refresh_to\Xffken=fakevalue123`, "fakevalue123"},
		{`Authorization: Bea\Trer fakecredential123`, "fakecredential123"},
		{`Authorization: Ba\Nsic dXNlcjpwYXNzd29yZA`, "dXNlcjpwYXNzd29yZA"},
	} {
		for name, entry := range map[string]func(string) string{"FreeText": FreeText, "FreeTextLine": FreeTextLine, "IdentifierText": IdentifierText} {
			if got := entry(c.text); strings.Contains(got, c.secret) {
				t.Errorf("%s(%q) = %q, which shows the secret", name, c.text, got)
			}
		}
	}
}

// FuzzGatesAreNecessary holds every gate to its pattern over whatever the fuzzer builds: no gate is false where its pattern
// matches. The seeds, which run in the normal suite, are the gates' own with a gap, a fold and an escape cut into their words,
// and a sample of the corpus. Run it with "go test -run '^$' -fuzz FuzzGatesAreNecessary ./internal/redact".
func FuzzGatesAreNecessary(f *testing.F) {
	for _, g := range gatedPatterns {
		for _, s := range g.seeds {
			f.Add(s)
			for _, in := range []string{"\u200b", "\u212a", "\u017f", `\u200b`, `\T`, "\x00", "\xff", "\U000e0001", "\u00a0"} {
				f.Add(s[:2] + in + s[2:])
			}
		}
	}
	corpus := redacttest.SplitSecretsSample()
	for i := 0; i < len(corpus); i += 31 {
		f.Add(corpus[i].Text)
	}
	f.Fuzz(func(t *testing.T, s string) {
		checkGates(t, s, true)
		if n := Normalize(s); n != s {
			checkGates(t, n, true)
		}
		if strings.ContainsAny(s, "\n\t") {
			checkGates(t, joinLines(s), true)
		}
	})
}

// asciiChars returns every ASCII character, in order.
func asciiChars() string {
	b := make([]byte, utf8.RuneSelf)
	for i := range b {
		b[i] = byte(i)
	}
	return string(b)
}

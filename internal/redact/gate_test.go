package redact

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// gated is a pattern that runs behind a gate: the gate as a function of the text the pattern reads, whether the pattern
// matches the text, texts it matches, which TestGatesTakeEveryCharacterCutIntoTheirWords cuts characters into, and whether
// it is a pattern of the token layer.
type gated struct {
	name  string
	gate  func(string) bool
	match func(string) bool
	seeds []string
	token bool
}

// tokenGated is a pattern of the token layer and its gate, which reads what holdsGap says of the text as well.
func tokenGated(name string, re *regexp.Regexp, gate func(string, bool) bool, seeds ...string) gated {
	return gated{name, func(s string) bool { return gate(s, holdsGap(s)) }, re.MatchString, seeds, true}
}

// freeGated is a pattern of matchFree and its gate.
func freeGated(name string, re *regexp.Regexp, gate func(string) bool, seeds ...string) gated {
	return gated{name, gate, re.MatchString, seeds, false}
}

// fakeRun is a run of the URL alphabet for the sk- keys, and fakeKey a run of the length of an AWS secret access key. Both are
// fake.
const (
	fakeRun = "Fake1234567890abcdefXYZ"
	fakeKey = "FAKEfakeFAKEfakeFAKEfake/+FAKEfake0123=="
)

// gatedPatterns are the patterns that run behind a gate, every one of them. A pattern has a seed for each word its gate looks
// for, and one for each way of writing it that the gate reads apart (glued to the scheme, at a word boundary); the cut-in test
// runs under -race, where each seed costs a tenth of a second or more.
var gatedPatterns = []gated{
	tokenGated("secretPattern", secretPattern, secretGate,
		"xoxb-1-fake", "xapp-1-fake", "Bearer fakecred", "auth: bearerFAKE1234 ok", "eyJhbGci.eyJzdWIi.sig"),
	tokenGated("skProject", skProject, skGate, "key sk-ant-"+fakeRun),
	tokenGated("skBare", skBare, skGate, "sk-"+fakeRun),
	tokenGated("oauthPattern", oauthPattern, oauthGate,
		"access_token=fake", "idToken: fake", `{"client_secret":"fake"}`),
	tokenGated("basicPattern", basicPattern, basicGate, "Basic dXNlcjpwYXNz"),
	tokenGated("gluedBasicPattern", gluedBasicPattern, basicGate, "BasicdXNlcjpwYXNzd29yZA"),
	freeGated("urlUserinfo", urlUserinfo, urlGate, "https://user:fake@host"),
	freeGated("urlSpaced", urlSpaced, urlGate, "https://user:fa ke@host"),
	freeGated("plainUserinfo", plainUserinfo, userinfoGate, "user:fake@host"),
	freeGated("envPair", envPair, envGate, "- name: DB_PASSWORD\n  value: fake"),
	freeGated("secretFlag", secretFlag, flagGate, "--token fake", "cmd -p fake"),
	freeGated("userFlag", userFlag, userFlagGate, "-u admin:fake", "curl --user admin:fake", "x --proxy-user=admin:fake"),
	{"bareRun", keyRunGate, func(s string) bool {
		for _, m := range bareRun.FindAllString(s, -1) {
			if len(m) == keyLen {
				return true
			}
		}
		return false
	}, []string{fakeKey}, false},
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
		s = st.replace(s)
	}
	return s
}

func ungatedFree(s string) string {
	for _, st := range freeSteps {
		s = st.replace(s)
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
	for _, e := range []string{"a", "b", "f", "n", "r", "t", "v", "0", "x00", "x7f", "u200b", "u00a0", "U000e0001"} {
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
	gapRe := regexp.MustCompile(`^(?:[` + deletable + `]|` + esc + `)$`)
	for _, s := range []string{"\xff", "\x80", "\xc0\xaf", "\xed\xa0\x80", "ok\xe2\x80"} {
		if !holdsGap(s) {
			t.Errorf("holdsGap(%q) is false, and a pattern reads invalid UTF-8 as U+FFFD, a gap", s)
		}
	}
	escs := []string{`\x00`, `\x7F`, `\u200b`, `\u00a0`, `\U000e0001`}
	for _, c := range "abfnrtv0" {
		escs = append(escs, `\`+string(c))
	}
	for _, s := range escs {
		if !gapRe.MatchString(s) {
			t.Fatalf("%q is no esc", s)
		}
		if !holdsGap(s) || !holdsGap("bear"+s+"er") {
			t.Errorf("holdsGap(%q) is false, and it is an esc", s)
		}
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

// FuzzGatesAreNecessary holds every gate to its pattern over whatever the fuzzer builds: no gate is false where its pattern
// matches. The seeds, which run in the normal suite, are the gates' own with a gap, a fold and an escape cut into their words,
// and a sample of the corpus. Run it with "go test -run '^$' -fuzz FuzzGatesAreNecessary ./internal/redact".
func FuzzGatesAreNecessary(f *testing.F) {
	for _, g := range gatedPatterns {
		for _, s := range g.seeds {
			f.Add(s)
			for _, in := range []string{"\u200b", "\u212a", "\u017f", `\u200b`, "\x00", "\xff", "\U000e0001", "\u00a0"} {
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

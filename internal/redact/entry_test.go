package redact

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// Free text comes back normalized and without any secret of the corpus, and keeps what is no secret. Matching
// before normalizing leaves the secrets whose giveaway an invisible character cuts, so it fails here.
func TestFreeTextTakesEverySplitSecret(t *testing.T) {
	for _, c := range redacttest.SplitSecrets() {
		c.CheckKept(t, FreeText(c.Text))
	}
}

// FreeTextCut takes the same corpus out, with the cut well beyond the text.
func TestFreeTextCutTakesEverySplitSecret(t *testing.T) {
	for _, c := range redacttest.SplitSecrets() {
		c.CheckKept(t, FreeTextCut(c.Text, 4096))
	}
}

// Identifier text takes the token layer of the corpus, and what it returns is one line.
func TestIdentifierTextTakesEveryTokenSecret(t *testing.T) {
	for _, c := range redacttest.Identifiers(redacttest.SplitSecrets()) {
		got := IdentifierText(c.Text)
		c.CheckKept(t, got)
		if strings.ContainsAny(got, "\n\t") {
			t.Errorf("%s: %q is not one line", c.Name, got)
		}
	}
}

// Free text keeps its line breaks, as "\n", and a secret on a later line is named by what is before it on that line.
func TestFreeTextKeepsLinesAsNewlines(t *testing.T) {
	for _, b := range redacttest.LineBreaks {
		c := redacttest.Breaks(b)
		got := FreeText(c.Text)
		c.Check(t, got)
		if want := "first\nsecond\ntoken=" + redacted; got != want {
			t.Errorf("%s: FreeText = %q, want %q", b.Name, got, want)
		}
	}
}

// Identifier text puts a text on one line: every run of white space, a line break of any kind among it, becomes
// one space, and the ends are trimmed.
func TestIdentifierTextIsOneLine(t *testing.T) {
	for _, b := range redacttest.LineBreaks {
		if got := IdentifierText("  a" + b.Text + b.Text + "b \t c" + b.Text); got != "a b c" {
			t.Errorf("%s: IdentifierText = %q, want %q", b.Name, got, "a b c")
		}
	}
	for in, want := range map[string]string{
		"": "", "   ": "", "a": "a", "a\U000000a0\U00002003b": "a b", "\t\na\x00b\U0000200bc\n": "abc", "ns/name": "ns/name",
	} {
		if got := IdentifierText(in); got != want {
			t.Errorf("IdentifierText(%q) = %q, want %q", in, got, want)
		}
	}
}

// An identifier is not read as a pair or a flag: "secret:ns/name" is the identity of a Secret, and a task ID may be
// 40 characters long. Free text takes both.
func TestIdentifierTextKeepsIdentifiers(t *testing.T) {
	for _, id := range []string{"secret:ns/name", "task-0123456789abcdefghijklmnopqrstuvwxyz1", "sa/investigator", "ns/bearer-gateway"} {
		if got := IdentifierText(id); got != id {
			t.Errorf("IdentifierText(%q) = %q, want it kept", id, got)
		}
	}
	if got := FreeText("secret:ns/name"); got == "secret:ns/name" {
		t.Errorf("FreeText(secret:ns/name) kept it; want the free-text rules to read it as a pair")
	}
}

// Normalization comes before the cut: invisible padding is not counted against the limit, and text that is short
// once it is gone is not marked as cut.
func TestFreeTextNormalizesBeforeItBounds(t *testing.T) {
	padded := strings.Repeat("\U0000200b", MaxBytes) + "short value"
	if got := FreeText(padded); got != "short value" {
		t.Errorf("FreeText of padding and a short value = %q (%d bytes), want only the value", got, len(got))
	}
}

// The patterns read what the reader sees: the characters cleaning used to delete after the thread block had
// redacted (the order TestThreadRepliesRedactsWhatCleaningWouldRejoin pins) are gone before any pattern runs.
func TestFreeTextMatchesTheNormalizedText(t *testing.T) {
	for _, split := range []string{"\x07", "\x00", "\x1b", "\U00003164", "\U00002800", "\U0000e000", "\U0000fe0f", "\U000e0100", "\xff"} {
		if got := FreeText("pass" + split + "word=hunter2hunter2 ok"); got != "password="+redacted+" ok" {
			t.Errorf("split by %q: FreeText = %q, want the name whole and the value gone", split, got)
		}
	}
}

// SecretName is a question put to a name that may be split by an invisible character, so it normalizes the name itself.
func TestSecretNameNormalizes(t *testing.T) {
	for _, inv := range redacttest.Invisibles {
		for _, name := range []string{"pass" + inv.Text + "word", "api" + inv.Text + "_key", "PGPASS" + inv.Text + "WORD"} {
			if !SecretName(name) {
				t.Errorf("SecretName(%q) = false (%s), want true", name, inv.Name)
			}
		}
		if SecretName("mon" + inv.Text + "key") {
			t.Errorf("SecretName of a name that is only monkey, split by %s, = true", inv.Name)
		}
	}
}

// The glued Bearer is one rule everywhere: "bearer" at a word boundary and a token of at least seven characters
// against it. A longer word that ends in "bearer", a plural, a possessive and a short word are no credential.
func TestGluedBearerIsBounded(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"bearerabc1234", redacted},
		{"BEARERabc1234", redacted},
		{"sent bearerabc1234 ok", "sent " + redacted + " ok"},
		{"x-bearerabc1234", "x-" + redacted},
		{"bearerabc123", "bearerabc123"},
		{"flagbearerabc1234", "flagbearerabc1234"},
		{"x_bearerabc1234", "x_bearerabc1234"},
		{"1bearerabc1234", "1bearerabc1234"},
		{"flagbearer", "flagbearer"},
		{"bearers", "bearers"},
		{"bearer's token", "bearer's token"},
		{"Bearer", "Bearer"},
		{"bearer-gateway", "bearer-gateway"},
		{"bearer_gateway.svc", "bearer_gateway.svc"},
		{"bearerabc!@#$%^&*", redacted},
	} {
		if got := secrets(tc.in); got != tc.want {
			t.Errorf("secrets(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := IdentifierText(tc.in); got != tc.want {
			t.Errorf("IdentifierText(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := FreeText(tc.in); got != tc.want {
			t.Errorf("FreeText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A Basic credential glued to its scheme, which is what an invisible separator leaves once it is deleted, is taken at a
// word boundary, from twelve characters, and only if the run is base64 (either alphabet, padding or not) of a
// "user:password": text with a colon in it. A word that merely follows "Basic" is not.
func TestGluedBasicIsBounded(t *testing.T) {
	userPass := []byte("user:pass>>>???")
	urlForm := base64.RawURLEncoding.EncodeToString(userPass)
	stdForm := base64.StdEncoding.EncodeToString(userPass)
	if !strings.ContainsAny(urlForm, "-_") || !strings.ContainsAny(stdForm, "+/") {
		t.Fatalf("the test's credential must use the characters the two alphabets differ in: %q %q", urlForm, stdForm)
	}
	noColon := base64.StdEncoding.EncodeToString([]byte("justsomewordsnocolon"))
	for _, tc := range []struct{ in, want string }{
		// Credentials: redacted.
		{"BasicdXNlcjpwYXNzd29yZA==", redacted},
		{"BasicdXNlcjpwYXNzd29yZA", redacted}, // padding optional
		{"basicdXNlcjpwYXNz", redacted},
		{"sent BasicdXNlcjpwYXNz ok", "sent " + redacted + " ok"},
		{"Basic" + stdForm, redacted},
		{"Basic" + urlForm, redacted},
		{"Basic" + strings.TrimRight(stdForm, "="), redacted},
		// A known limit: Latin-1 text glued to its scheme, with no gap to show it, is not taken. (Gapped, the first run catches it.)
		{"Basic" + base64.StdEncoding.EncodeToString([]byte("admin:p\xe9ss")), "Basic" + base64.StdEncoding.EncodeToString([]byte("admin:p\xe9ss"))},
		// Not credentials: kept.
		{"Basic" + base64.StdEncoding.EncodeToString([]byte("admin:pa\x07ss")), "Basic" + base64.StdEncoding.EncodeToString([]byte("admin:pa\x07ss"))}, // a control character
		{"BasicAuthMiddleware", "BasicAuthMiddleware"},
		{"BasicHTTPClientFactory", "BasicHTTPClientFactory"},
		{"Basic" + noColon, "Basic" + noColon}, // base64, but no user:password in it
		{"Basically", "Basically"},
		{"BasicConfiguration", "BasicConfiguration"},
		{"Basicabc12345", "Basicabc12345"},
		{"xBasicdXNlcjpwYXNz", "xBasicdXNlcjpwYXNz"},
		{"_BasicdXNlcjpwYXNz", "_BasicdXNlcjpwYXNz"},
		{"Basic", "Basic"},
	} {
		for name, fn := range map[string]func(string) string{"secrets": secrets, "IdentifierText": IdentifierText, "FreeText": FreeText} {
			if got := fn(tc.in); got != tc.want {
				t.Errorf("%s(%q) = %q, want %q", name, tc.in, got, tc.want)
			}
		}
	}
}

// Normalizing alone would glue a word to a scheme, or a credential to it, and the glued rules refuse what the gap
// hid; the token layer runs on the text as it came as well. Without that raw pass these leak.
func TestTheTokenLayerRunsOnTheRawTextToo(t *testing.T) {
	latin1 := base64.StdEncoding.EncodeToString([]byte("admin:p\xe9ss"))
	control := base64.StdEncoding.EncodeToString([]byte("admin:pa\x07ss"))
	for _, tc := range []struct{ name, in, want string }{
		{"a word, a zero-width space and a glued Bearer", "token\U0000200bBearerZq9vK3mT7wRx", "token" + redacted},
		{"a percent-encoded colon", "%3ABearer\U0000200bZq9vK3mT7wRx", "%3A" + redacted},
		{"a word run into Bearer", "authBearer\x07Zq9vK3mT7wRx", "auth" + redacted},
		{"a letter run into Bearer", "xBearer\x00Zq9vK3mT7wRx", "x" + redacted},
		{"Basic, a control character and Latin-1 text", "Basic\x07" + latin1, redacted},
		{"Basic, a control character and a password with a control character", "Basic\x07" + control, redacted},
		{"Basic, a private-use character and a password with a control character", "Basic\U0000e000" + control, redacted},
		{"Basic, invalid UTF-8 and a password with a control character", "Basic\xff" + control, redacted},
	} {
		for name, fn := range map[string]func(string) string{"IdentifierText": IdentifierText, "FreeText": FreeText, "FreeTextCut": func(s string) string { return FreeTextCut(s, 1000) }} {
			if got := fn(tc.in); got != tc.want {
				t.Errorf("%s: %s(%q) = %q, want %q", tc.name, name, tc.in, got, tc.want)
			}
		}
	}
}

// FreeTextCut cuts at a character, not at white space: text with none keeps its start, and the cut is marked. What it
// keeps is matched after it is cut, so a secret in it is gone.
func TestFreeTextCutKeepsTheStartOfAnUnbrokenRun(t *testing.T) {
	run := strings.Repeat("\U000065e5\U0000672c\U00008a9e", 1000) // 9000 bytes, no white space
	got := FreeTextCut(run, 4000)
	// The cut is at 3999, the last character boundary within 4000, and the last tailDrop bytes of that go (to a boundary).
	if want := run[:3999-1023] + Truncated; got != want {
		t.Errorf("FreeTextCut kept %d bytes ending %q, want %d bytes", len(got), got[len(got)-20:], len(want))
	}
	if got := FreeText(run); got != Truncated {
		t.Errorf("FreeText of an unbroken run over the bound = %d bytes, want it whole gone (the contrast)", len(got))
	}
	if got := FreeTextCut("short token=Zq9vK3mT7wRx", 4000); got != "short token="+redacted {
		t.Errorf("FreeTextCut of short text = %q", got)
	}
	const w = "x"
	if got := FreeTextCut(strings.Repeat(w, 50)+" token=Zq9vK3mT7wRx", 56); strings.Contains(got, "Zq9") {
		t.Errorf("a secret cut by the window is shown: %q", got)
	}
}

// What is left of a secret that the cut went through must not be shown: the end of the cut text goes before the
// mark, back to the last white space.
func TestFreeTextCutDropsTheRunTheCutWentThrough(t *testing.T) {
	const key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" // 40 characters: a bare AWS secret access key
	if len(key) != 40 {
		t.Fatal("the key is not 40 characters")
	}
	// A long value that matching shrinks pulls the end of the window to the start of the matched text.
	for name, tail := range map[string]string{
		"a bare key cut at 30 characters":       " " + key[:30],
		"a URL password cut before its @":       " https://admin:Zq9vK3mT7wRxLongPassword",
		"a key cut inside a word of the window": " word " + key[:30],
	} {
		text := "debug dump password=" + strings.Repeat("A", 11000) + " ok" + tail
		got := FreeTextCut(text, len(text)-1) // the window ends one byte short of the text
		for _, part := range []string{key[:20], "Zq9vK3", "wJalrXUt"} {
			if strings.Contains(got, part) {
				t.Errorf("%s: the cut text shows %q: %q", name, part, got)
			}
		}
		if !strings.HasPrefix(got, "debug dump password="+redacted+" ok") || !strings.HasSuffix(got, Truncated) {
			t.Errorf("%s: FreeTextCut = %q, want the redacted start kept and the cut marked", name, got)
		}
	}
}

// What only looks like a credential once an invisible character is deleted or ignored is kept: words that run into
// "Bearer" or "Basic", prose, a symbol after a scheme.
func TestWordsNextToASchemeAreKept(t *testing.T) {
	for _, in := range []string{
		"Bearer\U00002764\U0000fe0f friend", "Bearer\U0001F600 friend", "BasicAuthMiddlewareFactory", "BasicServerFactoryToken",
		"Basic auth is configured", "Basic\U0000200b auth is configured", "token\U0000200bBearerless", "token\U0000200bBearers of the realm",
		"flag\U0000200bbearer", "Basic configu\U0000200bration", "the Basic\x07 idea", "BasicHTTPClient\U0000200bFactory",
	} {
		want := Normalize(in)
		for name, fn := range map[string]func(string) string{"IdentifierText": IdentifierText, "FreeText": FreeText, "FreeTextCut": func(s string) string { return FreeTextCut(s, 1000) }} {
			if got := fn(in); got != want {
				t.Errorf("%s(%q) = %q, want it kept as %q", name, in, got, want)
			}
		}
	}
}

// The patterns read at most rawCap bytes of a text as it came, so the work is bounded whatever is sent, and what is
// cut off is marked.
func TestTheRawPassIsBounded(t *testing.T) {
	big := strings.Repeat("word ", 1<<19) // 2.5 MiB
	if got := FreeText(big); len(got) > MaxBytes+len(Truncated) || !strings.HasSuffix(got, Truncated) {
		t.Errorf("FreeText of %d bytes = %d bytes ending %q", len(big), len(got), got[max(len(got)-20, 0):])
	}
	if got := IdentifierText(strings.Repeat("a", 3*rawCap)); len(got) != rawCap+len(Truncated) || !strings.HasSuffix(got, Truncated) {
		t.Errorf("IdentifierText of %d bytes = %d bytes, want %d and the mark", 3*rawCap, len(got), rawCap+len(Truncated))
	}
	// Invisible padding past the cap hides what follows it, and the text is marked as cut.
	padded := strings.Repeat("\U0000200b", rawCap) + "tail"
	if got := FreeText(padded); got != Truncated {
		t.Errorf("FreeText of padding past the cap = %q, want only the mark", got)
	}
	// Padding within the cap is not counted against the limit of what is shown.
	if got := FreeText(strings.Repeat("\U0000200b", MaxBytes) + "short value"); got != "short value" {
		t.Errorf("FreeText of padding and a short value = %q", got)
	}
}

// A value of nothing but deleted characters is no value: the redaction would take them and leave the text after them.
func TestAnOAuthValueOfNothingButInvisiblesIsNoValue(t *testing.T) {
	in := "access_token=\U0000200b Zq9vK3mT7wRx tail"
	if got := FreeText(in); strings.Contains(got, "Zq9vK3mT7wRx") || !strings.HasSuffix(got, "tail") {
		t.Errorf("FreeText(%q) = %q, want the value after the invisible gone", in, got)
	}
}

// Invisible padding can bring the end of the raw cut up to the start of what is shown, so the last run of a text that was
// cut goes, as it does for FreeTextCut: here a bare key the cut went through.
func TestTheRawCutDropsTheRunItWentThrough(t *testing.T) {
	const key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" // 40 characters, cut at 30 below
	head := "ok  "
	pad := strings.Repeat("\U0000200b", (rawCap-len(head)-30)/3)
	text := head + pad + key
	if len(head)+len(pad)+30 != rawCap {
		t.Fatalf("the cut is not inside the key: %d", len(head)+len(pad)+30)
	}
	if got := FreeText(text); strings.Contains(got, "wJalrXUt") || !strings.HasPrefix(got, "ok") || !strings.HasSuffix(got, Truncated) {
		t.Errorf("FreeText = %q, want the start kept, the key gone and the cut marked", got)
	}
}

package redact

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// Every class of the deletion set has a sample in the corpus, and each is deleted from the middle of a word. A
// class dropped from the set fails here, by name.
func TestNormalizeDeletesEveryInvisible(t *testing.T) {
	for _, inv := range redacttest.Invisibles {
		if got := Normalize("pass" + inv.Text + "word"); got != "password" {
			t.Errorf("%s: Normalize = %q, want the character gone", inv.Name, got)
		}
		if got := Normalize(inv.Text); got != "" {
			t.Errorf("%s: Normalize of it alone = %q, want nothing", inv.Name, got)
		}
	}
}

// Every code point is either kept, turned into a line break or deleted, as the set says: a sweep of the whole
// range, so that a class that has no sample in the corpus cannot be dropped, nor a category table narrowed.
func TestNormalizeSweepsEveryCodePoint(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // a surrogate has no UTF-8 form: the corpus has its encoded bytes
		}
		var want string
		switch {
		case r == '\n' || r == '\r' || r == 0x85 || r == 0x2028 || r == 0x2029:
			want = "\n"
		case r == '\t':
			want = "\t"
		case unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co), r == 0x3164, r == 0x2800,
			r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF:
			want = ""
		default:
			want = string(r)
		}
		if got := Normalize(string(r)); got != want {
			t.Fatalf("Normalize(U+%04X) = %q, want %q", r, got, want)
		}
	}
}

func TestNormalizeLineBreaks(t *testing.T) {
	for _, b := range redacttest.LineBreaks {
		if got := Normalize("a" + b.Text + "b"); got != "a\nb" {
			t.Errorf("%s: Normalize = %q, want a\\nb", b.Name, got)
		}
	}
	for in, want := range map[string]string{
		"a\r\nb": "a\nb", "a\r\n\r\nb": "a\n\nb", "a\r\rb": "a\n\nb", "a\n\rb": "a\n\nb",
		"a\r\n\nb": "a\n\nb", "a\U00002028\U00002029b": "a\n\nb",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// What a reader sees stays, tab and white space of every width included; an emoji loses only its presentation hint.
func TestNormalizeKeepsWhatIsVisible(t *testing.T) {
	for _, s := range []string{
		"plain text", "tab\there", "no-break\U000000a0space", "em\U00002003space", "ideographic\U00003000space", "caf\U000000e9", "\U000065e5\U0000672c\U00008a9e",
		"emoji \U0001F600", "replacement \U0000fffd char", "A\U00000301 combining", "NBSP\U000000a0and\U0000202fnarrow",
	} {
		if got := Normalize(s); got != s {
			t.Errorf("Normalize(%q) = %q, want it unchanged", s, got)
		}
	}
	if got, want := Normalize("heart \U00002764\U0000fe0f and \U0001F44D\U0001F3FD"), "heart \U00002764 and \U0001F44D\U0001F3FD"; got != want {
		t.Errorf("Normalize of emoji = %q, want only the presentation selector gone: %q", got, want)
	}
}

// A literal escape is text: what %q prints for an invisible character is not the character, and the patterns of
// redaction read it as the gap it stands for. Normalization leaves it.
func TestNormalizeLeavesLiteralEscapes(t *testing.T) {
	for _, s := range []string{`pass\x07word`, `a\U0000200bb`, `x\U000e0100y`, `tab\there`} {
		if got := Normalize(s); got != s {
			t.Errorf("Normalize(%q) = %q, want the escape left as text", s, got)
		}
	}
}

func TestNormalizeAlwaysReturnsValidUTF8(t *testing.T) {
	for _, s := range []string{"\xff", "a\x80b", "\xed\xa0\x80", "\xc0\xaf", "ok\xe2\x80", strings.Repeat("\xf0\x28", 50)} {
		if got := Normalize(s); !utf8.ValidString(got) {
			t.Errorf("Normalize(%q) = %q, which is not valid UTF-8", s, got)
		}
	}
	if got := Normalize("ok\xe2\x80"); got != "ok" {
		t.Errorf("Normalize of a cut sequence = %q, want its bytes gone", got)
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	for _, c := range redacttest.SplitSecrets() {
		once := Normalize(c.Text)
		if twice := Normalize(once); twice != once {
			t.Errorf("%s: Normalize twice = %q, once = %q", c.Name, twice, once)
		}
	}
}

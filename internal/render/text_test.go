package render

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// entity is the only thing a "&" may begin in rendered text.
var entity = regexp.MustCompile(`&(?:amp|lt|gt);`)

// assertInert fails if out could still act as Slack syntax or hide text: an angle bracket, a bare
// ampersand, or a control or format character other than newline and tab.
func assertInert(t *testing.T, label, out string) {
	t.Helper()
	if strings.ContainsAny(out, "<>") {
		t.Errorf("%s: %q holds an angle bracket", label, out)
	}
	if bare := entity.ReplaceAllString(out, ""); strings.Contains(bare, "&") {
		t.Errorf("%s: %q holds a bare ampersand", label, out)
	}
	for _, r := range out {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r) || r == '\u3164' || r == '\u2800') {
			t.Errorf("%s: %q holds the invisible character %U", label, out, r)
		}
	}
	if !utf8.ValidString(out) {
		t.Errorf("%s: %q is not valid UTF-8", label, out)
	}
}

// utf16Len is how many UTF-16 units s takes: Slack's limits are counted in characters, which may be
// units (UNCONFIRMED(WP-04)), so the caps are met in units, which is never fewer than runes.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// assertMarkdownEscapes fails if a backslash in out does not start a pair with a backslash, "["
// or "]", or a bracket stands bare: no "[text](url)" can survive.
func assertMarkdownEscapes(t *testing.T, label, out string) {
	t.Helper()
	for i := 0; i < len(out); i++ {
		switch out[i] {
		case '\\':
			if i+1 >= len(out) || !strings.ContainsRune(`\[]`, rune(out[i+1])) {
				t.Errorf("%s: %q holds a backslash that does not start an escape pair", label, out)
				return
			}
			i++
		case '[', ']':
			t.Errorf("%s: %q holds an unescaped bracket", label, out)
			return
		}
	}
}

// hostile is text that would mention, link, broadcast or hide something if it reached Slack as is.
var hostile = []struct{ name, in, want string }{
	{"channel broadcast", "<!channel> look", "&lt;!channel&gt; look"},
	{"here and everyone", "<!here|here> <!everyone>", "&lt;!here|here&gt; &lt;!everyone&gt;"},
	{"user mention", "<@U12345> hi", "&lt;@U12345&gt; hi"},
	{"channel link", "<#C12345|general>", "&lt;#C12345|general&gt;"},
	{"labelled link", "<https://evil.example|your bank>", "&lt;https://evil.example|your bank&gt;"},
	{"subteam", "<!subteam^S123|@oncall>", "&lt;!subteam^S123|@oncall&gt;"},
	{"an entity is not decoded", "&lt;!channel&gt;", "&amp;lt;!channel&amp;gt;"},
	{"ampersand", "a & b", "a &amp; b"},
	{"greater-than", "a > b", "a &gt; b"},
	{"zero-width inside a mention", "<\u200b!chan\u200dnel>", "&lt;!channel&gt;"},
	{"bidi override and isolates", "ok\u202egnp.exe\u2066x\u2069", "okgnp.exex"},
	{"BOM, soft hyphen and tag characters", "\ufeffa\u00adb\U000e0041", "ab"},
	{"control characters", "a\x00b\x1b[31mc\x7f\u0085d", "ab[31mcd"},
	{"newlines and tabs stay", "a\nb\tc", "a\nb\tc"},
	{"carriage returns become newlines", "a\r\nb\rc\u2028d\u2029e", "a\nb\nc\nd\ne"},
	{"invalid UTF-8", "a\xffb", "a\ufffdb"},
	{"private use and blank fillers", "a\ue000\u3164\u2800b", "ab"},
	{"@here, #general and a bare URL stay plain text", "@here #general https://example.com/a?b=c", "@here #general https://example.com/a?b=c"},
}

func TestTextMakesHostileInputInert(t *testing.T) {
	for _, tc := range hostile {
		got := Text(tc.in, 40000)
		if got != tc.want {
			t.Errorf("%s: Text(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		assertInert(t, tc.name, got)
	}
}

func TestMarkdownAlsoDefusesLinksAndImages(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"inline link", "[your bank](https://evil.example)", `\[your bank\](https://evil.example)`},
		{"image", "![logo](https://evil.example/x.png)", `!\[logo\](https://evil.example/x.png)`},
		{"reference link", "[a][1]\n\n[1]: https://evil.example", "\\[a\\]\\[1\\]\n\n\\[1\\]: https://evil.example"},
		{"a backslash cannot cancel the escape", `\[x](https://evil.example)`, `\\\[x\](https://evil.example)`},
		{"angle autolink", "<https://evil.example>", "&lt;https://evil.example&gt;"},
		{"mention", "<!channel> & <@U1>", "&lt;!channel&gt; &amp; &lt;@U1&gt;"},
	} {
		got := Markdown(tc.in, 12000)
		if got != tc.want {
			t.Errorf("%s: Markdown(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestCapIsExactRuneSafeAndVisible(t *testing.T) {
	runes := utf8.RuneCountInString
	for _, tc := range []struct {
		name, in string
		limit    int
		want     string
	}{
		{"fits exactly", strings.Repeat("a", 30), 30, strings.Repeat("a", 30)},
		{"one over", strings.Repeat("a", 31), 30, strings.Repeat("a", 30-runes(Truncated)) + Truncated},
		{"multi-byte rune at the boundary", strings.Repeat("é", 40), 20, strings.Repeat("é", 20-runes(Truncated)) + Truncated},
		{"four-byte runes count as two units", strings.Repeat("😀", 40), 20, strings.Repeat("😀", 3) + Truncated},
		{"an entity is never cut", strings.Repeat("&", 50), 30, strings.Repeat("&amp;", 3) + Truncated},
		{"a limit below the marker", strings.Repeat("a", 50), 5, string([]rune(Truncated)[:5])},
		{"a limit of the marker's length", strings.Repeat("a", 50), 13, Truncated},
		{"text that fits at the marker's length", strings.Repeat("a", 13), 13, strings.Repeat("a", 13)},
		{"a limit of zero", "abc", 0, ""},
		{"a negative limit", "abc", -1, ""},
	} {
		got := Text(tc.in, tc.limit)
		if got != tc.want || !utf8.ValidString(got) || utf16Len(got) > max(tc.limit, 0) {
			t.Errorf("%s: Text(%d runes, %d) = %q (%d units), want %q", tc.name, runes(tc.in), tc.limit, got, utf16Len(got), tc.want)
		}
	}
}

func FuzzTextIsInert(f *testing.F) {
	for _, tc := range hostile {
		f.Add(tc.in, uint8(200))
	}
	f.Add("[x](https://evil.example) \\ ]", uint8(7))
	f.Fuzz(func(t *testing.T, s string, l uint8) {
		limit := int(l)
		text, md := Text(s, limit), Markdown(s, limit)
		for label, out := range map[string]string{"Text": text, "Markdown": md} {
			assertInert(t, label, out)
			if utf16Len(out) > limit {
				t.Errorf("%s = %q is %d units; limit %d", label, out, utf16Len(out), limit)
			}
		}
		assertMarkdownEscapes(t, "Markdown", md)
	})
}

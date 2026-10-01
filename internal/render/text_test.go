package render

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/agent"
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
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			t.Errorf("%s: %q holds the invisible character %U", label, out, r)
		}
	}
	if !utf8.ValidString(out) {
		t.Errorf("%s: %q is not valid UTF-8", label, out)
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
}

func TestTextMakesHostileInputInert(t *testing.T) {
	for _, tc := range hostile {
		got := Text(tc.in, MaxMessage)
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
		got := Markdown(tc.in, MaxBlock)
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
		{"four-byte runes", strings.Repeat("😀", 40), 20, strings.Repeat("😀", 20-runes(Truncated)) + Truncated},
		{"an entity is never cut", strings.Repeat("&", 50), 30, strings.Repeat("&amp;", 3) + Truncated},
		{"a limit below the marker", strings.Repeat("a", 50), 5, string([]rune(Truncated)[:5])},
	} {
		got := Text(tc.in, tc.limit)
		if got != tc.want || !utf8.ValidString(got) || runes(got) > tc.limit {
			t.Errorf("%s: Text(%d runes, %d) = %q (%d runes), want %q", tc.name, runes(tc.in), tc.limit, got, runes(got), tc.want)
		}
	}
}

func TestToolLineIsOneInertLine(t *testing.T) {
	line := ToolLine("<!channel>\nrun", map[string]any{
		"url": "<https://evil.example|click>", "who": "<@U1> & co", "b\nad": "\u202ex", "n": 3,
	})
	assertInert(t, "tool line", line)
	if strings.ContainsAny(line, "\n\t") {
		t.Errorf("tool line %q is not one line", line)
	}
	want := `Calling &lt;!channel&gt; run(b ad="x", n=3, url="&lt;https://evil.example|click&gt;", who="&lt;@U1&gt; &amp; co")`
	if line != want {
		t.Errorf("tool line = %q, want %q", line, want)
	}
}

func TestToolLineCapsNameAndArguments(t *testing.T) {
	line := ToolLine(strings.Repeat("n", 500), map[string]any{"q": strings.Repeat("v", 5000)})
	if got, most := utf8.RuneCountInString(line), len("Calling ")+maxToolName+len("(")+maxToolArgs+len(")"); got > most {
		t.Errorf("tool line is %d runes, want at most %d", got, most)
	}
	if strings.Count(line, Truncated) != 2 {
		t.Errorf("tool line %q should mark both the name and the arguments as truncated", line)
	}
}

func TestBodyKeepsArtifactsInOrderOfFirstAppearance(t *testing.T) {
	events := []agent.TextEvent{
		{ArtifactID: "a", Text: "first"},
		{ArtifactID: "b", Text: "<!channel> second"},
		{ArtifactID: "c", Text: "  "},
		{ArtifactID: "a", Text: "first, grown"}, // replaces a's text and keeps a's place
	}
	got := Body(events, MaxMessage)
	if want := "first, grown\n\n&lt;!channel&gt; second"; got != want {
		t.Errorf("Body = %q, want %q", got, want)
	}
	if got := Body(events, 20); utf8.RuneCountInString(got) > 20 || !strings.HasSuffix(got, Truncated) {
		t.Errorf("Body capped at 20 = %q, want at most 20 runes ending in the marker", got)
	}
	if got := Body(nil, MaxMessage); got != "" {
		t.Errorf("Body(nil) = %q, want empty", got)
	}
}

func FuzzTextIsInert(f *testing.F) {
	for _, tc := range hostile {
		f.Add(tc.in)
	}
	f.Fuzz(func(t *testing.T, s string) {
		assertInert(t, "Text", Text(s, 200))
		assertInert(t, "ToolLine", ToolLine(s, map[string]any{s: s}))
	})
}

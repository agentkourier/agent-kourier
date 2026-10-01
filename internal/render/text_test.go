package render

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf16"
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

func TestToolLineIsOneInertLine(t *testing.T) {
	line := ToolLine("<!channel>\nrun", map[string]any{
		"url": "<https://evil.example|click>", "who": "<@U1> & co", "b\nad": "\u202ex", "n": 3,
	}, FlavourText)
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
	line := ToolLine(strings.Repeat("n", 500), map[string]any{"q": strings.Repeat("v", 5000)}, FlavourText)
	if got, most := utf16Len(line), len("Calling ")+maxToolName+len("(")+maxToolArgs+len(")"); got > most {
		t.Errorf("tool line is %d units, want at most %d", got, most)
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
	got := Body(events, MaxMessage, FlavourText)
	if want := "first, grown\n\n&lt;!channel&gt; second"; got != want {
		t.Errorf("Body = %q, want %q", got, want)
	}
	if got := Body(events, 20, FlavourText); utf16Len(got) > 20 || !strings.HasSuffix(got, Truncated) {
		t.Errorf("Body capped at 20 = %q, want at most 20 runes ending in the marker", got)
	}
	if got := Body(nil, MaxMessage, FlavourText); got != "" {
		t.Errorf("Body(nil) = %q, want empty", got)
	}
}

func TestBodyHasNoFlavourSurprises(t *testing.T) {
	events := []agent.TextEvent{
		{ArtifactID: "a", Text: "x [link](https://evil.example)"},
		{ArtifactID: "b", Text: "\u200b\u200d"}, // nothing once cleaned: no blank paragraph for it
		{ArtifactID: "c", Text: "y"},
	}
	if got, want := Body(events, MaxBlock, FlavourMarkdown), `x \[link\](https://evil.example)`+"\n\ny"; got != want {
		t.Errorf("Body in markdown = %q, want %q", got, want)
	}
	if got, want := Body(events, MaxMessage, FlavourText), "x [link](https://evil.example)\n\ny"; got != want {
		t.Errorf("Body in text = %q, want %q", got, want)
	}
}

func TestToolLineRedactsBeforeItShowsOrCaps(t *testing.T) {
	line := ToolLine("run", map[string]any{
		"password":      "hunter2hunter2",
		"Authorization": "Bearer abcdefghijklmnop",
		"cfg":           map[string]any{"DB_Password": "hunter2hunter2", "hosts": []any{map[string]any{"apiKey": "k-1234567890", "name": "db1"}}},
		"url":           "https://admin:s3cretpass@db.example/x",
		"cmd":           "curl -H 'Authorization: Bearer xoxb-123-456-abcdefghijkl' https://db.example",
	}, FlavourText)
	for _, secret := range []string{"hunter2hunter2", "s3cretpass", "k-1234567890", "abcdefghijklmnop", "xoxb-123-456", "admin:"} {
		if strings.Contains(line, secret) {
			t.Errorf("tool line %q shows %q", line, secret)
		}
	}
	if !strings.Contains(line, "db1") { // what is not secret is shown
		t.Errorf("tool line %q lost a value that is no secret", line)
	}
	// A token's shape is enough, whatever its key is called.
	if got := ToolLine("run", map[string]any{"cmd": "curl -H 'Authorization: Bearer xoxb-123-456-abcdefghijkl'"}, FlavourText); strings.Contains(got, "xoxb-123-456") {
		t.Errorf("tool line %q shows a token", got)
	}
	for _, tc := range []struct{ url, want string }{
		{"https://admin:s3cretpass@db.example/x", "https://[redacted]@db.example/x"},
		{"https://user:pa/ss@h", "https://[redacted]@h"},    // a "/" in the password
		{"https://user:p@ss@h/x", "https://[redacted]@h/x"}, // an "@" in the password
	} {
		if got, want := ToolLine("run", map[string]any{"url": tc.url}, FlavourText), `Calling run(url="`+tc.want+`")`; got != want {
			t.Errorf("ToolLine of %q = %q, want %q", tc.url, got, want)
		}
	}
	// A secret that sits across the argument cap must not show as a prefix: redaction comes first.
	cut := ToolLine("run", map[string]any{"a": strings.Repeat("x", 118), "u": "https://user:s3cretpass@host", "z": strings.Repeat("y", 50)}, FlavourText)
	if strings.Contains(cut, "s3c") || strings.Contains(cut, "user:") {
		t.Errorf("tool line %q shows part of a secret at the cap", cut)
	}
}

func TestToolLineInMarkdownLeavesNoLink(t *testing.T) {
	got := ToolLine("[x](https://evil.example)", map[string]any{"v": "[your bank](https://evil.example)"}, FlavourMarkdown)
	want := `Calling \[x\](https://evil.example)(v="\[your bank\](https://evil.example)")`
	if got != want {
		t.Errorf("ToolLine in markdown = %q, want %q", got, want)
	}
	assertMarkdownEscapes(t, "tool line", got)
}

// TestEveryKeyAlternativeRedacts has one key for each of secretKey's alternatives.
func TestEveryKeyAlternativeRedacts(t *testing.T) {
	for _, key := range []string{"passwd", "x_secret", "x_token", "api_key", "Authorization", "my_credentials", "session_cookie"} {
		got := ToolLine("run", map[string]any{key: "VALUE-NOT-TO-SHOW"}, FlavourText)
		if strings.Contains(got, "VALUE-NOT-TO-SHOW") || !strings.Contains(got, "[redacted]") {
			t.Errorf("an argument named %q: %q; want its value redacted", key, got)
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
		ev := []agent.TextEvent{{ArtifactID: "a", Text: s}, {ArtifactID: "b", Text: s}}
		for label, out := range map[string]string{"Text": text, "Markdown": md, "Body": Body(ev, limit, FlavourText), "Body markdown": Body(ev, limit, FlavourMarkdown)} {
			assertInert(t, label, out)
			if utf16Len(out) > limit {
				t.Errorf("%s = %q is %d units; limit %d", label, out, utf16Len(out), limit)
			}
		}
		assertMarkdownEscapes(t, "Markdown", md)
		assertMarkdownEscapes(t, "Body markdown", Body(ev, limit, FlavourMarkdown))
		for flavour, label := range map[Flavour]string{FlavourText: "ToolLine", FlavourMarkdown: "ToolLine markdown"} {
			line := ToolLine(s, map[string]any{s: s}, flavour)
			assertInert(t, label, line)
			if most := len("Calling ") + maxToolName + len("(") + maxToolArgs + len(")"); utf16Len(line) > most {
				t.Errorf("%s = %q is %d units; most %d", label, line, utf16Len(line), most)
			}
			if flavour == FlavourMarkdown {
				assertMarkdownEscapes(t, label, line)
			}
		}
	})
}

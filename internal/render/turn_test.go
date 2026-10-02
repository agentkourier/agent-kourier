package render

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// hostileText mentions, links, escapes and hides in every platform's markup at once. The builders keep it
// literal: making it inert is the adapter's job.
const hostileText = "<!channel> [x](https://evil.example) & \x07 end"

func caps(n int) chat.Caps { return chat.Caps{MaxText: n} }

func TestToolIsOneLineOfLiteralText(t *testing.T) {
	line := Tool("<!channel>\nrun", map[string]any{
		"url": "<https://evil.example|click>", "who": "<@U1> & co", "b\nad": "\u202ex", "n": 3,
	})
	if line.Name != "<!channel> run" {
		t.Errorf("name = %q, want it on one line and not escaped", line.Name)
	}
	want := `b ad="x", n=3, url="<https://evil.example|click>", who="<@U1> & co"`
	if line.Summary != want {
		t.Errorf("summary = %q, want %q", line.Summary, want)
	}
	if got, want := ToolText(line), "Calling <!channel> run("+want+")"; got != want {
		t.Errorf("ToolText = %q, want %q", got, want)
	}
	for _, s := range []string{line.Name, line.Summary} {
		if strings.ContainsAny(s, "\n\t") {
			t.Errorf("%q is not one line", s)
		}
	}
}

func TestToolCapsNameAndArguments(t *testing.T) {
	line := Tool(strings.Repeat("n", 500), map[string]any{"q": strings.Repeat("v", 5000)})
	if utf16Len(line.Name) > maxToolName || utf16Len(line.Summary) > maxToolArgs {
		t.Errorf("name is %d units and summary %d, want at most %d and %d", utf16Len(line.Name), utf16Len(line.Summary), maxToolName, maxToolArgs)
	}
	if !strings.Contains(line.Name, Truncated) || !strings.Contains(line.Summary, Truncated) {
		t.Errorf("%+v should mark both the name and the arguments as truncated", line)
	}
}

func TestToolRedactsBeforeItShowsOrCaps(t *testing.T) {
	line := ToolText(Tool("run", map[string]any{
		"password":      "hunter2hunter2",
		"Authorization": "Bearer abcdefghijklmnop",
		"cfg":           map[string]any{"DB_Password": "hunter2hunter2", "hosts": []any{map[string]any{"apiKey": "k-1234567890", "name": "db1"}}},
		"url":           "https://admin:s3cretpass@db.example/x",
		"cmd":           "curl -H 'Authorization: Bearer xoxb-123-456-abcdefghijkl' https://db.example",
	}))
	for _, secret := range []string{"hunter2hunter2", "s3cretpass", "k-1234567890", "abcdefghijklmnop", "xoxb-123-456", "admin:"} {
		if strings.Contains(line, secret) {
			t.Errorf("tool line %q shows %q", line, secret)
		}
	}
	if !strings.Contains(line, "db1") { // what is not secret is shown
		t.Errorf("tool line %q lost a value that is no secret", line)
	}
	for _, tc := range []struct{ url, want string }{
		{"https://admin:s3cretpass@db.example/x", "https://[redacted]@db.example/x"},
		{"https://user:pa/ss@h", "https://[redacted]@h"},    // a "/" in the password
		{"https://user:p@ss@h/x", "https://[redacted]@h/x"}, // an "@" in the password
	} {
		if got, want := ToolText(Tool("run", map[string]any{"url": tc.url})), `Calling run(url="`+tc.want+`")`; got != want {
			t.Errorf("Tool of %q shows %q, want %q", tc.url, got, want)
		}
	}
	// A secret that sits across the argument cap must not show as a prefix: redaction comes first.
	cut := ToolText(Tool("run", map[string]any{"a": strings.Repeat("x", 118), "u": "https://user:s3cretpass@host", "z": strings.Repeat("y", 50)}))
	if strings.Contains(cut, "s3c") || strings.Contains(cut, "user:") {
		t.Errorf("tool line %q shows part of a secret at the cap", cut)
	}
}

// TestToolRedactsWhatOnlyShowsAcrossTheArgumentsBeforeTheCap: an argument's key is not redacted on its own, so a
// secret in a key (a URL with credentials, a "name=value") shows only in the summary the arguments make
// together, which is redacted as a whole. That has to come before the cap at 160 units, or a cap that falls
// inside the secret leaves a prefix of it with the part that makes it recognisable cut away. The filler is
// swept so that the cap falls at every place in the secret.
func TestToolRedactsWhatOnlyShowsAcrossTheArgumentsBeforeTheCap(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		leaks     []string // none of these may show: a prefix of the secret, and the credentials' shape
	}{
		{"a URL with credentials in a key", "https://user:s3cretpass@db.example", []string{"s3c", "user:"}},
		{"a name=value in a key", "password=hunter2hunter2", []string{"hunt", "hunter"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for filler := 100; filler <= 160; filler++ {
				line := Tool("run", map[string]any{"a": strings.Repeat("x", filler), tc.key: 1, "z": strings.Repeat("y", 40)})
				for _, leak := range tc.leaks {
					if strings.Contains(line.Summary, leak) {
						t.Fatalf("with %d units before the key the summary %q shows %q of the secret", filler, line.Summary, leak)
					}
				}
			}
		})
	}
}

// TestEveryKeyAlternativeRedacts has one key for each of secretKey's alternatives.
func TestEveryKeyAlternativeRedacts(t *testing.T) {
	for _, key := range []string{"passwd", "x_secret", "x_token", "api_key", "Authorization", "my_credentials", "session_cookie"} {
		got := ToolText(Tool("run", map[string]any{key: "VALUE-NOT-TO-SHOW"}))
		if strings.Contains(got, "VALUE-NOT-TO-SHOW") || !strings.Contains(got, "[redacted]") {
			t.Errorf("an argument named %q: %q; want its value redacted", key, got)
		}
	}
}

func TestBodyKeepsArtifactsInOrderOfFirstAppearance(t *testing.T) {
	events := []agent.TextEvent{
		{ArtifactID: "a", Text: "first"},
		{ArtifactID: "b", Text: "<!channel> second"},
		{ArtifactID: "c", Text: "  "},
		{ArtifactID: "a", Text: "first, grown"}, // replaces a's text and keeps a's place
	}
	if got, want := Body(events, 40000), "first, grown\n\n<!channel> second"; got != want {
		t.Errorf("Body = %q, want %q: literal, in order of first appearance", got, want)
	}
	if got := Body(events, 20); utf16Len(got) > 20 || !strings.HasSuffix(got, Truncated) {
		t.Errorf("Body capped at 20 = %q, want at most 20 units ending in the marker", got)
	}
	if got := Body(nil, 40000); got != "" {
		t.Errorf("Body(nil) = %q, want empty", got)
	}
}

func TestBodyLeavesNoBlankParagraphForHiddenText(t *testing.T) {
	events := []agent.TextEvent{
		{ArtifactID: "a", Text: "x [link](https://evil.example)"},
		{ArtifactID: "b", Text: "\u200b\u200d"}, // nothing once cleaned
		{ArtifactID: "c", Text: "y\r\nz"},
	}
	if got, want := Body(events, 40000), "x [link](https://evil.example)\n\ny\nz"; got != want {
		t.Errorf("Body = %q, want %q", got, want)
	}
}

func TestTurnHasTheBodyThenTheToolLine(t *testing.T) {
	tool := &agent.ToolCallEvent{Name: "run", Args: map[string]any{"v": "<!channel>"}}
	m, ok := Turn([]agent.TextEvent{{ArtifactID: "a", Text: hostileText}}, tool, caps(40000))
	if !ok || len(m.Parts) != 2 {
		t.Fatalf("Turn = %+v, %v; want a body and a tool line", m, ok)
	}
	if b, isBody := m.Parts[0].(chat.Body); !isBody || b.Markdown != "<!channel> [x](https://evil.example) &  end" {
		t.Errorf("first part = %+v, want the body, literal and cleaned", m.Parts[0])
	}
	if l, isLine := m.Parts[1].(chat.ToolLine); !isLine || l.Name != "run" || l.Summary != `v="<!channel>"` {
		t.Errorf("second part = %+v, want the tool line", m.Parts[1])
	}
	if m.Fallback != "<!channel> [x](https://evil.example) &  end" {
		t.Errorf("fallback = %q, want the text", m.Fallback)
	}
}

func TestTurnWithOnlyAToolCallFallsBackToItsStatusLine(t *testing.T) {
	m, ok := Turn(nil, &agent.ToolCallEvent{Name: "t"}, caps(40000))
	if !ok || len(m.Parts) != 1 || m.Fallback != "Calling t()" {
		t.Errorf("Turn = %+v, %v; want the status line as the fallback and the only part", m, ok)
	}
	if m, ok := Turn(nil, nil, caps(40000)); ok || len(m.Parts) != 0 {
		t.Errorf("Turn of nothing = %+v, %v", m, ok)
	}
}

func TestTurnCutsTheBodyAtTheCapsAndTheFallbackAtMaxFallback(t *testing.T) {
	long := []agent.TextEvent{{ArtifactID: "a", Text: strings.Repeat("x", 3000)}}
	m, _ := Turn(long, nil, caps(2000))
	body := m.Parts[0].(chat.Body).Markdown
	if utf16Len(body) != 2000 || !strings.HasSuffix(body, Truncated) {
		t.Errorf("body is %d units; want exactly the cap of 2000, marked truncated", utf16Len(body))
	}
	if utf16Len(m.Fallback) != MaxFallback || !strings.HasSuffix(m.Fallback, Truncated) {
		t.Errorf("fallback is %d units; want %d, marked truncated", utf16Len(m.Fallback), MaxFallback)
	}
	// A place that has its own limit uses it.
	small := chat.Caps{MaxText: 40000, Limits: map[chat.Where]int{chat.WhereBody: 100}}
	if m, _ := Turn(long, nil, small); utf16Len(m.Parts[0].(chat.Body).Markdown) != 100 {
		t.Errorf("body of %d units; want the body limit of 100", utf16Len(m.Parts[0].(chat.Body).Markdown))
	}
}

func TestNothingSaysNoOutput(t *testing.T) {
	m := Nothing()
	if b, ok := m.Parts[0].(chat.Body); len(m.Parts) != 1 || !ok || b.Markdown != NoOutput || m.Fallback != NoOutput {
		t.Errorf("Nothing = %+v; want a body saying %q", m, NoOutput)
	}
}

func TestNoticeKeepsItsDetailLiteralOnOneLineAndCapped(t *testing.T) {
	if got := Notice("Stopped.", "<!channel> &\n[x](u)"); got.Fallback != "Stopped. <!channel> & [x](u)" || len(got.Parts) != 0 {
		t.Errorf("Notice = %+v; want fixed words and the detail on one line, literal, with no parts", got)
	}
	if got := Notice("Stopped.", ""); got.Fallback != "Stopped." {
		t.Errorf("Notice with no detail = %q", got.Fallback)
	}
	if got := Notice("x", strings.Repeat("d", 500)); utf16Len(got.Fallback) > 2+maxNoticeDetail || !strings.HasSuffix(got.Fallback, Truncated) {
		t.Errorf("a long detail gives %d units; want it capped and marked", utf16Len(got.Fallback))
	}
}

// FuzzTurnIsBoundedAndClean: whatever the agent sends, a status line is one line, is short, and holds no
// hidden character; a body is cut to its limit and holds none either.
func FuzzTurnIsBoundedAndClean(f *testing.F) {
	for _, tc := range hostile {
		f.Add(tc.in, uint8(200))
	}
	f.Fuzz(func(t *testing.T, s string, l uint8) {
		limit := int(l)
		line := Tool(s, map[string]any{s: s})
		assertNoHidden(t, "name", line.Name, false)
		assertNoHidden(t, "summary", line.Summary, false)
		if utf16Len(line.Name) > maxToolName || utf16Len(line.Summary) > maxToolArgs {
			t.Errorf("%+v is over the caps", line)
		}
		body := Body([]agent.TextEvent{{ArtifactID: "a", Text: s}, {ArtifactID: "b", Text: s}}, limit)
		assertNoHidden(t, "body", body, true)
		if utf16Len(body) > limit {
			t.Errorf("body %q is %d units; limit %d", body, utf16Len(body), limit)
		}
	})
}

// assertNoHidden fails if out holds a control or format character, or a line break where there may be none.
func assertNoHidden(t *testing.T, label, out string, multiline bool) {
	t.Helper()
	for _, r := range out {
		if multiline && (r == '\n' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r) || r == 'ㅤ' || r == '⠀' || r == ' ' || r == ' ' {
			t.Errorf("%s: %q holds %U", label, out, r)
		}
	}
	if !utf8.ValidString(out) {
		t.Errorf("%s: %q is not valid UTF-8", label, out)
	}
}

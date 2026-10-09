package render

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"

	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// The thread block shows a reply's text, name and ID, and none of them holds a secret an invisible character splits,
// or any invisible character at all.
func TestThreadRepliesTakeTheSplitSecretCorpus(t *testing.T) {
	for _, c := range redacttest.SplitSecretsSample() {
		c.CheckKept(t, threadReplies([]ThreadReply{{UserID: "U1", Name: "Ada", Text: c.Text}}, 0))
		c.Check(t, threadReplies([]ThreadReply{{UserID: "U1", Name: c.Text, Text: "ok"}}, 0))
		c.Check(t, threadReplies([]ThreadReply{{UserID: c.Text, Name: "Ada", Text: "ok"}}, 0))
	}
	for _, b := range redacttest.LineBreaks {
		c := redacttest.Breaks(b)
		c.Check(t, threadReplies([]ThreadReply{{UserID: "U1", Name: "Ada", Text: c.Text}}, 0))
	}
}

// The alert block of a chat trigger holds each field as redaction leaves it: no secret an invisible character splits,
// in any field, and no invisible character. The footer is identifier text, so it is held to the token layer of the
// corpus; the other fields to every rule of free text.
func TestAlertBlockTakesTheSplitSecretCorpus(t *testing.T) {
	for _, c := range redacttest.SplitSecretsSample() {
		c.CheckKept(t, untrustedSection(TriggerMessage{Text: c.Text}))
		c.Check(t, untrustedSection(TriggerMessage{Title: c.Text, TitleLink: c.Text, Fallback: c.Text, AttachmentText: c.Text}))
	}
	for _, c := range redacttest.Identifiers(redacttest.SplitSecretsSample()) {
		c.Check(t, untrustedSection(TriggerMessage{Footer: c.Text}))
	}
	for _, b := range redacttest.LineBreaks {
		c := redacttest.Breaks(b)
		c.Check(t, untrustedSection(TriggerMessage{Text: c.Text, AttachmentText: c.Text, Fallback: c.Text, Title: c.Text}))
	}
}

// The footer keeps its "name: value" pairs: "agent-kourier-key: ns/alertname" is the key the footer carries, and the free-text
// rules would take it for a secret named "key". A token in it is still taken out.
func TestAlertFooterKeepsPairsAndLosesTokens(t *testing.T) {
	got := untrustedSection(TriggerMessage{Footer: "agent-kourier-key: KubePodCrashLooping/default"})
	if !strings.Contains(got, "<footer>agent-kourier-key: KubePodCrashLooping/default</footer>") {
		t.Errorf("the footer's key was taken out:\n%s", got)
	}
	got = untrustedSection(TriggerMessage{Footer: "agent-kourier-key: x Bearer Zq9vK3mT7wRx xoxb-1-Zq9vK3mT7wRx"})
	if strings.Contains(got, "Zq9vK3mT7wRx") || !strings.Contains(got, "agent-kourier-key: x") {
		t.Errorf("the footer must lose its tokens and keep its pair:\n%s", got)
	}
}

// What is stored of a tool call's arguments (RedactArgs) and what is shown of them (Tool) take the corpus out alike.
func TestToolArgumentsTakeTheSplitSecretCorpus(t *testing.T) {
	for _, c := range redacttest.SplitSecretsSample() {
		args := map[string]any{"note": c.Text, "list": []any{c.Text}}
		got := RedactArgs(args)
		c.CheckKept(t, got["note"].(string))
		c.CheckKept(t, got["list"].([]any)[0].(string))
		c.Check(t, Tool("run", args).Summary)
	}
	// A name is identifier text, and so is a key: a token that is one is taken out of it.
	for _, c := range redacttest.Identifiers(redacttest.SplitSecretsSample()) {
		c.Check(t, Tool(c.Text, nil).Name)
		for k := range RedactArgs(map[string]any{c.Text: "v"}) {
			c.Check(t, k)
		}
	}
}

// A secret in any field of the message does not reach the agent, and the block's tag is over what the block shows.
func TestPromptTakesSecretsOutOfTheMessage(t *testing.T) {
	const secret = "Zq9vK3mT7wRx"
	msg := TriggerMessage{
		Title:     "db is down password=" + secret,
		Text:      "connect with postgres://admin:" + secret + "@db.example/app\nthen pass\x07word=" + secret,
		TitleLink: "https://am.example/#/alerts?token=" + secret,
		Fallback:  "Authorization: Bearer " + secret,
		Footer:    "Bearer " + secret,
	}
	got, err := newPrompt(t, simpleTemplate).Render(simpleView(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, secret) {
		t.Errorf("the prompt holds the secret:\n%s", got)
	}
	for _, kept := range []string{"db is down", "db.example/app", "then password=" + redacted, "https://am.example/#/alerts?token=" + redacted} {
		if !strings.Contains(got, kept) {
			t.Errorf("the prompt lacks %q:\n%s", kept, got)
		}
	}
	// Two messages that differ only in a secret show the same block, so they share a tag: it is over what is shown.
	other := msg
	other.Title = "db is down password=another"
	again, err := newPrompt(t, simpleTemplate).Render(simpleView(), other)
	if err != nil {
		t.Fatal(err)
	}
	if blockTagOf(t, again) != blockTagOf(t, got) {
		t.Error("two blocks that show the same text got two tags")
	}
}

// An alert field with no white space in it keeps its start, up to the cap, redacted: it is cut at a character, not
// dropped whole. A body in Japanese and a long unbroken link are what one looks like.
func TestAlertFieldsWithoutWhiteSpaceKeepTheirStart(t *testing.T) {
	const secret = "Zq9vK3mT7wRx"
	shown := func(t *testing.T, section, tag string) string {
		t.Helper()
		open, closing := "<"+tag+">", "</"+tag+">"
		_, rest, ok := strings.Cut(section, open)
		body, _, ok2 := strings.Cut(rest, closing)
		if !ok || !ok2 {
			t.Fatalf("the block has no %s:\n%s", tag, section)
		}
		return body
	}
	capped := func(src string) string { // the first MaxFieldUnits units, the last of them the mark of the cut
		return string([]rune(src)[:MaxFieldUnits-chat.Units(chat.Truncated)]) + chat.Truncated
	}

	t.Run("a 6000-byte Japanese body is whole", func(t *testing.T) {
		body := strings.Repeat("\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8", 250) // 8 runes of 3 bytes, 250 times
		if len(body) != 6000 {
			t.Fatalf("the body is %d bytes", len(body))
		}
		if got := shown(t, untrustedSection(TriggerMessage{Text: body}), "text"); got != body {
			t.Errorf("the body came back as %d units: %.60q", chat.Units(got), got)
		}
	})
	t.Run("a longer Japanese body keeps its first 3000 units", func(t *testing.T) {
		body := strings.Repeat("\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8", 5000)
		if got, want := shown(t, untrustedSection(TriggerMessage{Text: body, AttachmentText: body}), "text"), capped(body); got != want {
			t.Errorf("the body came back as %d units, want the first %d and the mark", chat.Units(got), MaxFieldUnits)
		}
	})
	t.Run("a 5 KB unbroken link keeps its first 3000 units", func(t *testing.T) {
		const host = "https://grafana.example.com/d/xyz?orgId=1"
		params := strings.Repeat("&var-host=node-0123456789", 200)
		link := host + params + "&token=" + secret // the secret is past what is shown, and is in the window all the same
		if len(link) < 5000 || strings.ContainsAny(link, " \n") {
			t.Fatalf("the link is %d bytes or holds white space", len(link))
		}
		section := untrustedSection(TriggerMessage{TitleLink: link})
		want := escapeCut(host+params+"&token="+redacted, MaxFieldUnits)
		if got := shown(t, section, "titleLink"); got != want || chat.Units(got) != MaxFieldUnits {
			t.Errorf("the link came back as %d units: %.120q, want the first %d: %.120q", chat.Units(got), got, MaxFieldUnits, want)
		}
		if strings.Contains(section, secret) {
			t.Error("the block holds the secret")
		}
	})
	t.Run("a secret early in an unbroken link is redacted, and its value runs to the end of the link", func(t *testing.T) {
		link := "https://grafana.example.com/d/xyz?token=" + secret + strings.Repeat("&var-host=node-0123456789", 200)
		got := shown(t, untrustedSection(TriggerMessage{TitleLink: link}), "titleLink")
		if want := "https://grafana.example.com/d/xyz?token=" + redacted; got != want {
			t.Errorf("the link came back as %q, want %q", got, want)
		}
	})
	t.Run("a secret that runs across the end of what is shown is not shown", func(t *testing.T) {
		pad := strings.Repeat("a", MaxFieldUnits-chat.Units(chat.Truncated)-4)
		got := shown(t, untrustedSection(TriggerMessage{TitleLink: "https://h/" + pad + "&token=" + secret + "&x=1"}), "titleLink")
		if strings.Contains(got, secret[:4]) {
			t.Errorf("the end of the field shows a start of the secret: %q", got[len(got)-40:])
		}
	})
}

// A free-text rule that shrinks the text before the cut (the value of "password=" here, 11000 bytes that become
// "[redacted]") pulls what lay beside the cut into what the agent is shown: a secret the cut went through, a bare
// key short of its 40 characters or a URL password without its "@", would be shown as the start of it. The last
// run of a cut field is not shown.
func TestAlertFieldNeverShowsWhatTheCutWentThrough(t *testing.T) {
	const key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" // 40 characters: a bare AWS secret access key
	prefix := "debug dump password=" + strings.Repeat("A", 11000) + " ok "
	for name, straddler := range map[string]string{
		"a bare key":               key,
		"a URL password":           "https://admin:Zq9vK3mT7wRxLongPassword@host.example/x",
		"a URL password with text": "see https://admin:Zq9vK3mT7wRxLongPassword@host.example/x",
	} {
		// The straddler begins 20 bytes before the end of the window (alertWindowBytes), and so is cut inside.
		pad := strings.Repeat("x", alertWindowBytes-20-len(prefix)-1) + " "
		for field, msg := range map[string]TriggerMessage{
			"text": {Text: prefix + pad + straddler}, "attachmentText": {AttachmentText: prefix + pad + straddler},
			"fallback": {Fallback: prefix + pad + straddler}, "title": {Title: prefix + pad + straddler},
		} {
			got := untrustedSection(msg)
			for _, shown := range []string{"wJalrXUt", "https://admin", "admin:Zq9", "Zq9vK3"} {
				if strings.Contains(got, shown) {
					t.Errorf("%s in %s: the block shows %q of the cut secret:\n%s", name, field, shown, got)
				}
			}
			if !strings.Contains(got, "debug dump password="+redacted+" ok") {
				t.Errorf("%s in %s: the block lost the start of the field", name, field)
			}
		}
	}
}

// A field of a block that is one line (an alert's title and its link, an author's name and ID) is read joined as well as with
// its lines, so a flag and its value, a user and its password, or a URL's password that a line break cuts apart is taken
// there as it would be on one line. Redacting its lines and joining them after leaks all of them.
func TestOneLineFieldsTakeWhatALineBreakHides(t *testing.T) {
	for _, c := range redacttest.LineSplits() {
		c.Check(t, untrustedSection(TriggerMessage{Title: c.Text}))
		c.Check(t, untrustedSection(TriggerMessage{TitleLink: c.Text}))
		c.Check(t, threadReplies([]ThreadReply{{UserID: "U1", Name: c.Text, Text: "ok"}}, 0))
		c.Check(t, threadReplies([]ThreadReply{{UserID: c.Text, Name: "Ada", Text: "ok"}}, 0))
		for _, kind := range []fieldKind{kindFreeText, kindWindow} {
			f := untrustedField{Kind: kind, OneLine: true, Window: 4096}
			got := f.clean(c.Text)
			c.Check(t, got)
			if strings.ContainsAny(got, "\n\t") {
				t.Errorf("%s: kind %d: %q is not one line", c.Name, kind, got)
			}
		}
	}
}

// The alert title that the review round found: a flag whose value is on the next line reaches the agent redacted. The
// text field, which keeps its lines, is the contrast: its own line structure is what the agent sees of it.
func TestAlertTitleWithAFlagAndItsValueOnTheNextLine(t *testing.T) {
	got := untrustedSection(TriggerMessage{Title: "deploy --password\nhunter2", TitleLink: "https://user:pa\nss@host/x"})
	for _, leaked := range []string{"hunter2", "user:pa", "ss@host"} {
		if strings.Contains(got, leaked) {
			t.Errorf("the block shows %q:\n%s", leaked, got)
		}
	}
	if !strings.Contains(got, "<title>deploy --password [redacted]</title>") || !strings.Contains(got, "<titleLink>https://[redacted]@host/x</titleLink>") {
		t.Errorf("the block does not show the one-line fields redacted:\n%s", got)
	}
}

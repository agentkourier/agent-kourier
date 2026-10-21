package slack_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
	"go.uber.org/goleak"
)

// wire is a RoundTripper that keeps the form of every Web API request that sends text, and forwards it.
type wire struct {
	next http.RoundTripper
	mu   sync.Mutex
	reqs []sentRequest
}

type sentRequest struct {
	method string // the Web API method: chat.postMessage and the like
	form   url.Values
}

func (w *wire) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if form, err := url.ParseQuery(string(body)); err == nil && strings.HasPrefix(r.URL.Path, "/chat.") {
			w.mu.Lock()
			w.reqs = append(w.reqs, sentRequest{method: strings.TrimPrefix(r.URL.Path, "/"), form: form})
			w.mu.Unlock()
		}
	}
	return w.next.RoundTrip(r)
}

func (w *wire) take() []sentRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.reqs
	w.reqs = nil
	return out
}

// handlerTransport is a RoundTripper that has an http.Handler answer, in the caller's goroutine.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil { // a RoundTripper closes the body it was given, whatever it does with it
		defer func() { _ = r.Body.Close() }()
	}
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	res := rec.Result()
	res.Request = r
	return res, nil
}

// shownBy is what the person sees of the text of one request, and the constructs Slack acts on in it, read as
// Slack reads each kind of text, each by the reader of its own dialect: the top-level text and a mrkdwn object as mrkdwn (ScanSlack, which has no
// backslash escape), a markdown block and markdown_text as CommonMark with code shown literally (ScanCommonMarkLiteralCode:
// what the probe of 2026-10-04 measured, rows 04, 19 and 20 of docs/spikes/slack-code-rendering.md; the code that is
// there is only what Agent Kourier wrote; a "<!", "<@" or "<#" in it is still reported, as it is not in what Agent Kourier writes), and a plain_text object
// as the characters it holds (ScanSlackPlain), in which style is not a construct but a token or a link would
// still be one (a plain_text object is not known to parse them, UNCONFIRMED(WP-04), and the adapter does not rely
// on that).
func shownBy(r sentRequest) []chattest.Shown {
	var out []chattest.Shown
	if s := r.form.Get("text"); s != "" {
		out = append(out, chattest.ScanSlack(s))
	}
	if s := r.form.Get("markdown_text"); s != "" {
		out = append(out, chattest.ScanCommonMarkLiteralCode(s))
	}
	if raw := r.form.Get("chunks"); raw != "" {
		out = append(out, chunkTexts(raw)...)
	}
	if raw := r.form.Get("blocks"); raw != "" {
		var blocks any
		if json.Unmarshal([]byte(raw), &blocks) == nil {
			walk(blocks, &out)
		}
	}
	return out
}

// chunkTexts reads the chunks of a streaming request: a markdown_text chunk as CommonMark, as markdown_text is, and the
// title, details and output of a task_update chunk by taskShown.
func chunkTexts(raw string) []chattest.Shown {
	var chunks []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Title   string `json:"title"`
		Details string `json:"details"`
		Output  string `json:"output"`
	}
	if json.Unmarshal([]byte(raw), &chunks) != nil {
		return nil
	}
	var out []chattest.Shown
	if len(chunks) == 0 {
		return nil
	}
	for _, c := range chunks {
		if c.Text != "" {
			out = append(out, chattest.ScanCommonMarkLiteralCode(c.Text))
		}
		for _, s := range []string{c.Title, c.Details, c.Output} {
			if s != "" {
				out = append(out, taskShown(s))
			}
		}
	}
	return out
}

// taskEscapes undoes the escapes the adapter writes in a task's text: the three entities of Slack's text, and the
// backslash escapes that keep a link, an image or a link reference definition out of markdown.
var taskEscapes = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", `\\`, `\`, `\[`, "[", `\(`, "(")

// taskShown is how a task's text is read, which Slack does not say (UNCONFIRMED(WP-04)): the constructs in it are
// those of CommonMark, the strictest of the readings it may have, and what a person sees is the characters it holds with
// the adapter's escapes undone. A reading as plain text finds no construct and shows the backslashes; one as mrkdwn
// finds only the tokens that the entities already stop. Style is the one thing that may act (see slackSubject).
func taskShown(s string) chattest.Shown {
	return chattest.Shown{Displayed: taskEscapes.Replace(s), Active: chattest.ScanCommonMark(s).Active}
}

// richTextShown is how a rich_text block is read, which is where the adapter shows the name and the arguments of a tool: one
// Shown for each element of the block, with the characters of its text elements as what is displayed. A
// rich_text_section, which carries a name, is only that. A rich_text_preformatted, which carries arguments, is a block
// of code, the one construct reported. A token in a text element is not reported, because Block Kit has no token in
// text: a mention, a broadcast, a channel and a link in rich_text are elements of their own type. Every element or
// sub-element of the block that is not a text element with no other field (a user, a broadcast, a channel, a link,
// a styled or any other text, an element of another type) is reported as the construct it is, on every Shown of the block,
// so that the adapter cannot put one anywhere in it by accident. The 2026-10-13 live approval probe returned the
// generated hostile fields as text leaves; this reader remains structural so the unit suite still rejects any future
// renderer change that introduces an active rich_text element.
func richTextShown(block map[string]any) []chattest.Shown {
	var found []chattest.Active
	report := func(kind chattest.Kind, el map[string]any) {
		found = append(found, chattest.Active{Kind: kind, Text: fmt.Sprint(el)})
	}
	var shown []chattest.Shown
	var code []bool
	for _, e := range asList(block["elements"]) {
		el, _ := e.(map[string]any)
		typ, _ := el["type"].(string)
		if typ != "rich_text_section" && typ != "rich_text_preformatted" {
			report(chattest.KindHTML, el)
			continue
		}
		var text strings.Builder
		for _, c := range asList(el["elements"]) {
			child, _ := c.(map[string]any)
			ctyp, _ := child["type"].(string)
			s, isString := child["text"].(string)
			switch {
			case ctyp == "text" && isString && len(child) == 2: // no style, no other field
				text.WriteString(s)
			case ctyp == "user":
				report(chattest.KindMention, child)
			case ctyp == "broadcast":
				report(chattest.KindBroadcast, child)
			case ctyp == "channel":
				report(chattest.KindChannelLink, child)
			case ctyp == "link":
				report(chattest.KindLink, child)
			default:
				report(chattest.KindHTML, child)
			}
		}
		shown = append(shown, chattest.Shown{Displayed: text.String()})
		code = append(code, typ == "rich_text_preformatted")
	}
	if len(shown) == 0 { // a block of nothing readable still reports what it holds
		shown = append(shown, chattest.Shown{})
		code = append(code, false)
	}
	for i := range shown {
		shown[i].Active = slices.Clone(found)
		if code[i] {
			shown[i].Active = append(shown[i].Active, chattest.Active{Kind: chattest.KindCode, Text: shown[i].Displayed})
		}
	}
	return shown
}

func walk(v any, out *[]chattest.Shown) {
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			walk(e, out)
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		if typ == "rich_text" {
			*out = append(*out, richTextShown(v)...)
			return
		}
		if s, isString := v["title"].(string); typ == "task_card" && isString { // a card's title is read as a task's text is
			*out = append(*out, taskShown(s))
		}
		switch s, isString := v["text"].(string); {
		case typ == "plain_text" && isString:
			*out = append(*out, chattest.ScanSlackPlain(s))
		case typ == "mrkdwn" && isString:
			*out = append(*out, chattest.ScanSlack(s))
		case typ == "markdown" && isString:
			*out = append(*out, chattest.ScanCommonMarkLiteralCode(s))
		}
		for k, e := range v {
			if k != "text" || typ == "" { // a text object is a leaf; a block's text is walked as its own object
				walk(e, out)
			} else if _, isObject := e.(map[string]any); isObject {
				walk(e, out)
			}
		}
	}
}

// slackSubject is the Slack adapter over the fake Slack, as a conformance subject. Concessions are what
// Slack does to text that no escape stops, each with the reason; the adapter makes no others.
func slackSubject(t *testing.T) (chattest.Subject, *slack.Platform) {
	t.Helper()
	// The fake answers in-process, with no socket: the checks make tens of thousands of requests, and each over loopback
	// TCP costs the syscalls, the server's goroutines and the buffers of both ends. The client still sends, and reads, what
	// it would over the wire.
	before := goleak.IgnoreCurrent() // as newFake does: the adapter and the fake leave nothing running
	t.Cleanup(func() { goleak.VerifyNone(t, before) })
	fake := fakeslack.NewHandler()
	t.Cleanup(fake.Close)
	w := &wire{next: handlerTransport{fake}}
	cfg := slack.Config{BotToken: secret.New(botToken), AppToken: secret.New(appToken), APIURL: "http://fakeslack.test/",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cfg.HTTPClient = &http.Client{Transport: w}
	c := slack.New(cfg)
	const conn = "conformance/slack"
	root, err := c.Post(t.Context(), "C0CONFORM", "", slack.Content{Text: "root"}, "root")
	if err != nil {
		t.Fatal(err)
	}
	p := slack.NewPlatform(c, conn)
	w.take()
	return chattest.Subject{
		Platform: p,
		Target:   chat.Target{Conn: conn, Channel: "C0CONFORM", Thread: root},
		Sent: func() []chattest.Shown {
			var out []chattest.Shown
			for _, r := range w.take() {
				out = append(out, shownBy(r)...)
			}
			return out
		},
		Inert:    inertRequests(w),
		TooLarge: &chat.Prompt{ID: "ix-big", Questions: make([]chat.Question, 60)},
		// A prompt of tools too large for one message: 30 tools take 62 blocks.
		ToolApprovals: true,
		TooLargeTools: &chat.Prompt{ID: "ix-big-tools", Tools: make([]chat.Tool, 30)},
		// A user ID of Slack's shape is a mention in a Mention part, and nothing else is.
		MentionUser: "U0123456789",
		// Slack makes a bare URL in text and in markdown into a link that shows its own address, and no escape
		// stops it: a backslash before a colon is shown as a backslash, and Slack has no escape for mrkdwn. The
		// link goes where the text says and nowhere else, and every request is inert (no unfurl, so Slack does
		// not fetch it). UNCONFIRMED(WP-04): whether a plain_text object does the same.
		AllowAutolink: true,
		Concessions: []chattest.Concession{{
			Where: chat.WhereToolArgs, Kinds: []chattest.Kind{chattest.KindCode},
			Why: "the arguments of a tool are shown as preformatted text, which is code, on purpose: they are a command or data, and " +
				"must be read as typed. Nothing else a reader acts on is in the block (richTextShown)",
		}, {
			Where: chat.WhereStepTitle, Kinds: chattest.StyleKinds(),
			Why: "a task's text may be read as markdown, in which *a* is bold: cosmetic, and every construct that is not style (a link, an " +
				"image, a mention, a broadcast, HTML) is escaped, which is what is read",
		}, {
			Where: chat.WhereStepDetail, Kinds: chattest.StyleKinds(),
			Why: "a task's text may be read as markdown, in which *a* is bold: cosmetic, and every construct that is not style (a link, an " +
				"image, a mention, a broadcast, HTML) is escaped, which is what is read",
		}, {
			Where: chat.WhereFallback, Kinds: chattest.StyleKinds(),
			Why: "a message's top-level text is mrkdwn and has no plain form: *a* shows bold in a notification. Slack shows it only where " +
				"it cannot show the blocks, and the adapter sends every message's text as a plain_text block too, which is what is read",
		}},
	}, p
}

// inertRequests checks that every request was sent with Slack's parsing and unfurling off.
func inertRequests(w *wire) func() error {
	return func() error {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, r := range w.reqs {
			f := r.form
			if f.Get("unfurl_links") != "false" || f.Get("unfurl_media") != "false" || f.Has("link_names") || f.Get("parse") == "full" {
				return errors.New(r.method + " was not inert: " + f.Encode())
			}
		}
		return nil
	}
}

func TestSlackAdapterPassesConformanceAgainstTheFakeSlack(t *testing.T) {
	chattest.ConformanceSharded(t, 8, func(t *testing.T) chattest.Subject {
		s, _ := slackSubject(t)
		return s
	})
}

// An adapter's own wiring of Sent is part of the gate, so the Slack reading is itself tested: what it says
// of text that Slack would act on must be what it is.
func TestTheSlackReadingOfTheWireFindsWhatSlackWouldActOn(t *testing.T) {
	r := func(form url.Values) sentRequest { return sentRequest{method: "chat.postMessage", form: form} }
	for _, tc := range []struct {
		name string
		form url.Values
		want []chattest.Kind
	}{
		{"an unescaped broadcast in the top-level text", url.Values{"text": {"<!channel>"}}, []chattest.Kind{chattest.KindBroadcast}},
		{"an unescaped mention in a plain_text object", url.Values{"blocks": {`[{"type":"header","text":{"type":"plain_text","text":"<@U1>"}}]`}}, []chattest.Kind{chattest.KindMention}},
		{"an unescaped link in a markdown block", url.Values{"blocks": {`[{"type":"markdown","text":"[x](https://evil.test)"}]`}}, []chattest.Kind{chattest.KindLink}},
		{"a mention in a context block's mrkdwn element", url.Values{"blocks": {`[{"type":"context","elements":[{"type":"mrkdwn","text":"<@U1>"}]}]`}}, []chattest.Kind{chattest.KindMention}},
		{"a button label", url.Values{"blocks": {`[{"type":"actions","elements":[{"type":"button","text":{"type":"plain_text","text":"<!here>"},"value":"v"}]}]`}}, []chattest.Kind{chattest.KindBroadcast}},
		{"an option of a select", url.Values{"blocks": {`[{"type":"input","label":{"type":"plain_text","text":"l"},"element":{"type":"radio_buttons","options":[{"text":{"type":"plain_text","text":"<!channel>"},"value":"0"}]}}]`}}, []chattest.Kind{chattest.KindBroadcast}},
		{"escaped text is no construct", url.Values{"text": {"&lt;!channel&gt;"}, "blocks": {`[{"type":"header","text":{"type":"plain_text","text":"&lt;@U1&gt; *b*"}}]`}}, nil},
	} {
		var kinds []chattest.Kind
		for _, s := range shownBy(r(tc.form)) {
			for _, a := range s.Active {
				kinds = append(kinds, a.Kind)
			}
		}
		if len(kinds) != len(tc.want) || (len(kinds) > 0 && kinds[0] != tc.want[0]) {
			t.Errorf("%s: found %v, want %v", tc.name, kinds, tc.want)
		}
	}
}

// The reading of a rich_text block is itself tested: the text of a section and of a preformatted element is what is
// displayed, and a user, a broadcast, a channel, a link, a style or any other element, in either and in the block, is found
// and attached to every Shown of the block.
func TestTheReadingOfARichTextBlockFindsWhatIsNotPlainText(t *testing.T) {
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	el := func(typ string, children ...map[string]any) map[string]any {
		list := make([]any, len(children))
		for i, c := range children {
			list[i] = c
		}
		return map[string]any{"type": typ, "elements": list}
	}
	read := func(elements ...map[string]any) []chattest.Shown {
		list := make([]any, len(elements))
		for i, e := range elements {
			list[i] = e
		}
		var got []chattest.Shown
		walk([]any{map[string]any{"type": "rich_text", "elements": list}}, &got)
		return got
	}
	got := read(el("rich_text_section", text("1. <!channel>")), el("rich_text_preformatted", text("<@U0123456789> [x](https://evil.test)"), text("\nmore")))
	if len(got) != 2 || got[0].Displayed != "1. <!channel>" || got[1].Displayed != "<@U0123456789> [x](https://evil.test)\nmore" {
		t.Fatalf("shown = %+v, want the characters of each element", got)
	}
	if len(got[0].Active) != 0 || len(got[1].Active) != 1 || got[1].Active[0].Kind != chattest.KindCode {
		t.Errorf("active = %+v and %+v, want none for the name and the code the arguments are", got[0].Active, got[1].Active)
	}
	for kind, bad := range map[chattest.Kind]map[string]any{
		chattest.KindMention:     {"type": "user", "user_id": "U0123456789"},
		chattest.KindBroadcast:   {"type": "broadcast", "range": "channel"},
		chattest.KindChannelLink: {"type": "channel", "channel_id": "C1"},
		chattest.KindLink:        {"type": "link", "url": "https://evil.test"},
		chattest.KindHTML:        {"type": "text", "text": "x", "style": map[string]any{"bold": true}},
	} {
		for where, block := range map[string][]chattest.Shown{
			"the name's section":    read(el("rich_text_section", text("a"), bad), el("rich_text_preformatted", text("b"))),
			"the arguments":         read(el("rich_text_section", text("a")), el("rich_text_preformatted", text("b"), bad)),
			"a sibling section":     read(el("rich_text_section", text("a")), el("rich_text_section", bad), el("rich_text_preformatted", text("b"))),
			"a second preformatted": read(el("rich_text_preformatted", text("a")), el("rich_text_preformatted", bad)),
		} {
			for i, s := range block {
				found := false
				for _, a := range s.Active {
					found = found || a.Kind == kind
				}
				if !found {
					t.Errorf("%s: the element %v was not reported as a %s on Shown %d: %+v", where, bad, kind, i, s.Active)
				}
			}
		}
	}
	if odd := read(map[string]any{"type": "rich_text_list"}); len(odd[0].Active) == 0 {
		t.Error("an element of another type than a section or preformatted text was not reported")
	}
}

// The API names no acknowledgment reaction, so Slack says its own: the hourglass a Binding that names none gets.
func TestSlackSuppliesTheDefaultAcknowledgmentReaction(t *testing.T) {
	if got := slack.NewPlatform(nil, "ns/conn").Capabilities().AckReaction; got != "hourglass_flowing_sand" {
		t.Errorf("AckReaction = %q, want hourglass_flowing_sand", got)
	}
}

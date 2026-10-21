package slack_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// shownBy is what the person sees of the text of one request, and the constructs Slack acts on in it: the text,
// markdown_text, chunks and blocks of its form, read by chattest.ShownSlackMessage.
func shownBy(r sentRequest) []chattest.Shown {
	return chattest.ShownSlackMessage(chattest.SlackMessage{
		Text:         r.form.Get("text"),
		MarkdownText: r.form.Get("markdown_text"),
		Blocks:       json.RawMessage(r.form.Get("blocks")),
		Chunks:       json.RawMessage(r.form.Get("chunks")),
	})
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
				"must be read as typed. Nothing else a reader acts on is in the block (chattest.ShownSlackMessage)",
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
		blocks, err := json.Marshal([]any{map[string]any{"type": "rich_text", "elements": list}})
		if err != nil {
			t.Fatal(err)
		}
		return chattest.ShownSlackMessage(chattest.SlackMessage{Blocks: blocks})
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

package slack_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
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

// shownBy is what the person sees of the text of one request, and the constructs Slack acts on in it, read as
// Slack reads each kind of text, each by the reader of its own dialect: the top-level text and a mrkdwn object as mrkdwn (ScanSlack, which has no
// backslash escape), a markdown block and markdown_text as CommonMark (ScanCommonMark), and a plain_text object
// as the characters it holds (ScanSlackPlain), in which style is not a construct but a token or a link would
// still be one (a plain_text object is not known to parse them, UNCONFIRMED(WP-04), and the adapter does not rely
// on that).
func shownBy(r sentRequest) []chattest.Shown {
	var out []chattest.Shown
	if s := r.form.Get("text"); s != "" {
		out = append(out, chattest.ScanSlack(s))
	}
	if s := r.form.Get("markdown_text"); s != "" {
		out = append(out, chattest.ScanCommonMark(s))
	}
	if raw := r.form.Get("blocks"); raw != "" {
		var blocks any
		if json.Unmarshal([]byte(raw), &blocks) == nil {
			walk(blocks, &out)
		}
	}
	return out
}

func walk(v any, out *[]chattest.Shown) {
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			walk(e, out)
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		switch s, isString := v["text"].(string); {
		case typ == "plain_text" && isString:
			*out = append(*out, chattest.ScanSlackPlain(s))
		case typ == "mrkdwn" && isString:
			*out = append(*out, chattest.ScanSlack(s))
		case typ == "markdown" && isString:
			*out = append(*out, chattest.ScanCommonMark(s))
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
	fake := newFake(t)
	w := &wire{next: http.DefaultTransport}
	cfg := clientConfig(fake)
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
		// Slack makes a bare URL in text and in markdown into a link that shows its own address, and no escape
		// stops it: a backslash before a colon is shown as a backslash, and Slack has no escape for mrkdwn. The
		// link goes where the text says and nowhere else, and every request is inert (no unfurl, so Slack does
		// not fetch it). UNCONFIRMED(WP-04): whether a plain_text object does the same.
		AllowAutolink: true,
		Concessions: []chattest.Concession{{
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

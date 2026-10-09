package fakeslack_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

const testToken = "xoxb-test-token" // obviously fake

// newFake starts a fake Slack and a slack-go client pointed at it.
func newFake(t *testing.T, opts ...fakeslack.Option) (*fakeslack.Server, *slack.Client) {
	t.Helper()
	fake := fakeslack.New(opts...)
	t.Cleanup(fake.Close)
	return fake, slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/"))
}

func text(s string) slack.MsgOption { return slack.MsgOptionText(s, false) }

// rawBody calls a method over plain HTTP and returns the response body, for
// checks on JSON shape that slack-go's decoding would hide.
func rawBody(t *testing.T, fake *fakeslack.Server, method string, form url.Values) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fake.URL()+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body strings.Builder
	if _, err := io.Copy(&body, resp.Body); err != nil {
		t.Fatalf("read: %v", err)
	}
	return body.String()
}

func TestPostMessageStoresTextBlocksAndMetadata(t *testing.T) {
	fake, api := newFake(t)
	blocks := slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", "*bold*", false, false), nil, nil)
	meta := slack.SlackMetadata{EventType: "agentkourier_message", EventPayload: map[string]any{"outbox_id": "ob-1"}}

	channel, ts, err := api.PostMessage("C1", text("hello"), slack.MsgOptionBlocks(blocks), slack.MsgOptionMetadata(meta))
	if err != nil || channel != "C1" || ts == "" {
		t.Fatalf("PostMessage = %q, %q, %v", channel, ts, err)
	}

	msgs := fake.Messages("C1")
	if len(msgs) != 1 {
		t.Fatalf("stored %d messages; want 1", len(msgs))
	}
	m := msgs[0]
	if m.TS != ts || m.Text != "hello" || m.ThreadTS != "" || m.Metadata == nil ||
		m.Metadata.EventType != "agentkourier_message" || m.Metadata.EventPayload["outbox_id"] != "ob-1" {
		t.Fatalf("stored message = %+v", m)
	}
	// Blocks are stored opaquely, byte for byte as the client sent them.
	if want, _ := json.Marshal([]slack.Block{blocks}); string(m.Blocks) != string(want) {
		t.Fatalf("blocks = %s; want %s", m.Blocks, want)
	}
	if len(fake.Messages("C2")) != 0 {
		t.Fatal("a message leaked into another channel")
	}
}

func TestPostMessageThreadReply(t *testing.T) {
	fake, api := newFake(t)
	_, parent, err := api.PostMessage("C1", text("root"))
	if err != nil {
		t.Fatalf("post root: %v", err)
	}
	if got := fake.Thread("C1", parent); len(got) != 1 {
		t.Fatalf("thread before any reply = %+v; want just the parent", got)
	}
	_, reply, err := api.PostMessage("C1", text("reply"), slack.MsgOptionTS(parent))
	if err != nil {
		t.Fatalf("post reply: %v", err)
	}

	thread := fake.Thread("C1", parent)
	if len(thread) != 2 || thread[0].TS != parent || thread[1].TS != reply || thread[1].Text != "reply" {
		t.Fatalf("thread = %+v; want parent then reply", thread)
	}
	// Once a message has replies, Slack reports it as a thread parent.
	if thread[0].ThreadTS != parent || thread[1].ThreadTS != parent {
		t.Fatalf("thread_ts = %q, %q; want both %q", thread[0].ThreadTS, thread[1].ThreadTS, parent)
	}
	if fake.Thread("C1", "1.000001") != nil {
		t.Fatal("Thread of an unknown ts should be nil")
	}
	if _, _, err := api.PostMessage("C1", text("orphan"), slack.MsgOptionTS("1.000001")); err == nil || err.Error() != "thread_not_found" {
		t.Fatalf("reply to unknown thread = %v; want thread_not_found", err)
	}
}

func TestPostMessageTimestampsAreUniqueAndOrdered(t *testing.T) {
	fake, api := newFake(t)
	const posts = 40
	var wg sync.WaitGroup
	for range posts {
		wg.Go(func() {
			if _, _, err := api.PostMessage("C1", text("x")); err != nil {
				t.Errorf("PostMessage: %v", err)
			}
		})
	}
	wg.Wait()

	msgs := fake.Messages("C1")
	if len(msgs) != posts {
		t.Fatalf("stored %d messages; want %d", len(msgs), posts)
	}
	for i := 1; i < len(msgs); i++ { // fixed-width digits, so string order is time order
		if msgs[i-1].TS >= msgs[i].TS {
			t.Fatalf("ts %q is not before %q", msgs[i-1].TS, msgs[i].TS)
		}
	}
}

func TestErrorsUseSlackEnvelopeAndCodes(t *testing.T) {
	fake, api := newFake(t)
	if _, _, err := api.PostMessage("C1"); err == nil || err.Error() != "no_text" {
		t.Fatalf("post without content via slack-go = %v; want no_text", err)
	}
	bad := slack.New("not-a-bot-token", slack.OptionAPIURL(fake.URL()+"/"))
	if _, _, err := bad.PostMessage("C1", text("x")); err == nil || err.Error() != "invalid_auth" {
		t.Fatalf("post with a bad token via slack-go = %v; want invalid_auth", err)
	}

	valid := func(kv ...string) url.Values {
		v := url.Values{"token": {testToken}, "channel": {"C1"}, "text": {"x"}}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	for name, tc := range map[string]struct {
		method string
		form   url.Values
		header string
		want   string
	}{
		"no token":            {"chat.postMessage", valid("token", ""), "", "not_authed"},
		"non-bot token":       {"chat.postMessage", valid("token", "xoxp-user"), "", "invalid_auth"},
		"bearer header":       {"chat.postMessage", valid("token", ""), "Bearer " + testToken, ""},
		"bad bearer header":   {"chat.postMessage", valid("token", ""), "Bearer nope", "invalid_auth"},
		"unknown method":      {"chat.nope", valid(), "", "unknown_method"},
		"missing channel":     {"chat.postMessage", valid("channel", ""), "", "channel_not_found"},
		"blocks not an array": {"chat.postMessage", valid("blocks", "{}"), "", "invalid_blocks_format"},
		"block not an object": {"chat.postMessage", valid("blocks", `["x"]`), "", "invalid_blocks"},
		"block without type":  {"chat.postMessage", valid("blocks", `[{}]`), "", "invalid_blocks"},
		"block empty type":    {"chat.postMessage", valid("blocks", `[{"type":""}]`), "", "invalid_blocks"},
		"metadata not json":   {"chat.postMessage", valid("metadata", "nope"), "", "invalid_metadata_format"},
		"metadata no type":    {"chat.postMessage", valid("metadata", `{"event_payload":{}}`), "", "invalid_metadata_format"},
	} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, fake.URL()+"/"+tc.method,
				strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			var env struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// Slack reports API errors with HTTP 200 and ok=false.
			if resp.StatusCode != http.StatusOK || env.OK != (tc.want == "") || env.Error != tc.want {
				t.Fatalf("got HTTP %d, %+v; want 200 with error %q", resp.StatusCode, env, tc.want)
			}
		})
	}
}

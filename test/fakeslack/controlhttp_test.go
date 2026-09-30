package fakeslack_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// control calls a /_control/ endpoint over real HTTP, without a token. body is
// marshaled to JSON unless it is a string, which is sent as is. The response is
// decoded into out if it is not nil, and the status is returned.
func control(t *testing.T, fake *fakeslack.Server, method, path string, body, out any) int {
	t.Helper()
	raw, ok := body.(string)
	if !ok && body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		raw = string(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, fake.URL()+path, bytes.NewBufferString(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode (status %d): %v", method, path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

func TestControlActsAsAUser(t *testing.T) {
	fake, smc := connected(t)
	bot := slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/"))
	root := post(t, bot, text("investigating"))

	var mention, reply, dm fakeslack.UserMessage
	if s := control(t, fake, "POST", "/_control/mention", map[string]string{"channel": "C1", "user": "U1", "text": mentionText}, &mention); s != 200 ||
		len(mention.Envelopes) != 2 || mention.Channel != "C1" {
		t.Fatalf("mention = %d, %+v; want two envelopes", s, mention)
	}
	if env, _, first := nextInner(t, smc); env != mention.Envelopes[0] {
		t.Fatalf("first event = %q %#v; want the app_mention envelope", env, first)
	}
	nextInner(t, smc) // the mention's message event
	if s := control(t, fake, "POST", "/_control/reply", map[string]string{"channel": "C1", "thread_ts": root, "user": "U1", "text": "more"}, &reply); s != 200 {
		t.Fatalf("reply status = %d", s)
	}
	wantMessage(t, smc, reply, root, "channel", "more")
	if s := control(t, fake, "POST", "/_control/dm", map[string]string{"user": "U1", "text": "hi"}, &dm); s != 200 || dm.Channel != fakeslack.DMChannel("U1") {
		t.Fatalf("dm = %d, %+v", s, dm)
	}
	wantMessage(t, smc, dm, "", "im", "hi")
}

func TestControlClicksSubmitsAndReportsAcks(t *testing.T) {
	fake, smc := connected(t)
	_, ask := postAsk(t, fake)
	var click, submit map[string]string

	if s := control(t, fake, "POST", "/_control/click", map[string]string{"user": "U1", "message_ts": ask, "action_id": "approve", "value": "yes"}, &click); s != 200 {
		t.Fatalf("click status = %d", s)
	}
	if id, cb := nextInteraction(t, smc); id != click["envelope_id"] || cb.ActionCallback.BlockActions[0].ActionID != "approve" {
		t.Fatalf("interaction %q %+v; want the click %v", id, cb, click)
	}
	body := map[string]any{"user": "U1", "message_ts": ask, "action_id": "submit",
		"values": map[string]any{"q1": map[string]any{"answer_1": map[string]string{"type": "plain_text_input", "value": "prod"}}}}
	if s := control(t, fake, "POST", "/_control/submit", body, &submit); s != 200 {
		t.Fatalf("submit status = %d", s)
	}
	if _, cb := nextInteraction(t, smc); cb.BlockActionState.Values["q1"]["answer_1"].Value != "prod" {
		t.Fatalf("submit state = %+v; want the answer", cb.BlockActionState)
	}

	var ack struct {
		Acked   bool
		Payload json.RawMessage
	}
	for _, id := range []string{click["envelope_id"], submit["envelope_id"]} {
		if s := control(t, fake, "GET", "/_control/acks/"+id+"?timeout=5s", nil, &ack); s != 200 || !ack.Acked {
			t.Fatalf("ack of %s = %d, %+v; want acked", id, s, ack)
		}
	}
	// An envelope the client never acks: the wait ends at the timeout with acked false.
	unacked, err := fake.SendEnvelope("slash_commands", slashCommand)
	if err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}
	if s := control(t, fake, "GET", "/_control/acks/"+unacked+"?timeout=50ms", nil, &ack); s != 200 || ack.Acked {
		t.Fatalf("unacked = %d, %+v; want acked false", s, ack)
	}
	for path, want := range map[string]int{
		"/_control/acks/env-nope": 404, "/_control/acks/" + unacked + "?timeout=1h": 400, "/_control/acks/" + unacked + "?timeout=soon": 400,
	} {
		if s := control(t, fake, "GET", path, nil, nil); s != want {
			t.Fatalf("GET %s = %d; want %d", path, s, want)
		}
	}
}

func TestControlSetsUpAndInspects(t *testing.T) {
	fake, _ := connected(t)
	api := slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/"))
	root := post(t, api, text("root"))
	if s := control(t, fake, "POST", "/_control/users", fakeslack.User{ID: "U1", Name: "ada", Email: "ada@example.com"}, nil); s != 200 {
		t.Fatalf("users status = %d", s)
	}
	if u, err := api.GetUserInfo("U1"); err != nil || u.Profile.Email != "ada@example.com" {
		t.Fatalf("users.info = %+v, %v; want the user the control call added", u, err)
	}
	if s := control(t, fake, "POST", "/_control/members", map[string]any{"channel": "C1", "users": []string{"U1", "U2"}}, nil); s != 200 {
		t.Fatalf("members status = %d", s)
	}
	if members, _, err := api.GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: "C1"}); err != nil || len(members) != 2 {
		t.Fatalf("conversations.members = %v, %v; want the two set", members, err)
	}

	if _, err := fake.Reply("C1", root, "U1", "hello"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	var msgs, thread []map[string]any
	if s := control(t, fake, "GET", "/_control/messages?channel=C1", nil, &msgs); s != 200 || len(msgs) != 2 ||
		msgs[0]["bot_id"] != "BFAKEBOT" || msgs[1]["user"] != "U1" {
		t.Fatalf("messages = %d, %v; want the bot post and the user reply", s, msgs)
	}
	if s := control(t, fake, "GET", "/_control/thread?channel=C1&ts="+root, nil, &thread); s != 200 || len(thread) != 2 {
		t.Fatalf("thread = %d, %v; want the root and its reply", s, thread)
	}
	if s := control(t, fake, "GET", "/_control/thread?channel=C1&ts=1.000001", nil, nil); s != 404 {
		t.Fatalf("unknown thread status = %d; want 404", s)
	}
	var conns map[string]int
	if s := control(t, fake, "GET", "/_control/connections", nil, &conns); s != 200 || conns["connections"] != 1 {
		t.Fatalf("connections = %d, %v; want 1", s, conns)
	}
}

func TestControlMapsErrorsToStatuses(t *testing.T) {
	fake, _ := connected(t)
	_, ask := postAsk(t, fake)
	click := func(action, value string) map[string]string {
		return map[string]string{"user": "U1", "message_ts": ask, "action_id": action, "value": value}
	}
	tests := []struct {
		name, path string
		body       any
		want       int
	}{
		{"malformed JSON", "/_control/mention", "{", 400},
		{"misspelled field", "/_control/mention", `{"chanel": "C1"}`, 400},
		{"mention without the bot", "/_control/mention", map[string]string{"channel": "C1", "user": "U1", "text": "hi"}, 400},
		{"reply to a missing thread", "/_control/reply", map[string]string{"channel": "C1", "thread_ts": "1.000001", "user": "U1", "text": "x"}, 404},
		{"click on a missing message", "/_control/click", map[string]string{"message_ts": "1.000001", "action_id": "approve"}, 404},
		{"click on a missing action", "/_control/click", click("nope", ""), 404},
		{"click with the wrong value", "/_control/click", click("approve", "no"), 400},
		{"click on an input", "/_control/click", click("answer_1", ""), 400},
		{"no such endpoint", "/_control/nope", "{}", 404},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out map[string]string
			if s := control(t, fake, "POST", tc.path, tc.body, &out); s != tc.want || out["error"] == "" {
				t.Fatalf("status %d, body %v; want %d with an error message", s, out, tc.want)
			}
		})
	}

	leakCheck(t)
	bare := fakeslack.New() // no Socket Mode client
	t.Cleanup(bare.Close)
	var out map[string]string
	if s := control(t, bare, "POST", "/_control/dm", map[string]string{"user": "U1", "text": "hi"}, &out); s != 409 || out["error"] == "" {
		t.Fatalf("dm with no client = %d, %v; want 409", s, out)
	}
}

func TestCloseEndsAckWaits(t *testing.T) {
	fake, smc := connected(t)
	id, err := fake.SendEnvelope("slash_commands", slashCommand)
	if err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}
	waitEvent(t, smc, socketmode.EventTypeSlashCommand) // delivered, and never acked
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, fake.URL()+"/_control/acks/"+id+"?timeout=30s", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond) // let the poll start waiting
	start := time.Now()
	fake.Close()
	<-polled
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v behind a 30s ack poll", d)
	}
}

func TestControlAndSlackMethodsDoNotMix(t *testing.T) {
	fake, _ := newFake(t)
	// A Slack method name under /_control/ is not a Slack method, and posts nothing.
	var out map[string]string
	if s := control(t, fake, "POST", "/_control/chat.postMessage", map[string]string{"channel": "C1", "text": "x", "token": testToken}, &out); s != 404 {
		t.Fatalf("/_control/chat.postMessage = %d; want 404", s)
	}
	if n := len(fake.Messages("C1")); n != 0 {
		t.Fatalf("a request under /_control/ posted %d Slack messages", n)
	}
	// A control name without the prefix is not a control endpoint: Slack routing
	// answers, and with a valid token it does not know the method.
	body := rawBody(t, fake, "connections", url.Values{"token": {testToken}})
	if !strings.Contains(body, `"error":"unknown_method"`) {
		t.Fatalf("Slack method connections = %s; want unknown_method", body)
	}
	// The control surface takes no Slack token: none was sent above.
	if s := control(t, fake, "GET", "/_control/connections", nil, nil); s != 200 {
		t.Fatalf("connections without a token = %d; want 200", s)
	}
}

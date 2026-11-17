package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/chat/web/kourierv1"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/test/fakeweb"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

const webTestSecret = "fake-web-test-secret"

// newWebFake returns a client for an in-process fake web sender and receiver, whose chat listener answers 202.
func newWebFake(t *testing.T) (*Web, string) {
	t.Helper()
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	t.Cleanup(chatSrv.Close)
	fake, err := fakeweb.New(fakeweb.Config{ChatURL: chatSrv.URL, KourierV1Secret: []byte(webTestSecret), GitHubSecret: []byte(webTestSecret)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return NewWeb(srv.URL), srv.URL
}

// postReply sends the fake receiver the reply r as the kourier-v1 profile encodes it, and fails the test unless it answers want.
func postReply(t *testing.T, base string, r web.Reply, want int) {
	t.Helper()
	p, err := kourierv1.New(secret.New(webTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := p.Encode(r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+fakeweb.RepliesPath, bytes.NewReader(enc.Body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = enc.Header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("the receiver answered %d; want %d", resp.StatusCode, want)
	}
}

// TestWebSendsAndReadsTheThreadsReplies: Send reports the chat listener's answer; ThreadReplies keeps the replies of one thread of one
// connection; Posts leaves out the updates, the repeats and a try the receiver failed (FailNext); WaitThread returns once its predicate
// holds.
func TestWebSendsAndReadsTheThreadsReplies(t *testing.T) {
	w, base := newWebFake(t)
	ctx := t.Context()
	sent, err := w.Send(ctx, fakeweb.Send{Profile: fakeweb.ProfileKourierV1, Path: RouteWebKourier, Body: fakeweb.Message{Channel: ChannelWebKourier, ID: "m-1", User: "u", Text: "t"}.Body()})
	if err != nil || sent.Status != http.StatusAccepted {
		t.Fatalf("Send = %+v, %v; want 202", sent, err)
	}
	msg := chat.Say(chat.Message{Fallback: "hi", Parts: []chat.Part{chat.Body{Markdown: "hi"}}})
	post := web.Reply{Kind: web.ReplyPost, Connection: ConnWebKourier, Channel: ChannelWebKourier, Thread: "m-1", ID: "tag-1", Key: "tag-1", Message: msg}
	if err := w.FailNext(ctx, 1, http.StatusServiceUnavailable); err != nil {
		t.Fatal(err)
	}
	postReply(t, base, post, http.StatusServiceUnavailable) // failed on purpose
	postReply(t, base, post, http.StatusOK)
	postReply(t, base, post, http.StatusOK) // a repeat
	update := post
	update.Kind, update.Key = web.ReplyUpdate, "update-1"
	postReply(t, base, update, http.StatusOK)
	other := post
	other.Thread, other.ID, other.Key = "m-9", "tag-9", "tag-9"
	postReply(t, base, other, http.StatusOK)

	got, err := w.ThreadReplies(ctx, ConnWebKourier, ChannelWebKourier, "m-1")
	if err != nil || len(got) != 4 {
		t.Fatalf("ThreadReplies = %d replies, %v; want the failed try, the post, its repeat and the update of m-1", len(got), err)
	}
	if posts := Posts(got); len(posts) != 1 || posts[0].Key != "tag-1" {
		t.Errorf("Posts = %+v; want the one post tag-1", posts)
	}
	waited, err := w.WaitThread(ctx, 5*time.Second, "the other thread's post", ConnWebKourier, ChannelWebKourier, "m-9", func(rs []fakeweb.Reply) bool { return len(Posts(rs)) == 1 })
	if err != nil || len(waited) != 1 {
		t.Errorf("WaitThread = %+v, %v", waited, err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := w.WaitThread(ctx, 200*time.Millisecond, "a thread with no reply", ConnWebKourier, ChannelWebKourier, "m-none", func(rs []fakeweb.Reply) bool { return len(rs) > 0 }); err == nil {
		t.Error("WaitThread returned for a thread with no reply")
	}
}

// TestWebAgentTokenIsTheOneTheStubOwnsTheTasksBy: the token the scenarios list the web Bindings' tasks with names WebAgentEmail, which is
// who the stub says owns them; Tasks reads each task's context and the text it was started with, across pages, and another identity sees none.
func TestWebAgentTokenIsTheOneTheStubOwnsTheTasksBy(t *testing.T) {
	s := stubagent.New(stubagent.WithRequiredAuth())
	t.Cleanup(s.Close)
	stubagent.SpikeAgents(s)
	for i := range listPage + 2 { // more than a page
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "1", "method": "SendMessage", "params": map[string]any{
			"message": map[string]any{"messageId": fmt.Sprint("m", i), "role": "ROLE_USER", "parts": []map[string]any{{"text": fmt.Sprint("prompt ", i)}}}}})
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.AgentURL("kagent", AgentWebGitHub), bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+WebAgentToken())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("SendMessage answered %d", resp.StatusCode)
		}
	}
	stub := NewStub(s.URL())
	tasks, err := stub.Tasks(t.Context(), "kagent", AgentWebGitHub, WebAgentToken())
	if err != nil || len(tasks) != listPage+2 {
		t.Fatalf("Tasks = %d tasks, %v; want %d", len(tasks), err, listPage+2)
	}
	if tasks[0].Prompt != "prompt 0" || tasks[listPage+1].Prompt != fmt.Sprint("prompt ", listPage+1) || tasks[0].ContextID == "" || tasks[0].ID == "" {
		t.Errorf("the first and last tasks are %+v and %+v; want each with its prompt, its ID and its context", tasks[0], tasks[listPage+1])
	}
	enc := base64.RawURLEncoding
	other := enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(`{"email":"someone-else@example.test"}`)) + "."
	if mine, err := stub.Tasks(t.Context(), "kagent", AgentWebGitHub, other); err != nil || len(mine) != 0 {
		t.Errorf("another identity lists %d tasks (%v); want none", len(mine), err)
	}
	if _, err := stub.Tasks(t.Context(), "kagent", "no-such-agent", WebAgentToken()); err == nil {
		t.Error("Tasks of an agent the stub does not have succeeded")
	}
}

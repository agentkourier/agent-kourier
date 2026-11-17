package fakeweb_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/chat/web/github"
	"github.com/agentkourier/agent-kourier/internal/chat/web/kourierv1"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/test/fakeweb"
)

// The tests' secrets and login: fake constants.
const (
	kourierKey = "fake-web-kourier-secret"
	githubKey  = "fake-web-github-secret"
	login      = "pr-reviewer"
)

var epoch = time.Unix(1_800_000_000, 0)

// listener is a stand-in for the broker's chat listener: it keeps each request it is sent and answers it as answer says.
type listener struct {
	mu     sync.Mutex
	reqs   []*http.Request
	bodies [][]byte
	answer func(w http.ResponseWriter)
}

func (l *listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	l.reqs, l.bodies = append(l.reqs, r), append(l.bodies, body)
	answer := l.answer
	l.mu.Unlock()
	if answer == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	answer(w)
}

func (l *listener) last(t *testing.T) (*http.Request, []byte) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.reqs) == 0 {
		t.Fatal("the chat listener was sent nothing")
	}
	return l.reqs[len(l.reqs)-1], l.bodies[len(l.bodies)-1]
}

// newFake returns the fake, at its URL, sending to a stand-in chat listener.
func newFake(t *testing.T) (string, *listener) {
	t.Helper()
	l := &listener{}
	chat := httptest.NewServer(l)
	t.Cleanup(chat.Close)
	srv, err := fakeweb.New(fakeweb.Config{ChatURL: chat.URL, KourierV1Secret: []byte(kourierKey), GitHubSecret: []byte(githubKey), Now: func() time.Time { return epoch }})
	if err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewServer(srv)
	t.Cleanup(fake.Close)
	return fake.URL, l
}

// post sends body to url and returns the status and the answer's body.
func post(t *testing.T, url string, h http.Header, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if h != nil {
		req.Header = h
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, got
}

// send asks the fake to send in, and returns what it answered.
func send(t *testing.T, fake string, in fakeweb.Send) fakeweb.Sent {
	t.Helper()
	b, _ := json.Marshal(in)
	status, body := post(t, fake+fakeweb.ControlSendPath, nil, b)
	if status != http.StatusOK {
		t.Fatalf("send %+v: status %d: %s", in, status, body)
	}
	var sent fakeweb.Sent
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	return sent
}

func replies(t *testing.T, fake string) []fakeweb.Reply {
	t.Helper()
	resp, err := http.Get(fake + fakeweb.ControlRepliesPath) //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out []fakeweb.Reply
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func kourierProfile(t *testing.T) *kourierv1.Profile {
	t.Helper()
	p, err := kourierv1.New(secret.New(kourierKey))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func githubProfile(t *testing.T) *github.Profile {
	t.Helper()
	p, err := github.New(secret.New(githubKey), login)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTheSenderSignsAKourierV1MessageTheProfileTakes: what the sender sends as kourier-v1 is a message the profile verifies and decodes,
// at the route the test named; the same timestamp again is the same request byte for byte; and a bad or no signature is one the
// profile refuses.
func TestTheSenderSignsAKourierV1MessageTheProfileTakes(t *testing.T) {
	fake, l := newFake(t)
	msg := fakeweb.Message{Channel: "web-1", ID: "m-1", User: "u-alice", Text: "hello [there](http://example.test)"}
	sent := send(t, fake, fakeweb.Send{Profile: fakeweb.ProfileKourierV1, Path: "/chat/web/e2e-web/kourier", Body: msg.Body()})
	if sent.Status != http.StatusAccepted || sent.Timestamp != epoch.Unix() || sent.Error != "" {
		t.Fatalf("Sent = %+v; want 202, signed at the fake's now", sent)
	}
	req, body := l.last(t)
	if req.URL.Path != "/chat/web/e2e-web/kourier" || req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("the sender sent %s %s (%s); want a JSON POST to the route", req.Method, req.URL.Path, req.Header.Get("Content-Type"))
	}
	p := kourierProfile(t)
	if err := p.Verify(req.Header, body, epoch); err != nil {
		t.Fatalf("the profile does not verify the sender's signature: %v", err)
	}
	ev, ok, err := p.Decode(req.Header, body)
	if err != nil || !ok || ev.Channel != "web-1" || ev.ID != "m-1" || ev.User != "u-alice" || ev.Text != msg.Text || !ev.Mentioned {
		t.Fatalf("Decode = %+v, %v, %v; want the message, which opens a thread and so is mentioned", ev, ok, err)
	}
	firstSig := req.Header.Get(fakeweb.HeaderSignature)

	again := send(t, fake, fakeweb.Send{Profile: fakeweb.ProfileKourierV1, Path: "/chat/web/e2e-web/kourier", Body: msg.Body(), Timestamp: sent.Timestamp})
	req, body2 := l.last(t)
	if again.Timestamp != sent.Timestamp || req.Header.Get(fakeweb.HeaderSignature) != firstSig || !bytes.Equal(body, body2) {
		t.Errorf("the same body at the same timestamp was signed %q, not %q: it is not the same request", req.Header.Get(fakeweb.HeaderSignature), firstSig)
	}

	reply := fakeweb.Message{Channel: "web-1", Thread: "m-1", ID: "m-2", User: "u-bob", Text: "and you", Mention: true}
	send(t, fake, fakeweb.Send{Profile: fakeweb.ProfileKourierV1, Path: "/chat/web/e2e-web/kourier", Body: reply.Body()})
	req, body = l.last(t)
	if ev, ok, err := p.Decode(req.Header, body); err != nil || !ok || ev.Thread != "m-1" || !ev.Mentioned {
		t.Errorf("a mentioned reply decodes as %+v, %v, %v; want it in the thread m-1, mentioned", ev, ok, err)
	}

	for _, signing := range []string{fakeweb.SignBad, fakeweb.SignNone} {
		send(t, fake, fakeweb.Send{Profile: fakeweb.ProfileKourierV1, Path: "/chat/web/e2e-web/kourier", Body: msg.Body(), Signing: signing})
		req, body := l.last(t)
		if err := p.Verify(req.Header, body, epoch); err == nil {
			t.Errorf("signing %q: the profile verified it", signing)
		}
	}
}

// TestTheSenderSignsAGitHubDeliveryTheProfileTakes: what the sender sends as github is a delivery the profile verifies, with GitHub's
// headers, and an opened pull request decodes as the event of its thread; a bad or no signature is refused; a ping is answered and
// carries nothing.
func TestTheSenderSignsAGitHubDeliveryTheProfileTakes(t *testing.T) {
	fake, l := newFake(t)
	pr := fakeweb.PullRequest{Action: "opened", Repo: "agentkourier/e2e-web", Number: 7, Title: "Fix the flaky test", Sender: "octocat"}
	sent := send(t, fake, fakeweb.Send{Profile: fakeweb.ProfileGitHub, Path: "/chat/web/e2e-web/github", Body: pr.Body(), Event: "pull_request"})
	if sent.Status != http.StatusAccepted || sent.Delivery == "" {
		t.Fatalf("Sent = %+v; want 202 and the delivery's GUID", sent)
	}
	req, body := l.last(t)
	for _, h := range []string{fakeweb.HeaderGitHubDelivery, fakeweb.HeaderGitHubHookID, fakeweb.HeaderGitHubTargetType, fakeweb.HeaderGitHubTargetID, fakeweb.HeaderGitHubSignature1} {
		if req.Header.Get(h) == "" {
			t.Errorf("the delivery has no %s", h)
		}
	}
	if req.Header.Get(fakeweb.HeaderGitHubDelivery) != sent.Delivery || !strings.HasPrefix(req.Header.Get("User-Agent"), "GitHub-Hookshot/") {
		t.Errorf("the delivery's headers are %v; want the GUID it reports and GitHub's user agent", req.Header)
	}
	p := githubProfile(t)
	if err := p.Verify(req.Header, body, epoch); err != nil {
		t.Fatalf("the profile does not verify the sender's signature: %v", err)
	}
	ev, ok, err := p.Decode(req.Header, body)
	if err != nil || !ok || ev.Channel != pr.Repo || ev.Thread != "pr-7" || ev.User != "octocat" || !strings.Contains(ev.Text, pr.Title) || !ev.Mentioned {
		t.Fatalf("Decode = %+v, %v, %v; want the opened pull request in the thread pr-7, its title in the text", ev, ok, err)
	}

	c := fakeweb.IssueComment{Repo: pr.Repo, Number: 7, Title: pr.Title, Sender: "octocat", Text: "@" + login + " please look again", CommentID: 99}
	send(t, fake, fakeweb.Send{Profile: fakeweb.ProfileGitHub, Path: "/chat/web/e2e-web/github", Body: c.Body(), Event: "issue_comment"})
	req, body = l.last(t)
	if ev, ok, err := p.Decode(req.Header, body); err != nil || !ok || ev.Thread != "pr-7" || !strings.Contains(ev.Text, "please look again") {
		t.Errorf("a comment that mentions the login decodes as %+v, %v, %v; want it in the thread pr-7", ev, ok, err)
	}

	self := pr
	self.Sender = login
	send(t, fake, fakeweb.Send{Profile: fakeweb.ProfileGitHub, Path: "/chat/web/e2e-web/github", Body: self.Body(), Event: "pull_request"})
	req, body = l.last(t)
	if _, ok, err := p.Decode(req.Header, body); err != nil || ok {
		t.Errorf("a pull request the login opened decodes as an event to take (%v, %v); want it dropped", ok, err)
	}

	send(t, fake, fakeweb.Send{Profile: fakeweb.ProfileGitHub, Path: "/chat/web/e2e-web/github", Body: fakeweb.Ping(pr.Repo), Event: "ping"})
	req, body = l.last(t)
	if err := p.Verify(req.Header, body, epoch); err != nil {
		t.Errorf("a ping: %v", err)
	}
	if _, ok, err := p.Decode(req.Header, body); err != nil || ok {
		t.Errorf("a ping decodes as %v, %v; want nothing to take", ok, err)
	}

	for _, signing := range []string{fakeweb.SignBad, fakeweb.SignNone} {
		send(t, fake, fakeweb.Send{Profile: fakeweb.ProfileGitHub, Path: "/chat/web/e2e-web/github", Body: pr.Body(), Event: "pull_request", Signing: signing})
		req, body := l.last(t)
		if err := p.Verify(req.Header, body, epoch); err == nil {
			t.Errorf("signing %q: the profile verified it", signing)
		}
		if signing == fakeweb.SignNone && (req.Header.Get(fakeweb.HeaderGitHubSignature) != "" || req.Header.Get(fakeweb.HeaderGitHubSignature1) != "") {
			t.Errorf("an unsigned delivery carries a signature: %v", req.Header)
		}
	}
}

// TestTheSenderReportsHowItWasAnswered: the status and Retry-After of the answer, and the transport's error when there is no answer.
func TestTheSenderReportsHowItWasAnswered(t *testing.T) {
	fake, l := newFake(t)
	l.answer = func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	msg := fakeweb.Message{Channel: "web-1", ID: "m-1", User: "u-alice", Text: "hi"}
	if sent := send(t, fake, fakeweb.Send{Profile: fakeweb.ProfileKourierV1, Path: "/chat/web/e2e-web/kourier", Body: msg.Body()}); sent.Status != http.StatusServiceUnavailable || sent.RetryAfter != "5" {
		t.Errorf("Sent = %+v; want 503 with Retry-After 5", sent)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	srv, err := fakeweb.New(fakeweb.Config{ChatURL: gone.URL, KourierV1Secret: []byte(kourierKey), GitHubSecret: []byte(githubKey)})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	defer h.Close()
	if sent := send(t, h.URL, fakeweb.Send{Profile: fakeweb.ProfileKourierV1, Path: "/chat/web/e2e-web/kourier", Body: msg.Body()}); sent.Status != 0 || sent.Error == "" {
		t.Errorf("Sent = %+v; want no status and the transport's error", sent)
	}
}

// TestTheSenderRefusesWhatItCannotSend: a path outside the chat listener's web routes, a profile, a signing it does not know, a github
// send with no event, and a body that is not a Send are refused, and nothing is sent.
func TestTheSenderRefusesWhatItCannotSend(t *testing.T) {
	fake, l := newFake(t)
	body := fakeweb.Message{Channel: "web-1", ID: "m-1", User: "u", Text: "t"}.Body()
	for name, in := range map[string]fakeweb.Send{
		"another route":    {Profile: fakeweb.ProfileKourierV1, Path: "/_control/send", Body: body},
		"slack's route":    {Profile: fakeweb.ProfileKourierV1, Path: "/chat/slack/agent-kourier/slack-fast", Body: body},
		"a path with more": {Profile: fakeweb.ProfileKourierV1, Path: "/chat/web/e2e-web/kourier/x", Body: body},
		"another profile":  {Profile: "jira", Path: "/chat/web/e2e-web/kourier", Body: body},
		"another signing":  {Profile: fakeweb.ProfileKourierV1, Path: "/chat/web/e2e-web/kourier", Body: body, Signing: "later"},
		"no github event":  {Profile: fakeweb.ProfileGitHub, Path: "/chat/web/e2e-web/github", Body: body},
	} {
		b, _ := json.Marshal(in)
		if status, _ := post(t, fake+fakeweb.ControlSendPath, nil, b); status != http.StatusBadRequest {
			t.Errorf("%s: status %d; want 400", name, status)
		}
	}
	if status, _ := post(t, fake+fakeweb.ControlSendPath, nil, []byte(`{"profile":"github","bogus":1}`)); status != http.StatusBadRequest {
		t.Errorf("a body that is not a Send: status %d; want 400", status)
	}
	if len(l.reqs) != 0 {
		t.Errorf("the chat listener was sent %d requests; want none", len(l.reqs))
	}
}

// encode is the request the kourier-v1 profile makes for r, as the platform sends it.
func encode(t *testing.T, r web.Reply, at time.Time) web.Request {
	t.Helper()
	req, err := kourierProfile(t).Encode(r, at)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestTheReceiverTakesWhatTheProfileEncodes: a reply the kourier-v1 profile encodes is verified, answered 200 and recorded with its
// key, envelope and the markdown of its body as the profile wrote it; a post again with the same key is a repeat, and an update is not.
func TestTheReceiverTakesWhatTheProfileEncodes(t *testing.T) {
	fake, _ := newFake(t)
	msg := chat.Say(chat.Message{Fallback: "hi", Parts: []chat.Part{chat.Body{Markdown: "see [this](http://evil.test) <b>now</b>"}}})
	r := web.Reply{Kind: web.ReplyPost, Connection: "e2e-web/kourier", Channel: "web-1", Thread: "m-1", ID: "tag-1", Key: "tag-1", Message: msg}
	req := encode(t, r, epoch)
	if status, body := post(t, fake+fakeweb.RepliesPath, req.Header, req.Body); status != http.StatusOK {
		t.Fatalf("the receiver answered %d (%s); want 200", status, body)
	}
	if status, _ := post(t, fake+fakeweb.RepliesPath, req.Header, req.Body); status != http.StatusOK {
		t.Fatalf("the receiver answered a repeat %d; want 200", status)
	}
	u := r
	u.Kind, u.Key = web.ReplyUpdate, "update-1"
	req = encode(t, u, epoch)
	if status, _ := post(t, fake+fakeweb.RepliesPath, req.Header, req.Body); status != http.StatusOK {
		t.Fatalf("the receiver answered an update %d; want 200", status)
	}
	got := replies(t, fake)
	if len(got) != 3 {
		t.Fatalf("the receiver recorded %d replies; want 3: %+v", len(got), got)
	}
	first := got[0]
	if first.Problem != "" || first.Status != http.StatusOK || first.Key != "tag-1" || first.Repeat || first.Kind != "post" || first.Connection != "e2e-web/kourier" || first.Channel != "web-1" ||
		first.Thread != "m-1" || first.ID != "tag-1" || first.Fallback != "hi" || !first.At.Equal(epoch) {
		t.Errorf("the first reply is recorded as %+v", first)
	}
	if want := []string{`see \[this\](http://evil.test) &lt;b>now&lt;/b>`}; !slices.Equal(first.Markdown, want) {
		t.Errorf("its markdown is %q; want %q, as the profile wrote it", first.Markdown, want)
	}
	if !got[1].Repeat || got[1].Problem != "" {
		t.Errorf("the second post with the key tag-1 is %+v; want a repeat", got[1])
	}
	if got[2].Repeat || got[2].Kind != "update" || got[2].Key != "update-1" {
		t.Errorf("the update is %+v; want an update, not a repeat", got[2])
	}
}

// TestTheReceiverRefusesAReplyThatBreaksTheContract: an unsigned reply, a stale one, one signed with another secret or over another body
// is answered 401; one with no key, or that is not an envelope, 400. Each is recorded with its problem.
func TestTheReceiverRefusesAReplyThatBreaksTheContract(t *testing.T) {
	fake, _ := newFake(t)
	msg := chat.Say(chat.Message{Fallback: "hi", Parts: []chat.Part{chat.Body{Markdown: "hi"}}})
	good := encode(t, web.Reply{Kind: web.ReplyPost, Connection: "e2e-web/kourier", Channel: "web-1", ID: "tag-1", Key: "tag-1", Message: msg}, epoch)
	with := func(edit func(h http.Header)) http.Header {
		h := good.Header.Clone()
		edit(h)
		return h
	}
	stale := encode(t, web.Reply{Kind: web.ReplyPost, Connection: "e2e-web/kourier", Channel: "web-1", ID: "tag-1", Key: "tag-1", Message: msg}, epoch.Add(-fakeweb.MaxSkew-time.Second))
	other, err := kourierv1.New(secret.New("another-fake-secret"))
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := other.Encode(web.Reply{Kind: web.ReplyPost, Connection: "e2e-web/kourier", Channel: "web-1", ID: "tag-1", Key: "tag-1", Message: msg}, epoch)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		h       http.Header
		body    []byte
		status  int
		problem string
	}{
		{"no signature", with(func(h http.Header) { h.Del(fakeweb.HeaderSignature) }), good.Body, http.StatusUnauthorized, fakeweb.ProblemUnsigned},
		{"no timestamp", with(func(h http.Header) { h.Del(fakeweb.HeaderTimestamp) }), good.Body, http.StatusUnauthorized, fakeweb.ProblemUnsigned},
		{"a stale timestamp", stale.Header, stale.Body, http.StatusUnauthorized, fakeweb.ProblemStale},
		{"another secret", otherKey.Header, otherKey.Body, http.StatusUnauthorized, fakeweb.ProblemSignature},
		{"another body", good.Header, append(bytes.Clone(good.Body), ' '), http.StatusUnauthorized, fakeweb.ProblemSignature},
		{"no key", with(func(h http.Header) { h.Del(fakeweb.HeaderIdempotencyKey) }), good.Body, http.StatusBadRequest, fakeweb.ProblemNoKey},
		{"not an envelope", signed([]byte(`{"v":2}`)), []byte(`{"v":2}`), http.StatusBadRequest, fakeweb.ProblemEnvelope},
	} {
		status, _ := post(t, fake+fakeweb.RepliesPath, tc.h, tc.body)
		got := replies(t, fake)
		if status != tc.status || len(got) == 0 || got[len(got)-1].Problem != tc.problem || got[len(got)-1].Status != tc.status {
			t.Fatalf("%s: status %d, recorded %+v; want %d and the problem %s", tc.name, status, got[len(got)-1:], tc.status, tc.problem)
		}
		// A refused reply keeps the envelope it carried, so that a test waiting for its thread sees it, and why.
		if last := got[len(got)-1]; tc.problem != fakeweb.ProblemEnvelope && (last.Kind != "post" || last.Connection != "e2e-web/kourier" || last.Channel != "web-1" || last.ID != "tag-1") {
			t.Errorf("%s: recorded %+v; want the refused reply's envelope", tc.name, last)
		}
	}
	if got := replies(t, fake); slices.ContainsFunc(got, func(r fakeweb.Reply) bool { return r.Repeat }) {
		t.Errorf("a refused reply was taken as a post: the next was a repeat: %+v", got)
	}
}

// TestTheReceiverFailsTheRepliesATestAsksItTo: after a Failure the receiver answers the next replies that keep the contract with its
// status, a 503 with a Retry-After, and records each with that status; a failed post was not taken, so its next try with the same key is
// no repeat, and the one after that is. A reply that breaks the contract does not use up a failure. A Failure it cannot take is a 400.
func TestTheReceiverFailsTheRepliesATestAsksItTo(t *testing.T) {
	fake, _ := newFake(t)
	msg := chat.Say(chat.Message{Fallback: "hi", Parts: []chat.Part{chat.Body{Markdown: "hi"}}})
	good := encode(t, web.Reply{Kind: web.ReplyPost, Connection: "e2e-web/kourier", Channel: "web-1", ID: "tag-1", Key: "tag-1", Message: msg}, epoch)
	if status, body := post(t, fake+fakeweb.ControlFailNextPath, nil, []byte(`{"n":1,"status":503}`)); status != http.StatusNoContent {
		t.Fatalf("fail-next answered %d (%s); want 204", status, body)
	}
	unsigned := good.Header.Clone()
	unsigned.Del(fakeweb.HeaderSignature)
	if status, _ := post(t, fake+fakeweb.RepliesPath, unsigned, good.Body); status != http.StatusUnauthorized {
		t.Errorf("an unsigned reply after a Failure is answered %d; want 401", status)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fake+fakeweb.RepliesPath, bytes.NewReader(good.Body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = good.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Errorf("the failed post is answered %d with Retry-After %q; want 503 and 1", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	for i := range 2 {
		if status, _ := post(t, fake+fakeweb.RepliesPath, good.Header, good.Body); status != http.StatusOK {
			t.Errorf("try %d after the failed one is answered %d; want 200", i+2, status)
		}
	}
	got := replies(t, fake)
	if len(got) != 4 {
		t.Fatalf("recorded %d replies; want 4: %+v", len(got), got)
	}
	if failed := got[1]; failed.Status != http.StatusServiceUnavailable || failed.Problem != "" || failed.Repeat || failed.Key != "tag-1" {
		t.Errorf("the failed post is recorded as %+v; want status 503, no problem, no repeat", failed)
	}
	if got[2].Status != http.StatusOK || got[2].Repeat || !got[3].Repeat {
		t.Errorf("the tries after the failed one are %+v and %+v; want the first taken, not a repeat, and the second a repeat", got[2], got[3])
	}
	for _, bad := range []string{`{"n":0,"status":503}`, `{"n":11,"status":503}`, `{"n":1,"status":200}`, `{"n":1,"status":600}`, `{"n":1,"status":503,"x":1}`, `nope`} {
		if status, _ := post(t, fake+fakeweb.ControlFailNextPath, nil, []byte(bad)); status != http.StatusBadRequest {
			t.Errorf("fail-next %s answered %d; want 400", bad, status)
		}
	}
}

// signed is the headers of a kourier-v1 reply of body at epoch, signed with the tests' secret, with a key.
func signed(body []byte) http.Header {
	ts, sig := kourierv1.Sign([]byte(kourierKey), epoch, body)
	h := http.Header{}
	h.Set(fakeweb.HeaderTimestamp, ts)
	h.Set(fakeweb.HeaderSignature, sig)
	h.Set(fakeweb.HeaderIdempotencyKey, "k")
	return h
}

// TestNewRefusesAConfigItCannotRunWith: a chat URL that is not absolute, or has a path, and an empty secret.
func TestNewRefusesAConfigItCannotRunWith(t *testing.T) {
	for name, cfg := range map[string]fakeweb.Config{
		"no URL":            {KourierV1Secret: []byte(kourierKey), GitHubSecret: []byte(githubKey)},
		"a relative URL":    {ChatURL: "/chat", KourierV1Secret: []byte(kourierKey), GitHubSecret: []byte(githubKey)},
		"a path":            {ChatURL: "http://chat.test/chat/web", KourierV1Secret: []byte(kourierKey), GitHubSecret: []byte(githubKey)},
		"no kourier secret": {ChatURL: "http://chat.test", GitHubSecret: []byte(githubKey)},
		"no github secret":  {ChatURL: "http://chat.test", KourierV1Secret: []byte(kourierKey)},
	} {
		if _, err := fakeweb.New(cfg); err == nil {
			t.Errorf("%s: New succeeded", name)
		} else if strings.Contains(err.Error(), kourierKey) || strings.Contains(err.Error(), githubKey) {
			t.Errorf("%s: the error %q names a secret", name, err)
		}
	}
	if _, err := fakeweb.New(fakeweb.Config{ChatURL: "http://chat.test:8081/", KourierV1Secret: []byte(kourierKey), GitHubSecret: []byte(githubKey)}); err != nil {
		t.Errorf("a host with a port and a trailing slash: %v", err)
	}
}

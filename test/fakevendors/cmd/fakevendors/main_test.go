package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakevendors"
)

// lineWriter sends each line written to it on a channel: run writes one line, the listening line, and must not block on it.
type lineWriter chan string

func (l lineWriter) Write(p []byte) (int, error) {
	l <- strings.TrimSpace(string(p))
	return len(p), nil
}

// The fake secret and the identity of the test's GitHub source.
const (
	testSecret   = "fake-test-github-secret"
	testIdentity = "acme-review-agent"
	testPath     = "/chat/webhook/acme-webhooks/github"
)

// chatRequest is one request the fake chat listener was sent.
type chatRequest struct {
	path, signature, login string
	body                   []byte
}

// chatListener is a chat listener that records what it is sent and answers 200.
func chatListener(t *testing.T) (*httptest.Server, func() []chatRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var event struct {
			Sender struct {
				Login string `json:"login"`
			} `json:"sender"`
		}
		_ = json.Unmarshal(body, &event)
		mu.Lock()
		got = append(got, chatRequest{r.URL.Path, r.Header.Get("X-Hub-Signature-256"), event.Sender.Login, body})
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, func() []chatRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]chatRequest(nil), got...)
	}
}

// post sends body as JSON to url and decodes the 200 answer into out.
func post(t *testing.T, url string, body any, out any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s answered %d", url, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

func sign(body []byte) string {
	m := hmac.New(sha256.New, []byte(testSecret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// TestRunSignsWithTheSourcesGivenAndServesTheVendorAPI: a -source is a vendor's connection; the fake signs a delivery of it with its secret
// and sends it to the route on the chat URL, and a post on the vendor's API is sent back with the source's identity as the actor. It serves
// until its context ends.
func TestRunSignsWithTheSourcesGivenAndServesTheVendorAPI(t *testing.T) {
	chat, requests := chatListener(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-chat-url", chat.URL,
			"-source", "vendor=github,path=" + testPath + ",identity=" + testIdentity + ",secret=" + testSecret}, out)
	}()
	addr, ok := strings.CutPrefix(<-out, "fakevendors: listening on ")
	if !ok {
		t.Fatal("run did not announce its address")
	}
	base := "http://" + addr

	d := fakevendors.GitHubPR{Action: "opened", Repo: "acme/widgets", Number: 42, HeadSHA: "abc1234", Sender: "octocat"}.Delivery()
	var sent fakevendors.Sent
	post(t, base+fakevendors.ControlSendPath, d, &sent)
	if sent.Status != http.StatusOK {
		t.Fatalf("the delivery was answered %+v; want 200", sent)
	}
	post(t, base+"/vendor/github/repos/acme/widgets/pulls/42/reviews", map[string]string{"body": "looks right", "commit_id": "abc1234"}, nil)

	got := requests()
	if len(got) != 2 {
		t.Fatalf("the chat listener was sent %d requests; want the delivery and the one of the agent's review", len(got))
	}
	for i, r := range got {
		if r.path != testPath || r.signature != sign(r.body) {
			t.Errorf("request %d went to %s signed %q; want %s, signed with the source's secret", i, r.path, r.signature, testPath)
		}
	}
	if got[0].login != "octocat" || got[1].login != testIdentity {
		t.Errorf("the senders are %q and %q; want the delivery's and then the source's identity", got[0].login, got[1].login)
	}
	resp, err := http.Get(base + fakevendors.ControlPostsPath) //nolint:noctx // a test against a local server
	if err != nil {
		t.Fatal(err)
	}
	var posts []fakevendors.Post
	err = json.NewDecoder(resp.Body).Decode(&posts)
	_ = resp.Body.Close()
	if err != nil || len(posts) != 1 || posts[0].Target != "acme/widgets#42" || posts[0].SelfStatus != http.StatusOK {
		t.Errorf("the posts are %+v, %v; want the review, its delivery answered 200", posts, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after cancel = %v; want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
}

// TestRunRefusesSourcesItCannotUseWithoutRepeatingTheSecret: every bad flag is an error before the fake listens, and none of them repeats
// the secret given.
func TestRunRefusesSourcesItCannotUseWithoutRepeatingTheSecret(t *testing.T) {
	const secret = "fake-secret-not-to-repeat"
	good := "vendor=github,path=" + testPath + ",identity=" + testIdentity + ",secret=" + secret
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no chat URL", []string{"-source", good}},
		{"a chat URL that is not absolute", []string{"-chat-url", "agent-kourier-chat:8081", "-source", good}},
		{"no source", []string{"-chat-url", "http://chat.test"}},
		{"an unknown key", []string{"-chat-url", "http://chat.test", "-source", good + ",colour=" + secret}},
		{"a part with no =", []string{"-chat-url", "http://chat.test", "-source", good + "," + secret}},
		{"an unknown vendor", []string{"-chat-url", "http://chat.test", "-source", strings.Replace(good, "vendor=github", "vendor=gitlab", 1)}},
		{"no secret", []string{"-chat-url", "http://chat.test", "-source", strings.TrimSuffix(good, ",secret="+secret)}},
		{"a path off the webhook routes", []string{"-chat-url", "http://chat.test", "-source", strings.Replace(good, testPath, "/chat/slack/a/b", 1)}},
		{"the same vendor twice", []string{"-chat-url", "http://chat.test", "-source", good, "-source", good}},
		{"an unknown flag", []string{"-nope", secret}},
	} {
		err := run(t.Context(), append([]string{"-addr", "127.0.0.1:0"}, tc.args...), io.Discard)
		switch {
		case err == nil:
			t.Errorf("%s: run = nil; want an error", tc.name)
		case strings.Contains(err.Error(), secret):
			t.Errorf("%s: run = %v; the error repeats the secret", tc.name, err)
		}
	}
}

package e2e

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/test/fakevendors"
)

// TestVendorsDrivesTheFakeVendors: Send has the fake sign a delivery and send it to the vendor's connection, and returns how the connection
// answered, a refusal too; Posts reads what the agent posted on the vendor's API. A fault of the fake's own is an error.
func TestVendorsDrivesTheFakeVendors(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	answer := http.StatusOK
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		w.WriteHeader(answer)
	}))
	defer chat.Close()
	fake, err := fakevendors.New(fakevendors.Config{ChatURL: chat.URL, Sources: map[string]fakevendors.Source{
		fakevendors.GitHub: {Path: WebhookRoute(fakevendors.GitHub), Secret: []byte("fake-secret"), Identity: "acme-review-agent[bot]"}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	v := NewVendors(srv.URL)
	ctx := context.Background()

	d := fakevendors.GitHubPR{Action: "opened", Repo: "acme/widgets", Number: 7, HeadSHA: "abc1234", Sender: "octocat"}.Delivery()
	if sent, err := v.Send(ctx, d); err != nil || sent.Status != http.StatusOK {
		t.Fatalf("Send = %+v, %v; want the connection's 200", sent, err)
	}
	mu.Lock()
	answer = http.StatusUnauthorized
	mu.Unlock()
	d.Signing = fakevendors.SignBad
	if sent, err := v.Send(ctx, d); err != nil || sent.Status != http.StatusUnauthorized {
		t.Fatalf("Send of a forged delivery = %+v, %v; a 401 is an answer, not an error", sent, err)
	}
	if posts, err := v.Posts(ctx); err != nil || len(posts) != 0 {
		t.Fatalf("Posts = %+v, %v; want none yet", posts, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/vendor/github/repos/acme/widgets/issues/7/comments", strings.NewReader(`{"body":"yes"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	posts, err := v.Posts(ctx)
	if err != nil || len(posts) != 1 || posts[0].Target != "acme/widgets#7" || posts[0].Kind != "comment" {
		t.Fatalf("Posts = %+v, %v; want the comment on acme/widgets#7", posts, err)
	}
	mu.Lock()
	if want := WebhookRoute(fakevendors.GitHub); len(paths) != 3 || paths[0] != want || paths[1] != want || paths[2] != want {
		t.Errorf("the connection was sent %q; want the two deliveries and the comment's, each at %s", paths, want)
	}
	mu.Unlock()

	var se *StatusError
	if _, err := v.Send(ctx, fakevendors.Delivery{Vendor: fakevendors.GitHub, Signing: "sideways"}); err != nil {
		t.Fatalf("Send with a signing the fake does not know = %v; the fake answers 200 with the reason in Sent", err)
	}
	if _, err := NewVendors(srv.URL + "/nowhere").Posts(ctx); !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Errorf("Posts from a path the fake does not serve = %v; want its 404", err)
	}
}

func TestWebhookNames(t *testing.T) {
	if got := WebhookConnection(fakevendors.Jira); got != "acme-webhooks/jira" {
		t.Errorf("WebhookConnection(jira) = %q", got)
	}
	if got := WebhookRoute(fakevendors.Linear); got != "/chat/webhook/acme-webhooks/linear" {
		t.Errorf("WebhookRoute(linear) = %q", got)
	}
	if got := WebhookBinding(fakevendors.GitHub); got != "acme-webhooks/github-reviews" {
		t.Errorf("WebhookBinding(github) = %q", got)
	}
}

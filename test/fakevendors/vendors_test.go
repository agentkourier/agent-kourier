package fakevendors_test

import (
	"bytes"
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

	"github.com/agentkourier/agent-kourier/test/fakevendors"
)

const fakeSecret = "fake-vendor-secret"

// chat is a chat listener that records each request and answers status.
type chat struct {
	mu     sync.Mutex
	got    []*http.Request
	bodies []string
	status int
}

func (c *chat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.got, c.bodies = append(c.got, r), append(c.bodies, string(b))
	status := c.status
	c.mu.Unlock()
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
}

func (c *chat) last() (*http.Request, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[len(c.got)-1], c.bodies[len(c.bodies)-1]
}

// identities are the agent's accounts on each vendor, in these tests.
var identities = map[string]string{
	fakevendors.GitHub: "acme-review-agent[bot]", fakevendors.Bitbucket: "712020:bot", fakevendors.Jira: "712020:agent", fakevendors.Linear: "agent-id",
}

// vendors is a Vendors of the four vendors that sends to a chat listener answering status.
func vendors(t *testing.T, status int) (*fakevendors.Vendors, *chat, *httptest.Server) {
	t.Helper()
	c := &chat{status: status}
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	sources := map[string]fakevendors.Source{}
	for v, id := range identities {
		sources[v] = fakevendors.Source{Path: "/chat/webhook/acme/" + v, Secret: []byte(fakeSecret), Identity: id}
	}
	v, err := fakevendors.New(fakevendors.Config{ChatURL: srv.URL, Sources: sources})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(v)
	t.Cleanup(api.Close)
	return v, c, api
}

func hmacHex(key, body string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

// Each vendor signs the raw body with HMAC-SHA256 under its secret, in its own header, behind its own prefix.
func TestSignSignsAsEachVendorDoes(t *testing.T) {
	body := []byte(`{"a":1}`)
	for _, tc := range []struct{ vendor, header, prefix string }{
		{fakevendors.GitHub, "X-Hub-Signature-256", "sha256="},
		{fakevendors.Bitbucket, "X-Hub-Signature", "sha256="},
		{fakevendors.Jira, "X-Hub-Signature", "sha256="},
		{fakevendors.Linear, "Linear-Signature", ""},
	} {
		h, v, err := fakevendors.Sign(tc.vendor, []byte(fakeSecret), body)
		if err != nil || h != tc.header || v != tc.prefix+hmacHex(fakeSecret, string(body)) {
			t.Errorf("Sign(%s) = %s: %s, %v; want %s: %s<hex HMAC-SHA256>", tc.vendor, h, v, err, tc.header, tc.prefix)
		}
	}
	if _, _, err := fakevendors.Sign("gitlab", []byte(fakeSecret), body); err == nil {
		t.Error("Sign of a vendor it does not know passed")
	}
}

func TestNewRefusesASourceItCannotSendFor(t *testing.T) {
	good := fakevendors.Source{Path: "/chat/webhook/acme/github", Secret: []byte(fakeSecret), Identity: "bot"}
	for name, cfg := range map[string]fakevendors.Config{
		"a relative chat URL": {ChatURL: "chat.example.test", Sources: map[string]fakevendors.Source{fakevendors.GitHub: good}},
		"an unknown vendor":   {ChatURL: "http://chat.test", Sources: map[string]fakevendors.Source{"gitlab": good}},
		"no secret": {ChatURL: "http://chat.test", Sources: map[string]fakevendors.Source{
			fakevendors.GitHub: {Path: good.Path, Identity: good.Identity}}},
		"a path off the webhook routes": {ChatURL: "http://chat.test", Sources: map[string]fakevendors.Source{
			fakevendors.GitHub: {Path: "/chat/slack/acme/github", Secret: good.Secret, Identity: good.Identity}}},
		"no identity": {ChatURL: "http://chat.test", Sources: map[string]fakevendors.Source{
			fakevendors.GitHub: {Path: good.Path, Secret: good.Secret}}},
	} {
		if _, err := fakevendors.New(cfg); err == nil {
			t.Errorf("New with %s passed", name)
		}
	}
}

// Send posts the body byte for byte to the source's route, with the delivery's headers, signed as the delivery's Signing says.
func TestSendSignsTheBodyAsItIsAndSaysHowItWasAnswered(t *testing.T) {
	v, c, _ := vendors(t, http.StatusOK)
	d := fakevendors.GitHubPR{Action: "opened", Repo: "acme/widgets", Number: 42, HeadSHA: "abc1234", Sender: "octocat"}.Delivery()
	if sent := v.Send(d); sent.Status != http.StatusOK {
		t.Fatalf("Send = %+v; want 200", sent)
	}
	req, body := c.last()
	if req.URL.Path != "/chat/webhook/acme/github" || body != d.Body || req.Header.Get("X-GitHub-Event") != "pull_request" ||
		req.Header.Get("X-GitHub-Delivery") != d.ID() || req.Header.Get("X-Hub-Signature-256") != "sha256="+hmacHex(fakeSecret, d.Body) {
		t.Errorf("the request was %s %v %q; want the body as it is, the delivery's headers and its signature", req.URL.Path, req.Header, body)
	}

	d.Signing = fakevendors.SignNone
	v.Send(d)
	if req, _ := c.last(); req.Header.Get("X-Hub-Signature-256") != "" {
		t.Errorf("SignNone sent the signature %q", req.Header.Get("X-Hub-Signature-256"))
	}
	d.Signing = fakevendors.SignBad
	v.Send(d)
	if req, _ := c.last(); req.Header.Get("X-Hub-Signature-256") == "sha256="+hmacHex(fakeSecret, d.Body) || req.Header.Get("X-Hub-Signature-256") == "" {
		t.Errorf("SignBad sent %q; want a signature under another secret", req.Header.Get("X-Hub-Signature-256"))
	}
	d.Signing = "sometimes"
	if sent := v.Send(d); sent.Error == "" || sent.Status != 0 {
		t.Errorf("Send of an unknown signing = %+v; want an error and no request", sent)
	}
	if sent := v.Send(fakevendors.Delivery{Vendor: "gitlab", Body: "{}"}); sent.Error == "" {
		t.Errorf("Send of a vendor with no source = %+v; want an error", sent)
	}

	busy, _, _ := vendors(t, http.StatusServiceUnavailable)
	if sent := busy.Send(fakevendors.JiraIssue{Event: "jira:issue_created", Key: "OPS-7", Project: "OPS"}.Delivery()); sent.Status != http.StatusServiceUnavailable || sent.RetryAfter != "1" {
		t.Errorf("Send to a busy listener = %+v; want 503 with its Retry-After", sent)
	}
}

// pointer reads a JSON Pointer of the recipes' simple form (no escapes) from body.
func pointer(t *testing.T, body, ptr string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	for _, k := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

// A call of the agent to a vendor's API is recorded, and the vendor sends the delivery of it back with the agent as the actor, at the
// pointer the vendor's recipe reads first, in the issue's or pull request's channel and thread.
func TestTheAPIRecordsAPostAndSendsItsDeliveryBackAsTheAgent(t *testing.T) {
	v, c, api := vendors(t, http.StatusOK)
	// Linear knows an issue by the delivery a test sent of it, as the issue's comment delivery names its team.
	if sent := v.Send(fakevendors.LinearIssue{Action: "create", IssueID: "uuid-7", Ident: "ENG-7", Team: "ENG"}.Delivery()); sent.Status != http.StatusOK {
		t.Fatal(sent)
	}
	for _, tc := range []struct {
		name, path, call  string
		post              fakevendors.Post
		actor, channel    string // pointers the vendor's recipe reads
		wantChan, threadP string
		wantThread        any
	}{
		{name: "GitHub review", path: "/vendor/github/repos/acme/widgets/pulls/42/reviews", call: `{"body":"LGTM","commit_id":"abc1234"}`,
			post:  fakevendors.Post{Vendor: fakevendors.GitHub, Kind: "review", Target: "acme/widgets#42", Body: "LGTM", Commit: "abc1234"},
			actor: "/sender/login", channel: "/repository/full_name", wantChan: "acme/widgets", threadP: "/pull_request/number", wantThread: float64(42)},
		{name: "GitHub comment", path: "/vendor/github/repos/acme/widgets/issues/42/comments", call: `{"body":"Yes."}`,
			post:  fakevendors.Post{Vendor: fakevendors.GitHub, Kind: "comment", Target: "acme/widgets#42", Body: "Yes."},
			actor: "/comment/user/login", channel: "/repository/full_name", wantChan: "acme/widgets", threadP: "/issue/number", wantThread: float64(42)},
		{name: "Bitbucket comment", path: "/vendor/bitbucket/2.0/repositories/acme/widgets/pullrequests/7/comments", call: `{"content":{"raw":"Yes."}}`,
			post:  fakevendors.Post{Vendor: fakevendors.Bitbucket, Kind: "comment", Target: "acme/widgets#7", Body: "Yes."},
			actor: "/actor/account_id", channel: "/repository/full_name", wantChan: "acme/widgets", threadP: "/pullrequest/id", wantThread: float64(7)},
		{name: "Jira comment", path: "/vendor/jira/rest/api/3/issue/OPS-7/comment", call: `{"body":"A plan."}`,
			post:  fakevendors.Post{Vendor: fakevendors.Jira, Kind: "comment", Target: "OPS-7", Body: "A plan."},
			actor: "/comment/author/accountId", channel: "/issue/fields/project/key", wantChan: "OPS", threadP: "/issue/key", wantThread: "OPS-7"},
		{name: "Linear comment", path: "/vendor/linear/graphql", call: `{"variables":{"input":{"issueId":"ENG-7","body":"A plan."}}}`,
			post:  fakevendors.Post{Vendor: fakevendors.Linear, Kind: "comment", Target: "ENG-7", Body: "A plan."},
			actor: "/actor/id", channel: "/data/issue/team/key", wantChan: "ENG", threadP: "/data/issue/identifier", wantThread: "ENG-7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(v.Posts())
			resp, err := http.Post(api.URL+tc.path, "application/json", strings.NewReader(tc.call)) //nolint:noctx // a test
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("the API answered %d", resp.StatusCode)
			}
			posts := v.Posts()
			want := tc.post
			want.SelfStatus = http.StatusOK
			if len(posts) != before+1 || posts[before] != want {
				t.Fatalf("the posts are %+v; want %+v last", posts, want)
			}
			req, body := c.last()
			if req.URL.Path != "/chat/webhook/acme/"+tc.post.Vendor {
				t.Errorf("the delivery went to %s", req.URL.Path)
			}
			if got := pointer(t, body, tc.actor); got != identities[tc.post.Vendor] {
				t.Errorf("%s of the delivery back = %v; want the agent, %s", tc.actor, got, identities[tc.post.Vendor])
			}
			if got := pointer(t, body, tc.channel); got != tc.wantChan {
				t.Errorf("%s of the delivery back = %v; want %s", tc.channel, got, tc.wantChan)
			}
			if got := pointer(t, body, tc.threadP); got != tc.wantThread {
				t.Errorf("%s of the delivery back = %v; want %v", tc.threadP, got, tc.wantThread)
			}
		})
	}

	resp, err := http.Post(api.URL+"/vendor/linear/graphql", "application/json", //nolint:noctx // a test
		strings.NewReader(`{"variables":{"input":{"issueId":"ENG-404","body":"?"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a comment on a Linear issue it was never sent = %d; want 404", resp.StatusCode)
	}
}

// The control routes send a Delivery given as JSON and list the posts, for a test that runs in another process.
func TestTheControlRoutesSendADeliveryAndListThePosts(t *testing.T) {
	v, c, api := vendors(t, http.StatusAccepted)
	d := fakevendors.JiraIssue{Event: "jira:issue_created", Key: "OPS-7", Project: "OPS", Summary: "Rename it"}.Delivery()
	b, _ := json.Marshal(d)
	resp, err := http.Post(api.URL+fakevendors.ControlSendPath, "application/json", bytes.NewReader(b)) //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	var sent fakevendors.Sent
	if err := json.NewDecoder(resp.Body).Decode(&sent); err != nil || sent.Status != http.StatusAccepted {
		t.Errorf("the send route answered %+v, %v; want the listener's 202", sent, err)
	}
	_ = resp.Body.Close()
	if _, body := c.last(); body != d.Body {
		t.Errorf("the listener got %q; want the delivery's body", body)
	}

	resp, err = http.Post(api.URL+fakevendors.ControlSendPath, "application/json", strings.NewReader(`{"vendor":"jira","secret":"x"}`)) //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a body that is not a Delivery = %d; want 400", resp.StatusCode)
	}

	resp, err = http.Get(api.URL + fakevendors.ControlPostsPath) //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	var posts []fakevendors.Post
	if err := json.NewDecoder(resp.Body).Decode(&posts); err != nil || len(posts) != len(v.Posts()) {
		t.Errorf("the posts route answered %+v, %v; want the posts", posts, err)
	}
	_ = resp.Body.Close()
	if !fakevendors.Handles(fakevendors.ControlPostsPath) || !fakevendors.Handles("/vendor/jira/x") || fakevendors.Handles("/chat/webhook/a/b") {
		t.Error("Handles does not say which paths are the fake's")
	}
}

// A delivery's ID is the one its vendor's header carries.
func TestDeliveryIDIsTheVendorsHeader(t *testing.T) {
	for _, d := range []fakevendors.Delivery{
		fakevendors.GitHubComment{Repo: "a/b", Number: 1}.Delivery(),
		fakevendors.BitbucketComment{Repo: "a/b", ID: 1}.Delivery(),
		fakevendors.JiraComment{Key: "OPS-1", Project: "OPS"}.Delivery(),
		fakevendors.LinearComment{Ident: "ENG-1"}.Delivery(),
	} {
		if id := d.ID(); id == "" {
			t.Errorf("the delivery of %s has no ID: %v", d.Vendor, d.Header)
		}
	}
	if id := (fakevendors.Delivery{Vendor: fakevendors.GitHub}).ID(); id != "" {
		t.Errorf("a delivery with no header has the ID %q", id)
	}
}

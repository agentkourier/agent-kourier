package stubagent_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// vendorCall is one call the webhook agent made to the vendor API.
type vendorCall struct {
	path string
	body obj
}

// vendorAPI is a vendor API that records each call and answers status.
type vendorAPI struct {
	mu     sync.Mutex
	calls  []vendorCall
	status int
}

func (v *vendorAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var b obj
	_ = json.Unmarshal(raw, &b)
	v.mu.Lock()
	v.calls = append(v.calls, vendorCall{r.URL.Path, b})
	status := v.status
	v.mu.Unlock()
	w.WriteHeader(status)
}

func (v *vendorAPI) recorded() []vendorCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]vendorCall(nil), v.calls...)
}

// webhookAgent starts a stub whose agents/webhook is the webhook recipes' agent, on a vendor API that answers status.
func webhookAgent(t *testing.T, status int) (*client, *vendorAPI, *[]string) {
	t.Helper()
	s := stubagent.New()
	t.Cleanup(s.Close)
	api := &vendorAPI{status: status}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	var seen []string
	stubagent.WebhookAgent(s, "agents", "webhook", srv.URL, func(_ int, prompt string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, prompt)
	})
	return &client{t: t, url: s.AgentURL("agents", "webhook"), hdr: http.Header{}}, api, &seen
}

// Each recipe's prompt makes the agent call its vendor's API where a real agent's tools would: the review or the comment, on the pull
// request or the issue the prompt names, with what it read in the body. Its reply says what it posted.
func TestTheWebhookAgentPostsWhereEachRecipesPromptSays(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, path string
		body               []string // substrings of the call's JSON
	}{
		{name: "GitHub review",
			prompt: "GitHub pull request #42 in repository acme/widgets needs a review.\nHead commit 9f8e7d6c5b4a39281706f5e4d3c2b1a098765432, base commit 0123456.",
			path:   "/vendor/github/repos/acme/widgets/pulls/42/reviews",
			body:   []string{`"commit_id":"9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"`, "Review of acme/widgets#42 at 9f8e7d6c5b4a39281706f5e4d3c2b1a098765432 (turn 1)"}},
		{name: "GitHub comment", prompt: "A comment on GitHub pull request #42 in repository acme/widgets mentions you; the comment is in the event data.",
			path: "/vendor/github/repos/acme/widgets/issues/42/comments", body: []string{"Reply on acme/widgets#42 (turn 1)"}},
		{name: "Bitbucket review", prompt: "Bitbucket pull request #7 of acme/widgets needs a review at commit a1b2c3d4e5f6.",
			path: "/vendor/bitbucket/2.0/repositories/acme/widgets/pullrequests/7/comments", body: []string{`"raw":"Review of acme/widgets#7 at a1b2c3d4e5f6 (turn 1)`}},
		{name: "Bitbucket comment", prompt: "A comment on Bitbucket pull request #7 of acme/widgets mentions you; the comment is in the event data.",
			path: "/vendor/bitbucket/2.0/repositories/acme/widgets/pullrequests/7/comments", body: []string{"Reply on acme/widgets#7 (turn 1)"}},
		{name: "Jira", prompt: "Jira issue OPS-7 was created in project OPS.\n<summary>Rename codename FALCON to OSPREY</summary>",
			path: "/vendor/jira/rest/api/3/issue/OPS-7/comment", body: []string{"Plan for OPS-7 (turn 1): rename codename FALCON to OSPREY"}},
		{name: "Linear", prompt: "The description of Linear issue ENG-7 of team ENG changed.",
			path: "/vendor/linear/graphql", body: []string{`"issueId":"ENG-7"`, "Triage of ENG-7 (turn 1): no codename change asked for."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, api, seen := webhookAgent(t, http.StatusOK)
			evs := c.send(msg("m1", tc.prompt, ""))
			calls := api.recorded()
			if len(calls) != 1 || calls[0].path != tc.path {
				t.Fatalf("the agent called %+v; want one call of %s", calls, tc.path)
			}
			raw, _ := json.Marshal(calls[0].body)
			for _, want := range tc.body {
				if !strings.Contains(string(raw), want) {
					t.Errorf("the call %s lacks %q", raw, want)
				}
			}
			if got := taskText(last(evs)); !strings.HasPrefix(got, "Posted: ") {
				t.Errorf("the reply is %q; want what it posted", got)
			}
			if len(*seen) != 1 || (*seen)[0] != tc.prompt {
				t.Errorf("seen was given %q; want the prompt", *seen)
			}
		})
	}
}

// The agent's post names its turn in the A2A context: the second prompt of a context is turn 2.
func TestTheWebhookAgentCountsTheTurnsOfItsContext(t *testing.T) {
	c, api, _ := webhookAgent(t, http.StatusOK)
	_, ctxID, _ := ids(last(c.send(msg("m1", "Jira issue OPS-7 was created in project OPS.", ""))))
	c.send(msg("m2", "Jira issue OPS-7 of project OPS was updated (its summary changed).", ctxID))
	calls := api.recorded()
	if len(calls) != 2 || !strings.Contains(calls[1].body["body"].(string), "(turn 2)") {
		t.Errorf("the calls are %+v; want the second to name turn 2", calls)
	}
}

// A prompt of no recipe calls nothing; a call the vendor API refuses fails the turn.
func TestTheWebhookAgentFailsAPostTheAPIRefusesAndCallsNothingForAnotherPrompt(t *testing.T) {
	c, api, _ := webhookAgent(t, http.StatusOK)
	if got := taskText(last(c.send(msg("m1", "Summarise the incident.", "")))); got != "No webhook recipe's prompt." || len(api.recorded()) != 0 {
		t.Errorf("a prompt of no recipe: the reply %q, the calls %+v; want no call", got, api.recorded())
	}
	c, _, _ = webhookAgent(t, http.StatusForbidden)
	ev := last(c.send(msg("m1", "Jira issue OPS-7 was created in project OPS.", "")))
	if _, _, state := ids(ev); state != "TASK_STATE_FAILED" || !strings.Contains(statusText(ev), "the vendor API answered 403") {
		t.Errorf("a refused post: the task is %s, %q; want it failed, saying the API answered 403", state, statusText(ev))
	}
}

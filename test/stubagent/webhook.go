package stubagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// The prompts of the webhook recipes (examples/recipes/webhook-*), as their templates write them, and the codename change a Jira or
// Linear issue's untrusted block may ask for.
var (
	whGitHubReview     = regexp.MustCompile(`GitHub pull request #([0-9]+) in repository ([A-Za-z0-9._-]+/[A-Za-z0-9._-]+) needs a review\.`)
	whGitHubHead       = regexp.MustCompile(`Head commit ([0-9a-f]{7,64})`)
	whGitHubComment    = regexp.MustCompile(`A comment on GitHub pull request #([0-9]+) in repository ([A-Za-z0-9._-]+/[A-Za-z0-9._-]+) mentions you`)
	whBitbucketReview  = regexp.MustCompile(`Bitbucket pull request #([0-9]+) of ([A-Za-z0-9._-]+/[A-Za-z0-9._-]+) needs a review at commit ([0-9a-f]+)\.`)
	whBitbucketComment = regexp.MustCompile(`A comment on Bitbucket pull request #([0-9]+) of ([A-Za-z0-9._-]+/[A-Za-z0-9._-]+) mentions you`)
	whJira             = regexp.MustCompile(`Jira issue ([A-Z][A-Z0-9]*-[0-9]+) `)
	whLinear           = regexp.MustCompile(`Linear issue ([A-Z][A-Z0-9]*-[0-9]+) `)
	whCodename         = regexp.MustCompile(`(?i)codename ([A-Za-z]+) to ([A-Za-z]+)`)
)

// webhookPostTimeout bounds one call of the webhook agent to the vendor API.
const webhookPostTimeout = 10 * time.Second

// WebhookAgent scripts the agent ns/name of s as the webhook recipes' agent. It reads what each recipe's prompt template writes (the
// repository, the pull request or the issue, the commit) and, from the prompt's untrusted block, a codename change, and calls the vendor
// API at api (scheme and host: test/fakevendors) as a real agent would with the vendor's own tools. What it posts names what it read
// and the turn of its session (1 for the first prompt of an A2A context), so a test reads the prompt's fields and the session's
// continuity from the post. seen, if not nil, is called with each prompt and its turn before the agent acts on it.
func WebhookAgent(s *Server, ns, name, api string, seen func(turn int, prompt string)) {
	client := &http.Client{Timeout: webhookPostTimeout}
	post := func(path string, body any) error {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode the call: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), webhookPostTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+path, bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("build the call: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("call the vendor API: %w", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the vendor API answered %d", resp.StatusCode)
		}
		return nil
	}
	s.Agent(ns, name).ReplyWithHistory(func(prior []string, prompt string) []Step {
		turn := len(prior) + 1
		if seen != nil {
			seen(turn, prompt)
		}
		plan := func(subject string) string {
			if m := whCodename.FindStringSubmatch(prompt); m != nil {
				return fmt.Sprintf("Plan for %s (turn %d): rename codename %s to %s in the code, the configs and the docs.", subject, turn, m[1], m[2])
			}
			return fmt.Sprintf("Triage of %s (turn %d): no codename change asked for.", subject, turn)
		}
		var err error
		var said string
		switch {
		case whGitHubReview.MatchString(prompt):
			m, head := whGitHubReview.FindStringSubmatch(prompt), whGitHubHead.FindStringSubmatch(prompt)
			if head == nil {
				return []Step{Text("The prompt names no head commit.")}
			}
			said = fmt.Sprintf("Review of %s#%s at %s (turn %d): looks right.", m[2], m[1], head[1], turn)
			err = post(fmt.Sprintf("/vendor/github/repos/%s/pulls/%s/reviews", m[2], m[1]),
				map[string]string{"body": said, "commit_id": head[1], "event": "COMMENT"})
		case whGitHubComment.MatchString(prompt):
			m := whGitHubComment.FindStringSubmatch(prompt)
			said = fmt.Sprintf("Reply on %s#%s (turn %d): yes, the retry is bounded.", m[2], m[1], turn)
			err = post(fmt.Sprintf("/vendor/github/repos/%s/issues/%s/comments", m[2], m[1]), map[string]string{"body": said})
		case whBitbucketReview.MatchString(prompt):
			m := whBitbucketReview.FindStringSubmatch(prompt)
			said = fmt.Sprintf("Review of %s#%s at %s (turn %d): looks right.", m[2], m[1], m[3], turn)
			err = post(fmt.Sprintf("/vendor/bitbucket/2.0/repositories/%s/pullrequests/%s/comments", m[2], m[1]),
				map[string]any{"content": map[string]string{"raw": said}})
		case whBitbucketComment.MatchString(prompt):
			m := whBitbucketComment.FindStringSubmatch(prompt)
			said = fmt.Sprintf("Reply on %s#%s (turn %d): yes, the retry is bounded.", m[2], m[1], turn)
			err = post(fmt.Sprintf("/vendor/bitbucket/2.0/repositories/%s/pullrequests/%s/comments", m[2], m[1]),
				map[string]any{"content": map[string]string{"raw": said}})
		case whJira.MatchString(prompt):
			key := whJira.FindStringSubmatch(prompt)[1]
			said = plan(key)
			err = post("/vendor/jira/rest/api/3/issue/"+key+"/comment", map[string]string{"body": said})
		case whLinear.MatchString(prompt):
			key := whLinear.FindStringSubmatch(prompt)[1]
			said = plan(key)
			err = post("/vendor/linear/graphql", map[string]any{
				"query":     "mutation($input: CommentCreateInput!) { commentCreate(input: $input) { success } }",
				"variables": map[string]any{"input": map[string]string{"issueId": key, "body": said}}})
		default:
			return []Step{Text("No webhook recipe's prompt.")}
		}
		if err != nil {
			return []Step{Fail("posting failed: " + err.Error())}
		}
		return []Step{Text("Posted: " + said)}
	})
}

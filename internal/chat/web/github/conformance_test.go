package github_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/chat/web/github"
)

// The web platform with the github profile passes the profile suite: every documented event that the profile reads, through the
// platform's handler, with GitHub's signature and no replay window (a delivery is taken once by its ID).
func TestTheGitHubProfilePassesTheProfileSuite(t *testing.T) {
	t.Parallel()
	pr, ic, rc := "pull_request.opened.json", "issue_comment.created.json", "pull_request_review_comment.created.json"
	action := func(a string) func(map[string]any) { return func(m map[string]any) { m["action"] = a } }
	draft := func(a string) func(map[string]any) {
		return func(m map[string]any) { m["action"] = a; obj(m, "pull_request")["draft"] = true }
	}
	event := func(text, delivery, user string) *web.Event {
		return &web.Event{Channel: "octo-org/hello-world", Thread: "pr-42", ID: delivery, User: user, Text: text, Mentioned: true}
	}
	opened := func(a string) string { return strings.Replace(openedText, "action opened", "action "+a, 1) }
	ev := func(name, kind, delivery string, body []byte, want *web.Event) chattest.ProfileEvent {
		return chattest.ProfileEvent{Name: name, Header: headers(kind, delivery), Body: body, Want: want}
	}
	const (
		d1, d2, d3, d4, d5, d6 = "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-000000000003",
			"00000000-0000-0000-0000-000000000004", "00000000-0000-0000-0000-000000000005", "00000000-0000-0000-0000-000000000006"
		other = "00000000-0000-0000-0000-0000000000ff"
	)
	chattest.ProfileConformance(t, chattest.ProfileSubject{
		Profile: newProfile(t),
		Secret:  fakeSecret,
		Sign: func(secret string, body []byte, _ time.Time) http.Header {
			h := http.Header{}
			h.Set(github.HeaderSignature, github.Sign([]byte(secret), body))
			return h
		},
		MaxBody: github.MaxBody,
		Events: []chattest.ProfileEvent{
			ev("a pull request opened", "pull_request", d1, fixture(t, pr), event(openedText, d1, "octocat")),
			ev("a push to it", "pull_request", d2, patched(t, pr, action("synchronize")), event(opened("synchronize"), d2, "octocat")),
			ev("it reopened", "pull_request", d3, patched(t, pr, action("reopened")), event(opened("reopened"), d3, "octocat")),
			ev("a draft made ready for review", "pull_request", d4, patched(t, pr, draft("ready_for_review")), event(opened("ready_for_review"), d4, "octocat")),
			ev("a comment that mentions the login", "issue_comment", d5, fixture(t, ic), event(issueCommentText, d5, "hubot")),
			ev("a review comment that mentions the login", "pull_request_review_comment", d6, fixture(t, rc), event(reviewCommentText, d6, "hubot")),
			ev("a draft opened", "pull_request", other, patched(t, pr, draft("opened")), nil),
			ev("a pull request closed", "pull_request", other, patched(t, pr, action("closed")), nil),
			ev("a comment that does not mention the login", "issue_comment", other,
				patched(t, ic, func(m map[string]any) { obj(m, "comment")["body"] = "looks good" }), nil),
			ev("a comment by the login", "issue_comment", other,
				patched(t, ic, func(m map[string]any) {
					obj(obj(m, "comment"), "user")["login"] = login
					obj(m, "sender")["login"] = login
				}), nil),
			ev("a comment on an issue", "issue_comment", other, patched(t, ic, func(m map[string]any) { delete(obj(m, "issue"), "pull_request") }), nil),
			ev("a comment edited", "issue_comment", other, patched(t, ic, action("edited")), nil),
			ev("ping", "ping", other, fixture(t, "ping.json"), nil),
			ev("push", "push", other, []byte(`{"ref":"refs/heads/main"}`), nil),
		},
		Malformed: []chattest.ProfileCase{
			{Name: "no event header", Header: headers("", d1), Body: fixture(t, pr)},
			{Name: "no delivery ID", Header: headers("pull_request", ""), Body: fixture(t, pr)},
			{Name: "a delivery ID with ':'", Header: headers("pull_request", "00000000:0001"), Body: fixture(t, pr)},
			{Name: "no repository", Header: headers("pull_request", d1), Body: patched(t, pr, func(m map[string]any) { delete(m, "repository") })},
			{Name: "no pull request", Header: headers("pull_request", d1), Body: patched(t, pr, func(m map[string]any) { delete(m, "pull_request") })},
			{Name: "no comment", Header: headers("issue_comment", d5), Body: patched(t, ic, func(m map[string]any) { delete(m, "comment") })},
		},
	})
}

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
// platform's handler, with GitHub's signature and no replay window (an event is taken once by its ID, the hash of its body).
func TestTheGitHubProfilePassesTheProfileSuite(t *testing.T) {
	t.Parallel()
	pr, ic, rc := "pull_request.opened.json", "issue_comment.created.json", "pull_request_review_comment.created.json"
	action := func(a string) func(map[string]any) { return func(m map[string]any) { m["action"] = a } }
	draft := func(a string) func(map[string]any) {
		return func(m map[string]any) { m["action"] = a; obj(m, "pull_request")["draft"] = true }
	}
	byLogin := func(a string) func(map[string]any) {
		return func(m map[string]any) { m["action"] = a; obj(m, "sender")["login"] = login }
	}
	opened := func(a string) string { return strings.Replace(openedText, "action opened", "action "+a, 1) }
	take := func(name, kind string, body []byte, text, user string) chattest.ProfileEvent {
		want := &web.Event{Channel: "octo-org/hello-world", Thread: "pr-42", ID: idOf(body), User: user, Text: text, Mentioned: true}
		return chattest.ProfileEvent{Name: name, Header: headers(kind), Body: body, Want: want}
	}
	skip := func(name, kind string, body []byte) chattest.ProfileEvent {
		return chattest.ProfileEvent{Name: name, Header: headers(kind), Body: body}
	}
	chattest.ProfileConformance(t, chattest.ProfileSubject{
		Profile:  newProfile(t),
		Platform: platform(t),
		Secret:   fakeSecret,
		Sign: func(secret string, body []byte, _ time.Time) http.Header {
			h := http.Header{}
			h.Set(github.HeaderSignature, github.Sign([]byte(secret), body))
			return h
		},
		MaxBody: github.MaxBody,
		Events: []chattest.ProfileEvent{
			take("a pull request opened", "pull_request", fixture(t, pr), openedText, "octocat"),
			take("a push to it", "pull_request", patched(t, pr, action("synchronize")), opened("synchronize"), "octocat"),
			take("it reopened", "pull_request", patched(t, pr, action("reopened")), opened("reopened"), "octocat"),
			take("a draft made ready for review", "pull_request", patched(t, pr, draft("ready_for_review")), opened("ready_for_review"), "octocat"),
			take("a comment that mentions the login", "issue_comment", fixture(t, ic), issueCommentText, "hubot"),
			take("a review comment that mentions the login", "pull_request_review_comment", fixture(t, rc), reviewCommentText, "hubot"),
			skip("a draft opened", "pull_request", patched(t, pr, draft("opened"))),
			skip("a pull request closed", "pull_request", patched(t, pr, action("closed"))),
			skip("a pull request opened by the login", "pull_request", patched(t, pr, byLogin("opened"))),
			skip("a push by the login", "pull_request", patched(t, pr, byLogin("synchronize"))),
			skip("a pull request reopened by the login", "pull_request", patched(t, pr, byLogin("reopened"))),
			skip("a comment that does not mention the login", "issue_comment",
				patched(t, ic, func(m map[string]any) { obj(m, "comment")["body"] = "looks good" })),
			skip("a comment by the login", "issue_comment",
				patched(t, ic, func(m map[string]any) {
					obj(obj(m, "comment"), "user")["login"] = login
					obj(m, "sender")["login"] = login
				})),
			skip("a comment on an issue", "issue_comment", patched(t, ic, func(m map[string]any) { delete(obj(m, "issue"), "pull_request") })),
			skip("a comment edited", "issue_comment", patched(t, ic, action("edited"))),
			skip("ping", "ping", fixture(t, "ping.json")),
			skip("push", "push", []byte(`{"ref":"refs/heads/main"}`)),
		},
		Malformed: []chattest.ProfileCase{
			{Name: "no event header", Header: headers(""), Body: fixture(t, pr)},
			{Name: "no repository", Header: headers("pull_request"), Body: patched(t, pr, func(m map[string]any) { delete(m, "repository") })},
			{Name: "no pull request", Header: headers("pull_request"), Body: patched(t, pr, func(m map[string]any) { delete(m, "pull_request") })},
			{Name: "no comment", Header: headers("issue_comment"), Body: patched(t, ic, func(m map[string]any) { delete(m, "comment") })},
		},
	})
}

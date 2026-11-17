package fakeweb

import (
	"encoding/json"
	"fmt"
)

// The bodies of the GitHub events a test sends, with the fields of GitHub's webhook documentation for the pull_request, issue_comment
// and ping events (read 2026-11-16, not checked against a live delivery) that a reviewer of a pull request would read, and some that
// it would not, as a real payload has many. The values are made up.

// PullRequest is a pull_request event.
type PullRequest struct {
	// Action is what happened: opened, reopened, synchronize, ready_for_review, closed.
	Action string
	// Repo is the repository, owner/name; Number, Title and Draft are the pull request's; Sender is the login of who did it.
	Repo   string
	Number int
	Title  string
	Draft  bool
	Sender string
	// HeadSHA is the head commit; a made-up one of the number if empty. A push (synchronize) has a new one.
	HeadSHA string
}

// Body is the event's JSON.
func (p PullRequest) Body() string {
	sha := p.HeadSHA
	if sha == "" {
		sha = fmt.Sprintf("%040x", p.Number)
	}
	return marshal(map[string]any{
		"action": p.Action,
		"number": p.Number,
		"pull_request": map[string]any{
			"number": p.Number, "title": p.Title, "draft": p.Draft, "state": "open",
			"html_url": "https://github.com/" + p.Repo + "/pull/" + fmt.Sprint(p.Number),
			"user":     account(p.Sender),
			"head":     map[string]any{"ref": "feature-" + fmt.Sprint(p.Number), "sha": sha},
			"base":     map[string]any{"ref": "main", "sha": fmt.Sprintf("%040x", 0)},
		},
		"repository": repository(p.Repo),
		"sender":     account(p.Sender),
	})
}

// IssueComment is an issue_comment event of a new comment on a pull request.
type IssueComment struct {
	// Repo and Number are the pull request's, and Title its title; Sender is who wrote the comment, and Text what they wrote.
	Repo   string
	Number int
	Title  string
	Sender string
	Text   string
	// CommentID is the comment's own: GitHub's IDs make two comments' bodies differ.
	CommentID int
}

// Body is the event's JSON.
func (c IssueComment) Body() string {
	url := "https://github.com/" + c.Repo + "/pull/" + fmt.Sprint(c.Number)
	return marshal(map[string]any{
		"action": "created",
		"issue": map[string]any{
			"number": c.Number, "title": c.Title, "state": "open",
			"pull_request": map[string]any{"html_url": url},
		},
		"comment": map[string]any{
			"id": c.CommentID, "body": c.Text, "user": account(c.Sender),
			"html_url": url + "#issuecomment-" + fmt.Sprint(c.CommentID),
		},
		"repository": repository(c.Repo),
		"sender":     account(c.Sender),
	})
}

// Ping is the body of the ping event GitHub sends when a webhook is made.
func Ping(repo string) string {
	return marshal(map[string]any{"zen": "Keep it logically awesome.", "hook_id": 424242, "repository": repository(repo), "sender": account("octocat")})
}

func account(login string) map[string]any { return map[string]any{"login": login, "type": "User"} }

func repository(fullName string) map[string]any {
	return map[string]any{"full_name": fullName, "private": true}
}

// marshal is v as JSON. The maps above hold strings, numbers, bools and maps of them, which always marshal.
func marshal(v any) string {
	b, _ := json.Marshal(v) //nolint:errchkjson // see above
	return string(b)
}

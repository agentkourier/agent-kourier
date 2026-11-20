package fakevendors

import (
	"encoding/json"
	"strconv"
)

// The deliveries of four vendors, as the webhook recipes (examples/recipes/webhook-*) read them: only the paths a recipe reads, and a
// few beside them, with made-up values. The paths the recipes read were seen in the vendors' live deliveries in the run of
// 2026-11-17 (docs/how-to/webhooks), except the delivery headers and Bitbucket's /pullrequest/draft, which follow the vendors'
// documentation, read 2026-11-18.

// The vendors.
const (
	GitHub    = "github"
	Bitbucket = "bitbucket"
	Jira      = "jira"
	Linear    = "linear"
)

// The headers the vendors send beside the body: the event and the ID of a delivery, as the recipes read them.
const (
	HeaderGitHubEvent      = "X-GitHub-Event"
	HeaderGitHubDelivery   = "X-GitHub-Delivery"
	HeaderBitbucketEvent   = "X-Event-Key"
	HeaderBitbucketRequest = "X-Request-UUID"
	HeaderJiraIdentifier   = "X-Atlassian-Webhook-Identifier"
	HeaderLinearEvent      = "Linear-Event"
	HeaderLinearDelivery   = "Linear-Delivery"
)

// Delivery is one vendor's webhook: its headers, but for the signature, which Send adds, and its body, sent byte for byte. The same
// Delivery sent twice is the same request twice, as a vendor's redelivery is.
type Delivery struct {
	Vendor string            `json:"vendor"`
	Header map[string]string `json:"header,omitempty"`
	Body   string            `json:"body"`
	// Signing is how Send signs it: SignValid (the default), SignNone or SignBad.
	Signing string `json:"signing,omitempty"`
}

// ID is the delivery's ID, from the header the vendor carries it in, or "" for a vendor that has none here.
func (d Delivery) ID() string {
	for _, h := range []string{HeaderGitHubDelivery, HeaderBitbucketRequest, HeaderJiraIdentifier, HeaderLinearDelivery} {
		if v, ok := d.Header[h]; ok {
			return v
		}
	}
	return ""
}

// GitHubPR is a pull_request event: the base repository, its clone URL, and the head and base commits.
type GitHubPR struct {
	Action, Repo                  string
	Number                        int
	Title, Body, HeadSHA, BaseSHA string
	HeadRef, Sender               string
	Draft                         bool
}

// Delivery is the event's delivery.
func (p GitHubPR) Delivery() Delivery {
	return Delivery{Vendor: GitHub, Header: map[string]string{HeaderGitHubEvent: "pull_request", HeaderGitHubDelivery: newGUID()},
		Body: marshal(map[string]any{
			"action": p.Action,
			"number": p.Number,
			"pull_request": map[string]any{
				"number": p.Number, "title": p.Title, "body": p.Body, "draft": p.Draft, "state": "open",
				"html_url": "https://github.com/" + p.Repo + "/pull/" + strconv.Itoa(p.Number),
				"user":     githubAccount(p.Sender),
				"head":     map[string]any{"ref": p.HeadRef, "sha": p.HeadSHA, "repo": githubRepository(p.Repo)},
				"base":     map[string]any{"ref": "main", "sha": p.BaseSHA, "repo": githubRepository(p.Repo)},
			},
			"repository": githubRepository(p.Repo),
			"sender":     githubAccount(p.Sender),
		})}
}

// GitHubComment is an issue_comment event of a new comment on a pull request.
type GitHubComment struct {
	Repo      string
	Number    int
	CommentID int
	Text      string
	Author    string
}

// Delivery is the event's delivery.
func (c GitHubComment) Delivery() Delivery {
	return Delivery{Vendor: GitHub, Header: map[string]string{HeaderGitHubEvent: "issue_comment", HeaderGitHubDelivery: newGUID()},
		Body: marshal(map[string]any{
			"action": "created",
			"issue": map[string]any{"number": c.Number, "title": "PR " + strconv.Itoa(c.Number), "state": "open",
				"pull_request": map[string]any{"html_url": "https://github.com/" + c.Repo + "/pull/" + strconv.Itoa(c.Number)}},
			"comment":    map[string]any{"id": c.CommentID, "body": c.Text, "user": githubAccount(c.Author)},
			"repository": githubRepository(c.Repo),
			"sender":     githubAccount(c.Author),
		})}
}

// GitHubReview is a pull_request_review event of a submitted review.
type GitHubReview struct {
	Repo      string
	Number    int
	ReviewID  int
	Text, SHA string
	Author    string
}

// Delivery is the event's delivery.
func (r GitHubReview) Delivery() Delivery {
	return Delivery{Vendor: GitHub, Header: map[string]string{HeaderGitHubEvent: "pull_request_review", HeaderGitHubDelivery: newGUID()},
		Body: marshal(map[string]any{
			"action":       "submitted",
			"review":       map[string]any{"id": r.ReviewID, "body": r.Text, "commit_id": r.SHA, "state": "commented", "user": githubAccount(r.Author)},
			"pull_request": map[string]any{"number": r.Number, "draft": false, "head": map[string]any{"sha": r.SHA}},
			"repository":   githubRepository(r.Repo),
			"sender":       githubAccount(r.Author),
		})}
}

func githubAccount(login string) map[string]any {
	return map[string]any{"login": login, "type": "User"}
}

func githubRepository(full string) map[string]any {
	return map[string]any{"full_name": full, "clone_url": "https://github.com/" + full + ".git", "private": true}
}

// BitbucketPR is a pullrequest:created event.
type BitbucketPR struct {
	Repo   string
	ID     int
	Title  string
	Commit string
	Actor  string // an account_id
}

// Delivery is the event's delivery.
func (p BitbucketPR) Delivery() Delivery {
	return Delivery{Vendor: Bitbucket, Header: map[string]string{HeaderBitbucketEvent: "pullrequest:created", HeaderBitbucketRequest: newGUID()},
		Body: marshal(map[string]any{
			"actor":       map[string]any{"account_id": p.Actor, "display_name": "Sam"},
			"pullrequest": bitbucketPR(p.Repo, p.ID, p.Title, p.Commit),
			"repository":  map[string]any{"full_name": p.Repo},
		})}
}

// BitbucketComment is a pullrequest:comment_created event.
type BitbucketComment struct {
	Repo      string
	ID        int
	CommentID int
	Text      string
	Actor     string
}

// Delivery is the event's delivery.
func (c BitbucketComment) Delivery() Delivery {
	return Delivery{Vendor: Bitbucket, Header: map[string]string{HeaderBitbucketEvent: "pullrequest:comment_created", HeaderBitbucketRequest: newGUID()},
		Body: marshal(map[string]any{
			"actor":       map[string]any{"account_id": c.Actor},
			"comment":     map[string]any{"id": c.CommentID, "content": map[string]any{"raw": c.Text}, "user": map[string]any{"account_id": c.Actor}},
			"pullrequest": bitbucketPR(c.Repo, c.ID, "PR "+strconv.Itoa(c.ID), ""),
			"repository":  map[string]any{"full_name": c.Repo},
		})}
}

func bitbucketPR(repo string, id int, title, commit string) map[string]any {
	return map[string]any{"id": id, "title": title, "description": "See the incident.", "draft": false,
		"links":       map[string]any{"html": map[string]any{"href": "https://bitbucket.org/" + repo + "/pull-requests/" + strconv.Itoa(id)}},
		"source":      map[string]any{"branch": map[string]any{"name": "retry"}, "commit": map[string]any{"hash": commit}},
		"destination": map[string]any{"branch": map[string]any{"name": "main"}}}
}

// JiraIssue is a jira:issue_created or jira:issue_updated event of an admin webhook.
type JiraIssue struct {
	Event                     string // jira:issue_created or jira:issue_updated
	Key, Project              string
	Summary, Description      string
	Actor                     string // an accountId
	Timestamp                 int64
	ChangedField, FromSummary string // an update's changelog, if set
}

// Delivery is the event's delivery.
func (j JiraIssue) Delivery() Delivery {
	body := map[string]any{
		"timestamp": j.Timestamp, "webhookEvent": j.Event,
		"user":  map[string]any{"accountId": j.Actor},
		"issue": jiraIssue(j.Key, j.Project, j.Summary, j.Description),
	}
	if j.ChangedField != "" {
		body["changelog"] = map[string]any{"items": []any{map[string]any{"field": j.ChangedField, "fromString": j.FromSummary, "toString": j.Summary}}}
	}
	return Delivery{Vendor: Jira, Header: map[string]string{HeaderJiraIdentifier: newID()}, Body: marshal(body)}
}

// JiraComment is a comment_created event.
type JiraComment struct {
	Key, Project string
	CommentID    string
	Text         string
	Author       string
	Timestamp    int64
}

// Delivery is the event's delivery.
func (c JiraComment) Delivery() Delivery {
	return Delivery{Vendor: Jira, Header: map[string]string{HeaderJiraIdentifier: newID()}, Body: marshal(map[string]any{
		"timestamp": c.Timestamp, "webhookEvent": "comment_created",
		"comment": map[string]any{"id": c.CommentID, "body": c.Text, "author": map[string]any{"accountId": c.Author}},
		"issue":   jiraIssue(c.Key, c.Project, "", ""),
	})}
}

func jiraIssue(key, project, summary, description string) map[string]any {
	return map[string]any{"id": "10" + key[len(key)-1:], "key": key, "fields": map[string]any{
		"summary": summary, "description": description, "project": map[string]any{"key": project}, "issuetype": map[string]any{"name": "Task"}}}
}

// LinearIssue is an Issue create or update event.
type LinearIssue struct {
	Action             string // create or update
	IssueID, Ident     string // the issue's UUID and its identifier, ENG-321
	Team               string
	Title, Description string
	Actor              string // the actor's ID
	Timestamp          int64  // webhookTimestamp, in ms
	// FromTitle and FromDescription are an update's updatedFrom.title and updatedFrom.description: each is in the body when it is set.
	FromTitle, FromDescription string
}

// Delivery is the event's delivery.
func (l LinearIssue) Delivery() Delivery {
	body := map[string]any{
		"action": l.Action, "type": "Issue",
		"actor": map[string]any{"id": l.Actor, "name": "Sam", "type": "user"},
		"data": map[string]any{"id": l.IssueID, "identifier": l.Ident, "title": l.Title, "description": l.Description,
			"team": map[string]any{"key": l.Team}},
		"url":              "https://linear.app/acme/issue/" + l.Ident,
		"webhookTimestamp": l.Timestamp,
	}
	from := map[string]any{}
	if l.FromTitle != "" {
		from["title"] = l.FromTitle
	}
	if l.FromDescription != "" {
		from["description"] = l.FromDescription
	}
	if len(from) > 0 {
		body["updatedFrom"] = from
	}
	return Delivery{Vendor: Linear, Header: map[string]string{HeaderLinearEvent: "Issue", HeaderLinearDelivery: newGUID()}, Body: marshal(body)}
}

// LinearComment is a Comment create event.
type LinearComment struct {
	CommentID, IssueID, Ident, Team string
	Text, Actor                     string
	Timestamp                       int64
}

// Delivery is the event's delivery.
func (c LinearComment) Delivery() Delivery {
	return Delivery{Vendor: Linear, Header: map[string]string{HeaderLinearEvent: "Comment", HeaderLinearDelivery: newGUID()}, Body: marshal(map[string]any{
		"action": "create", "type": "Comment",
		"actor": map[string]any{"id": c.Actor, "type": "user"},
		"data": map[string]any{"id": c.CommentID, "body": c.Text, "issueId": c.IssueID, "userId": c.Actor,
			"issue": map[string]any{"id": c.IssueID, "identifier": c.Ident, "team": map[string]any{"key": c.Team}}},
		"webhookTimestamp": c.Timestamp,
	})}
}

// marshal is v as JSON. The maps above hold strings, numbers, bools and maps and slices of them, which always marshal.
func marshal(v any) string {
	b, _ := json.Marshal(v) //nolint:errchkjson // see above
	return string(b)
}

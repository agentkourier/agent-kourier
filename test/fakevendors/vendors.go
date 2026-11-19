// Package fakevendors is the vendors' side of the webhook connections, for tests: one Vendors plays, for each of GitHub, Bitbucket
// Cloud, Jira Cloud and Linear,
//
//   - the sender: on a test's word it signs a Delivery as the vendor does and POSTs it to the vendor's connection on the broker's
//     chat listener;
//   - the vendor's API, as an agent calls it through the vendor's tools: a review or a comment on a pull request, a comment on an
//     issue. It records each post, and then sends the webhook the vendor would send for it, with the agent's own account as the
//     actor, so that the broker's self-drop is tested end to end.
//
// The routes:
//
//	POST /_control/webhook/send                                                      a Delivery to sign and send; answers a Sent
//	GET  /_control/vendor/posts                                                      what the API was sent, oldest first
//	POST /vendor/github/repos/{owner}/{repo}/pulls/{n}/reviews                       a review: {"body","commit_id","event"}
//	POST /vendor/github/repos/{owner}/{repo}/issues/{n}/comments                     a comment: {"body"}
//	POST /vendor/bitbucket/2.0/repositories/{ws}/{repo}/pullrequests/{id}/comments   a comment: {"content":{"raw"}}
//	POST /vendor/jira/rest/api/3/issue/{key}/comment                                 a comment: {"body"}
//	POST /vendor/linear/graphql                                                      commentCreate: {"variables":{"input":{"issueId","body"}}}
//
// It has no authentication: run it only in a test process or behind a ClusterIP Service.
package fakevendors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The control routes of Vendors.
const (
	ControlSendPath  = "/_control/webhook/send"
	ControlPostsPath = "/_control/vendor/posts"
	vendorPrefix     = "/vendor/"
)

const (
	// maxBody is the most bytes of a request's body the server reads: the chat listener's own cap on a body.
	maxBody = 1 << 20
	// maxAnswerRead is the most of the chat listener's answer Send reads, so that the connection can be used again.
	maxAnswerRead = 4 << 10
	contentJSON   = "application/json"
)

// chatWebhookPath is what a Source's Path may be: the chat listener's route of a webhook connection, /chat/webhook/<namespace>/<name>.
var chatWebhookPath = regexp.MustCompile(`^/chat/webhook/[a-z0-9]([a-z0-9-]*[a-z0-9])?/[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// Source is one vendor's connection on the broker: its route on the chat listener, the secret it signs with, and the agent's own
// account on the vendor, which the deliveries of the agent's posts name as their actor.
type Source struct {
	Path     string `json:"path"`
	Secret   []byte `json:"-"`
	Identity string `json:"identity"`
}

// Config is what a Vendors is built from.
type Config struct {
	// ChatURL is the broker's chat listener, scheme and host (and port); its path is not read.
	ChatURL string
	// Sources are the vendors' connections, by vendor (GitHub, Bitbucket, Jira, Linear).
	Sources map[string]Source
	// Client sends the deliveries; one with a 10 s timeout if nil.
	Client *http.Client
	// Now is the clock of the timestamps of the deliveries of the agent's posts; time.Now if nil.
	Now func() time.Time
}

// Post is one call of the agent to a vendor's API, and how the broker answered the delivery the vendor sent back for it.
type Post struct {
	Vendor string `json:"vendor"`
	// Kind is review or comment; Target the pull request or issue: owner/repo#42, ws/repo#7, OPS-7, or a Linear issue's identifier.
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Body   string `json:"body"`
	Commit string `json:"commit,omitempty"`
	// SelfStatus is the broker's answer to the delivery sent back with the agent as the actor.
	SelfStatus int `json:"selfStatus"`
}

// Sent is how one delivery was answered.
type Sent struct {
	// Status is the answer's status, or 0 when no answer came: Error then says why (a refused connection, a timeout).
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
	// RetryAfter is the answer's Retry-After.
	RetryAfter string `json:"retryAfter,omitempty"`
}

// Vendors is the fake of the vendors. It is an http.Handler and is safe for concurrent use.
type Vendors struct {
	chat    *url.URL
	sources map[string]Source
	client  *http.Client
	now     func() time.Time

	mu    sync.Mutex
	posts []Post // guarded by mu
	seq   int    // the IDs of the comments and reviews the API makes, guarded by mu
	// linear holds the UUID and the team of each Linear issue a test sent, by its identifier, as Linear knows its issues: the delivery of
	// a comment names its issue's. Guarded by mu.
	linear map[string][2]string
}

// New returns the fake of cfg. Each source must be of a known vendor, with a secret, a /chat/webhook/ path and an identity.
func New(cfg Config) (*Vendors, error) {
	u, err := url.Parse(cfg.ChatURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("fakevendors: the chat URL is not an absolute http or https URL")
	}
	for v, s := range cfg.Sources {
		if _, _, ok := signatureOf(v); !ok || len(s.Secret) == 0 || !chatWebhookPath.MatchString(s.Path) || s.Identity == "" {
			return nil, fmt.Errorf("fakevendors: the source %q needs a known vendor, a secret, a /chat/webhook/ path and an identity", v)
		}
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return &Vendors{chat: u, sources: cfg.Sources, client: client, now: now, seq: 1000, linear: map[string][2]string{}}, nil
}

// Handles says whether path is one of the routes of a Vendors.
func Handles(path string) bool {
	return path == ControlSendPath || path == ControlPostsPath || strings.HasPrefix(path, vendorPrefix)
}

// The API's routes.
var (
	ghReview     = regexp.MustCompile(`^/vendor/github/repos/([^/]+/[^/]+)/pulls/([0-9]+)/reviews$`)
	ghComment    = regexp.MustCompile(`^/vendor/github/repos/([^/]+/[^/]+)/issues/([0-9]+)/comments$`)
	bbComment    = regexp.MustCompile(`^/vendor/bitbucket/2\.0/repositories/([^/]+/[^/]+)/pullrequests/([0-9]+)/comments$`)
	jiraComment  = regexp.MustCompile(`^/vendor/jira/rest/api/3/issue/([A-Z][A-Z0-9]*-[0-9]+)/comment$`)
	linearGraphQ = "/vendor/linear/graphql"
)

// Posts returns what the API was sent, oldest first.
func (v *Vendors) Posts() []Post {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]Post(nil), v.posts...)
}

// ServeHTTP routes a request (see the package's doc).
func (v *Vendors) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch p := r.URL.Path; {
	case p == ControlPostsPath && r.Method == http.MethodGet:
		writeJSON(w, v.Posts())
	case p == ControlSendPath && r.Method == http.MethodPost:
		var in Delivery
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			http.Error(w, "the body is not a Delivery", http.StatusBadRequest)
			return
		}
		writeJSON(w, v.Send(in))
	case !Handles(p):
		http.NotFound(w, r)
	case r.Method != http.MethodPost:
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
	default:
		v.api(w, r)
	}
}

// Send signs d as its vendor does and POSTs it to the vendor's connection, and says how it was answered. A Delivery whose header already
// holds the signature header is sent with it as it is.
func (v *Vendors) Send(d Delivery) Sent {
	src, ok := v.sources[d.Vendor]
	if !ok {
		return Sent{Error: "no source for vendor " + d.Vendor}
	}
	switch d.Signing {
	case SignValid, SignNone, SignBad:
	default:
		return Sent{Error: fmt.Sprintf("the signing %q is not valid, none or bad", d.Signing)}
	}
	v.learnLinearIssue(d)
	u := *v.chat
	u.Path = src.Path
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader([]byte(d.Body))) //nolint:noctx // the client's timeout bounds it
	if err != nil {
		return Sent{Error: err.Error()}
	}
	req.Header.Set("Content-Type", contentJSON)
	for k, val := range d.Header {
		req.Header.Set(k, val)
	}
	if d.Signing != SignNone {
		h, sig, err := Sign(d.Vendor, keyFor(src.Secret, d.Signing), []byte(d.Body))
		if err != nil {
			return Sent{Error: err.Error()}
		}
		if req.Header.Get(h) == "" {
			req.Header.Set(h, sig)
		}
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return Sent{Error: err.Error()}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswerRead))
	_ = resp.Body.Close()
	return Sent{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
}

// learnLinearIssue keeps the UUID and the team of a Linear issue that d is the delivery of, by its identifier.
func (v *Vendors) learnLinearIssue(d Delivery) {
	if d.Vendor != Linear {
		return
	}
	var issue struct {
		Type string `json:"type"`
		Data struct {
			ID         string `json:"id"`
			Identifier string `json:"identifier"`
			Team       struct {
				Key string `json:"key"`
			} `json:"team"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(d.Body), &issue) == nil && issue.Type == "Issue" && issue.Data.Identifier != "" {
		v.mu.Lock()
		v.linear[issue.Data.Identifier] = [2]string{issue.Data.ID, issue.Data.Team.Key}
		v.mu.Unlock()
	}
}

// api takes one call of the agent, sends back the vendor's delivery of it with the agent as the actor, and records the call with how the
// broker answered that delivery.
func (v *Vendors) api(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body     string `json:"body"`
		CommitID string `json:"commit_id"`
		Content  struct {
			Raw string `json:"raw"`
		} `json:"content"`
		Variables struct {
			Input struct {
				IssueID string `json:"issueId"`
				Body    string `json:"body"`
			} `json:"input"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
		http.Error(w, "the body is not JSON", http.StatusBadRequest)
		return
	}
	v.mu.Lock()
	v.seq++
	id := v.seq
	v.mu.Unlock()
	now := v.now().UnixMilli()
	identity := func(vendor string) string { return v.sources[vendor].Identity }
	var post Post
	var back Delivery
	switch p := r.URL.Path; {
	case ghReview.MatchString(p):
		m := ghReview.FindStringSubmatch(p)
		n, _ := strconv.Atoi(m[2])
		post = Post{Vendor: GitHub, Kind: "review", Target: m[1] + "#" + m[2], Body: in.Body, Commit: in.CommitID}
		back = GitHubReview{Repo: m[1], Number: n, ReviewID: id, Text: in.Body, SHA: in.CommitID, Author: identity(GitHub)}.Delivery()
	case ghComment.MatchString(p):
		m := ghComment.FindStringSubmatch(p)
		n, _ := strconv.Atoi(m[2])
		post = Post{Vendor: GitHub, Kind: "comment", Target: m[1] + "#" + m[2], Body: in.Body}
		back = GitHubComment{Repo: m[1], Number: n, CommentID: id, Text: in.Body, Author: identity(GitHub)}.Delivery()
	case bbComment.MatchString(p):
		m := bbComment.FindStringSubmatch(p)
		n, _ := strconv.Atoi(m[2])
		post = Post{Vendor: Bitbucket, Kind: "comment", Target: m[1] + "#" + m[2], Body: in.Content.Raw}
		back = BitbucketComment{Repo: m[1], ID: n, CommentID: id, Text: in.Content.Raw, Actor: identity(Bitbucket)}.Delivery()
	case jiraComment.MatchString(p):
		key := jiraComment.FindStringSubmatch(p)[1]
		project, _, _ := strings.Cut(key, "-")
		post = Post{Vendor: Jira, Kind: "comment", Target: key, Body: in.Body}
		back = JiraComment{Key: key, Project: project, CommentID: strconv.Itoa(id), Text: in.Body, Author: identity(Jira), Timestamp: now}.Delivery()
	case p == linearGraphQ:
		input := in.Variables.Input
		v.mu.Lock()
		issue, ok := v.linear[input.IssueID]
		v.mu.Unlock()
		if !ok {
			http.Error(w, "no such issue", http.StatusNotFound)
			return
		}
		post = Post{Vendor: Linear, Kind: "comment", Target: input.IssueID, Body: input.Body}
		back = LinearComment{CommentID: "c-" + strconv.Itoa(id), IssueID: issue[0], Ident: input.IssueID, Team: issue[1], Text: input.Body,
			Actor: identity(Linear), Timestamp: now}.Delivery()
	default:
		http.NotFound(w, r)
		return
	}
	post.SelfStatus = v.Send(back).Status
	v.mu.Lock()
	v.posts = append(v.posts, post)
	v.mu.Unlock()
	writeJSON(w, map[string]any{"id": id})
}

// writeJSON answers v as JSON with status 200.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", contentJSON)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, fmt.Sprintf("encode: %v", err), http.StatusInternalServerError)
	}
}

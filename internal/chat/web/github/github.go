// Package github is the github profile of the web platform (ChatConnection spec.web.profile github): GitHub's webhooks, from a
// repository's or an organization's webhook, or a GitHub App's. A delivery is signed with the connection's webhookSecret, and the
// profile sends no replies: the agent posts on GitHub itself, so a Binding on it takes output none alone (v1alpha1.ChatPlatformOf).
//
// What GitHub sends, and how, is GitHub's: the headers, the signature and the payloads below follow GitHub's webhook documentation,
// read 2026-11-16 and not yet checked against a live delivery. The fixtures of the tests are written from that documentation too.
//
// The events that start a turn are a pull request opened, reopened, made ready for review, or pushed to (synchronize), unless it is
// a draft; and a comment on a pull request (issue_comment) or a review comment (pull_request_review_comment), when it is new and
// mentions the connection's login (spec.web.github.login). No event that the login sent starts a turn, nor any comment it wrote: the
// agent does not answer what it does itself, such as a push it makes to a pull request. Every other event, and every other action,
// is answered and handed on to nothing.
//
// Every event of a pull request is in one thread, the pull request's (threadOf), so that a comment joins the session its pull
// request opened, and a later push is a new turn in it. Each event is its own message, with the SHA-256 of its signed body as its
// ID (bodyID): the reply queue drops a body it has taken, so a signed body sent again, by GitHub or by anyone who captured it, starts
// no second turn. X-GitHub-Delivery, which no signature covers, is not read.
package github

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// The bounds of the profile. Each is a constant: no setting changes it.
const (
	// maxBody is the most bytes of a delivery's body: a longer one is refused (413). It is the chat listener's own bound on a body,
	// so that no body the listener hands on is refused here for its size; GitHub's own cap on a payload is 25 MB (unverified).
	maxBody = 1 << 20
)

// The bounds of the fields of an event's text, in bytes. A field past its bound is cut, and ends in chat.Truncated. The repository
// and the logins are held to their shapes instead (v1alpha1.ChatPlatformOf), and the event, the action, the number and the line are
// the profile's own words or numbers.
const (
	// maxTitle bounds the pull request's title: GitHub's titles are at most 256 characters (unverified), which in UTF-8 is at most
	// 1024 bytes.
	maxTitle = 1 << 10
	// maxURL bounds the pull request's URL and the comment's: the length of a ChatConnection's reply URL.
	maxURL = 2 << 10
	// maxRef bounds the names of the head and the base branch: Git allows longer names, which GitHub may not (unverified).
	maxRef = 255
	// maxSHA bounds the head commit's SHA: 40 hex characters, or 64 in a repository of SHA-256 object names.
	maxSHA = 64
	// maxPath bounds the path of the file a review comment is on.
	maxPath = 1 << 10
	// maxComment bounds the comment's body: GitHub's comments are at most 65536 characters (unverified), which can be more bytes.
	maxComment = 64 << 10
	// maxText bounds the whole text, which the bounds of its fields keep it under (TestTheTextOfTheLargestEventIsWithinItsBound):
	// the cut that it makes is never expected to happen.
	maxText = 80 << 10
)

// The headers of a delivery (GitHub's).
const (
	// HeaderEvent names the event: pull_request, issue_comment, ping.
	HeaderEvent = "X-GitHub-Event"
	// HeaderSignature is "sha256=" and the hex HMAC-SHA256 of the body under the webhook's secret.
	HeaderSignature = "X-Hub-Signature-256"
)

// The events and actions the profile reads.
const (
	eventPing          = "ping"
	eventPullRequest   = "pull_request"
	eventIssueComment  = "issue_comment"
	eventReviewComment = "pull_request_review_comment"
)

// Profile is the github profile of one connection, made with its webhook secret and its login. It implements web.Profile.
type Profile struct {
	key    secret.Value
	login  string
	handle string // the login as a mention writes it: an app's login without its [bot]
	shapes v1alpha1.ChatPlatform
}

var _ web.Profile = (*Profile)(nil)

// New returns the profile that checks deliveries with webhookSecret (the credentials Secret's webhookSecret) for the agent that posts as
// login (spec.web.github.login), or an error for no secret, or a login that is not one.
func New(webhookSecret secret.Value, login string) (*Profile, error) {
	if webhookSecret.IsEmpty() {
		return nil, errors.New("github: no webhook secret")
	}
	shapes, ok := v1alpha1.ChatPlatformOf(v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformWeb,
		Web: &v1alpha1.WebConnectionSpec{Profile: v1alpha1.WebProfileGitHub}})
	if !ok {
		return nil, errors.New("github: the API has no github profile")
	}
	if !shapes.User.Matches(login) {
		return nil, fmt.Errorf("github: the login %q is not a GitHub login", login)
	}
	return &Profile{key: webhookSecret, login: login, handle: strings.TrimSuffix(login, "[bot]"), shapes: shapes}, nil
}

// Name implements web.Profile.
func (p *Profile) Name() v1alpha1.WebProfile { return v1alpha1.WebProfileGitHub }

// Caps implements web.Profile: none, as the profile sends no replies (v1alpha1.ChatPlatform.Replies is false for it).
func (p *Profile) Caps() chat.Caps { return chat.Caps{} }

// Encoder implements web.Profile: the profile sends no replies.
func (p *Profile) Encoder() web.Encoder { return nil }

// Verify implements web.Profile: the body is signed with the connection's webhook secret. GitHub's signature carries no time, so a
// captured delivery is not refused for its age: its body is taken once, as its ID is the body's hash (bodyID).
func (p *Profile) Verify(h http.Header, body []byte, _ time.Time) error {
	return VerifySignature([]byte(p.key.Reveal()), h.Get(HeaderSignature), body)
}

// errSchema is the error of a body that is not the event its header names. It quotes nothing of the body.
var errSchema = errors.New("github: the body is not the event's payload")

// Decode implements web.Profile: the event a delivery carries, if it is one that starts a turn (see the package comment).
func (p *Profile) Decode(h http.Header, body []byte) (web.Event, bool, error) {
	if len(body) > maxBody {
		return web.Event{}, false, fmt.Errorf("github: the body is %d bytes, over the %d byte bound: %w", len(body), maxBody, web.ErrTooLarge)
	}
	event := h.Get(HeaderEvent)
	if event == "" {
		return web.Event{}, false, fmt.Errorf("%w: no %s", errSchema, HeaderEvent)
	}
	switch event {
	case eventPullRequest, eventIssueComment, eventReviewComment:
	default:
		// ping, and every event the webhook was set up for beyond these: answered, and nothing to take. Its body is not read.
		return web.Event{}, false, nil
	}
	var pl payload
	if err := json.Unmarshal(body, &pl); err != nil {
		return web.Event{}, false, errSchema
	}
	if err := p.check(pl); err != nil {
		return web.Event{}, false, err
	}
	if strings.EqualFold(pl.Sender.Login, p.login) {
		// What the agent did itself, as its push to a pull request, or its own comment: answering it would answer its own work, and
		// that answer could make another (issue #347). Logins are compared as GitHub compares them, without regard to case
		// (unverified).
		return web.Event{}, false, nil
	}
	var (
		f    fields
		take bool
		err  error
	)
	switch event {
	case eventPullRequest:
		f, take, err = p.pullRequest(pl)
	case eventIssueComment:
		f, take, err = p.issueComment(pl)
	case eventReviewComment:
		f, take, err = p.reviewComment(pl)
	}
	if err != nil || !take {
		return web.Event{}, false, err
	}
	f.event, f.action, f.repo, f.sender = event, pl.Action, pl.Repository.FullName, pl.Sender.Login
	return web.Event{Channel: pl.Repository.FullName, Thread: threadOf(f.number), ID: bodyID(body), User: pl.Sender.Login, Text: f.text(), Mentioned: true}, true, nil
}

// bodyID is the ID of the event a body carries: the hex SHA-256 of the body, which the signature covers, so that no one can send a
// signed body again under a new ID. Its 64 hex digits hold no ':', which chat.MessageID joins with, and are never a thread's ID
// (threadOf). The body is hashed without the white space around it, which JSON does not read and which a sender may add; any change
// to the bytes the signature covers is refused by Verify, so the white space is no way to make a new ID either.
//
// The hash takes two events as one only if their bodies are the same. That two events GitHub sends have two bodies is an assumption
// of the profile (unverified): a push carries the commits before and after it (before and after), a comment its own id, and every
// payload the time of what it reports (updated_at). A redelivery from the webhook's page is assumed to resend the same body
// (unverified), so that it starts no second turn; if it does not, it starts one.
func bodyID(body []byte) string {
	sum := sha256.Sum256(bytes.TrimSpace(body))
	return hex.EncodeToString(sum[:])
}

// check holds what every event the profile reads carries: an action, and the repository and the sender, of GitHub's shapes.
func (p *Profile) check(pl payload) error {
	switch {
	case pl.Action == "":
		return fmt.Errorf("%w: no action", errSchema)
	case pl.Repository == nil || !p.shapes.Channel.Matches(pl.Repository.FullName):
		return fmt.Errorf("%w: the repository's full_name is not owner/name", errSchema)
	case pl.Sender == nil || !p.shapes.User.Matches(pl.Sender.Login):
		return fmt.Errorf("%w: the sender's login is not a GitHub login", errSchema)
	}
	return nil
}

// threadOf is the thread of every event of the pull request number: "pr-" and the number, which no event's ID can be (bodyID).
func threadOf(number int) string { return "pr-" + strconv.Itoa(number) }

// pullRequest reads a pull_request event: a turn when the pull request is opened, reopened or pushed to and is not a draft, or is
// made ready for review.
func (p *Profile) pullRequest(pl payload) (fields, bool, error) {
	switch pl.Action {
	case "opened", "reopened", "synchronize", "ready_for_review":
	default:
		return fields{}, false, nil
	}
	pr := pl.PullRequest
	if pr == nil || pr.Number <= 0 {
		return fields{}, false, fmt.Errorf("%w: no pull_request with a number", errSchema)
	}
	if pr.Draft && pl.Action != "ready_for_review" {
		return fields{}, false, nil
	}
	return pr.fields(), true, nil
}

// issueComment reads an issue_comment event: a turn for a new comment on a pull request that mentions the login and that the login
// did not write. A comment on an issue is not a pull request's, and starts nothing.
func (p *Profile) issueComment(pl payload) (fields, bool, error) {
	if pl.Action != "created" {
		return fields{}, false, nil
	}
	is, c := pl.Issue, pl.Comment
	if is == nil || is.Number <= 0 || c == nil {
		return fields{}, false, fmt.Errorf("%w: no issue with a number, or no comment", errSchema)
	}
	if is.PullRequest == nil || !p.takes(c) {
		return fields{}, false, nil
	}
	f := fields{number: is.Number, title: is.Title, url: is.PullRequest.HTMLURL}
	f.comment(c, false)
	return f, true, nil
}

// reviewComment reads a pull_request_review_comment event: a turn for a new review comment that mentions the login and that the login
// did not write.
func (p *Profile) reviewComment(pl payload) (fields, bool, error) {
	if pl.Action != "created" {
		return fields{}, false, nil
	}
	pr, c := pl.PullRequest, pl.Comment
	if pr == nil || pr.Number <= 0 || c == nil {
		return fields{}, false, fmt.Errorf("%w: no pull_request with a number, or no comment", errSchema)
	}
	if !p.takes(c) {
		return fields{}, false, nil
	}
	f := pr.fields()
	f.comment(c, true)
	return f, true, nil
}

// takes says whether a comment is one to start a turn for: it mentions the login, and the login did not write it. Decode has dropped
// every event the login sent; the comment's author is checked too, as GitHub's documentation does not say that the author of a new
// comment is always its event's sender (unverified). Logins are compared without regard to case, as in Decode.
func (p *Profile) takes(c *comment) bool {
	return !strings.EqualFold(c.User.Login, p.login) && Mentions(c.Body, p.handle)
}

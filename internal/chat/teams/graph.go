package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file reads Microsoft Graph as the bot's app: a channel's message and the replies of a thread, under the resource-specific
// permission ChannelMessage.Read.Group that the app's manifest asks for and that the team grants when the app is installed in it.
// What was measured on a test tenant in November 2026: the app's Graph token from the client credentials grant, a 200 for a read of a
// channel's messages under that permission alone (no tenant admin's consent), the path GET /teams/{group}/channels/{channel}/messages
// with the channel's ID unescaped but for what a path segment must escape, a message's shape (messageType, from.user or
// from.application, body.contentType html, attachments with a card's JSON as a string), and a system event of messageType
// unknownFutureValue. What is Microsoft's documentation and not measured: the replies endpoint itself (GET
// …/messages/{id}/replies), its $top of at most 50, that it takes no other query, its paging by @odata.nextLink, the order it gives
// (its example shows the newest first, which the reader does not rely on), and the errors (429 with Retry-After; a 403 for an app
// without the permission, whose body was not seen).

// DefaultGraphURL is Microsoft Graph's v1.0 endpoint, which the thread reads go to.
const DefaultGraphURL = "https://graph.microsoft.com/v1.0"

const (
	// graphPageTop is the $top of a read of replies, the most that Microsoft documents for it.
	graphPageTop = 50
	// graphMaxBody is the most of one answer of Graph that is read: a page of graphPageTop messages, each of at most about 100 KB as
	// Microsoft documents a bot's message (not measured), with a card's JSON quoted again as a string.
	graphMaxBody = 8 << 20
)

// errNotGraph is the error of a read of a URL that is not of the Graph endpoint, which an @odata.nextLink named: it is not asked, and
// the token does not go to it.
var errNotGraph = errors.New("teams: Microsoft Graph named a page that is not Graph's")

// GraphError is Microsoft Graph's refusal of a read: its HTTP status, its error code ("Forbidden", say) and the wait it asked for. It
// holds no token and not Graph's message, which may repeat what the request named.
type GraphError struct {
	// Status is the HTTP status of the answer.
	Status int
	// Code is Graph's error code, "other" for one of another form, or "".
	Code string
	// RetryAfter is the wait that the answer asked for (Retry-After), or 0.
	RetryAfter time.Duration
}

func (e *GraphError) Error() string {
	s := fmt.Sprintf("teams: Microsoft Graph answered %d", e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Status == http.StatusForbidden {
		// Microsoft documents this read under the resource-specific permission alone; the body of the 403 that an app without it gets
		// was not seen, so every 403 is told the fix.
		s += ": the app may not read the channel's messages. Its manifest must ask for the resource-specific permission " +
			"ChannelMessage.Read.Group, and the app must be installed in the channel's team, which grants it if the tenant's consent " +
			"policy lets the team's owner consent: install or update the app in the team"
	}
	return s
}

// graphClient reads Microsoft Graph with the app's Graph token.
type graphClient struct {
	base   *url.URL     // DefaultGraphURL, or Config.GraphURL
	client *http.Client // follows no redirect
	tokens *tokenSource // for graphScope
	now    func() time.Time
}

// newGraphClient is the client of the Graph endpoint base (a URL with a scheme and a host, and no query, fragment or credentials).
func newGraphClient(base string, client *http.Client, tokens *tokenSource, now func() time.Time) (*graphClient, error) {
	u, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("teams: the Graph URL %q is not a URL of a Graph endpoint", base)
	}
	return &graphClient{base: u, client: client, tokens: tokens, now: now}, nil
}

// resource is the URL of the Graph resource whose path segments are segs, each escaped as a path segment.
func (g *graphClient) resource(segs ...string) string {
	var b strings.Builder
	b.WriteString(g.base.String())
	for _, s := range segs {
		b.WriteString("/")
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// ours says whether link is a URL of the Graph endpoint: the same scheme and host, a path under its own with no "." or ".." segment
// (escaped or not) that could climb out of it, and no credentials. Only such a URL is given the token: an @odata.nextLink that names
// another is refused.
func (g *graphClient) ours(link string) bool {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != g.base.Scheme || u.Host != g.base.Host || u.User != nil ||
		!strings.HasPrefix(u.EscapedPath(), g.base.EscapedPath()+"/") {
		return false
	}
	for seg := range strings.SplitSeq(u.Path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// get reads the Graph URL link into out. An answer other than 2xx is a *GraphError.
func (g *graphClient) get(ctx context.Context, link string, out any) error {
	if !g.ours(link) {
		return errNotGraph
	}
	tok, err := g.tokens.token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return fmt.Errorf("teams: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	req.Header.Set("Accept", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		// The *url.Error repeats the URL, whose path names the team, the channel and the thread: only what it wraps is kept.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("teams: Microsoft Graph read: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, graphMaxBody+1))
	if err != nil {
		return fmt.Errorf("teams: reading Microsoft Graph's answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &GraphError{Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), g.now())}
		var wire struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &wire) == nil && wire.Error.Code != "" {
			e.Code = codeOf(wire.Error.Code)
		}
		return e
	}
	if len(data) > graphMaxBody {
		return fmt.Errorf("teams: Microsoft Graph's answer is longer than %d bytes", graphMaxBody)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("teams: Microsoft Graph's answer is not JSON: %w", err)
	}
	return nil
}

// graphPage is a page of a Graph collection of messages.
type graphPage struct {
	Value    []graphMessage `json:"value"`
	NextLink string         `json:"@odata.nextLink"`
}

// pages reads the collection at first, and each page that its @odata.nextLink names, at most maxPages of them, and gives each page's
// messages to visit, which ends the read by returning true. more says Graph named a page past maxPages, which was not read. A page
// that names one the read already took (a cycle) is an error: a read that cannot reach the end fails, and never passes a part for the
// whole.
func (g *graphClient) pages(ctx context.Context, first string, maxPages int, visit func([]graphMessage) (stop bool)) (more bool, err error) {
	seen := map[string]bool{first: true}
	link := first
	for n := 1; ; n++ {
		var page graphPage
		if err := g.get(ctx, link, &page); err != nil {
			return false, err
		}
		switch {
		case visit(page.Value), page.NextLink == "":
			return false, nil
		case n >= maxPages:
			return true, nil
		case seen[page.NextLink]:
			return false, errors.New("teams: a page of Microsoft Graph names as its next a page that the read already took")
		}
		seen[page.NextLink] = true
		link = page.NextLink
	}
}

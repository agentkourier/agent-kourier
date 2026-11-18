package teams

import (
	"bytes"
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

// DefaultServiceURL is the Bot Connector that a post goes to when the channel has no record of its own service URL (no activity of
// it has come to the bot since the store last lost its records). It is Microsoft's global route for Teams, which forwards to the
// tenant's region: it answered the bot's read of a team on a test tenant on 2026-11-17 (a post through it is not measured).
const DefaultServiceURL = "https://smba.trafficmanager.net/teams/"

// connectorMaxBody is the most of the Bot Connector's answer that is read: an activity's ID, an error, or a team's channels.
const connectorMaxBody = 1 << 20

// APIError is the Bot Connector's refusal of a call: its HTTP status, its error code ("ActivityNotFoundInConversation", say) and
// the wait it asked for. It holds no token and not the Bot Connector's message, which may repeat what the request held.
type APIError struct {
	// Status is the HTTP status of the answer.
	Status int
	// Code is the Bot Connector's error code, "other" for one of another form, or "".
	Code string
	// RetryAfter is the wait that the answer asked for (Retry-After), or 0.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("teams: the Bot Connector answered %d", e.Status)
	}
	return fmt.Sprintf("teams: the Bot Connector answered %d %s", e.Status, e.Code)
}

// noRedirects is a copy of client that follows no redirect: the bearer token goes to the service URL alone, and the client secret
// to Entra's token URL alone. A redirect comes back as its own answer, which is an error.
func noRedirects(client *http.Client) *http.Client {
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// connector calls the Bot Connector's REST API as the bot.
type connector struct {
	client *http.Client // follows no redirect
	tokens *tokenSource
	now    func() time.Time
}

// serviceBase checks the service URL that a call goes to and returns it with a trailing slash: an https URL with a host and no
// credentials, query or fragment. A record that holds anything else is refused, and no token is sent to it.
func serviceBase(serviceURL string) (string, error) {
	u, err := url.Parse(serviceURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("teams: the service URL %q is not an https URL of a Bot Connector", serviceURL)
	}
	return strings.TrimSuffix(serviceURL, "/") + "/", nil
}

// escapeID escapes a conversation's or an activity's ID for a path segment as the Bot Framework's clients do (encodeURIComponent):
// ":", ";", "=" and "@" are escaped too.
func escapeID(id string) string { return strings.ReplaceAll(url.QueryEscape(id), "+", "%20") }

// activityPath is the path of the activities of the conversation conv, or of its activity id when id is set.
func activityPath(conv, id string) string {
	p := "v3/conversations/" + escapeID(conv) + "/activities"
	if id != "" {
		p += "/" + escapeID(id)
	}
	return p
}

// call makes one call of the Bot Connector at serviceURL, with the bot's token, and decodes its answer into out if it is set. An
// answer other than 2xx is an *APIError.
func (c *connector) call(ctx context.Context, method, serviceURL, path string, body any, out any) error {
	base, err := serviceBase(serviceURL)
	if err != nil {
		return err
	}
	var payload io.Reader
	if body != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(body); err != nil {
			return fmt.Errorf("teams: encoding the activity: %w", err)
		}
		payload = &buf
	}
	tok, err := c.tokens.token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, payload)
	if err != nil {
		return fmt.Errorf("teams: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("teams: %s %s: %w", method, scrubPath(path), err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, connectorMaxBody+1))
	if err != nil {
		return fmt.Errorf("teams: reading the answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &APIError{Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), c.now())}
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
	if len(data) > connectorMaxBody {
		return fmt.Errorf("teams: the answer is longer than %d bytes", connectorMaxBody)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("teams: the answer is not JSON: %w", err)
	}
	return nil
}

// scrubPath is the kind of a call's path, for an error: no conversation or activity ID, which a log has no need of.
func scrubPath(path string) string {
	switch {
	case strings.HasPrefix(path, "v3/teams/"):
		return "v3/teams/…/conversations"
	case strings.Count(path, "/") > 3:
		return "v3/conversations/…/activities/…"
	}
	return "v3/conversations/…/activities"
}

// errNoID is the error of a post that the Bot Connector took without saying the new message's ID.
var errNoID = errors.New("teams: the Bot Connector took the post and named no activity ID")

// send posts the activity a to the conversation conv, and returns the new message's ID.
func (c *connector) send(ctx context.Context, serviceURL, conv string, a outActivity) (string, error) {
	var res struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, http.MethodPost, serviceURL, activityPath(conv, ""), a, &res); err != nil {
		return "", err
	}
	if res.ID == "" {
		return "", errNoID
	}
	return res.ID, nil
}

// update replaces the message id of the conversation conv with a.
func (c *connector) update(ctx context.Context, serviceURL, conv, id string, a outActivity) error {
	return c.call(ctx, http.MethodPut, serviceURL, activityPath(conv, id), a, nil)
}

// teamChannels returns the IDs of the channels of the team that the Bot Connector lists for the bot.
func (c *connector) teamChannels(ctx context.Context, serviceURL, team string) ([]string, error) {
	var res struct {
		Conversations []struct {
			ID string `json:"id"`
		} `json:"conversations"`
	}
	if err := c.call(ctx, http.MethodGet, serviceURL, "v3/teams/"+escapeID(team)+"/conversations", nil, &res); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Conversations))
	for _, cv := range res.Conversations {
		ids = append(ids, cv.ID)
	}
	return ids, nil
}

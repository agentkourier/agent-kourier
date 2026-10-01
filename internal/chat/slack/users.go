package slack

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// cachedUser is a resolved user and when it was fetched.
type cachedUser struct {
	user    dialect.User
	fetched time.Time
}

// ResolveUser returns the Slack user id for attribution (spec 8.5): users.info, cached for
// the TTL. Name is the display name, else the real name, else the handle. Email is empty
// unless the app holds users:read.email. A failure is not cached, and its error holds no
// token. It satisfies session.Users.
func (c *Client) ResolveUser(ctx context.Context, id string) (dialect.User, error) {
	now := c.now()
	c.mu.Lock()
	e, ok := c.users[id]
	c.mu.Unlock()
	if ok && now.Sub(e.fetched) < c.ttl {
		return e.user, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u, err := c.api.GetUserInfoContext(ctx, id)
	if err != nil {
		return dialect.User{}, c.scrub("users.info", err)
	}
	user := dialect.User{ID: id, Name: cmp.Or(u.Profile.DisplayName, u.Profile.RealName, u.Name), Email: u.Profile.Email}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.users[id]; !ok && len(c.users) >= c.max { // make room: the expired first, then any
		for k, v := range c.users {
			if now.Sub(v.fetched) >= c.ttl {
				delete(c.users, k)
			}
		}
		for k := range c.users {
			if len(c.users) < c.max {
				break
			}
			delete(c.users, k)
		}
	}
	c.users[id] = cachedUser{user, now}
	return user, nil
}

// scrubbed is an error whose text has had both tokens taken out. It unwraps to what a caller
// may act on and that cannot hold a token: the context errors, and slack-go's code-only
// errors (as copies with only the code, delay or status), so errors.Is and errors.As work.
// It never unwraps to the original, whose text may quote a token, as a *url.Error does.
type scrubbed struct {
	msg    string
	causes []error
}

func (e *scrubbed) Error() string   { return e.msg }
func (e *scrubbed) Unwrap() []error { return e.causes }

// scrub returns err as an error that holds no token. A Web API call fails with an error from
// the HTTP stack or slack-go, whose text may quote what it was given, and the session manager
// logs these errors. It replaces the configured tokens, then anything shaped like a Slack token.
func (c *Client) scrub(op string, err error) error {
	msg := err.Error()
	for _, t := range []string{c.bot.Reveal(), c.app.Reveal()} {
		if t != "" {
			msg = strings.ReplaceAll(msg, t, "[redacted]")
		}
	}
	var causes []error
	for _, e := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, e) {
			causes = append(causes, e)
		}
	}
	if code := new(slackgo.SlackErrorResponse); errors.As(err, code) {
		causes = append(causes, slackgo.SlackErrorResponse{Err: code.Err})
	}
	if rl := new(*slackgo.RateLimitedError); errors.As(err, rl) {
		causes = append(causes, &slackgo.RateLimitedError{RetryAfter: (*rl).RetryAfter})
	}
	if sc := new(slackgo.StatusCodeError); errors.As(err, sc) {
		causes = append(causes, slackgo.StatusCodeError{Code: sc.Code, Status: sc.Status})
	}
	return &scrubbed{msg: "slack " + op + ": " + store.RedactSecrets(msg), causes: causes}
}

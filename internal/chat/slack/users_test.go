package slack_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// Client resolves users for the session manager.
var _ session.Users = (*slack.Client)(nil)

// counter is an http.RoundTripper that counts requests by path. It answers each one with err,
// if set, else with the status, if set (with statusText, or the usual, the body and a
// Retry-After of 7 s), else by hanging until the request's context ends, if hang is set;
// otherwise it passes it on.
type counter struct {
	mu               sync.Mutex
	n                map[string]int
	err              error
	status           int
	statusText, body string
	hang             bool
}

func (c *counter) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.n[r.URL.Path]++
	err, status, hang, text, body := c.err, c.status, c.hang, c.statusText, c.body
	c.mu.Unlock()
	switch {
	case err != nil:
		return nil, err
	case status != 0:
		return &http.Response{StatusCode: status, Status: cmp.Or(text, http.StatusText(status)), Header: http.Header{"Retry-After": {"7"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	case hang:
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (c *counter) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[path]
}

// userClient returns a Client over a fake Slack that knows alice, bob and carol, with a
// clock the test moves through *now. mod may change the config.
func userClient(t *testing.T, mod func(*slack.Config)) (c *slack.Client, calls *counter, now *time.Time) {
	t.Helper()
	fake := newFake(t)
	fake.AddUser(fakeslack.User{ID: "U1", Name: "alice", DisplayName: "Ali", RealName: "Alice A", Email: "alice@example.com"})
	fake.AddUser(fakeslack.User{ID: "U2", Name: "bob", RealName: "Bob B"}) // no display name
	fake.AddUser(fakeslack.User{ID: "U3", Name: "carol"})
	calls, now = &counter{n: map[string]int{}}, new(time.Time)
	*now = time.Unix(1_000_000, 0)
	cfg := clientConfig(fake)
	cfg.HTTPClient, cfg.Now, cfg.UserTTL, cfg.UserCacheSize = &http.Client{Transport: calls}, func() time.Time { return *now }, time.Minute, 2
	if mod != nil {
		mod(&cfg)
	}
	return slack.New(cfg), calls, now
}

func TestResolveUser(t *testing.T) {
	c, _, _ := userClient(t, nil)
	for id, want := range map[string]dialect.User{
		"U1": {ID: "U1", Name: "Ali", Email: "alice@example.com"},
		"U2": {ID: "U2", Name: "Bob B"}, // the real name when there is no display name
		"U3": {ID: "U3", Name: "carol"}, // the handle when there is neither
	} {
		if got, err := c.ResolveUser(t.Context(), id); err != nil || got != want {
			t.Errorf("ResolveUser(%s) = %+v, %v; want %+v", id, got, err, want)
		}
	}
}

func TestResolveUserCache(t *testing.T) {
	c, calls, now := userClient(t, func(c *slack.Config) { c.UserCacheSize = 10 })
	resolve := func(id string) {
		t.Helper()
		if _, err := c.ResolveUser(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct {
		id    string
		after time.Duration // the clock moves on by this first
		calls int           // users.info calls so far
	}{
		{"U1", 0, 1},
		{"U1", 0, 1},               // cached
		{"U2", 0, 2},               // another user is another entry
		{"U1", time.Minute - 1, 2}, // still within the TTL
		{"U1", 1, 3},               // exactly the TTL old: expired
		{"U1", 0, 3},               // and cached again
		{"U2", 2 * time.Minute, 4}, // expired long ago
	}
	for _, s := range steps {
		*now = now.Add(s.after)
		resolve(s.id)
		if got := calls.count("/users.info"); got != s.calls {
			t.Fatalf("after ResolveUser(%s) +%v: %d users.info calls; want %d", s.id, s.after, got, s.calls)
		}
	}
}

func TestResolveUserCacheIsBounded(t *testing.T) { // the cache holds 2
	c, calls, _ := userClient(t, nil)
	for _, id := range []string{"U1", "U2", "U3", "U1", "U2", "U3"} {
		if _, err := c.ResolveUser(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.count("/users.info"); got <= 3 {
		t.Fatalf("%d users.info calls for three users twice with room for two; want an eviction to cost a call", got)
	}
}

func TestResolveUserDoesNotCacheErrors(t *testing.T) {
	c, calls, _ := userClient(t, nil)
	for range 2 {
		if _, err := c.ResolveUser(t.Context(), "U9"); err == nil || !strings.Contains(err.Error(), "user_not_found") {
			t.Fatalf("ResolveUser(U9) = %v; want user_not_found", err)
		}
	}
	if got := calls.count("/users.info"); got != 2 {
		t.Fatalf("%d users.info calls for two failures; want 2", got)
	}
}

// TestErrorsNameNoToken makes every request fail with an error that quotes both tokens, in
// the form the configured values have and in the form of a Slack token. ResolveUser's and
// Run's errors must hold neither.
func TestErrorsNameNoToken(t *testing.T) {
	const bot, app, other = "canary-bot-token", "canary-app-token", "xoxp-1234-5678-abcdefgh"
	c, calls, _ := userClient(t, func(c *slack.Config) { c.BotToken, c.AppToken = secret.New(bot), secret.New(app) })
	calls.err = fmt.Errorf("proxy refused %s, %s and %s", bot, app, other)
	_, userErr := c.ResolveUser(t.Context(), "U1")
	runErr := c.Run(t.Context(), &sink{})
	for name, err := range map[string]error{"ResolveUser": userErr, "Run": runErr} {
		if err == nil {
			t.Fatalf("%s succeeded through a failing transport", name)
		}
		for _, tok := range []string{bot, app, other} {
			if strings.Contains(err.Error(), tok) {
				t.Errorf("%s error holds %q: %v", name, tok, err)
			}
		}
		if ue := new(*url.Error); errors.As(err, ue) { // its text quotes what it was given
			t.Errorf("%s error unwraps to the transport's error: %v", name, *ue)
		}
	}
}

// TestContextErrorsAreKept: errors.Is finds the context error whether the caller's context
// ended or the HTTP stack hit a deadline of its own.
func TestContextErrorsAreKept(t *testing.T) {
	c, calls, _ := userClient(t, nil)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.ResolveUser(cancelled, "U1"); !errors.Is(err, context.Canceled) {
		t.Errorf("ResolveUser with a cancelled context = %v; want context.Canceled", err)
	}
	calls.err = fmt.Errorf("dial: %w", context.DeadlineExceeded)
	if _, err := c.ResolveUser(t.Context(), "U1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ResolveUser after a transport deadline = %v; want context.DeadlineExceeded", err)
	}
}

// TestErrorsKeepSlackCodes: what a caller needs to act on an error, such as a restart policy
// for a revoked token, is there to errors.As, and is only a code.
func TestErrorsKeepSlackCodes(t *testing.T) {
	const revoked = "canary-revoked-token"
	cfg := clientConfig(newFake(t))
	cfg.BotToken = secret.New(revoked)
	err := func() error { _, wait := run(t, slack.New(cfg), &sink{}); return wait() }()
	if code := new(slackgo.SlackErrorResponse); !errors.As(err, code) || code.Err != "invalid_auth" || strings.Contains(err.Error(), revoked) {
		t.Errorf("Run with a revoked token = %v; want an invalid_auth code, and no token", err)
	}
	c, calls, _ := userClient(t, nil)
	calls.status = http.StatusTooManyRequests
	_, err = c.ResolveUser(t.Context(), "U1")
	if rl := new(*slackgo.RateLimitedError); !errors.As(err, rl) || (*rl).RetryAfter != 7*time.Second {
		t.Errorf("ResolveUser on a 429 = %v; want a RateLimitedError with its delay", err)
	}
	calls.status = http.StatusBadGateway
	_, err = c.ResolveUser(t.Context(), "U1")
	if sc := new(slackgo.StatusCodeError); !errors.As(err, sc) || sc.Code != http.StatusBadGateway {
		t.Errorf("ResolveUser on a 502 = %v; want a StatusCodeError", err)
	}
}

// TestCausesQuoteNoToken: a Slack error code and a status line are the server's words, and
// a server may echo a token into either. The causes errors.As hands out must not carry one.
func TestCausesQuoteNoToken(t *testing.T) {
	const bot, other = "canary-bot-token", "xoxp-1234-5678-abcdefgh"
	c, calls, _ := userClient(t, func(c *slack.Config) { c.BotToken = secret.New(bot) })
	calls.status, calls.body = http.StatusOK, `{"ok":false,"error":"bad `+bot+` `+other+`"}`
	_, err := c.ResolveUser(t.Context(), "U1")
	if code := new(slackgo.SlackErrorResponse); !errors.As(err, code) || strings.Contains(code.Err, bot) || strings.Contains(code.Err, other) {
		t.Errorf("the Slack error code is %+v; want it found, without a token", code)
	}
	calls.status, calls.statusText = http.StatusBadGateway, "502 "+bot+" "+other
	_, err = c.ResolveUser(t.Context(), "U1")
	if sc := new(slackgo.StatusCodeError); !errors.As(err, sc) || strings.Contains(sc.Status, bot) || strings.Contains(sc.Status, other) {
		t.Errorf("the status error is %+v; want it found, without a token", sc)
	}
}

// TestResolveUserTimesOut: a Web API call that never answers is cut off at the configured
// timeout, though the caller's context (the runner's, long-lived) has no deadline of its own.
func TestResolveUserTimesOut(t *testing.T) {
	c, calls, _ := userClient(t, func(c *slack.Config) { c.UserTimeout = 50 * time.Millisecond })
	calls.hang = true
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second) // so that a missing timeout fails the test
	defer cancel()
	if _, err := c.ResolveUser(ctx, "U1"); !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("ResolveUser = %v, caller's context: %v; want the call's own deadline to end it", err, ctx.Err())
	}
}

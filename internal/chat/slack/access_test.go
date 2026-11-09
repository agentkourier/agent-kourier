package slack_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// accessClient is membersClient with channels the bot stands to in each way: CJOINED, CNOTJOINED and CARCHIVED, whose member
// lists hold the fake's bot user (UFAKEBOT) or not.
func accessClient(t *testing.T, mod func(*slack.Config)) (*fakeslack.Server, *slack.Client, *counter) {
	t.Helper()
	fake, c, calls := membersClient(t, mod)
	fake.SetMembers("CJOINED", "U1", "UFAKEBOT")
	fake.SetMembers("CNOTJOINED", "U1")
	fake.SetMembers("CARCHIVED", "UFAKEBOT")
	fake.SetArchived("CARCHIVED", true)
	return fake, c, calls
}

func TestChannelAccess(t *testing.T) {
	_, c, calls := accessClient(t, nil)
	for _, tc := range []struct {
		name    string
		channel string
		want    chat.Access
		err     error // the sentinel the error wraps, if any
	}{
		{"a channel the bot is in", "CJOINED", chat.Access{Member: true}, nil},
		{"a channel the bot is not in", "CNOTJOINED", chat.Access{}, nil},
		{"an archived channel the bot is in", "CARCHIVED", chat.Access{Member: true, Archived: true}, nil},
		{"a channel Slack does not know", "CNONE", chat.Access{}, chat.ErrChannelNotFound},
	} {
		before := calls.count("/conversations.info")
		got, err := c.ChannelAccess(t.Context(), tc.channel)
		if used := calls.count("/conversations.info") - before; got != tc.want || used != 1 ||
			(tc.err == nil && err != nil) || (tc.err != nil && !errors.Is(err, tc.err)) {
			t.Errorf("%s: ChannelAccess(%s) = %+v, %v after %d calls; want %+v, %v after 1", tc.name, tc.channel, got, err, used, tc.want, tc.err)
		}
	}
}

// TestChannelAccessMakesOneCallAndPagesNothing: the answer is the one call's, whatever the size of the channel, and the
// request names the channel and asks for nothing more.
func TestChannelAccessMakesOneCallAndPagesNothing(t *testing.T) {
	_, c, calls := accessClient(t, nil)
	if _, err := c.ChannelAccess(t.Context(), "CJOINED"); err != nil {
		t.Fatal(err)
	}
	if n := calls.count("/conversations.members"); n != 0 {
		t.Errorf("%d calls of conversations.members; want none", n)
	}
	f := calls.sent("/conversations.info")
	if len(f) != 1 || f[0].Get("channel") != "CJOINED" || f[0].Get("include_num_members") == "true" || f[0].Get("include_locale") == "true" || f[0].Has("cursor") {
		t.Errorf("requests %v; want one for channel CJOINED, with no member count, locale or cursor", f)
	}
	if n := len(calls.n); n != 1 {
		t.Errorf("the client called %d methods, %v; want conversations.info alone", n, calls.n)
	}
}

// TestChannelAccessIsReadAtTheCall: nothing is kept between calls, so a bot that was removed, or a channel that was
// archived, shows at the next one.
func TestChannelAccessIsReadAtTheCall(t *testing.T) {
	fake, c, calls := accessClient(t, nil)
	if got, err := c.ChannelAccess(t.Context(), "CJOINED"); err != nil || got != (chat.Access{Member: true}) {
		t.Fatalf("ChannelAccess = %+v, %v; want a member", got, err)
	}
	fake.SetMembers("CJOINED", "U1")
	fake.SetArchived("CJOINED", true)
	if got, err := c.ChannelAccess(t.Context(), "CJOINED"); err != nil || got != (chat.Access{Archived: true}) {
		t.Errorf("ChannelAccess after the removal and the archiving = %+v, %v; want not a member, archived", got, err)
	}
	if n := calls.count("/conversations.info"); n != 2 {
		t.Errorf("%d calls for two reads; want 2, none served from a cache", n)
	}
}

func TestChannelAccessWithNoChannelMakesNoCall(t *testing.T) {
	_, c, calls := accessClient(t, nil)
	got, err := c.ChannelAccess(t.Context(), "")
	if got != (chat.Access{}) || !errors.Is(err, chat.ErrChannelNotFound) || len(calls.n) != 0 {
		t.Errorf("ChannelAccess of no channel = %+v, %v after %d calls; want the zero Access, chat.ErrChannelNotFound and no call", got, err, len(calls.n))
	}
}

// TestChannelAccessSaysWhichFailuresCanNeverSucceed: the two codes of conversations.info that only an operator can mend wrap
// their sentinels, and everything else does not. Classify(OpOther) says Permanent, with Slack's code, for the two sentinels and
// for nothing else, so a caller that follows ChannelAccess's doc does not retry them. A token Slack rejects is Classify's
// FatalAuth, and a rate limit keeps the wait Slack named.
func TestChannelAccessSaysWhichFailuresCanNeverSucceed(t *testing.T) {
	slackError := func(code string) string { return `{"ok":false,"error":"` + code + `"}` }
	for name, tc := range map[string]struct {
		status int
		body   string
		err    error
		hang   bool
		want   error         // the sentinel the error wraps, or nil for neither
		perm   chat.Reason   // Classify says Permanent, with this Reason and Slack's code in the Verdict, for a sentinel
		code   string        // the code Classify keeps, with perm
		fatal  bool          // Classify says FatalAuth: a credential Slack rejects
		wait   time.Duration // Classify's RetryAfter: the counter's 429 names 7 seconds
	}{
		"a channel that is not found":            {status: http.StatusOK, body: slackError("channel_not_found"), want: chat.ErrChannelNotFound, perm: chat.ReasonChannelGone, code: "channel_not_found"},
		"an app without the scope":               {status: http.StatusOK, body: slackError("missing_scope"), want: chat.ErrMissingScope, perm: chat.ReasonOther, code: "missing_scope"},
		"a rejected token is fatal auth, not us": {status: http.StatusOK, body: slackError("invalid_auth"), fatal: true},
		"a revoked token":                        {status: http.StatusOK, body: slackError("token_revoked"), fatal: true},
		"an expired token":                       {status: http.StatusOK, body: slackError("token_expired"), fatal: true},
		"a deactivated account":                  {status: http.StatusOK, body: slackError("account_inactive"), fatal: true},
		"no token":                               {status: http.StatusOK, body: slackError("not_authed"), fatal: true},
		"access denied names no cause":           {status: http.StatusOK, body: slackError("access_denied")},
		"a workspace the token cannot reach":     {status: http.StatusOK, body: slackError("team_access_not_granted")},
		"a rate limit as a 429":                  {status: http.StatusTooManyRequests, wait: 7 * time.Second},
		"a rate limit as a code":                 {status: http.StatusOK, body: slackError("ratelimited")},
		"a 500":                                  {status: http.StatusInternalServerError, body: "oops"},
		"a 502":                                  {status: http.StatusBadGateway, body: "bad gateway"},
		"an internal error":                      {status: http.StatusOK, body: slackError("internal_error")},
		"service unavailable":                    {status: http.StatusOK, body: slackError("service_unavailable")},
		"a network error":                        {err: errors.New("connection reset")},
		"a timeout":                              {hang: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, c, calls := accessClient(t, func(cfg *slack.Config) { cfg.AuthzTimeout = 200 * time.Millisecond })
			calls.status, calls.body, calls.err, calls.hang = tc.status, tc.body, tc.err, tc.hang
			got, err := c.ChannelAccess(t.Context(), "CJOINED")
			if got != (chat.Access{}) || err == nil {
				t.Fatalf("ChannelAccess = %+v, %v; want the zero Access and the error: a failed read is no answer", got, err)
			}
			for _, sentinel := range []error{chat.ErrChannelNotFound, chat.ErrMissingScope} {
				if is, want := errors.Is(err, sentinel), errors.Is(tc.want, sentinel); is != want {
					t.Errorf("error %v: wraps %v = %t; want %t", err, sentinel, is, want)
				}
			}
			v := slack.NewPlatform(c, "ns/conn").Classify(chat.OpOther, err)
			if (v.Class == chat.FatalAuth) != tc.fatal || v.RetryAfter != tc.wait {
				t.Errorf("Classify = %+v; want FatalAuth %t and RetryAfter %v", v, tc.fatal, tc.wait)
			}
			if (v.Class == chat.Permanent) != (tc.perm != chat.ReasonNone) || v.Reason != tc.perm || (tc.perm != chat.ReasonNone && v.Code != tc.code) {
				t.Errorf("Classify = %+v; want Permanent only for a sentinel, and then with Reason %q and Code %q", v, tc.perm, tc.code)
			}
		})
	}
}

// TestChannelAccessKeepsSlacksCodeAndNoToken: a sentinel does not replace what Slack said, and the error holds no token.
func TestChannelAccessKeepsSlacksCodeAndNoToken(t *testing.T) {
	const bot = "canary-bot-token"
	for code, sentinel := range map[string]error{"channel_not_found": chat.ErrChannelNotFound, "missing_scope": chat.ErrMissingScope} {
		_, c, calls := accessClient(t, func(cfg *slack.Config) { cfg.BotToken = secret.New(bot) })
		calls.status, calls.body = http.StatusOK, `{"ok":false,"error":"`+code+`"}`
		_, err := c.ChannelAccess(t.Context(), "CJOINED")
		var resp slackgo.SlackErrorResponse
		if !errors.Is(err, sentinel) || !errors.As(err, &resp) || resp.Err != code || strings.Contains(err.Error(), bot) || !strings.Contains(err.Error(), code) {
			t.Errorf("error %v; want %v, Slack's code %s and no token", err, sentinel, code)
		}
	}
}

// TestACancelledChannelAccessIsNotAVerdictOnTheChannel: a shutdown in the middle of the read is neither a missing channel
// nor a missing scope.
func TestACancelledChannelAccessIsNotAVerdictOnTheChannel(t *testing.T) {
	_, c, _ := accessClient(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := c.ChannelAccess(ctx, "CJOINED")
	if got != (chat.Access{}) || !errors.Is(err, context.Canceled) || errors.Is(err, chat.ErrChannelNotFound) || errors.Is(err, chat.ErrMissingScope) {
		t.Errorf("ChannelAccess on a cancelled context = %+v, %v; want the zero Access and a cancellation that is neither sentinel", got, err)
	}
}

// The fake can be put in the state of an app without the scope.
func TestChannelAccessWithoutTheScope(t *testing.T) {
	fake, c, _ := accessClient(t, nil)
	fake.SetMissingScope("conversations.info", true)
	if got, err := c.ChannelAccess(t.Context(), "CJOINED"); got != (chat.Access{}) || !errors.Is(err, chat.ErrMissingScope) {
		t.Errorf("ChannelAccess = %+v, %v; want the zero Access and chat.ErrMissingScope", got, err)
	}
}

// TestPlatformChannelAccessIsTheClients: the neutral platform passes the answer and the sentinels through.
func TestPlatformChannelAccessIsTheClients(t *testing.T) {
	_, c, _ := accessClient(t, nil)
	p := slack.NewPlatform(c, "ns/conn")
	if got, err := p.ChannelAccess(t.Context(), "CARCHIVED"); err != nil || got != (chat.Access{Member: true, Archived: true}) {
		t.Errorf("ChannelAccess = %+v, %v; want a member of an archived channel", got, err)
	}
	if _, err := p.ChannelAccess(t.Context(), "CNONE"); !errors.Is(err, chat.ErrChannelNotFound) {
		t.Errorf("ChannelAccess of an unknown channel = %v; want chat.ErrChannelNotFound", err)
	}
}

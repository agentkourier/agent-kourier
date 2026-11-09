package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// identifyTimeout bounds the two calls, auth.test and bots.info, that tell whose bot a connection's token is: each is given this long.
const identifyTimeout = 10 * time.Second

// appIdentity is who a ChatConnection's bot is on its platform: the app the bot belongs to, as bots.info gives it for the bot that auth.test
// names, and the workspace, the bot's user and its bot ID. Two connections with the same App are one Slack app, whether in one workspace or in
// two (an app installed in two workspaces has one app-level token's sockets and one stream of events), and the broker serves the older of them
// (spec D52), because Slack hands each event of an app to one of the app's Socket Mode connections (docs/spikes/slack-mention-probe.md). The bot
// ID is what the posts of the bot carry (bot_id), which is how another connection's bot is told from a person.
type appIdentity struct{ App, Team, User, Bot string }

// known says the identity is a bot's: one with no app is what a test double, which asks no platform, gives, and takes part in no comparison.
func (a appIdentity) known() bool { return a.App != "" }

// errCredentialsRejected is wrapped by the error of an identity call that Slack refused because the token is not valid (invalid_auth,
// token_revoked, account_inactive and the like): no retry mends it until the token changes, so the supervisor does not ask again for the same
// credentials. It is the place where a rejected team connection is first seen.
var errCredentialsRejected = errors.New("the platform rejected the credentials")

// errIdentityMayPass is wrapped by the error of an identity call that failed in a way that may pass when it is asked again: it timed out, the
// request failed, Slack answered with a status other than 200 (a 5xx, or a 429), or with one of transientCodes. Any other failure (a scope the
// app lacks, a token that is no bot's, an answer with no app) is given again for the same token, so the supervisor does not wait on it.
var errIdentityMayPass = errors.New("the identity call may succeed when it is asked again")

// mayPassError marks err as one that errors.Is errIdentityMayPass, and says what err says.
type mayPassError struct{ error }

// Is makes the error errIdentityMayPass.
func (e mayPassError) Is(target error) bool { return target == errIdentityMayPass }

// Unwrap returns the error that is marked.
func (e mayPassError) Unwrap() error { return e.error }

// rejectedCodes are the codes of Slack's that mean the token is no good, as opposed to a call that failed for a reason that may pass.
var rejectedCodes = []string{"invalid_auth", "token_revoked", "token_expired", "account_inactive", "not_authed"}

// transientCodes are the codes of Slack's that say the service could not answer now, whatever the token: the call may succeed when it is asked
// again (errIdentityMayPass). Their meanings are taken from Slack's API documentation and were not checked against Slack here.
var transientCodes = []string{"ratelimited", "internal_error", "service_unavailable", "request_timeout", "fatal_error"}

// identityOf asks the platform whose bot conn's token is, each call within identifyTimeout. RunConfig.identify replaces the call (a seam for this
// package's tests); when a test has replaced the Slack client (newSlack) and gives no identify, the identity is not asked, since the
// client reaches no Slack, and the zero identity comes back, which takes part in no comparison.
func (c RunConfig) identityOf(ctx context.Context, conn Connection) (appIdentity, error) {
	switch {
	case c.identify != nil:
		return c.identify(ctx, conn)
	case c.newSlack != nil:
		return appIdentity{}, nil
	}
	return slackIdentity(ctx, conn, testhooks.SlackAPIURL())
}

// slackIdentity is auth.test with conn's bot token, and then bots.info for the bot it names, on a client of its own at apiURL (Slack's own when
// it is empty; the adapter's, from testhooks.SlackAPIURL, in a test image only). It never falls back to the workspace and user when bots.info
// fails: the app is what the answer is about, so the call is an error and is asked again. An error says what Slack said, or that the call failed,
// and holds no token: whatever the transport put in its own error is not repeated. One that is a token Slack refuses wraps
// errCredentialsRejected.
func slackIdentity(ctx context.Context, conn Connection, apiURL string) (appIdentity, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	opts := []slackgo.Option{slackgo.OptionHTTPClient(&http.Client{Transport: transport})}
	if apiURL != "" {
		opts = append(opts, slackgo.OptionAPIURL(apiURL))
	}
	client := slackgo.New(conn.Credentials["botToken"].Reveal(), opts...)

	authCtx, cancel := context.WithTimeout(ctx, identifyTimeout)
	defer cancel()
	auth, err := client.AuthTestContext(authCtx)
	if err != nil {
		return appIdentity{}, identifyError("auth.test", err)
	}
	if auth.UserID == "" || auth.BotID == "" {
		return appIdentity{}, errors.New("auth.test: Slack gave no bot")
	}
	botCtx, cancelBot := context.WithTimeout(ctx, identifyTimeout)
	defer cancelBot()
	bot, err := client.GetBotInfoContext(botCtx, slackgo.GetBotInfoParameters{Bot: auth.BotID})
	if err != nil {
		return appIdentity{}, identifyError("bots.info", err)
	}
	if bot.AppID == "" {
		return appIdentity{}, errors.New("bots.info: Slack gave no app for the bot")
	}
	return appIdentity{App: bot.AppID, Team: auth.TeamID, User: auth.UserID, Bot: auth.BotID}, nil
}

// identifyError describes a failed call of the method for the log without the transport's own words, which could carry more than a code. A failure
// that may pass when the call is asked again wraps errIdentityMayPass (see there).
func identifyError(method string, err error) error {
	var resp slackgo.SlackErrorResponse
	var status slackgo.StatusCodeError
	var limited *slackgo.RateLimitedError
	switch {
	case errors.As(err, &resp):
		if slices.Contains(rejectedCodes, resp.Err) {
			return &credentialsRejectedError{method: method, code: resp.Err}
		}
		e := fmt.Errorf("%s: Slack answered %q", method, resp.Err)
		if slices.Contains(transientCodes, resp.Err) {
			return mayPassError{e}
		}
		return e
	case errors.Is(err, context.DeadlineExceeded):
		return mayPassError{fmt.Errorf("%s: timed out", method)}
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", method, context.Canceled)
	case errors.As(err, &status):
		return mayPassError{fmt.Errorf("%s: Slack answered with the status %d", method, status.Code)}
	case errors.As(err, &limited):
		return mayPassError{fmt.Errorf("%s: Slack rate-limited the call", method)}
	}
	return mayPassError{fmt.Errorf("%s: the request failed", method)}
}

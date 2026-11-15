package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// identifyTimeout bounds the two calls, auth.test and bots.info, that tell whose bot a connection's token is: each is given this long.
const identifyTimeout = 10 * time.Second

// appIdentity is who a ChatConnection's bot is on its platform: the app the bot belongs to, and the workspace, the bot's user and its bot ID, as
// the platform's identity call gives them (platformSpec.Identify; on Slack the app is what bots.info gives for the bot that auth.test names).
// Platform is the connection's spec.platform, which the supervisor sets on what the call gave (supervisor.identityOf): every ID is the platform's,
// so an app, a bot or a user of one platform is never one of another's. Two connections with the same Platform and App are one app, whether in one
// workspace or in two (a Slack app installed in two workspaces has one app-level token's sockets and one stream of events), and the broker serves
// the older of them (spec D52), because Slack hands each event of an app to one of the app's Socket Mode connections
// (docs/spikes/slack-mention-probe.md). The bot ID is what the posts of the bot carry (bot_id), which is how another connection's bot is told from
// a person.
type appIdentity struct{ Platform, App, Team, User, Bot string }

// known says the identity is a bot's: one with no app is what a platform with no identity call, or a test double that asks no platform, gives,
// and takes part in no comparison.
func (a appIdentity) known() bool { return a.App != "" }

// appKey is an app as the supervisor tells the connections of one app apart from the others: its platform and its app ID on that platform.
type appKey struct{ platform, app string }

// key is the app of the identity.
func (a appIdentity) key() appKey { return appKey{platform: a.Platform, app: a.App} }

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

// transientCodes are the codes of Slack's that say the service could not answer now, whatever the token: the call may succeed when it is asked
// again (errIdentityMayPass). Their meanings are taken from Slack's API documentation and were not checked against Slack here.
var transientCodes = []string{"ratelimited", "internal_error", "service_unavailable", "request_timeout", "fatal_error"}

// identityOf asks conn's platform whose bot conn's credentials are, with the platform's own identity call (platformSpec.Identify). A platform
// with none, or one that platformFor does not know, gives the zero identity and no error, and nothing is asked of any platform: the zero identity
// takes part in no comparison. RunConfig.identify replaces the call (a seam for this package's tests).
func (c RunConfig) identityOf(ctx context.Context, conn Connection) (appIdentity, error) {
	if c.identify != nil {
		return c.identify(ctx, conn)
	}
	spec, ok := platformFor(conn.Platform)
	if !ok || spec.Identify == nil {
		return appIdentity{}, nil
	}
	return spec.Identify(ctx, c, conn)
}

// slackIdentityOf is Slack's identity call (platformSpec.Identify): slackIdentity at the API URL of testhooks.SlackAPIURL. When a test has
// replaced the Slack client (newSlack), the identity is not asked, since the client reaches no Slack, and the zero identity comes back.
func slackIdentityOf(ctx context.Context, c RunConfig, conn Connection) (appIdentity, error) {
	if c.newSlack != nil {
		return appIdentity{}, nil
	}
	return slackIdentity(ctx, conn, testhooks.SlackAPIURL(c.hooks))
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
		return appIdentity{}, errors.New("auth.test call: Slack gave no bot")
	}
	botCtx, cancelBot := context.WithTimeout(ctx, identifyTimeout)
	defer cancelBot()
	bot, err := client.GetBotInfoContext(botCtx, slackgo.GetBotInfoParameters{Bot: auth.BotID})
	if err != nil {
		return appIdentity{}, identifyError("bots.info", err)
	}
	if bot.AppID == "" {
		return appIdentity{}, errors.New("bots.info call: Slack gave no app for the bot")
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
		if slack.TokenRejected(resp.Err) {
			return &credentialsRejectedError{method: method, code: resp.Err}
		}
		e := fmt.Errorf("%s call: Slack answered %q", method, resp.Err)
		if slices.Contains(transientCodes, resp.Err) {
			return mayPassError{e}
		}
		return e
	case errors.Is(err, context.DeadlineExceeded):
		return mayPassError{fmt.Errorf("%s call: timed out", method)}
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s call: %w", method, context.Canceled)
	case errors.As(err, &status):
		return mayPassError{fmt.Errorf("%s call: Slack answered with the status %d", method, status.Code)}
	case errors.As(err, &limited):
		return mayPassError{fmt.Errorf("%s call: Slack rate-limited the call", method)}
	}
	return mayPassError{fmt.Errorf("%s call: the request failed", method)}
}

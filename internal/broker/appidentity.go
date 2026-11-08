package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// identifyTimeout bounds the auth.test call that tells whose bot a connection's token is.
const identifyTimeout = 10 * time.Second

// appIdentity is who a ChatConnection's bot is on its platform, as auth.test says: the workspace, the bot's user and its bot ID.
// Two connections with the same Team and User are one Slack app in one workspace, and the broker serves the older of them
// (spec D51), because Slack hands each event of an app to one of the app's Socket Mode connections (docs/spikes/slack-mention-probe.md).
// The bot ID is what the posts of the bot carry (bot_id), which is how another connection's bot is told from a person.
type appIdentity struct{ Team, User, Bot string }

// known says the identity is a bot's: one with no user is what a test double, which asks no platform, gives, and takes part in no comparison.
func (a appIdentity) known() bool { return a.User != "" }

// app is the key two connections of one app share.
func (a appIdentity) app() string { return a.Team + "/" + a.User }

// identityOf asks the platform whose bot conn's token is, within identifyTimeout. RunConfig.identify replaces the call (a seam for this
// package's tests); when a test has replaced the Slack client (newSlack) and gives no identify, the identity is not asked, since the
// client reaches no Slack, and the zero identity comes back, which takes part in no comparison.
func (c RunConfig) identityOf(ctx context.Context, conn Connection) (appIdentity, error) {
	switch {
	case c.identify != nil:
		return c.identify(ctx, conn)
	case c.newSlack != nil:
		return appIdentity{}, nil
	}
	return slackIdentity(ctx, conn)
}

// slackIdentity is auth.test with conn's bot token, on a client of its own (and, like the adapter's, at testhooks.SlackAPIURL in a test
// image only). Its error says what Slack said, or that the call failed, and holds no token: whatever the transport put in its own
// error is not repeated.
func slackIdentity(ctx context.Context, conn Connection) (appIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, identifyTimeout)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	opts := []slackgo.Option{slackgo.OptionHTTPClient(&http.Client{Transport: transport})}
	if url := testhooks.SlackAPIURL(); url != "" {
		opts = append(opts, slackgo.OptionAPIURL(url))
	}
	auth, err := slackgo.New(conn.Credentials["botToken"].Reveal(), opts...).AuthTestContext(ctx)
	if err != nil {
		var resp slackgo.SlackErrorResponse
		switch {
		case errors.As(err, &resp):
			return appIdentity{}, fmt.Errorf("auth.test: Slack answered %q", resp.Err)
		case errors.Is(err, context.DeadlineExceeded):
			return appIdentity{}, errors.New("auth.test: timed out")
		case errors.Is(err, context.Canceled):
			return appIdentity{}, fmt.Errorf("auth.test: %w", context.Canceled)
		}
		return appIdentity{}, errors.New("auth.test: the request failed")
	}
	if auth.UserID == "" {
		return appIdentity{}, errors.New("auth.test: Slack gave no bot user")
	}
	return appIdentity{Team: auth.TeamID, User: auth.UserID, Bot: auth.BotID}, nil
}

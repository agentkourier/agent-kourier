package broker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// Connection is the one ChatConnection v1 serves, with its credentials as read at startup: the Secret keys that
// its platform needs (platformFor), as secret.Values, which print as redacted.
type Connection struct {
	Ref v1alpha1.ObjectRef
	// Platform is ChatConnection.spec.platform, which chooses the adapter (newPlatform).
	Platform    string
	Credentials map[string]secret.Value
}

// platformSpec is what the broker knows of a platform: the keys of its credentials Secret that it reads, the modes it
// is served in, and how to build its adapter.
type platformSpec struct {
	Keys  []string
	Modes []v1alpha1.ChatMode
	Build func(c RunConfig, conn Connection, log *slog.Logger) (builtPlatform, error)
}

// platformFor is the one place that names a platform: a second platform is one more case here, with its keys, its modes
// and its adapter. One that is not here has no adapter, and a ChatConnection of it is a wiring error. It is a func and
// not a table, so that there is no state to change.
func platformFor(name string) (platformSpec, bool) {
	if name == v1alpha1.PlatformSlack {
		return platformSpec{Keys: []string{"botToken", "appToken"}, Modes: []v1alpha1.ChatMode{v1alpha1.ModeSocket}, Build: buildSlack}, true
	}
	return platformSpec{}, false
}

// slackAPI is the Slack client as the Slack adapter takes it, which is the seam of RunConfig.newSlack.
type slackAPI = slack.API

// builtPlatform is a chat platform ready to run, with what stopping it needs.
type builtPlatform struct {
	chat.Platform
	// closeIdle closes the connections the adapter's calls left open, which the shutdown does after the final drain
	// so that no goroutine outlives Run.
	closeIdle func()
}

// newPlatform builds the adapter of the connection's platform: the one place that names a platform. Only
// slack exists; any other platform is a wiring error, which no retry mends. The Slack client gets a transport of its
// own, so that its idle connections can be closed at shutdown. onConnection, if set, is told whether the platform's
// connection is up: it is Config.OnConnection of the Slack adapter, and another platform would report through the same func.
func (c RunConfig) newPlatform(conn Connection, log *slog.Logger, onConnection func(connected bool)) (builtPlatform, error) {
	name := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	if conn.Platform == v1alpha1.PlatformSlack { // a second platform is one more case here, and one more entry in platformKeys
		transport := http.DefaultTransport.(*http.Transport).Clone()
		sc := slackConfig(conn, log)
		sc.HTTPClient = &http.Client{Transport: telemetry.Transport(transport, false)}
		sc.OnConnection = onConnection
		var api slack.API
		if c.newSlack != nil {
			api = c.newSlack(sc)
		} else {
			api = slack.New(sc)
		}
		return builtPlatform{Platform: slack.NewPlatform(api, name), closeIdle: transport.CloseIdleConnections}, nil
	}
	return builtPlatform{}, fmt.Errorf("%w: ChatConnection %s: the platform %q has no adapter", ErrWiring, name, conn.Platform)
}

// buildSlack builds the Slack adapter over a client of its own transport, so that its idle connections can be closed at
// shutdown. RunConfig.newSlack, a seam of the tests, may replace the client.
func buildSlack(c RunConfig, conn Connection, log *slog.Logger) (builtPlatform, error) {
	name := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	sc := slackConfig(conn, log)
	sc.HTTPClient = &http.Client{Transport: telemetry.Transport(transport, false)}
	var api slack.API
	if c.newSlack != nil {
		api = c.newSlack(sc)
	} else {
		api = slack.New(sc)
	}
	return builtPlatform{Platform: slack.NewPlatform(api, name), closeIdle: transport.CloseIdleConnections}, nil
}

// slackConfig is the Slack client's config for conn. Its API URL comes from testhooks.SlackAPIURL and
// from nothing else: that is "" in a production build, so the bot token can only be sent to Slack,
// and a test image sets it to reach the fake.
func slackConfig(conn Connection, log *slog.Logger) slack.Config {
	// AuthzTimeout is what a press is given for its membership checks, whose callers have no deadline.
	cfg := slack.Config{BotToken: conn.Credentials["botToken"], AppToken: conn.Credentials["appToken"], Logger: log, AuthzTimeout: interact.PressAuthzTimeout}
	if url := testhooks.SlackAPIURL(); url != "" {
		cfg.APIURL = url
	}
	return cfg
}

// users is the session manager's Users over a platform: the platform's user, as the dialects name one.
type users struct{ p chat.Platform }

func (u users) ResolveUser(ctx context.Context, id string) (dialect.User, error) {
	user, err := u.p.ResolveUser(ctx, id)
	if err != nil {
		return dialect.User{}, err
	}
	return dialect.User{ID: user.ID, Name: user.Name, Email: user.Email}, nil
}

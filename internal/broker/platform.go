package broker

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// slackAPITimeout bounds one Slack Web API call as a whole: the dial, the TLS handshake, the wait for the response and the
// reading of its body. slack-go sets no deadline on any call, including the apps.connections.open that every Socket Mode
// reconnect makes, and a connection that a network blip left dead can hold a call for up to two minutes (53 s and 118 s were seen)
// before the read fails. The bound is on the HTTP client, which only the Web API calls use: the Socket Mode WebSocket is
// dialled by slack-go's own dialer (handshake cut at 45 s, a silent connection dropped by its ping check), so a connection that
// is up for hours is not touched. The adapter makes no upload and no download, and every call it makes answers a small JSON
// body, so no call is slow by design; Slack answers in well under a second normally. 30 s is the bound the CLI's own Slack
// clients already use (dry-run, init).
const slackAPITimeout = 30 * time.Second

// Connection is the one ChatConnection v1 serves, with its credentials as read at startup: the Secret keys that
// its platform needs (platformFor), as secret.Values, which print as redacted.
type Connection struct {
	Ref v1alpha1.ObjectRef
	// Platform is ChatConnection.spec.platform, which chooses the adapter (newPlatform).
	Platform    string
	Credentials map[string]secret.Value
}

// platformSpec is what the broker knows of a platform: the keys of its credentials Secret that it reads, and the name
// it is called in the text an agent reads ("Slack"). Its adapter is built by RunConfig.newPlatform, which
// leaderHalf.openPlatform calls.
type platformSpec struct {
	Keys []string
	Name string
}

// platformFor is the one place that names a platform: a second platform is one more case here, with its keys,
// and one more in newPlatform for its adapter. One that is not here has no adapter, and a ChatConnection of it is a wiring error. It is a func and
// not a table, so that there is no state to change.
func platformFor(name string) (platformSpec, bool) {
	if name == v1alpha1.PlatformSlack {
		return platformSpec{Keys: []string{"botToken", "appToken"}, Name: "Slack"}, true
	}
	return platformSpec{}, false
}

// actorPlatform is the platform a ChatConnection's users are on, as the dialects attribute a turn to them: the ID is
// spec.platform, and the display name is the one platformFor knows. A platform it does not know has an ID alone.
func actorPlatform(id string) dialect.Platform {
	spec, _ := platformFor(id)
	return dialect.Platform{ID: id, Name: spec.Name}
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
		sc.HTTPClient = &http.Client{Transport: telemetry.Transport(transport, false), Timeout: cmp.Or(c.slackHTTPTimeout, slackAPITimeout)}
		sc.OnConnection = onConnection
		var api slack.API
		if c.newSlack != nil {
			api = c.newSlack(sc)
		} else {
			api = slack.New(sc)
		}
		return builtPlatform{Platform: slack.NewPlatform(api, name).WithLogger(log), closeIdle: transport.CloseIdleConnections}, nil
	}
	return builtPlatform{}, fmt.Errorf("%w: ChatConnection %s: the platform %q has no adapter", ErrWiring, name, conn.Platform)
}

// slackConfig is the Slack client's config for conn. Its API URL comes from testhooks.SlackAPIURL and
// from nothing else: that is "" in a production build, so the bot token can only be sent to Slack,
// and a test image sets it to reach the fake.
func slackConfig(conn Connection, log *slog.Logger) slack.Config {
	// AuthzTimeout is what a press is given for its membership checks, whose callers have no deadline.
	cfg := slack.Config{BotToken: conn.Credentials["botToken"], AppToken: conn.Credentials["appToken"], Logger: log, AuthzTimeout: interact.PressAuthzTimeout,
		// The connection label of the series the adapter records (see slack.Config.Connection).
		Connection: v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)}
	if url := testhooks.SlackAPIURL(); url != "" {
		cfg.APIURL = url
	}
	return cfg
}

// users is the session manager's Users over the platforms of the connections: the platform's user, as the dialects name one,
// asked of the platform of the connection the user's turn is in.
type users struct{ platforms chat.Resolver[chat.Platform] }

func (u users) ResolveUser(ctx context.Context, conn, id string) (dialect.User, error) {
	p, err := u.platforms.Platform(conn)
	if err != nil {
		return dialect.User{}, err
	}
	user, err := p.ResolveUser(ctx, id)
	if err != nil {
		return dialect.User{}, err
	}
	return dialect.User{ID: user.ID, Name: user.Name, Email: user.Email}, nil
}

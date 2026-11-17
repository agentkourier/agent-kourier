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
// its platform and mode need (v1alpha1.CredentialKeys), as secret.Values, which print as redacted.
type Connection struct {
	Ref v1alpha1.ObjectRef
	// Platform is ChatConnection.spec.platform, which chooses the adapter (newPlatform).
	Platform string
	// Web is ChatConnection.spec.web, a copy, on a web connection: its profile, the URL of its replies and the github profile's login.
	// Nil on any other platform.
	Web         *v1alpha1.WebConnectionSpec
	Credentials map[string]secret.Value
}

// hasReplies says whether Agent Kourier posts the agent's replies on the connection's platform (v1alpha1.ChatPlatform.Replies): false
// on a web connection of the github profile, where the agent replies itself. A platform the API does not know has them, as before
// there were platforms without.
func (c Connection) hasReplies() bool {
	p, ok := v1alpha1.ChatPlatformOf(v1alpha1.ChatConnectionSpec{Platform: c.Platform, Web: c.Web})
	return !ok || p.Replies
}

// platformSpec is what the broker knows of a platform: the name it is called in the text an agent reads ("Slack"), and the call that tells
// whose bot a connection's credentials are. The keys of its credentials Secret depend on the connection's mode as well, so they are not
// here: v1alpha1.CredentialKeys gives them for a connection's spec. Its adapter is built by RunConfig.newPlatform, which
// leaderHalf.openPlatform calls.
type platformSpec struct {
	Name string
	// Identify asks the platform which app, workspace, bot and bot user conn's credentials are (appIdentity, whose Platform it leaves
	// unset), each call within identifyTimeout. Nil is a platform with no such call: its connections have the zero identity, which takes part in
	// no comparison, and nothing is asked (RunConfig.identityOf). An error that the platform refused the credentials wraps
	// errCredentialsRejected, and one that may pass when it is asked again wraps errIdentityMayPass.
	Identify func(ctx context.Context, c RunConfig, conn Connection) (appIdentity, error)
}

// platformFor is the one place that names a platform: a second platform is one more case here, with its identity call, one more in
// newPlatform for its adapter, and its keys in v1alpha1.CredentialKeys. One that is not here has no adapter, and a ChatConnection of it is a wiring
// error. It is a func and not a table, so that there is no state to change.
func platformFor(name string) (platformSpec, bool) {
	switch name {
	case v1alpha1.PlatformSlack:
		return platformSpec{Name: "Slack", Identify: slackIdentityOf}, true
	case v1alpha1.PlatformWeb:
		// No identity call: a web connection has no bot that the platform could name, and no app that two connections could share, so
		// its connections have the zero identity. The bot guard has no bot of a web connection to tell from a person, and needs none:
		// a web profile hands on no bot's message (web.Platform.IsBot), and the github profile drops what its own login sent.
		return platformSpec{Name: "web"}, true
	}
	return platformSpec{}, false
}

// actorPlatform is the platform a ChatConnection's users are on, as the dialects attribute a turn to them: the ID is
// spec.platform, and the display name is the one platformFor knows. A platform it does not know has an ID alone. The users of a web
// connection of the github profile are GitHub's, which is the name the agent reads ("[GitHub: octocat]").
func actorPlatform(spec v1alpha1.ChatConnectionSpec) dialect.Platform {
	p, _ := platformFor(spec.Platform)
	if spec.Platform == v1alpha1.PlatformWeb && spec.Web != nil && spec.Web.Profile == v1alpha1.WebProfileGitHub {
		p.Name = "GitHub"
	}
	return dialect.Platform{ID: spec.Platform, Name: p.Name}
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

// newPlatform builds the adapter of the connection's platform: the one place that names a platform. Slack and web
// exist; any other platform is a wiring error, which no retry mends. Each client gets a transport of its
// own, so that its idle connections can be closed at shutdown. onConnection, if set, is told whether the platform's
// connection is up: it is Config.OnConnection of the Slack adapter and of the web platform.
// It refuses a connection that needs the chat listener on a broker that has none (needsChatListener).
func (c RunConfig) newPlatform(conn Connection, log *slog.Logger, onConnection func(connected bool)) (builtPlatform, error) {
	name := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	if err := c.needsChatListener(conn); err != nil {
		return builtPlatform{}, err
	}
	switch conn.Platform { // a platform is one more case here, and one in platformFor
	case v1alpha1.PlatformWeb:
		return newWebPlatform(conn, log, onConnection)
	case v1alpha1.PlatformSlack:
		sc := c.slackConfig(conn, log)
		transport := http.DefaultTransport.(*http.Transport).Clone()
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

// needsChatListener refuses, with a wiring error, a connection that takes its platform's requests on the chat listener when the broker has
// none (AGENTKOURIER_CHAT_LISTEN unset): a Slack connection in HTTP mode, whose credentials are read with the signing secret
// (v1alpha1.CredentialKeys), and every web connection. The platform could not reach it, and it would show as connected while nothing came
// in. Run checks its own connection with it at start, on every replica, so that a standby refuses the config as the leader does; newPlatform
// checks each connection it builds.
func (c RunConfig) needsChatListener(conn Connection) error {
	if c.ChatListener != nil {
		return nil
	}
	name := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	switch {
	case conn.Platform == v1alpha1.PlatformWeb:
		return fmt.Errorf("%w: ChatConnection %s is a web connection, and the broker has no chat listener for its sender to send to: set AGENTKOURIER_CHAT_LISTEN",
			ErrWiring, name)
	case conn.Platform == v1alpha1.PlatformSlack && !conn.Credentials[v1alpha1.SlackSigningSecretKey].IsEmpty():
		return fmt.Errorf("%w: ChatConnection %s is in Slack HTTP mode, and the broker has no chat listener for Slack to send to: set AGENTKOURIER_CHAT_LISTEN",
			ErrWiring, name)
	}
	return nil
}

// slackConfig is the Slack client's config for conn. Its API URL comes from testhooks.SlackAPIURL and
// from nothing else: that is "" in a production build, so the bot token can only be sent to Slack,
// and a test image sets it to reach the fake (a unit test, through c.hooks). The credentials are the keys of the connection's mode
// (v1alpha1.CredentialKeys): the app token in Socket Mode, and the signing secret in HTTP mode, which puts the client in HTTP mode.
func (c RunConfig) slackConfig(conn Connection, log *slog.Logger) slack.Config {
	// AuthzTimeout is what a press is given for its membership checks, whose callers have no deadline.
	cfg := slack.Config{BotToken: conn.Credentials[v1alpha1.SlackBotTokenKey], AppToken: conn.Credentials[v1alpha1.SlackAppTokenKey],
		SigningSecret: conn.Credentials[v1alpha1.SlackSigningSecretKey], Logger: log, AuthzTimeout: interact.PressAuthzTimeout,
		// The connection label of the series the adapter records (see slack.Config.Connection).
		Connection: v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)}
	if url := testhooks.SlackAPIURL(c.hooks); url != "" {
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

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
	"github.com/agentkourier/agent-kourier/internal/chat/teams"
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

// teamsAPITimeout bounds one call of a Microsoft Teams connection as a whole, from the dial to the end of the answer's body: a token
// from Microsoft Entra, a post, reply or edit through the Bot Connector, a read of a thread's page through Microsoft Graph, and a fetch
// of the Bot Framework's keys. The adapter bounds a token fetch (15 s), a key fetch (10 s) and a read of a thread, all its pages
// (10 s), more closely itself, so this is the bound of a Bot Connector call, which answers a small JSON body. It is Slack's
// bound (slackAPITimeout), for the same reason: a connection that a network blip left dead does not hold a post for minutes.
const teamsAPITimeout = 30 * time.Second

// Connection is the one ChatConnection v1 serves, with its credentials as read at startup: the Secret keys that
// its platform and mode need (v1alpha1.CredentialKeys), as secret.Values, which print as redacted.
type Connection struct {
	Ref v1alpha1.ObjectRef
	// Platform is ChatConnection.spec.platform, which chooses the adapter (newPlatform).
	Platform    string
	Credentials map[string]secret.Value
	// Teams is spec.teams of a Microsoft Teams connection, its tenant, app and mode, and zero for any other.
	Teams v1alpha1.TeamsConnectionSpec
}

// platformSpec is what the broker knows of a platform: the call that tells whose bot a connection's credentials are. The name it is
// called in the text an agent reads is the API's (actorPlatform). The keys of its credentials Secret depend on the connection's mode as well, so they are not
// here: v1alpha1.CredentialKeys gives them for a connection's spec. Its adapter is built by RunConfig.newPlatform, which
// leaderHalf.openPlatform calls.
type platformSpec struct {
	// Identify asks the platform which app, workspace, bot and bot user conn's credentials are (appIdentity, whose Platform it leaves
	// unset), each call within identifyTimeout. Nil is a platform with no such call: its connections have the zero identity, which takes part in
	// no comparison, and nothing is asked (RunConfig.identityOf). An error that the platform refused the credentials wraps
	// errCredentialsRejected, and one that may pass when it is asked again wraps errIdentityMayPass.
	Identify func(ctx context.Context, c RunConfig, conn Connection) (appIdentity, error)
}

// platformFor is the one place that names a platform: a platform is a case here, with its identity call, one in newPlatform for its
// adapter, and its keys in v1alpha1.CredentialKeys. One that is not here has no adapter, and a ChatConnection of it is a wiring error. It
// is a func and not a table, so that there is no state to change.
func platformFor(name string) (platformSpec, bool) {
	switch name {
	case v1alpha1.PlatformSlack:
		return platformSpec{Identify: slackIdentityOf}, true
	case v1alpha1.PlatformTeams:
		return platformSpec{Identify: teamsIdentityOf}, true
	}
	return platformSpec{}, false
}

// teamsIdentityOf is Microsoft Teams' identity call (platformSpec.Identify), which asks Teams nothing: a Teams bot is its Entra app, which
// spec.teams.appID names. Two connections of one app are then one app to the supervisor, and the older serves it, as two of one Slack app
// do. Its bot is the bare app ID, not its Teams ID (28:<app ID>): a bot's post reaches the broker only through Microsoft Graph, which names
// the app by that ID (chat.BotMessage.ID), as a botMessage trigger does, so the bot guard (botGuard) knows another connection's bot by it.
// Whether the client secret is the app's is for the connection's run to find (a FatalAuth).
func teamsIdentityOf(_ context.Context, _ RunConfig, conn Connection) (appIdentity, error) {
	return appIdentity{App: conn.Teams.AppID, Bot: conn.Teams.AppID}, nil
}

// actorPlatform is the platform a ChatConnection's users are on, as the dialects attribute a turn to them: the ID is
// spec.platform, and the display name is the API's (v1alpha1.ChatPlatform.ActorName), such as Slack. A platform the API does not know
// has an ID alone.
func actorPlatform(spec v1alpha1.ChatConnectionSpec) dialect.Platform {
	p, _ := v1alpha1.ChatPlatformOf(spec)
	return dialect.Platform{ID: spec.Platform, Name: p.ActorName}
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

// newPlatform builds the adapter of the connection's platform: the one place that names a platform. Slack and Microsoft Teams
// (in http or socket mode, spec.teams.mode) exist; any other platform is a wiring error, which no retry mends. Each client gets
// a transport of its own, so that its idle connections can be closed at shutdown. onConnection, if set, is told whether the
// platform's connection is up: it is Config.OnConnection of the adapter. convs keeps what a Microsoft Teams connection's activities say
// of each channel: the leader gives the broker's store (newTeamsConversations), so that a restart or a takeover does not post through
// the global route until an activity comes, and a Teams connection with none is a wiring error; a Slack connection does not read it. It
// refuses a connection that needs the chat listener on a broker that has none (needsChatListener).
func (c RunConfig) newPlatform(conn Connection, log *slog.Logger, onConnection func(connected bool),
	convs teams.ConversationStore) (builtPlatform, error) {
	name := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	if err := c.needsChatListener(conn); err != nil {
		return builtPlatform{}, err
	}
	if conn.Platform == v1alpha1.PlatformSlack { // a second platform is one more case here, and one in platformFor
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
	if conn.Platform == v1alpha1.PlatformTeams {
		if convs == nil {
			return builtPlatform{}, fmt.Errorf("%w: ChatConnection %s: a Microsoft Teams connection needs a store for its channels' conversations", ErrWiring, name)
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		tc := teams.Config{Connection: name, TenantID: conn.Teams.TenantID, AppID: conn.Teams.AppID,
			ClientSecret: conn.Credentials[v1alpha1.TeamsClientSecretKey], Logger: log, OnConnection: onConnection,
			HTTPClient: &http.Client{Transport: telemetry.Transport(transport, false), Timeout: teamsAPITimeout}, Conversations: convs,
			Socket: conn.Teams.Mode == v1alpha1.TeamsModeSocket, NotificationURL: conn.Teams.NotificationURL}
		if c.teamsFakes != nil {
			c.teamsFakes(&tc)
		}
		client, err := teams.New(tc)
		if err != nil {
			return builtPlatform{}, fmt.Errorf("%w: ChatConnection %s: %w", ErrWiring, name, err)
		}
		return builtPlatform{Platform: client, closeIdle: transport.CloseIdleConnections}, nil
	}
	return builtPlatform{}, fmt.Errorf("%w: ChatConnection %s: the platform %q has no adapter", ErrWiring, name, conn.Platform)
}

// needsChatListener refuses, with a wiring error, a connection that takes its platform's requests on the chat listener when the broker has
// none (AGENTKOURIER_CHAT_LISTEN unset): a Slack connection in HTTP mode, whose credentials are read with the signing secret
// (v1alpha1.CredentialKeys), a Microsoft Teams connection in http mode (spec.teams.mode unset or http), and one in socket mode with a
// spec.teams.notificationURL, to which Microsoft Graph sends the notifications of its channels' messages. The platform could not reach
// it, and it would show as connected while nothing came in. Run checks its own connection with it at start, on every replica, so that a
// standby refuses the config as the leader does; newPlatform checks each connection it builds.
func (c RunConfig) needsChatListener(conn Connection) error {
	if c.ChatListener != nil {
		return nil
	}
	name := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	switch {
	case conn.Platform == v1alpha1.PlatformSlack && !conn.Credentials[v1alpha1.SlackSigningSecretKey].IsEmpty():
		return fmt.Errorf("%w: ChatConnection %s is in Slack HTTP mode, and the broker has no chat listener for Slack to send to: set AGENTKOURIER_CHAT_LISTEN",
			ErrWiring, name)
	case conn.Platform == v1alpha1.PlatformTeams && conn.Teams.Mode != v1alpha1.TeamsModeSocket:
		return fmt.Errorf("%w: ChatConnection %s is a Microsoft Teams connection in http mode, and the broker has no chat listener for Teams to send to: set AGENTKOURIER_CHAT_LISTEN",
			ErrWiring, name)
	case conn.Platform == v1alpha1.PlatformTeams && conn.Teams.NotificationURL != "":
		return fmt.Errorf("%w: ChatConnection %s is a Microsoft Teams connection with a notificationURL, and the broker has no chat listener for Microsoft Graph to send to: set AGENTKOURIER_CHAT_LISTEN",
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

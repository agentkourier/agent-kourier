package teams

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// Config configures a Client.
type Config struct {
	// Connection is the ChatConnection, "namespace/name". Required.
	Connection string
	// TenantID is the connection's Microsoft Entra tenant (spec.teams.tenantID), and AppID the bot's application
	// (spec.teams.appID). Required.
	TenantID, AppID string
	// ClientSecret is the app's client secret (the Secret's clientSecret), which gets the bot's token from Microsoft Entra.
	// Required.
	ClientSecret secret.Value
	// TokenURL is Entra's token endpoint; DefaultTokenURL(TenantID) if empty. A test sets a fake's.
	TokenURL string
	// MetadataURL is the Bot Framework's OpenID metadata (ReceiverConfig.MetadataURL).
	MetadataURL string
	// HTTPClient makes every call: Entra's, the Bot Connector's and the key fetches; http.DefaultClient if nil. The Client makes
	// its own copy that follows no redirect for the calls that carry the secret or the token. Its Timeout bounds each call.
	HTTPClient *http.Client
	// Conversations keeps each channel's service URL and team, which the receiver records and the posts read; a
	// MemoryConversations if nil. It is the seam of the persistent store.
	Conversations ConversationStore
	// Logger is slog.Default() if nil. The caller names the connection on it.
	Logger *slog.Logger
	// Now is the clock of the tokens, the keys and the dedup; time.Now if nil.
	Now func() time.Time
	// OnConnection, if set, is told true when Run starts taking activities and false when it stops. It is called from the
	// goroutine of Run, in order; it must not block.
	OnConnection func(connected bool)
}

// Client is a Microsoft Teams connection as a chat.Platform, in http mode: Teams sends the bot's activities to the connection's
// route on the chat listener (HTTPHandler, the Receiver), and the Client posts, replies and edits through the Bot Connector with
// the bot's token from Microsoft Entra.
//
// A message's progress steps are a list under the rest of the message (Caps.Steps), which each update of a live reply shows anew.
// What it does not do yet, and says so: streams (ErrNoStreams), ephemeral messages (ErrNoEphemeral), reactions
// (ErrNoReactions), the busy mark (ErrNoBusy), buttons and tool approvals (a prompt is shown as text, a prompt of tools is
// ErrInvalid), reading a thread (ErrNoThreadReads), finding a post by its tag (Find says not found), and reading membership,
// bots and names (IsMember and IsBot fail closed, ResolveUser gives the ID alone).
type Client struct {
	conn, tenant string
	recv         *Receiver
	api          *connector
	convs        ConversationStore
	log          *slog.Logger
	onConnection func(bool)
}

// New returns the Client of cfg. It refuses a config with no connection, tenant, app or secret.
func New(cfg Config) (*Client, error) {
	if cfg.ClientSecret.IsEmpty() {
		return nil, errors.New("teams: no client secret")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	convs := cfg.Conversations
	if convs == nil {
		convs = NewMemoryConversations()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	recv, err := NewReceiver(ReceiverConfig{Connection: cfg.Connection, TenantID: cfg.TenantID, AppID: cfg.AppID,
		MetadataURL: cfg.MetadataURL, HTTPClient: client, Conversations: convs, Logger: log, Now: now})
	if err != nil {
		return nil, err
	}
	direct := noRedirects(client)
	tokens := newTokenSource(cmp.Or(cfg.TokenURL, DefaultTokenURL(cfg.TenantID)), cfg.AppID, cfg.ClientSecret, direct, now)
	return &Client{conn: cfg.Connection, tenant: cfg.TenantID, recv: recv, convs: convs, log: log, onConnection: cfg.OnConnection,
		api: &connector{client: direct, tokens: tokens, now: now}}, nil
}

// Run takes the activities that Teams sends the connection and hands them to sink until ctx ends (Receiver.Serve). Serve starts
// first, so that what Teams sends while the bot's token is fetched is taken and counted; then Run gets the token, so that a client
// secret, app or tenant that Microsoft Entra refuses ends Run, with Serve, and an error that Classify says is FatalAuth. A fetch that
// fails otherwise is logged, and Run goes on, since the token is fetched again when a post needs it. OnConnection is told up once the
// token was asked for, and down when Serve has returned.
func (c *Client) Run(ctx context.Context, sink chat.Sink) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- c.recv.Serve(ctx, sink) }()
	if _, err := c.api.tokens.token(ctx); err != nil {
		if c.Classify(chat.OpOther, err).Class == chat.FatalAuth {
			cancel()
			_ = <-served // Serve's own error says only that it was cancelled
			return err
		}
		if ctx.Err() == nil {
			c.log.WarnContext(ctx, "teams: the bot's token could not be fetched; taking activities, and fetching it again for the next post",
				"error", err.Error())
		}
	}
	if c.onConnection != nil {
		c.onConnection(true)
		defer c.onConnection(false)
	}
	return <-served
}

// HTTPHandler implements chat.HTTPReceiver: the Receiver, the handler of the connection's route.
func (c *Client) HTTPHandler() (http.Handler, bool) { return c.recv, true }

// conversation is the Bot Connector's conversation of a thread: the channel with the thread's root, or the channel alone for a post
// that starts a thread.
func conversation(channel, root string) string {
	if root == "" {
		return channel
	}
	return channel + threadMarker + root
}

// serviceURL is the Bot Connector of channel: the one its record names, or DefaultServiceURL with none. A store that fails is
// logged, and the default used.
func (c *Client) serviceURL(ctx context.Context, channel string) string {
	rec, ok, err := c.convs.Lookup(ctx, c.conn, channel)
	if err != nil {
		c.log.WarnContext(ctx, "teams: the channel's service URL could not be read; posting through the default", "channel", channel,
			"error", err.Error())
	}
	if ok && rec.ServiceURL != "" {
		return rec.ServiceURL
	}
	return DefaultServiceURL
}

func (c *Client) ref(channel, thread, id string) chat.Ref {
	return chat.Ref{Platform: PlatformName, Conn: c.conn, Channel: channel, ID: id, Thread: threadOf(thread, id)}
}

// Post implements chat.Platform. A post with no thread starts one in the channel, and its ID is the thread's root; a post with a
// thread replies in it. tag is not kept: Find cannot read a post back yet.
func (c *Client) Post(ctx context.Context, t chat.Target, o chat.Outgoing, _ string) (ref chat.Ref, err error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.Post")
	defer func() { c.finish(ctx, op, "Post", err) }()
	if t.Channel == "" {
		return chat.Ref{}, fmt.Errorf("%w: a post with no channel", chat.ErrInvalid)
	}
	a, err := render(o)
	if err != nil {
		return chat.Ref{}, err
	}
	id, err := c.api.send(ctx, c.serviceURL(ctx, t.Channel), conversation(t.Channel, t.Thread), a)
	if err != nil {
		return chat.Ref{}, err
	}
	return c.ref(t.Channel, t.Thread, id), nil
}

// Update implements chat.Platform. Every message on Teams holds only text, so content that shows none is ErrWouldDropText.
func (c *Client) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) (err error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.Update")
	defer func() { c.finish(ctx, op, "Update", err) }()
	if r.Channel == "" || r.ID == "" {
		return fmt.Errorf("%w: an update with no channel or message", chat.ErrInvalid)
	}
	a, err := render(o)
	if errors.Is(err, errEmpty) {
		return fmt.Errorf("teams: %w: %w", chat.ErrWouldDropText, errEmpty)
	}
	if err != nil {
		return err
	}
	return c.api.update(ctx, c.serviceURL(ctx, r.Channel), conversation(r.Channel, cmp.Or(r.Thread, r.ID)), r.ID, a)
}

// Find implements chat.Platform: Teams' posts cannot be read back yet (that needs Microsoft Graph), so nothing is found, and a
// post that the outbox made before a crash, and did not record, is made again.
func (c *Client) Find(context.Context, chat.Target, string, time.Time) (chat.Ref, bool, error) {
	return chat.Ref{}, false, nil
}

// RefAt implements chat.Platform.
func (c *Client) RefAt(channel, thread, id string) chat.Ref { return c.ref(channel, thread, id) }

// Stream implements chat.Platform: Teams has no streams in this release.
func (c *Client) Stream(context.Context, chat.Target, string, string) (chat.Stream, error) {
	return nil, chat.ErrNoStreams
}

// StreamAt implements chat.Platform: no message on Teams is streaming.
func (c *Client) StreamAt(r chat.Ref) chat.Stream { return notStreaming{r} }

// notStreaming is a message that is not streaming, as StreamAt finds it.
type notStreaming struct{ ref chat.Ref }

func (s notStreaming) Ref() chat.Ref { return s.ref }

func (notStreaming) Append(context.Context, string) error {
	return fmt.Errorf("teams: %w", chat.ErrNotStreaming)
}

func (notStreaming) Step(context.Context, chat.Step) error {
	return fmt.Errorf("teams: %w", chat.ErrNotStreaming)
}

func (notStreaming) Stop(context.Context, string, chat.Message, string) error {
	return fmt.Errorf("teams: %w", chat.ErrNotStreaming)
}

// Ephemeral implements chat.Platform: Teams shows no message to one person alone.
func (c *Client) Ephemeral(context.Context, chat.Target, string, chat.Message) error {
	return fmt.Errorf("teams: %w", chat.ErrNoEphemeral)
}

// React implements chat.Platform: a bot cannot react on Teams.
func (c *Client) React(context.Context, chat.Ref, string) error { return chat.ErrNoReactions }

// Unreact implements chat.Platform: a bot cannot react on Teams.
func (c *Client) Unreact(context.Context, chat.Ref, string) error { return chat.ErrNoReactions }

// errMembership is the error of the reads of people that the Client does not make yet.
var errMembership = errors.New("teams: membership is not read on Microsoft Teams yet")

// IsMember implements chat.Platform. It does not read the channel's members yet, and fails closed: false, with an error.
func (c *Client) IsMember(context.Context, string, string) (bool, error) { return false, errMembership }

// IsBot implements chat.Platform. It does not read who a user is yet, and fails closed: a bot, with an error.
func (c *Client) IsBot(context.Context, string) (bool, error) { return true, errMembership }

// ResolveUser implements chat.Platform. It does not read names yet: the user is the ID alone.
func (c *Client) ResolveUser(_ context.Context, id string) (chat.User, error) {
	return chat.User{ID: id}, nil
}

// errNoGroups is the cause of the errors of the group reads: a Binding of a Teams connection takes no approver groups.
var errNoGroups = errors.New("a Microsoft Teams connection has no approver groups")

// IsInAnyGroup implements chat.Platform: Teams has no approver groups, so a non-empty groups is false and an error wrapping
// chat.ErrGroupsMisconfigured.
func (c *Client) IsInAnyGroup(ctx context.Context, groups []string, user string) (bool, error) {
	if len(groups) == 0 || user == "" {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("teams: %w", err)
	}
	return false, fmt.Errorf("teams: %w: %w", chat.ErrGroupsMisconfigured, errNoGroups)
}

// GroupsReadable implements chat.Platform: no group can be read on Teams, so the first group is a *chat.GroupError wrapping
// chat.ErrGroupsMisconfigured and chat.ErrGroupNotFound.
func (c *Client) GroupsReadable(ctx context.Context, groups []string) error {
	if len(groups) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("teams: %w", err)
	}
	return &chat.GroupError{Group: groups[0], Err: fmt.Errorf("%w: %w: %w", chat.ErrGroupsMisconfigured, chat.ErrGroupNotFound, errNoGroups)}
}

// ChannelAccess implements chat.Platform by the Bot Connector's list of the channels of the channel's team: a channel it lists is
// one the bot is a member of, and one it does not list, or a team it does not know, is chat.ErrChannelNotFound. The team is the
// one that the channel's record names, so a channel that no activity has come from yet cannot be read, and that error wraps no
// sentinel. Teams says nothing of a channel's archive or the app's permissions here: Access.Archived is never set, and
// chat.ErrMissingScope never given.
func (c *Client) ChannelAccess(ctx context.Context, channel string) (a chat.Access, err error) {
	if channel == "" {
		return chat.Access{}, fmt.Errorf("teams: %w: no channel", chat.ErrChannelNotFound)
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.ChannelAccess")
	defer func() { c.finish(ctx, op, "ChannelAccess", err) }()
	rec, ok, err := c.convs.Lookup(ctx, c.conn, channel)
	switch {
	case err != nil:
		return chat.Access{}, fmt.Errorf("teams: reading the channel's record: %w", err)
	case !ok || rec.TeamID == "":
		return chat.Access{}, errors.New("teams: the channel's team is not known yet: no activity of the channel has come to the bot")
	}
	ids, err := c.api.teamChannels(ctx, cmp.Or(rec.ServiceURL, DefaultServiceURL), rec.TeamID)
	var ae *APIError
	switch {
	case errors.As(err, &ae) && (ae.Status == http.StatusNotFound || gone403(ae)):
		return chat.Access{}, fmt.Errorf("teams: %w: %w", chat.ErrChannelNotFound, err)
	case err != nil:
		return chat.Access{}, err
	case !slices.Contains(ids, channel):
		return chat.Access{}, fmt.Errorf("teams: %w: the team does not list it", chat.ErrChannelNotFound)
	}
	return chat.Access{Member: true}, nil
}

// Fits implements chat.Platform: a prompt fits if it renders within the budget of a message. A prompt of tools is chat.ErrInvalid.
func (c *Client) Fits(p chat.Prompt) error {
	_, err := render(chat.Ask(p))
	return err
}

// Capabilities implements chat.Platform.
func (c *Client) Capabilities() chat.Caps { return capabilities() }

// Permalink implements chat.Permalinker from what the receiver has recorded of the channel, with no call: a link needs the team's
// Microsoft Entra group, which only an activity of the channel tells, so a channel that none has come from in this run has none.
func (c *Client) Permalink(r chat.Ref) (string, bool) {
	if r.Platform != PlatformName || r.Conn != c.conn || r.Channel == "" || r.ID == "" {
		return "", false
	}
	c.recv.recMu.Lock()
	rec, ok := c.recv.recorded[r.Channel]
	c.recv.recMu.Unlock()
	if !ok || rec.AADGroupID == "" {
		return "", false
	}
	q := url.Values{"tenantId": {c.tenant}, "groupId": {rec.AADGroupID}, "parentMessageId": {cmp.Or(r.Thread, r.ID)}}
	return "https://teams.microsoft.com/l/message/" + url.PathEscape(r.Channel) + "/" + url.PathEscape(r.ID) + "?" + q.Encode(), true
}

// ThreadBetween implements chat.ThreadReader: a thread cannot be read on Teams yet.
func (c *Client) ThreadBetween(context.Context, chat.Target, string, string, int) ([]chat.HistoryMessage, int, error) {
	return nil, 0, chat.ErrNoThreadReads
}

// ThreadFromStart implements chat.ThreadReader: a thread cannot be read on Teams yet.
func (c *Client) ThreadFromStart(context.Context, chat.Target, string, int) ([]chat.HistoryMessage, int, error) {
	return nil, 0, chat.ErrNoThreadReads
}

// MarkBusy implements chat.BusyMarker: Teams shows no busy mark in a channel.
func (c *Client) MarkBusy(context.Context, chat.Target, bool) error { return chat.ErrNoBusy }

var (
	_ chat.Platform     = (*Client)(nil)
	_ chat.HTTPReceiver = (*Client)(nil)
)

package teams

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
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
	// GraphURL is Microsoft Graph's endpoint, which a thread is read from; DefaultGraphURL if empty. A test sets a fake's.
	GraphURL string
	// HTTPClient makes every call: Entra's, the Bot Connector's, Graph's and the key fetches; http.DefaultClient if nil. The Client makes
	// its own copy that follows no redirect for the calls that carry the secret or the token. Its Timeout bounds each call.
	HTTPClient *http.Client
	// Conversations keeps each channel's service URL and team, which the receiver records and the posts read; a
	// MemoryConversations if nil. It is the seam of the persistent store.
	Conversations ConversationStore
	// Logger is slog.Default() if nil. The caller names the connection on it.
	Logger *slog.Logger
	// Now is the clock of the tokens, the keys and the dedup; time.Now if nil.
	Now func() time.Time
	// OnConnection, if set, is told whether the connection takes activities: in http mode true when Run starts taking them and false
	// when it stops; in socket mode true when a socket is ready and none was, and false when the last ready one ends or Run stops. It
	// is called from the goroutine of Run, in order; it must not block.
	OnConnection func(connected bool)
	// Socket says the connection is in socket mode (spec.teams.mode socket): Run opens its sockets to Microsoft (socket.go) and the
	// Client offers no handler for the chat listener. False is http mode.
	Socket bool
	// SocketURL is the connect endpoint of a region in socket mode; https://<region>.botapi.skype.com/v3/websockets/connect if nil. A
	// test sets a fake's.
	SocketURL func(region string) string
	// SocketTLS is the TLS config of a socket's WebSocket; nil trusts the system's roots. A test sets one that trusts a fake's
	// certificate. The connect call and the negotiate go through HTTPClient.
	SocketTLS *tls.Config
	// OnSocketGap, if set, is told of each interval in socket mode in which no socket was ready, when a socket is ready again (Gap): what
	// Teams sent in it was not delivered. With no NotificationURL the Client then polls every bound channel at once, with a catch-up read
	// of what was posted since the gap began (poll.go); with one, Graph notified those posts all the same, and nothing more is read. It is
	// called from the goroutine of Run, in order; it must not block.
	OnSocketGap func(Gap)
	// NotificationURL is the connection's spec.teams.notificationURL: when set, Run keeps a Microsoft Graph subscription to the messages
	// of each channel that a Binding of the connection names (subscriptions.go), with this as its notification and lifecycle URL, and
	// the connection's route on the chat listener takes what Graph sends there, in either mode (push.go). It is how another app's post,
	// an alert's that Workflows posted, reaches the connection, since Teams sends a bot none. Unset, the connection subscribes to nothing,
	// and Run polls each bound channel's messages from Graph instead (poll.go).
	NotificationURL string
	// OnNotificationsMissed, if set, is told the channel of each lifecycle notification missed: Graph did not deliver some notifications
	// of the channel's messages. The Client then makes one catch-up read of the channel's recent posts, within the look-back (poll.go). It is
	// called from a request's goroutine; it must not block.
	OnNotificationsMissed func(channel string)
}

// Client is a Microsoft Teams connection as a chat.Platform. In http mode Teams sends the bot's activities to the connection's route
// on the chat listener (HTTPHandler, the Receiver); in socket mode Run opens a socket to Microsoft in each region that answers, and the
// activities come on it to the same Receiver (socket.go). Either way the Client posts, replies and edits through the Bot Connector
// with the bot's token from Microsoft Entra.
//
// A message's progress steps are a list under the rest of the message (Caps.Steps), which each update of a live reply shows anew.
// A prompt, a prompt of tools among them, is an Adaptive Card, and so are a message's buttons; the Client is the Receiver's
// InvokeHandler, which takes their presses (Invoke). What it does not do yet, and says so: streams (ErrNoStreams), ephemeral
// messages (ErrNoEphemeral), reactions (ErrNoReactions) and the busy mark (ErrNoBusy).
// It reads people from the Bot Connector's rosters (members.go): who is in a channel's team, whether a user is a bot, and a person's
// name, which the ending of a prompt shows for its decider; and from Microsoft Graph, with the app's Graph token, a thread's messages
// (threadread.go) and a post by the tag that Post carries in it (find.go).
type Client struct {
	conn, tenant  string
	app           string // the bot's application ID
	recv          *Receiver
	api           *connector
	graph         *graphClient
	convs         ConversationStore
	names         *ttlCache[string, account]                // the accounts that roster reads found (namesTTL, namesMax)
	misses        *ttlCache[missKey, struct{}]              // the mentions that could not be named (namesMissTTL, namesMissMax)
	cards         *buttonCards                              // the cards of buttons posted under a text (buttons.go)
	buttonKey     []byte                                    // the key of the buttons' MACs (buttonKey); never logged
	buttonsWait   func(context.Context, time.Duration) bool // the wait before a post of buttons is tried again (waitFor); a test shortens it
	rosterTimeout time.Duration                             // rosterTimeout; a test shortens it
	pressWait     time.Duration                             // pressWait; a test shortens it
	threadTimeout time.Duration                             // threadTimeout; a test shortens it
	log           *slog.Logger
	onConnection  func(bool)
	posted        *recent // the posts this Client made, by messageKey, within the dedup's window: Graph's notification of one is not read
	groupMu       sync.Mutex
	groups        map[string]string // by channel: the team's Microsoft Entra group that the Graph intake read for it in this run; guarded by groupMu
	sockets       *sockets          // the connection's sockets in socket mode, nil in http mode
	push          *push             // the Graph intake of a connection with a notificationURL, nil without one
	poll          *poller           // the Graph intake of a connection with no notificationURL, nil with one
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
	key, err := buttonKey(cfg.ClientSecret.Reveal())
	if err != nil {
		return nil, err
	}
	direct := noRedirects(client)
	tokenURL := cmp.Or(cfg.TokenURL, DefaultTokenURL(cfg.TenantID))
	tokens := newTokenSource(tokenURL, cfg.AppID, botFrameworkScope, cfg.ClientSecret, direct, now)
	graph, err := newGraphClient(cmp.Or(cfg.GraphURL, DefaultGraphURL), direct,
		newTokenSource(tokenURL, cfg.AppID, graphScope, cfg.ClientSecret, direct, now), now)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: cfg.Connection, tenant: cfg.TenantID, app: cfg.AppID, convs: convs, log: log, onConnection: cfg.OnConnection,
		api: &connector{client: direct, tokens: tokens, now: now}, graph: graph, names: newTTLCache[string, account](now, namesTTL, namesMax),
		misses: newTTLCache[missKey, struct{}](now, namesMissTTL, namesMissMax), cards: newButtonCards(), rosterTimeout: rosterTimeout,
		pressWait: pressWait, threadTimeout: threadTimeout, buttonKey: key, buttonsWait: waitFor, posted: newRecent(now),
		groups: map[string]string{}}
	recv, err := NewReceiver(ReceiverConfig{Connection: cfg.Connection, TenantID: cfg.TenantID, AppID: cfg.AppID,
		MetadataURL: cfg.MetadataURL, HTTPClient: client, Conversations: convs, Logger: log, Now: now, Invokes: c})
	if err != nil {
		return nil, err
	}
	c.recv = recv
	recv.onGroup = c.groupKnown
	if cfg.NotificationURL == "" {
		c.poll = newPoller(c)
		onGap := cfg.OnSocketGap
		cfg.OnSocketGap = func(g Gap) {
			if onGap != nil {
				onGap(g)
			}
			c.poll.gap(g)
		}
	}
	if cfg.Socket {
		c.sockets = newSockets(cfg.Connection, recv, tokens, direct, cfg, log)
	}
	if cfg.NotificationURL != "" {
		state, err := clientStateOf(cfg.ClientSecret.Reveal(), cfg.Connection)
		if err != nil {
			return nil, err
		}
		c.push = &push{c: c, url: cfg.NotificationURL, state: state, onMissed: cfg.OnNotificationsMissed, fetchTimeout: fetchTimeout,
			fetchRetry: fetchRetry, fetchRetryMax: fetchRetryMax}
		c.push.subs = newSubscriber(c, cfg.NotificationURL, state)
	}
	return c, nil
}

// groupKnown is told that the receiver recorded the Microsoft Entra group of channel's team, which the Graph intake needs to read the
// channel: the subscriptions are reconciled at once, or the poller reads the channel at its next round, if it was waiting for it.
func (c *Client) groupKnown(channel string) {
	if c.push != nil {
		c.push.subs.poke()
	}
	if c.poll != nil {
		c.poll.groupKnown(channel)
	}
}

// Run takes the activities that Teams sends the connection and hands them to sink until ctx ends (Receiver.Serve). Serve starts
// first, so that what Teams sends while the bot's token is fetched is taken and counted; then Run gets the token, so that a client
// secret, app or tenant that Microsoft Entra refuses ends Run, with Serve, and an error that Classify says is FatalAuth. A fetch that
// fails otherwise is logged, and Run goes on, since the token is fetched again when a post needs it. In http mode OnConnection is told
// up once the token was asked for, and down when Serve has returned.
//
// In socket mode Run then runs the connection's sockets until ctx ends (sockets.run), and returns sooner with the error that ends them:
// the bot's credentials rejected (FatalAuth), or every region refusing the socket (an error Classify says may pass). It closes the
// sockets before it returns, so a run that ends, as the leader's does when it stops leading, leaves none open.
//
// With a NotificationURL, Run keeps the subscriptions of the channels that sink names (chat.ChannelLister; a sink that names none has
// none made) while it runs, and stops keeping them before it returns. It leaves them in Graph, for the next leader to adopt. With none,
// Run polls the messages of those channels from Graph while it runs (poller.run), and what it read is lost when it returns.
func (c *Client) Run(ctx context.Context, sink chat.Sink) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lister, _ := sink.(chat.ChannelLister)
	if c.push != nil || c.poll != nil {
		kept := make(chan struct{})
		go func() {
			defer close(kept)
			if c.push != nil {
				c.push.subs.run(ctx, lister)
				return
			}
			c.poll.run(ctx, lister)
		}()
		defer func() {
			cancel()
			<-kept
		}()
	}
	served := make(chan error, 1)
	go func() { served <- c.recv.serve(ctx, sink, c.sockets == nil) }()
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
	if c.sockets != nil {
		err := c.sockets.run(ctx)
		cancel()
		_ = <-served // the receiver drains what it took; its error says only that it was cancelled
		return err
	}
	if c.onConnection != nil {
		c.onConnection(true)
		defer c.onConnection(false)
	}
	return <-served
}

// HTTPHandler implements chat.HTTPReceiver: the handler of the connection's route. In http mode it is the Receiver, which takes the
// activities, and with a NotificationURL Graph's requests too (httpsRoute). In socket mode the activities come on the sockets, so there
// is a handler only with a NotificationURL, which takes Graph's requests alone (graphRoute).
func (c *Client) HTTPHandler() (http.Handler, bool) {
	switch {
	case c.push != nil && c.sockets != nil:
		return graphRoute{c.push}, true
	case c.push != nil:
		return httpsRoute{p: c.push, recv: c.recv}, true
	case c.sockets != nil:
		return nil, false
	}
	return c.recv, true
}

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
// thread replies in it. It carries tag where people do not see it and Graph gives it back (withCarrier), which Find reads. A mention
// (chat.Mention) names its person by the name that the channel's roster gives (mentionNames), and as someone when it cannot be had.
//
// A message with text and buttons is two posts (buttons.go): the text, which is the message Post returns, and then its buttons, as a
// card of their own in the text's thread. A post of the buttons that fails is tried once more after a short wait (buttonsAfterPost);
// one that fails then is logged at WARN and counted as PostButtons, and Post returns the text with no error: an error would have the
// outbox look the post up, adopt the text and post nothing more, while the next update of the message that has the buttons posts
// them, and a message that is never updated keeps its text without them. A crash between the two posts leaves the text alone, which
// the outbox's lookup adopts, and the buttons are posted only if the message is updated with them after.
func (c *Client) Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (ref chat.Ref, err error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.Post")
	defer func() { c.finish(ctx, op, "Post", err) }()
	if t.Channel == "" {
		return chat.Ref{}, fmt.Errorf("%w: a post with no channel", chat.ErrInvalid)
	}
	if err := o.Validate(); err != nil { // before mentionNames, so that what cannot be posted reads no roster
		return chat.Ref{}, err
	}
	m, err := renderWith(o, c.mentionNames(ctx, t.Channel, o))
	if err != nil {
		return chat.Ref{}, err
	}
	serviceURL := c.serviceURL(ctx, t.Channel)
	id, err := c.api.send(ctx, serviceURL, conversation(t.Channel, t.Thread), withCarrier(m.activity, carrierOf(tag)))
	if err != nil {
		return chat.Ref{}, err
	}
	c.sent(t.Channel, id)
	ref = c.ref(t.Channel, t.Thread, id)
	if len(m.buttons) > 0 {
		c.buttonsAfterPost(ctx, serviceURL, ref, m.buttons)
	}
	return ref, nil
}

// Update implements chat.Platform. Content that shows nothing on Teams, no text and no card, is ErrWouldDropText: a message with
// buttons and no text keeps its card, and a prompt and its ending are a card alone. A message with text and buttons edits the text,
// and shows the buttons under it as a card of their own (buttonsUnder): posted by the first update that has them, edited when they
// change, and left as they are otherwise. The update fails when they cannot be shown, after the text was edited, so that the caller's
// next try, which edits the text to the same content, posts them; one whose card Teams took and named no ID is done, and so is one
// whose card was gone (404) and that Teams refused for good to post anew (a failure for this try only fails the update). The ending of a
// prompt names its decider, and a message each person it mentions, a Microsoft Entra object ID, by the name that the channel's roster
// gives (mentionNames), and as someone when it cannot be had.
func (c *Client) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) (err error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.Update")
	defer func() { c.finish(ctx, op, "Update", err) }()
	if r.Channel == "" || r.ID == "" {
		return fmt.Errorf("%w: an update with no channel or message", chat.ErrInvalid)
	}
	if err := o.Validate(); err != nil { // before mentionNames, so that what cannot be shown reads no roster
		return err
	}
	m, err := renderWith(o, c.mentionNames(ctx, r.Channel, o))
	if errors.Is(err, errEmpty) {
		return fmt.Errorf("teams: %w: %w", chat.ErrWouldDropText, errEmpty)
	}
	if err != nil {
		return err
	}
	serviceURL := c.serviceURL(ctx, r.Channel)
	if err := c.api.update(ctx, serviceURL, conversation(r.Channel, cmp.Or(r.Thread, r.ID)), r.ID, m.activity); err != nil {
		return err
	}
	if len(m.buttons) == 0 {
		return nil
	}
	if !nameableText(r.ID) {
		c.log.WarnContext(ctx, "teams: a message's buttons were not posted: its text's ID cannot be named on a card", "channel", r.Channel)
		return nil
	}
	err = c.buttonsUnder(ctx, serviceURL, r, m.buttons)
	switch {
	case errors.Is(err, errNoID):
		c.log.WarnContext(ctx, "teams: Teams named no ID for a message's buttons; they are shown, and other buttons would be posted anew",
			"channel", r.Channel, "message", r.ID)
		return nil
	case errors.Is(err, errCardGone): // refused for good: a retry would be refused too, and the text is edited
		c.log.WarnContext(ctx, "teams: the card of a message's buttons is gone, and Teams refused to post it anew; the text stands",
			"channel", r.Channel, "message", r.ID, "error", err.Error())
		return nil
	}
	return err
}

// sent remembers that this Client posted the message id in channel, so that Graph's notification of it is counted own and not read
// (push.intake). The window is the dedup's: a notification comes seconds after the post.
func (c *Client) sent(channel, id string) {
	if id != "" {
		c.posted.claim(messageKey(channel, id))
	}
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
// chat.ErrMissingScope never given. Whether the list leaves out an archived channel, or a channel of an archived team, is not known
// (Microsoft's documentation does not say, and it was not measured), so an archived channel reads as a member's, or as not found.
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

// Fits implements chat.Platform: a prompt fits if its card renders within the budget of a message, with no tool whose name or
// arguments are over their cap (chat.ErrTooLarge).
func (c *Client) Fits(p chat.Prompt) error {
	_, err := render(chat.Ask(p))
	return err
}

// Capabilities implements chat.Platform.
func (c *Client) Capabilities() chat.Caps { return capabilities() }

// Permalink implements chat.Permalinker with no call, as the interface asks: a link needs the team's Microsoft Entra group, which it takes
// from what the receiver recorded of the channel in this run, or else from what the Graph intake read for it in this run (knowGroup): the
// store's record, or the group that an adopted subscription names. So a bound channel that no activity has come from in this run has a
// link once the intake has polled or subscribed it, and another channel has none until an activity of it comes.
func (c *Client) Permalink(r chat.Ref) (string, bool) {
	if r.Platform != PlatformName || r.Conn != c.conn || r.Channel == "" || r.ID == "" {
		return "", false
	}
	c.recv.recMu.Lock()
	group := c.recv.recorded[r.Channel].AADGroupID
	c.recv.recMu.Unlock()
	if group == "" {
		c.groupMu.Lock()
		group = c.groups[r.Channel]
		c.groupMu.Unlock()
	}
	if group == "" {
		return "", false
	}
	q := url.Values{"tenantId": {c.tenant}, "groupId": {group}, "parentMessageId": {cmp.Or(r.Thread, r.ID)}}
	return "https://teams.microsoft.com/l/message/" + url.PathEscape(r.Channel) + "/" + url.PathEscape(r.ID) + "?" + q.Encode(), true
}

// knowGroup keeps group as the Microsoft Entra group of channel's team, for Permalink: the group of a channel's stored record, which
// teamGroup reads for a poll, a subscription, a Find or a read of a thread (bound or not), and the group of a subscription that the
// Graph intake made or adopted. It keeps one for each such channel in the run: at most the channels that the store has records of, and
// the channels of the app's subscriptions.
func (c *Client) knowGroup(channel, group string) {
	if channel == "" || group == "" {
		return
	}
	c.groupMu.Lock()
	c.groups[channel] = group
	c.groupMu.Unlock()
}

// MarkBusy implements chat.BusyMarker: Teams shows no busy mark in a channel.
func (c *Client) MarkBusy(context.Context, chat.Target, bool) error { return chat.ErrNoBusy }

var (
	_ chat.Platform     = (*Client)(nil)
	_ chat.HTTPReceiver = (*Client)(nil)
	_ chat.HTTPDeadline = (*Receiver)(nil)
)

// Package slack is Agent Kourier's Slack adapter. This part takes what Slack delivers, over Socket Mode or as HTTP requests,
// decodes message events and hands those that may be turns to a Sink (spec 6.1, 8.2). Behavior the
// fake Slack guesses carries the tag UNCONFIRMED(WP-04).
package slack

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/lanes"
	"github.com/agentkourier/agent-kourier/internal/telemetry"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/agentkourier/agent-kourier/internal/secret"
)

// Config configures a Client.
type Config struct {
	// BotToken (xoxb-) calls the Web API; AppToken (xapp-) opens the Socket Mode connection.
	BotToken, AppToken secret.Value
	// SigningSecret, if set, puts the Client in HTTP mode: Slack sends its events and presses as requests signed with it, which the
	// handler of HTTPHandler takes, and Run opens no Socket Mode connection, so AppToken is not used. Empty is Socket Mode.
	SigningSecret secret.Value
	// Connection is the ChatConnection ("namespace/name") the client belongs to: the value of the connection label of the series it
	// records (agentkourier_chat_api_calls_total, agentkourier_slack_events_shed_total, agentkourier_slack_ack_duration_seconds,
	// agentkourier_slack_http_requests_total and agentkourier_slack_http_retries_total), which is how a broker that serves several
	// connections tells their adapters apart. Empty for a client that belongs to no connection, as in the dry run.
	Connection string
	// APIURL is the Web API base URL with a trailing slash; Slack's by default. A test sets the fake's.
	APIURL string
	Logger *slog.Logger // slog.Default() if nil
	// HTTPClient makes the Web API calls; slack-go's default if nil.
	HTTPClient *http.Client
	// Now is the clock of the user cache, of the pace of the log line for shed events, and of the age of a signed request in HTTP
	// mode; time.Now if nil.
	Now func() time.Time
	// OnConnection, if set, is told true when the transport is up and false when it is down again, so the broker can show it as a
	// metric. In Socket Mode the transport is up once Slack has said hello, and down from when the next connection attempt begins, or
	// Run ends. In HTTP mode it is up while Run hands requests to the sink: from when auth.test has passed until Run ends. It is
	// called from the goroutine of Run, in order, and only when the state changes; it must not block.
	OnConnection func(connected bool)
	// UserTTL is how long a resolved user is kept (default 1 hour), and UserCacheSize how many
	// users are (default 1000).
	UserTTL       time.Duration
	UserCacheSize int
	// UserTimeout bounds one users.info call (default 5 seconds): the caller's context may have
	// no deadline, and an http.Client has none either.
	UserTimeout time.Duration
	// PageLimit is how many messages Find asks for per page (default 200), and how many members
	// IsChannelMember does (default 1000, Slack's largest). A test sets it small. It also sets how much of a thread
	// ThreadBetween reads: five pages of it, and a window of more is chat.ErrThreadTooLong.
	PageLimit int
	// AuthzTimeout bounds each IsChannelMember, each ChannelAccess and each IsBot call, and each IsInAnyUserGroup and GroupsReadable call as a whole, every group of it (default 2 seconds, which a zero or negative value also means). A caller off
	// Slack's 3 second acknowledgement path may allow more.
	AuthzTimeout time.Duration
}

// Client is Agent Kourier's connection to one Slack workspace. It is safe for concurrent use.
type Client struct {
	api      *slackgo.Client
	log      *slog.Logger
	conn     string       // Config.Connection
	bot, app secret.Value // kept as Values: they are revealed only to scrub them from an error
	signing  secret.Value // Config.SigningSecret: revealed only to verify a request, and never sent anywhere
	now      func() time.Time
	ttl      time.Duration
	timeout  time.Duration
	max      int
	pageSize int
	// membersPage is IsChannelMember's page size, and authz the bound of it, of IsBot, of ChannelAccess, of IsInAnyUserGroup and of GroupsReadable.
	membersPage int
	authz       time.Duration
	onConn      func(connected bool) // Config.OnConnection
	// ackWait is how long the handler of a request in HTTP mode waits for the sink (httpAckWait; a test sets it).
	ackWait time.Duration

	mu    sync.Mutex
	users map[string]cachedUser // guarded by mu
	team  string                // the workspace ID, from auth.test; guarded by mu
	url   string                // the workspace's origin, from auth.test, once Run has asked; guarded by mu

	// lanes is the lanes of the Run that is going, for a test to read what they hold (PendingEvents in export_test.go).
	lanes atomic.Pointer[lanes.Lanes]
	// http is the latest Run in HTTP mode, which the handler of HTTPHandler hands requests to while it is open; nil before the first.
	http atomic.Pointer[httpRun]

	// ownNamed makes the warning that a trigger names Agent Kourier's own bot ID one, however many of its posts arrive.
	ownNamed sync.Once
}

// positiveOr is d if it is positive, else def: a zero or a negative bound would expire a call at once.
func positiveOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// New returns a Client over cfg. It makes no request: a missing or wrong token fails Run.
func New(cfg Config) *Client {
	opts := []slackgo.Option{slackgo.OptionAppLevelToken(cfg.AppToken.Reveal())}
	if cfg.APIURL != "" {
		opts = append(opts, slackgo.OptionAPIURL(cfg.APIURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, slackgo.OptionHTTPClient(cfg.HTTPClient))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Client{
		api: slackgo.New(cfg.BotToken.Reveal(), opts...), log: cmp.Or(cfg.Logger, slog.Default()), conn: cfg.Connection, bot: cfg.BotToken, app: cfg.AppToken,
		signing: cfg.SigningSecret,
		now:     cfg.Now, ttl: cmp.Or(cfg.UserTTL, time.Hour), timeout: cmp.Or(cfg.UserTimeout, 5*time.Second), max: cmp.Or(cfg.UserCacheSize, 1000),
		users: map[string]cachedUser{}, pageSize: cmp.Or(cfg.PageLimit, 200),
		membersPage: cmp.Or(cfg.PageLimit, membersPage), authz: positiveOr(cfg.AuthzTimeout, authzTimeout),
		onConn: cfg.OnConnection, ackWait: httpAckWait,
	}
}

// overHTTP says whether the Client is in HTTP mode: it has a signing secret.
func (c *Client) overHTTP() bool { return !c.signing.IsEmpty() }

// Run gives sink each message that may be a turn and each button press that Slack delivers, until ctx ends (the error wraps ctx's) or
// the transport cannot be kept. It first asks auth.test who the bot is, and fails if Slack will not say. Then it takes what Slack sends
// over the connection's transport: a Socket Mode connection that it opens (runSocket), or, in HTTP mode, the requests that Slack sends to
// the handler of HTTPHandler, which hands them to this Run until it returns (serveHTTP). Run tells Config.OnConnection whether the
// transport is up.
//
// What Slack sends is handled alike on either transport. The events of one channel reach the sink one at a time and in the order they
// arrived, and the events of different channels at the same time, so a sink that is slow for one channel (a Binding whose regexes are
// costly to try) holds up that channel's events, and not the reading of the others. It does not keep the CPU for the others: with many
// busy channels the acknowledgments of healthy ones slow too. The adapter holds at most lanes.PerChannel events for a channel, and
// lanes.Total in all, past which a channel that already has an event held is refused another; a channel with none always gets its
// first. An event refused is not queued but left unacknowledged, which is what a sink's error does, so Slack may send it again (how often
// is Slack's), and it can be lost; one that comes back after a later event of its channel was taken is handled after it. It is counted as
// agentkourier_slack_events_shed_total and logged at WARN (shedLog). When ctx ends, the events still waiting on a lane are not given to
// the sink and stay unacknowledged, and Run returns when the ones in flight have.
//
// A bot's message is given to the sink only if the sink is a BotFilter that takes the bot in the message's
// channel, and Agent Kourier's own posts never are (decode, takes).
//
// An event or a press is acknowledged only after the sink returns nil, so a message the sink could not take is sent again by Slack; a
// press probably is not (UNCONFIRMED(WP-04)), and its failure is only shown to the person. What is neither a turn nor a press is
// acknowledged as soon as its channel's earlier events are done. Slash commands are not handled here. A sink is called from several
// goroutines, one for each channel with events to handle, so it must be safe for concurrent use. A slow sink delays the acknowledgements
// of its channel's events: Slack expects each within 3 seconds, and sends the event again if late.
func (c *Client) Run(ctx context.Context, sink Sink) error {
	auth, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return c.scrub("auth.test call", err)
	}
	if auth.UserID == "" { // without it Agent Kourier cannot tell its own posts that arrive with no bot_id
		return errors.New("slack auth.test call: no bot user ID")
	}
	c.mu.Lock()
	c.url = workspaceOrigin(auth.URL)
	c.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	r := &runState{me: self{user: auth.UserID, bot: auth.BotID}, sink: sink, lanes: lanes.New(), shed: &shedLog{}}
	c.lanes.Store(r.lanes)
	// Events in flight see the end of Run, those waiting on a lane are dropped unhandled, and Run returns when the ones in flight have
	// returned: the next Run must not overlap the lanes of this one. Each transport has stopped submitting to the lanes when it returns.
	defer func() { cancel(); r.lanes.Wait(); c.flushShed(r.shed) }()
	if c.overHTTP() {
		return c.serveHTTP(ctx, &httpRun{runState: r, ctx: ctx})
	}
	return c.runSocket(ctx, cancel, r)
}

// runState is what one Run holds while it takes events, whichever the transport: who the bot is, the sink, and the lanes the events
// wait on.
type runState struct {
	me    self
	sink  Sink
	lanes *lanes.Lanes
	shed  *shedLog
}

// delivery is one thing Slack sent, as the shared core takes it: an Events API event or a button press, or neither. kind is its
// envelope type in Socket Mode's words ("events_api", "interactive"), which the log line of a shed event names, whatever the transport.
type delivery struct {
	kind  string
	event *slackevents.EventsAPIEvent
	press *slackgo.InteractionCallback
}

// channel is the channel the delivery is about, which is the lane it is handled on: the message's, or the one the pressed button is
// in. A delivery that decodes to nothing has none, and shares the lane of the empty channel, where it is acknowledged without a call to
// the sink.
func (d delivery) channel(me self) string {
	switch {
	case d.event != nil:
		m, _ := decode(me, d.event.InnerEvent.Data)
		return m.Channel
	case d.press != nil:
		return d.press.Container.ChannelID
	}
	return ""
}

// deliver gives sink the message of the event, or the press, that d holds, and returns the sink's error, which leaves the delivery
// unacknowledged for Slack to send again. It returns nil when the sink took it, and when there was nothing to give: a message that is
// no turn, a press of no button of Agent Kourier's, or neither.
func (c *Client) deliver(ctx context.Context, me self, sink Sink, d delivery) error {
	switch {
	case d.event != nil:
		m, v := decode(me, d.event.InnerEvent.Data)
		if v == forward && m.Mentioned && m.Bot.ID != "" && fromAnotherWorkspace(*d.event) {
			v = dropped // a bot of another workspace may have posted it: a channel shared across workspaces fails closed
		}
		if c.takes(ctx, sink, m, v) {
			if err := sink.Message(ctx, m); err != nil {
				c.log.WarnContext(ctx, "slack: message not taken; left unacknowledged for Slack to send again",
					"channel", m.Channel, "ts", m.TS, "err", err)
				return err
			}
		}
	case d.press != nil:
		if i, ok := decodeInteraction(me.user, *d.press); ok {
			if err := sink.Interaction(ctx, i); err != nil {
				c.log.WarnContext(ctx, "slack: interaction not taken; left unacknowledged",
					"channel", i.Channel, "ts", i.MessageTS, "err", err)
				return err
			}
		}
	}
	return nil
}

// shedLogEvery is the pace of the WARN line for shed events: the first at once, and then one line for each interval, which
// says how many were shed since the last. An overload that sheds every event would otherwise write a line for each.
const shedLogEvery = 10 * time.Second

// shedLog paces the log line of shed events. In Socket Mode Run's loop is its only caller; in HTTP mode the handlers of requests that
// come at once are, so it has a lock.
type shedLog struct {
	mu         sync.Mutex
	last       time.Time // guarded by mu
	suppressed int       // guarded by mu
}

// shed counts an event that the lanes would not take, which is left unacknowledged, and logs it (shedLog) with where it was
// and what kind of envelope it was, and never what it said.
func (c *Client) shed(ctx context.Context, l *shedLog, reason, channel, kind string) {
	telemetry.Count(ctx, "agentkourier_slack_events_shed_total", reason, c.conn)
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := c.now(); l.last.IsZero() || now.Sub(l.last) >= shedLogEvery {
		c.log.WarnContext(ctx, "slack: an event was left unacknowledged for Slack to send again; the adapter holds as many events as it may",
			"reason", reason, "channel", channel, "type", kind, "alsoShedSinceLastLine", l.suppressed)
		l.last, l.suppressed = now, 0
		return
	}
	l.suppressed++
}

// flushShed logs the events shed since the last line, if there are any, so that the end of Run does not lose their count.
func (c *Client) flushShed(l *shedLog) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.suppressed == 0 {
		return
	}
	c.log.Warn("slack: events were left unacknowledged for Slack to send again before the connection ended; the adapter held as many events as it may",
		"alsoShedSinceLastLine", l.suppressed)
	l.suppressed = 0
}

// takes says whether the sink is given m, which decode made a v of. A person's message always is. A bot's is
// only if the sink says, for the message's channel and bot ID, that a trigger names the bot (BotFilter), and a sink
// that cannot say is given none. Agent Kourier's own post never is, even if a trigger names its bot ID, which load
// cannot know is Agent Kourier's: that misconfiguration is logged once, at WARN, the first time it would have
// mattered. The sink may route on the answer at once, so it is asked per message and nothing is kept.
func (c *Client) takes(ctx context.Context, sink Sink, m Message, v verdict) bool {
	if v == dropped {
		return false
	}
	if m.Mentioned && m.Bot.ID != "" {
		filter, filters := sink.(BotMentionFilter)
		return v != ownPost && filters && filter.AcceptsBotMention(m.Channel, m.Bot.ID)
	}
	filter, filters := sink.(BotFilter)
	named := m.Bot.ID != "" && filters && filter.AcceptsBot(m.Channel, m.Bot.ID)
	if v == ownPost {
		if named {
			c.ownNamed.Do(func() {
				c.log.WarnContext(ctx, "slack: a botMessage trigger names Agent Kourier's own bot ID; Agent Kourier's own messages are never forwarded, so it matches nothing",
					"channel", m.Channel, "bot", m.Bot.ID)
			})
		}
		return false
	}
	return m.Bot.ID == "" || named
}

// fromAnotherWorkspace says whether an event is in a channel shared with another workspace, by Slack's flag for a channel shared
// outside the organization, or was written from a team other than the one it was delivered for, which is how a channel shared inside
// an organization may show. Neither shape has been seen from Slack: this fails closed on either.
func fromAnotherWorkspace(outer slackevents.EventsAPIEvent) bool {
	if outer.IsExtSharedChannel {
		return true
	}
	e, ok := outer.InnerEvent.Data.(*slackevents.AppMentionEvent)
	if !ok {
		return false
	}
	other := func(team string) bool { return team != "" && team != outer.TeamID }
	return other(e.UserTeam) || other(e.SourceTeam)
}

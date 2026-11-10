// Package slack is Agent Kourier's Slack adapter. This part connects over Socket Mode, decodes
// message events and hands those that may be turns to a Sink (spec 6.1, 8.2). Behavior the
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

	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/agentkourier/agent-kourier/internal/secret"
)

// Config configures a Client.
type Config struct {
	// BotToken (xoxb-) calls the Web API; AppToken (xapp-) opens the Socket Mode connection.
	BotToken, AppToken secret.Value
	// Connection is the ChatConnection ("namespace/name") the client belongs to: the value of the connection label of the series it
	// records (agentkourier_chat_api_calls_total, agentkourier_slack_events_shed_total and agentkourier_slack_ack_duration_seconds), which
	// is how a broker that serves several connections tells their adapters apart. Empty for a client that belongs to no connection, as
	// in the dry run.
	Connection string
	// APIURL is the Web API base URL with a trailing slash; Slack's by default. A test sets the fake's.
	APIURL string
	Logger *slog.Logger // slog.Default() if nil
	// HTTPClient makes the Web API calls; slack-go's default if nil.
	HTTPClient *http.Client
	// Now is the clock of the user cache and of the pace of the log line for shed events; time.Now if nil.
	Now func() time.Time
	// OnConnection, if set, is told true when the Socket Mode connection is up (Slack has said hello) and false when it is
	// down again (the next connection attempt begins, or Run ends), so the broker can show it as a metric. It is
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
	now      func() time.Time
	ttl      time.Duration
	timeout  time.Duration
	max      int
	pageSize int
	// membersPage is IsChannelMember's page size, and authz the bound of it, of IsBot, of ChannelAccess, of IsInAnyUserGroup and of GroupsReadable.
	membersPage int
	authz       time.Duration
	onConn      func(connected bool) // Config.OnConnection

	mu    sync.Mutex
	users map[string]cachedUser // guarded by mu
	team  string                // the workspace ID, from auth.test; guarded by mu
	url   string                // the workspace's origin, from auth.test, once Run has asked; guarded by mu

	// lanes is the lanes of the Run that is going, for a test to read what they hold (PendingEvents in export_test.go).
	lanes atomic.Pointer[lanes]

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
		now: cfg.Now, ttl: cmp.Or(cfg.UserTTL, time.Hour), timeout: cmp.Or(cfg.UserTimeout, 5*time.Second), max: cmp.Or(cfg.UserCacheSize, 1000),
		users: map[string]cachedUser{}, pageSize: cmp.Or(cfg.PageLimit, 200),
		membersPage: cmp.Or(cfg.PageLimit, membersPage), authz: positiveOr(cfg.AuthzTimeout, authzTimeout),
		onConn: cfg.OnConnection,
	}
}

// Run connects over Socket Mode and gives sink each message that may be a turn and each
// button press, until ctx ends (the error wraps ctx's) or the connection cannot be kept. Slack's refresh, which it asks
// for with a disconnect, and the first attempt to connect again after a connection is lost, slack-go does by itself, at
// once. When that attempt, or any attempt to open the connection, fails, Run returns it as an error (watch.lost) and
// the caller connects again with its own pause: slack-go would otherwise retry inside Run without end, with a pause that
// starts at 100 ms, doubles to 5 minutes and has no jitter. A rejected token is such an error, and Platform.Classify says
// it is fatal. Run logs each step (watch) and tells Config.OnConnection whether the connection is up; the connection is down
// from the start of the attempt to connect again, whether or not that is the one that fails, until the hello of the next Run.
//
// A lost connection ends Run as ctx ending does: the sink calls in flight see their context end, and the events queued on the lanes
// are dropped, all unacknowledged, which Slack then sends again on the next connection (how often is Slack's). The intake
// deduplicates a redelivery only of an event whose first call finished (internal/chat/intake.go, seen), so one cut off in flight
// is decided afresh, and one that had finished is not decided twice.
//
// The events of one channel reach the sink one at a time and in the order they arrived, and the events of different
// channels at the same time, so a sink that is slow for one channel (a Binding whose regexes are costly to try) holds up that
// channel's events, and not the reading of the connection. It does not keep the CPU for the others: with many busy channels
// the acknowledgments of healthy ones slow too. The adapter holds at most maxChannelBacklog events for a channel, and
// maxPendingEvents in all, past which a channel that already has an event held is refused another; a channel with none always
// gets its first. An event refused is not queued but left unacknowledged, which is what a sink's error does, so Slack may send
// it again (how often is Slack's), and it can be lost; one that comes back after a later event of its channel was taken is
// handled after it. It is counted as agentkourier_slack_events_shed_total and logged at WARN (shedLog). When ctx ends, the
// events still waiting on a lane are not given to the sink and stay unacknowledged, and Run returns when the ones in flight have.
//
// A bot's message is given to the sink only if the sink is a BotFilter that takes the bot in the message's
// channel, and Agent Kourier's own posts never are (decode, takes).
//
// An events_api or interactive envelope is acknowledged only after the sink returns nil, so
// a message the sink could not take is sent again by Slack; a press probably is not
// (UNCONFIRMED(WP-04)), and its failure is only shown to the person. Envelopes that are neither
// a turn nor a press are acknowledged as soon as their channel's earlier events are done. Other envelope kinds (slash commands) are not handled here.
// A sink is called from several goroutines, one for each channel with events to handle, so it must be safe for concurrent
// use. A slow sink delays the acknowledgements of its channel's events: Slack expects each within 3 seconds, and sends the event again if late.
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
	lanes, shed := newLanes(maxChannelBacklog, maxPendingEvents), &shedLog{}
	c.lanes.Store(lanes)
	// Events in flight see the end of Run, those waiting on a lane are dropped unhandled, and Run returns when the ones in flight
	// have returned: the next Run must not overlap the lanes of this one.
	defer func() { cancel(); lanes.wait(); c.flushShed(shed) }()
	smc := socketmode.New(c.api)
	done := make(chan error, 1)
	go func() { done <- smc.RunContext(ctx) }()
	w := &watch{c: c}
	defer w.close()
	me := self{user: auth.UserID, bot: auth.BotID}
	for {
		select {
		case ev := <-smc.Events: // every type is drained, or slack-go's reader stalls
			w.event(ctx, ev)
			if w.lost != nil {
				// slack-go's run was about to wait and try again: end it and wait for it, so that no goroutine outlives Run.
				cancel()
				_ = <-done
				return w.lost
			}
			if ev.Request == nil {
				continue
			}
			channel := eventChannel(me, ev)
			received := time.Now()
			if reason := lanes.submit(channel, func() { c.handle(ctx, smc, me, sink, ev, received) }); reason != "" {
				c.shed(ctx, shed, reason, channel, ev)
			}
		case err := <-done:
			return c.scrub("socket mode", err)
		}
	}
}

// eventChannel is the channel an event is about, which is the lane it is handled on: the message's, or the one the pressed
// button is in. An event that decodes to nothing has none, and shares the lane of the empty channel, where it is
// acknowledged without a call to the sink.
func eventChannel(me self, ev socketmode.Event) string {
	switch ev.Type {
	case socketmode.EventTypeEventsAPI:
		if outer, ok := ev.Data.(slackevents.EventsAPIEvent); ok {
			m, _ := decode(me, outer.InnerEvent.Data)
			return m.Channel
		}
	case socketmode.EventTypeInteractive:
		if cb, ok := ev.Data.(slackgo.InteractionCallback); ok {
			return cb.Container.ChannelID
		}
	}
	return ""
}

// shedLogEvery is the pace of the WARN line for shed events: the first at once, and then one line for each interval, which
// says how many were shed since the last. An overload that sheds every event would otherwise write a line for each.
const shedLogEvery = 10 * time.Second

// shedLog paces the log line of shed events. Run's loop is its only caller.
type shedLog struct {
	last       time.Time
	suppressed int
}

// shed counts an event that the lanes would not take, which is left unacknowledged, and logs it (shedLog) with where it was
// and what kind of envelope it was, and never what it said.
func (c *Client) shed(ctx context.Context, l *shedLog, reason, channel string, ev socketmode.Event) {
	telemetry.Count(ctx, "agentkourier_slack_events_shed_total", reason, c.conn)
	if now := c.now(); l.last.IsZero() || now.Sub(l.last) >= shedLogEvery {
		c.log.WarnContext(ctx, "slack: an event was left unacknowledged for Slack to send again; the adapter holds as many events as it may",
			"reason", reason, "channel", channel, "type", ev.Type, "alsoShedSinceLastLine", l.suppressed)
		l.last, l.suppressed = now, 0
		return
	}
	l.suppressed++
}

// flushShed logs the events shed since the last line, if there are any, so that the end of Run does not lose their count.
func (c *Client) flushShed(l *shedLog) {
	if l.suppressed == 0 {
		return
	}
	c.log.Warn("slack: events were left unacknowledged for Slack to send again before the connection ended; the adapter held as many events as it may",
		"alsoShedSinceLastLine", l.suppressed)
	l.suppressed = 0
}

// handle gives sink the message of an events_api event, or the press of an interactive one,
// then acknowledges the event. received is when Run took the event off the connection: the time the ack duration counts
// from, which is Slack's clock, so a wait on the lane of the event's channel is part of it.
func (c *Client) handle(ctx context.Context, smc *socketmode.Client, me self, sink Sink, ev socketmode.Event, received time.Time) {
	if ev.Request == nil || ctx.Err() != nil {
		// Run has ended: an event that waited on a lane is not given to the sink, which may pay a costly match for it before
		// it looks at ctx. It stays unacknowledged, as a shed one does, for the next Run.
		return
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.slack.envelope", attribute.String("agentkourier.platform", "slack"))
	outcome := "withheld"
	defer func() {
		telemetry.Observe(ctx, "agentkourier_slack_ack_duration_seconds", time.Since(received), outcome, c.conn)
		op.FinishOutcome(nil, outcome)
	}()
	switch ev.Type {
	case socketmode.EventTypeEventsAPI:
		if outer, ok := ev.Data.(slackevents.EventsAPIEvent); ok {
			m, v := decode(me, outer.InnerEvent.Data)
			if v == forward && m.Mentioned && m.Bot.ID != "" && fromAnotherWorkspace(outer) {
				v = dropped // a bot of another workspace may have posted it: a channel shared across workspaces fails closed
			}
			if c.takes(ctx, sink, m, v) {
				if err := sink.Message(ctx, m); err != nil {
					c.log.WarnContext(ctx, "slack: message not taken; left unacknowledged for Slack to send again",
						"channel", m.Channel, "ts", m.TS, "err", err)
					return
				}
			}
		}
	case socketmode.EventTypeInteractive:
		if cb, ok := ev.Data.(slackgo.InteractionCallback); ok {
			if i, ok := decodeInteraction(me.user, cb); ok {
				if err := sink.Interaction(ctx, i); err != nil {
					c.log.WarnContext(ctx, "slack: interaction not taken; left unacknowledged",
						"channel", i.Channel, "ts", i.MessageTS, "err", err)
					return
				}
			}
		}
	default:
		return
	}
	// Bounded: slack-go queues the ack for a writer that is gone after a disconnect, and a full queue would block here.
	ackCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	outcome = "acknowledged"
	if err := smc.AckCtx(ackCtx, ev.Request.EnvelopeID, nil); err != nil {
		outcome = "failed"
		c.log.WarnContext(ctx, "slack: acknowledging an event failed", "err", err)
	}
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

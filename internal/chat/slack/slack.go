// Package slack is courier's Slack adapter. This part connects over Socket Mode, decodes
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
	// APIURL is the Web API base URL with a trailing slash; Slack's by default. A test sets the fake's.
	APIURL string
	Logger *slog.Logger // slog.Default() if nil
	// HTTPClient makes the Web API calls; slack-go's default if nil.
	HTTPClient *http.Client
	// Now is the clock of the user cache and of the pace of repeated connection errors in the log; time.Now if nil.
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
	// IsChannelMember does (default 1000, Slack's largest). A test sets it small.
	PageLimit int
	// AuthzTimeout bounds each IsChannelMember and each IsBot call (default 2 seconds, which a zero or negative value also means). A caller off
	// Slack's 3 second acknowledgement path may allow more.
	AuthzTimeout time.Duration
}

// Client is courier's connection to one Slack workspace. It is safe for concurrent use.
type Client struct {
	api      *slackgo.Client
	log      *slog.Logger
	bot, app secret.Value // kept as Values: they are revealed only to scrub them from an error
	now      func() time.Time
	ttl      time.Duration
	timeout  time.Duration
	max      int
	pageSize int
	// membersPage and authz are IsChannelMember's page size and the bound of it and IsBot.
	membersPage int
	authz       time.Duration
	onConn      func(connected bool) // Config.OnConnection

	mu    sync.Mutex
	users map[string]cachedUser // guarded by mu
	team  string                // the workspace ID, from auth.test; guarded by mu

	// ownNamed makes the warning that a trigger names courier's own bot ID one, however many of its posts arrive.
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
		api: slackgo.New(cfg.BotToken.Reveal(), opts...), log: cmp.Or(cfg.Logger, slog.Default()), bot: cfg.BotToken, app: cfg.AppToken,
		now: cfg.Now, ttl: cmp.Or(cfg.UserTTL, time.Hour), timeout: cmp.Or(cfg.UserTimeout, 5*time.Second), max: cmp.Or(cfg.UserCacheSize, 1000),
		users: map[string]cachedUser{}, pageSize: cmp.Or(cfg.PageLimit, 200),
		membersPage: cmp.Or(cfg.PageLimit, membersPage), authz: positiveOr(cfg.AuthzTimeout, authzTimeout),
		onConn: cfg.OnConnection,
	}
}

// Run connects over Socket Mode and gives sink each message that may be a turn and each
// button press, one at a time and in order, until ctx ends (the error wraps ctx's) or the connection fails for
// good, as with a rejected token. slack-go reconnects by itself otherwise, and Run logs each step of that (watch)
// and tells Config.OnConnection whether the connection is up.
//
// A bot's message is given to the sink only if the sink is a BotFilter that takes the bot in the message's
// channel, and courier's own posts never are (decode, takes).
//
// An events_api or interactive envelope is acknowledged only after the sink returns nil, so
// a message the sink could not take is sent again by Slack; a press probably is not
// (UNCONFIRMED(WP-04)), and its failure is only shown to the person. Envelopes that are neither
// a turn nor a press are acknowledged as soon as they are decoded. Other envelope kinds (slash commands) are not handled here.
// Run calls the sink itself, between reads of the connection, so a slow sink delays the
// acknowledgements: Slack expects each within 3 seconds, and sends the event again if late.
func (c *Client) Run(ctx context.Context, sink Sink) error {
	auth, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return c.scrub("auth.test", err)
	}
	if auth.UserID == "" { // without it courier cannot tell its own posts that arrive with no bot_id
		return errors.New("slack auth.test: no bot user ID")
	}
	smc := socketmode.New(c.api)
	done := make(chan error, 1)
	go func() { done <- smc.RunContext(ctx) }()
	w := &watch{c: c}
	defer w.close()
	for {
		select {
		case ev := <-smc.Events: // every type is drained, or slack-go's reader stalls
			w.event(ctx, ev)
			c.handle(ctx, smc, self{user: auth.UserID, bot: auth.BotID}, sink, ev)
		case err := <-done:
			return c.scrub("socket mode", err)
		}
	}
}

// handle gives sink the message of an events_api event, or the press of an interactive one,
// then acknowledges the event.
func (c *Client) handle(ctx context.Context, smc *socketmode.Client, me self, sink Sink, ev socketmode.Event) {
	if ev.Request == nil {
		return
	}
	started := time.Now()
	ctx, op := telemetry.Start(ctx, "courier.slack.envelope", attribute.String("courier.platform", "slack"))
	outcome := "withheld"
	defer func() {
		telemetry.Observe(ctx, "courier_slack_ack_duration_seconds", time.Since(started), outcome)
		op.FinishOutcome(nil, outcome)
	}()
	switch ev.Type {
	case socketmode.EventTypeEventsAPI:
		if outer, ok := ev.Data.(slackevents.EventsAPIEvent); ok {
			if m, v := decode(me, outer.InnerEvent.Data); c.takes(ctx, sink, m, v) {
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
// that cannot say is given none. Courier's own post never is, even if a trigger names its bot ID, which load
// cannot know is courier's: that misconfiguration is logged once, at WARN, the first time it would have
// mattered. The sink may route on the answer at once, so it is asked per message and nothing is kept.
func (c *Client) takes(ctx context.Context, sink Sink, m Message, v verdict) bool {
	if v == dropped {
		return false
	}
	filter, filters := sink.(BotFilter)
	named := m.Bot.ID != "" && filters && filter.AcceptsBot(m.Channel, m.Bot.ID)
	if v == ownPost {
		if named {
			c.ownNamed.Do(func() {
				c.log.WarnContext(ctx, "slack: a botMessage trigger names courier's own bot ID; courier's own messages are never forwarded, so it matches nothing",
					"channel", m.Channel, "bot", m.Bot.ID)
			})
		}
		return false
	}
	return m.Bot.ID == "" || named
}

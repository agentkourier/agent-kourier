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
	// Now is the clock of the user cache; time.Now if nil.
	Now func() time.Time
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

	mu    sync.Mutex
	users map[string]cachedUser // guarded by mu
	team  string                // the workspace ID, from auth.test; guarded by mu
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
	}
}

// Run connects over Socket Mode and gives sink each message that may be a turn and each
// button press, one at a time and in order, until ctx ends (the error wraps ctx's) or the connection fails for
// good, as with a rejected token. slack-go reconnects by itself otherwise.
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
	for {
		select {
		case ev := <-smc.Events: // every type is drained, or slack-go's reader stalls
			c.handle(ctx, smc, auth.UserID, sink, ev)
		case err := <-done:
			return c.scrub("socket mode", err)
		}
	}
}

// handle gives sink the message of an events_api event, or the press of an interactive one,
// then acknowledges the event.
func (c *Client) handle(ctx context.Context, smc *socketmode.Client, botUser string, sink Sink, ev socketmode.Event) {
	if ev.Request == nil {
		return
	}
	switch ev.Type {
	case socketmode.EventTypeEventsAPI:
		if outer, ok := ev.Data.(slackevents.EventsAPIEvent); ok {
			if m, ok := decode(botUser, outer.InnerEvent.Data); ok {
				if err := sink.Message(ctx, m); err != nil {
					c.log.WarnContext(ctx, "slack: message not taken; left unacknowledged for Slack to send again",
						"channel", m.Channel, "ts", m.TS, "err", err)
					return
				}
			}
		}
	case socketmode.EventTypeInteractive:
		if cb, ok := ev.Data.(slackgo.InteractionCallback); ok {
			if i, ok := decodeInteraction(botUser, cb); ok {
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
	if err := smc.AckCtx(ackCtx, ev.Request.EnvelopeID, nil); err != nil {
		c.log.WarnContext(ctx, "slack: acknowledging an event failed", "err", err)
	}
}

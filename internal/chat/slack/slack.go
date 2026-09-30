// Package slack is courier's Slack adapter. This part connects over Socket Mode, decodes
// message events and hands those that may be turns to a Sink (spec 6.1, 8.2). Behavior the
// fake Slack guesses carries the tag UNCONFIRMED(WP-04).
package slack

import (
	"context"
	"fmt"
	"log/slog"

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
}

// Client is courier's connection to one Slack workspace. It is safe for concurrent use.
type Client struct {
	api *slackgo.Client
}

// New returns a Client over cfg. It makes no request: a missing or wrong token fails Run.
func New(cfg Config) *Client {
	opts := []slackgo.Option{slackgo.OptionAppLevelToken(cfg.AppToken.Reveal())}
	if cfg.APIURL != "" {
		opts = append(opts, slackgo.OptionAPIURL(cfg.APIURL))
	}
	return &Client{api: slackgo.New(cfg.BotToken.Reveal(), opts...)}
}

// Run connects over Socket Mode and gives sink each message that may be a turn, one at a
// time and in order, until ctx ends (the error wraps ctx's) or the connection fails for
// good, as with a rejected token. slack-go reconnects by itself otherwise.
//
// An events_api envelope is acknowledged only after the sink returns nil, so a turn the
// sink could not take is sent again by Slack; events that are not turns are acknowledged
// as soon as they are decoded. Other envelope kinds (interactions) are not handled here.
// The sink runs on the reading goroutine and must return promptly: Slack expects the
// acknowledgement within 3 seconds.
func (c *Client) Run(ctx context.Context, sink Sink) error {
	auth, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("slack auth.test: %w", err)
	}
	smc := socketmode.New(c.api)
	done := make(chan error, 1)
	go func() { done <- smc.RunContext(ctx) }()
	for {
		select {
		case ev := <-smc.Events: // every type is drained, or slack-go's reader stalls
			handle(ctx, smc, auth.UserID, sink, ev)
		case err := <-done:
			return fmt.Errorf("slack socket mode: %w", err)
		}
	}
}

// handle gives sink the message of an events_api event, then acknowledges the event.
func handle(ctx context.Context, smc *socketmode.Client, botUser string, sink Sink, ev socketmode.Event) {
	if ev.Type != socketmode.EventTypeEventsAPI || ev.Request == nil {
		return
	}
	if outer, ok := ev.Data.(slackevents.EventsAPIEvent); ok {
		if m, ok := decode(botUser, outer.InnerEvent.Data); ok {
			if err := sink.Message(ctx, m); err != nil {
				slog.WarnContext(ctx, "slack: message not taken; left unacknowledged for Slack to send again",
					"channel", m.Channel, "ts", m.TS, "err", err)
				return
			}
		}
	}
	if err := smc.AckCtx(ctx, ev.Request.EnvelopeID, nil); err != nil {
		slog.WarnContext(ctx, "slack: acknowledging an event failed", "err", err)
	}
}

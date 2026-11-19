package slack

import (
	"context"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"go.opentelemetry.io/otel/attribute"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// runSocket is Run in Socket Mode: it opens the connection and hands its envelopes to r's lanes, until ctx, Run's, ends or the connection
// cannot be kept. cancel ends ctx. Slack's refresh, which it asks for with a disconnect, and the first attempt to connect again
// after a connection is lost, slack-go does by itself, at once. When that attempt, or any attempt to open the connection, fails, runSocket
// returns it as an error (watch.lost) and the caller connects again with its own pause: slack-go would otherwise retry inside Run without
// end, with a pause that starts at 100 ms, doubles to 5 minutes and has no jitter. A rejected token is such an error, and
// Platform.Classify says it is fatal. runSocket logs each step (watch) and tells Config.OnConnection whether the connection is up; the
// connection is down from the start of the attempt to connect again, whether or not that is the one that fails, until the hello of the
// next Run.
//
// A lost connection ends Run as ctx ending does: the sink calls in flight see their context end, and the events queued on the lanes
// are dropped, all unacknowledged, which Slack then sends again on the next connection (how often is Slack's). The intake
// deduplicates a redelivery only of an event whose first call finished (internal/trigger/intake.go, seen), so one cut off in flight
// is decided afresh, and one that had finished is not decided twice.
//
// An events_api or interactive envelope is acknowledged by its envelope ID (handle). Other envelope kinds (slash commands) are not.
func (c *Client) runSocket(ctx context.Context, cancel context.CancelFunc, r *runState) error {
	smc := socketmode.New(c.api)
	done := make(chan error, 1)
	go func() { done <- smc.RunContext(ctx) }()
	w := &watch{c: c}
	defer w.close()
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
			d, _ := deliveryOf(ev)
			channel := d.channel(r.me)
			received := time.Now()
			if reason := r.lanes.Submit(channel, func() { c.handle(ctx, r, smc, ev, received) }); reason != "" {
				c.shed(ctx, r.shed, reason, channel, string(ev.Type))
			}
		case err := <-done:
			return c.scrub("socket mode", err)
		}
	}
}

// deliveryOf is the delivery of a Socket Mode envelope, and false for an envelope kind that is neither an event nor a press, which is not
// acknowledged. An event or a press whose payload slack-go could not give is a delivery of nothing, which is.
func deliveryOf(ev socketmode.Event) (delivery, bool) {
	d := delivery{kind: string(ev.Type)}
	switch ev.Type {
	case socketmode.EventTypeEventsAPI:
		if outer, ok := ev.Data.(slackevents.EventsAPIEvent); ok {
			d.event = &outer
		}
	case socketmode.EventTypeInteractive:
		if cb, ok := ev.Data.(slackgo.InteractionCallback); ok {
			d.press = &cb
		}
	default:
		return d, false
	}
	return d, true
}

// handle gives r's sink the message of an events_api event, or the press of an interactive one (deliver), then acknowledges the event.
// ctx is Run's. received is when Run took the event off the connection: the time the ack duration counts from, which is Slack's clock, so a wait on
// the lane of the event's channel is part of it.
func (c *Client) handle(ctx context.Context, r *runState, smc *socketmode.Client, ev socketmode.Event, received time.Time) {
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
	d, ok := deliveryOf(ev)
	if !ok {
		return
	}
	if err := c.deliver(ctx, r.me, r.sink, d); err != nil {
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

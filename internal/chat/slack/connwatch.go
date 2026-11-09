package slack

import (
	"cmp"
	"context"
	"errors"
	"net"
	"strings"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// ConnectedMessage is the message of the INFO line logged each time the Socket Mode connection comes up (Slack's hello).
// Scripts that wait for the connection, such as the sandbox's up.sh, look for it in the log, and a test keeps them to it.
const ConnectedMessage = "slack: socket mode connected"

// causeLimit bounds the text of an error taken from a message that could not be read. Such a cause comes from
// a JSON decoder or from slack-go's parser, which can quote a little of the message, and a message is what people
// wrote.
const causeLimit = 200

// watch follows the lifecycle events of one Run's Socket Mode connection. slack-go sends them on the same channel as
// the envelopes, and says nothing of them otherwise: no log, and no error from its run while it retries. watch logs
// them, tells Config.OnConnection when the connection comes up and goes down, and holds the failed attempt to connect
// that ends Run (lost). Only the goroutine of Run uses it.
type watch struct {
	c  *Client
	up bool

	// lost is why the connection cannot be kept, once an attempt to open it failed: the error Run returns, so that the
	// caller paces the retry. slack-go would retry without end instead, with a pause that grows to 5 minutes and no jitter.
	// It holds no token or socket URL, and keeps the Slack code and the wait that Platform.Classify reads. Nil until then.
	lost error
}

// set records whether the connection is up, and tells OnConnection if that changed.
func (w *watch) set(up bool) {
	if w.up == up {
		return
	}
	w.up = up
	if w.c.onConn != nil {
		w.c.onConn(up)
	}
}

// close ends the watch: whatever ended Run, the connection is down.
func (w *watch) close() { w.set(false) }

// event logs ev if it is a lifecycle event, or an envelope (at DEBUG, by type and ID alone), and follows the state it
// tells of. The connection is up from Slack's hello, which is when it can deliver, and down from the start of the
// next attempt: slack-go reports no loss, and after one it goes straight to connecting. A failed attempt takes it down
// and is kept as lost, which ends Run (the attempt after a loss is slack-go's, at once; a retry of it is the caller's).
// Nothing here logs an envelope's payload, which holds what people wrote, or the URL of the socket, whose ticket opens a
// connection for the app.
//
// Once ctx has ended, Run is stopping and slack-go still delivers what it was sending: the attempt that the end of
// ctx cancelled, and the error of a read on a socket it closed. They are not news, so a connecting, a connection error
// or a read error is neither logged nor followed then; close, which Run defers, takes the state down.
func (w *watch) event(ctx context.Context, ev socketmode.Event) {
	log := w.c.log
	stopping := ctx.Err() != nil
	switch ev.Type {
	case socketmode.EventTypeConnecting:
		if stopping {
			return
		}
		d, _ := ev.Data.(*slackgo.ConnectingEvent)
		if d == nil {
			d = &slackgo.ConnectingEvent{}
		}
		again := w.up
		w.set(false)
		switch {
		case again: // a refresh that Slack asked for, or a connection that broke: slack-go says which of them to nobody
			log.InfoContext(ctx, "slack: socket mode reconnecting", "connection_count", d.ConnectionCount)
		case d.ConnectionCount == 0 && d.Attempt <= 1:
			log.InfoContext(ctx, "slack: socket mode connecting")
		default:
			log.DebugContext(ctx, "slack: socket mode connecting", "connection_count", d.ConnectionCount, "attempt", d.Attempt)
		}
	case socketmode.EventTypeConnected:
		d, _ := ev.Data.(*socketmode.ConnectedEvent)
		if d == nil {
			d = &socketmode.ConnectedEvent{}
		}
		log.InfoContext(ctx, "slack: socket mode websocket opened", "connection_count", d.ConnectionCount)
	case socketmode.EventTypeHello:
		w.set(true)
		var n int
		if ev.Request != nil {
			n = ev.Request.NumConnections
		}
		// num_connections above 1 says that another process uses the same app token, and Slack spreads events over them.
		log.InfoContext(ctx, ConnectedMessage, "num_connections", n)
	case socketmode.EventTypeConnectionError:
		d, _ := ev.Data.(*slackgo.ConnectionErrorEvent)
		if d == nil {
			d = &slackgo.ConnectionErrorEvent{}
		}
		if stopping || errors.Is(d.ErrorObj, context.Canceled) { // the attempt that the end of ctx cancelled
			return
		}
		w.set(false)
		if w.lost == nil { // not logged: it is returned from Run, and whoever retries logs it
			w.lost = w.c.scrub("socket mode connection attempt failed", cmp.Or(d.ErrorObj, errors.New("no error given")))
		}
	case socketmode.EventTypeInvalidAuth:
		w.set(false)
		log.WarnContext(ctx, "slack: socket mode refused the connection request (404 from apps.connections.open); not retrying")
	case socketmode.EventTypeIncomingError:
		d, _ := ev.Data.(*slackgo.IncomingEventError)
		if d == nil {
			d = &slackgo.IncomingEventError{}
		}
		switch {
		case stopping:
		case errors.Is(d.ErrorObj, net.ErrClosed):
			// slack-go closed the socket itself, as it does for each refresh Slack asks for, and then read from it. The
			// original error is tested: the scrubbed one keeps no cause that says so.
			log.DebugContext(ctx, "slack: socket mode read ended on a closed socket", "error", w.cause(d.ErrorObj))
		default:
			log.WarnContext(ctx, "slack: socket mode read failed", "error", w.cause(d.ErrorObj))
		}
	case socketmode.EventTypeErrorBadMessage:
		d, _ := ev.Data.(*socketmode.ErrorBadMessage)
		if d == nil {
			d = &socketmode.ErrorBadMessage{}
		}
		// The message is dropped and not acknowledged, so Slack sends it again; only its size is logged.
		log.WarnContext(ctx, "slack: socket mode message could not be read; left unacknowledged",
			"error", w.cause(d.Cause), "bytes", len(d.Message))
	case socketmode.EventTypeErrorWriteFailed:
		d, _ := ev.Data.(*socketmode.ErrorWriteFailed)
		if d == nil {
			d = &socketmode.ErrorWriteFailed{}
		}
		attrs := []any{"error", w.cause(d.Cause)}
		if d.Response != nil {
			attrs = append(attrs, "envelope_id", d.Response.EnvelopeID)
		}
		log.WarnContext(ctx, "slack: socket mode write failed", attrs...)
	default:
		if ev.Request != nil {
			log.DebugContext(ctx, "slack: envelope", "type", string(ev.Type), "envelope_id", ev.Request.EnvelopeID, "retry_attempt", ev.Request.RetryAttempt)
		}
	}
}

// cause is the text of err for a log line: scrubbed of tokens and socket URLs, and no longer than causeLimit.
func (w *watch) cause(err error) string {
	if err == nil {
		err = errors.New("no error given")
	}
	s := w.c.scrub("socket mode", err).Error()
	if len(s) > causeLimit {
		s = strings.ToValidUTF8(s[:causeLimit], "") + "..."
	}
	return s
}

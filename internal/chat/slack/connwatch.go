package slack

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// ConnectedMessage is the message of the INFO line logged each time the Socket Mode connection comes up (Slack's hello).
// Scripts that wait for the connection, such as the sandbox's up.sh, look for it in the log, and a test keeps them to it.
const ConnectedMessage = "slack: socket mode connected"

const (
	// errorLogEvery is how often a failing connection attempt is logged again when it fails the same way: slack-go
	// retries without end, with a pause that grows to 5 minutes, and the first attempts come in seconds.
	errorLogEvery = time.Minute
	// causeLimit bounds the text of an error taken from a message that could not be read. Such a cause comes from
	// a JSON decoder or from slack-go's parser, which can quote a little of the message, and a message is what people
	// wrote.
	causeLimit = 200
)

// watch follows the lifecycle events of one Run's Socket Mode connection. slack-go sends them on the same channel as
// the envelopes, and says nothing of them otherwise: no log, and no error from Run while it retries. watch logs
// them, tells Config.OnConnection when the connection comes up and goes down, and collapses a retry loop's
// repeated failures. Only the goroutine of Run uses it.
type watch struct {
	c  *Client
	up bool

	// The last connection failure that was logged, when, and how many like it were left out since.
	lastErr    string
	lastLogged time.Time
	suppressed int
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
// next attempt: slack-go reports no loss, and after one it goes straight to connecting. Nothing here logs an
// envelope's payload, which holds what people wrote, or the URL of the socket, whose ticket opens a connection
// for the app.
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
		attrs := []any{"num_connections", n}
		if w.suppressed > 0 { // the outage ends here: say how many failures it left out of the log
			attrs = append(attrs, "suppressed", w.suppressed, "suppressed_error", w.lastErr)
		}
		log.InfoContext(ctx, ConnectedMessage, attrs...)
		w.lastErr, w.suppressed = "", 0 // the next failure is a new outage
	case socketmode.EventTypeConnectionError:
		d, _ := ev.Data.(*slackgo.ConnectionErrorEvent)
		if d == nil {
			d = &slackgo.ConnectionErrorEvent{}
		}
		if stopping || errors.Is(d.ErrorObj, context.Canceled) { // the attempt that the end of ctx cancelled
			return
		}
		w.set(false)
		w.connectionError(ctx, d)
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

// connectionError logs a failed connection attempt: at once if it is the first, or differs from the last one logged, and
// otherwise once every errorLogEvery. The number of failures left out goes on the line that ends the run of them, and
// names the error it counts: this line if it is the same error again, else as earlier_suppressed of earlier_error.
// (A hello ends a run too; see event.)
func (w *watch) connectionError(ctx context.Context, d *slackgo.ConnectionErrorEvent) {
	err := w.cause(d.ErrorObj)
	now := w.c.now()
	if err == w.lastErr && now.Sub(w.lastLogged) < errorLogEvery {
		w.suppressed++
		return
	}
	attrs := []any{"error", err, "attempt", d.Attempt, "retry_in", d.Backoff}
	switch {
	case w.suppressed == 0:
	case err == w.lastErr:
		attrs = append(attrs, "suppressed", w.suppressed)
	default:
		attrs = append(attrs, "earlier_error", w.lastErr, "earlier_suppressed", w.suppressed)
	}
	w.c.log.WarnContext(ctx, "slack: socket mode connection attempt failed; retrying", attrs...)
	w.lastErr, w.lastLogged, w.suppressed = err, now, 0
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

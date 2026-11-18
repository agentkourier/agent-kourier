package chat

import (
	"net/http"
	"time"
)

// HTTPReceiver is the part of a Platform that takes what the platform sends to Agent Kourier over HTTP (a webhook), in place of, or
// beside, a connection that Run opens itself. A platform that takes nothing over HTTP does not implement it.
//
// The broker serves the handler on its chat listener at POST /chat/<platform>/<namespace>/<name>, the ChatConnection's platform and
// name, for as long as the broker runs the connection on this replica: from just before it first calls Run until it stops calling it (a
// Run that fails is called again after a wait, and the route stays), and never on a replica that does not lead. Elsewhere, and at any
// other time, the listener answers 503 for the route itself, so a platform that retries finds the leader. The broker reads the body
// before the handler is called, and refuses one past its cap, so the handler is given a body that is whole and in memory (it can be read
// once, or replaced and read again), and a context with a deadline, by which it must have answered.
//
// Taking the route away stops new requests only. A request that was routed before may still be in ServeHTTP after the connection's run
// has ended (or while the run is being stopped), so the handler must not rely on the run: once the Sink that Run was given is no longer
// the live one (Run has returned, or its context has ended), the handler refuses the request (a 503, which the platform sends again)
// instead of handing it to that Sink.
type HTTPReceiver interface {
	// HTTPHandler returns the handler of the connection's inbound requests, and false when this connection takes none over HTTP (a
	// platform whose connections choose their transport, and this one does not use HTTP). It is asked each time the connection starts
	// running here, and the handler it gives is routed to until that run ends or is stopped, and may be serving a request after it.
	HTTPHandler() (http.Handler, bool)
}

// HTTPDeadline is an optional interface of the handler that HTTPReceiver.HTTPHandler returns, for a platform that waits longer for an
// answer than the chat listener gives a handler by default (Slack's wait of about 3 seconds), and whose handler uses that time:
// Microsoft Teams waits about 14 seconds for the answer to a press of a card, and its handler waits in the request for the press to be
// decided (InlineDecider). The listener gives such a handler a context that ends HandlerTimeout after the body was read, in place of
// its default, and the same time as any handler to write the answer after that. A value no longer than the default changes nothing.
// The deadline is the route's, for every request of the connection, so a handler that answers some requests at once must not wait on
// anything for them that its deadline would not cut.
type HTTPDeadline interface {
	// HandlerTimeout is the deadline of the handler's context, counted from when the request's body has been read.
	HandlerTimeout() time.Duration
}

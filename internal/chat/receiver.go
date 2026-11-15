package chat

import "net/http"

// HTTPReceiver is the part of a Platform that takes what the platform sends to Agent Kourier over HTTP (a webhook), in place of, or
// beside, a connection that Run opens itself. A platform that takes nothing over HTTP does not implement it.
//
// The broker serves the handler on its chat listener at POST /chat/<platform>/<namespace>/<name>, the ChatConnection's platform and
// name, for as long as the broker runs the connection on this replica: from just before it first calls Run until it stops calling it (a
// Run that fails is called again after a wait, and the route stays), and never on a replica that does not lead. Elsewhere, and at any
// other time, the listener answers 503 for the route itself, so a platform that retries finds the leader. The broker reads the body before the handler is called, and refuses one past its cap, so the handler is given a
// body that is whole and in memory (it can be read once, or replaced and read again), and a context with a deadline, by which it must
// have answered.
type HTTPReceiver interface {
	// HTTPHandler returns the handler of the connection's inbound requests, and false when this connection takes none over HTTP (a
	// platform whose connections choose their transport, and this one does not use HTTP). It is asked each time the connection starts
	// running here, and the handler it gives is served until that run ends.
	HTTPHandler() (http.Handler, bool)
}

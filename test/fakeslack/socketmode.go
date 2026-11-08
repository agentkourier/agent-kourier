package fakeslack

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// socketPath is where the fake serves its Socket Mode websocket. The URL that
// apps.connections.open returns points here; Slack's own looks different.
const socketPath = "/link"

// defaultPingInterval is well inside slack-go's 30 second limit on the gap
// between pings, past which its client drops the connection and reconnects.
const defaultPingInterval = 10 * time.Second

// WithPingInterval sets how often the fake pings a Socket Mode client. The
// default is 10 seconds; an interval of zero or less turns pings off, which
// makes slack-go's client reconnect after 30 seconds.
func WithPingInterval(d time.Duration) Option { return func(s *Server) { s.pingEvery = d } }

// sockets is the fake's Socket Mode state.
type sockets struct {
	mu       sync.Mutex
	conns    []*sockConn          // oldest first; guarded by mu
	turn     map[string]int       // per AppID, how many events WithSocketSpread has dealt out; guarded by mu
	shutdown bool                 // set by Close, guarded by mu
	closing  chan struct{}        // closed by Close, so that waits for acks end
	waiters  atomic.Int32         // WaitAck calls about to block or blocked, for tests
	issued   int                  // envelopes issued, guarded by mu
	pending  map[string]*envelope // every envelope sent, by envelope_id; guarded by mu
	wg       sync.WaitGroup       // the handler and ping goroutines
}

// sockConn is one client websocket. gorilla allows one writer at a time, so
// wmu serializes them; pings go out through WriteControl, which is exempt.
type sockConn struct {
	ws  *websocket.Conn
	app string // the AppID of the app this client connected as
	wmu sync.Mutex
}

func (c *sockConn) write(v any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.send(v)
}

// send writes v as a text message. The caller holds wmu.
func (c *sockConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

// connectionsOpen implements apps.connections.open
// (https://docs.slack.dev/reference/methods/apps.connections.open/): it points
// the client at this server's websocket, on whatever host the request used. A
// registered app's URL carries its app ID in an app parameter, which is how
// the websocket knows whose connection it is; the default app's URL has none.
// The parameter is not called ticket, because a gate in front of the fake may
// add a ticket of its own, which the fake ignores (internal/chat/slack's tests do).
//
// UNCONFIRMED(MC-10): the app parameter. Slack's URL carries an opaque
// ticket and no app, and nothing depends on this one's form.
func (s *Server) connectionsOpen(r *http.Request, app App) (response, string) {
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	u := scheme + "://" + r.Host + socketPath
	if app.AppID != s.defaultApp().AppID {
		u += "?" + url.Values{"app": {app.AppID}}.Encode()
	}
	return response{"url": u}, ""
}

// serveSocket accepts a Socket Mode client: it sends hello, then reads until
// the client goes away or the Server closes.
//
// UNCONFIRMED(WP-04): the shape of hello (Slack's debug_info is filler here),
// and that the connection needs no ticket. The connection belongs to the app
// named by the app parameter of the URL, which apps.connections.open hands
// out, and to the default app without one; an app no Server hosts is refused.
func (s *Server) serveSocket(w http.ResponseWriter, r *http.Request) {
	app, err := s.appNamed(r.URL.Query().Get("app"))
	if err != nil {
		http.Error(w, "fakeslack: unknown app", http.StatusBadRequest)
		return
	}
	if s.isRevoked(app.AppID) {
		http.Error(w, "fakeslack: token_revoked", http.StatusUnauthorized)
		return
	}
	sk := &s.sock
	if !sk.begin() {
		http.Error(w, "fakeslack: closing", http.StatusServiceUnavailable)
		return
	}
	defer sk.wg.Done()
	// slack-go dials with an Origin of https://api.slack.com.
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already replied
	}
	c := &sockConn{ws: ws, app: app.AppID}
	defer sk.remove(c)
	if s.beforeSocketAdd != nil {
		s.beforeSocketAdd()
	}

	// Register c and send hello under the write lock, so that a connection is
	// counted before its client can see hello, and hello goes out first.
	c.wmu.Lock()
	if !sk.add(c) {
		c.wmu.Unlock()
		return
	}
	// The check at the top of this function and add are not atomic: SetRevoked may run between them, and its
	// disconnect would not have seen c. Checked again now that c is in the list, a revocation either comes after
	// this (and disconnect finds c) or is seen here.
	if s.isRevoked(app.AppID) {
		c.wmu.Unlock()
		return
	}
	err = c.send(hello(app))
	c.wmu.Unlock()
	if err != nil {
		return
	}

	done := make(chan struct{})
	defer close(done)
	if s.pingEvery > 0 {
		sk.wg.Go(func() {
			tick := time.NewTicker(s.pingEvery)
			defer tick.Stop()
			for {
				select {
				case <-done:
					return
				case <-tick.C:
					_ = ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
				}
			}
		})
	}
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			return
		}
		sk.ack(msg)
	}
}

// hello returns the hello message for a new connection.
func hello(app App) map[string]any {
	return map[string]any{
		"type":            "hello",
		"num_connections": 1,
		"debug_info":      map[string]any{"host": "fakeslack", "build_number": 1, "approximate_connection_time": 18060},
		"connection_info": map[string]any{"app_id": app.AppID},
	}
}

// begin counts a handler in the WaitGroup, so that Close waits for it, and
// reports false if the Server is closing. The handler calls wg.Done.
func (sk *sockets) begin() bool {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	if sk.shutdown {
		return false
	}
	sk.wg.Add(1)
	return true
}

// add registers a connection, or reports false if the Server is closing.
func (sk *sockets) add(c *sockConn) bool {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	if sk.shutdown {
		return false
	}
	sk.conns = append(sk.conns, c)
	return true
}

func (sk *sockets) remove(c *sockConn) {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	for i, x := range sk.conns {
		if x == c {
			sk.conns = append(sk.conns[:i], sk.conns[i+1:]...)
			break
		}
	}
	_ = c.ws.Close()
}

// disconnect takes the app's connections out of the list, so that nothing is sent to them or counted for them
// from then on, and tells each to go with a disconnect envelope and closes it. The envelope and the close are
// under the connection's write lock, so no other envelope can come between them.
func (sk *sockets) disconnect(appID string) {
	sk.mu.Lock()
	var gone []*sockConn
	sk.conns = slices.DeleteFunc(sk.conns, func(c *sockConn) bool {
		if c.app == appID {
			gone = append(gone, c)
			return true
		}
		return false
	})
	sk.mu.Unlock()
	for _, c := range gone {
		c.wmu.Lock()
		_ = c.send(map[string]any{"type": "disconnect", "reason": "link_disabled"})
		_ = c.ws.Close()
		c.wmu.Unlock()
	}
}

// stop stops accepting clients, drops the connected ones, and waits for their
// goroutines.
func (sk *sockets) stop() {
	sk.mu.Lock()
	if !sk.shutdown {
		close(sk.closing)
	}
	sk.shutdown = true
	for _, c := range sk.conns {
		_ = c.ws.Close()
	}
	sk.mu.Unlock()
	sk.wg.Wait()
}

// Connections returns the number of Socket Mode clients connected, whichever
// app they are, each of which has been sent hello.
func (s *Server) Connections() int {
	s.sock.mu.Lock()
	defer s.sock.mu.Unlock()
	return len(s.sock.conns)
}

// AppConnections returns the number of Socket Mode clients connected as the
// app appID, each of which has been sent hello.
func (s *Server) AppConnections(appID string) int {
	s.sock.mu.Lock()
	defer s.sock.mu.Unlock()
	return s.sock.countLocked(appID)
}

// countLocked is the number of connections of the app. The caller holds mu.
func (sk *sockets) countLocked(appID string) int {
	n := 0
	for _, c := range sk.conns {
		if c.app == appID {
			n++
		}
	}
	return n
}

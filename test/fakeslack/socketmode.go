package fakeslack

import (
	"encoding/json"
	"net/http"
	"sync"
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
	shutdown bool                 // set by Close, guarded by mu
	closing  chan struct{}        // closed by Close, so that waits for acks end
	issued   int                  // envelopes issued, guarded by mu
	pending  map[string]*envelope // every envelope sent, by envelope_id; guarded by mu
	wg       sync.WaitGroup       // the handler and ping goroutines
}

// sockConn is one client websocket. gorilla allows one writer at a time, so
// wmu serializes them; pings go out through WriteControl, which is exempt.
type sockConn struct {
	ws  *websocket.Conn
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
// the client at this server's websocket, on whatever host the request used.
func (s *Server) connectionsOpen(r *http.Request) (response, string) {
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	return response{"url": scheme + "://" + r.Host + socketPath}, ""
}

// serveSocket accepts a Socket Mode client: it sends hello, then reads until
// the client goes away or the Server closes.
//
// UNCONFIRMED(WP-04): the shape of hello (Slack's debug_info is filler here),
// and that the connection needs no ticket.
func (s *Server) serveSocket(w http.ResponseWriter, r *http.Request) {
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
	c := &sockConn{ws: ws}
	defer sk.remove(c)

	// Register c and send hello under the write lock, so that a connection is
	// counted before its client can see hello, and hello goes out first.
	c.wmu.Lock()
	if !sk.add(c) {
		c.wmu.Unlock()
		return
	}
	err = c.send(s.hello())
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
func (s *Server) hello() map[string]any {
	return map[string]any{
		"type":            "hello",
		"num_connections": 1,
		"debug_info":      map[string]any{"host": "fakeslack", "build_number": 1, "approximate_connection_time": 18060},
		"connection_info": map[string]any{"app_id": s.bot.app},
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

// Connections returns the number of Socket Mode clients connected, each of
// which has been sent hello.
func (s *Server) Connections() int {
	s.sock.mu.Lock()
	defer s.sock.mu.Unlock()
	return len(s.sock.conns)
}

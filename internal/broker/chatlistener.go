package broker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// The chat listener's bounds. It is a port that a chat platform reaches from the internet, so every request is bounded before any
// work is done for it: its headers, its body, the time to read them, the time its handler has, and the time a kept-alive connection
// may sit idle. The tightest platform sets the times: Slack waits about 3 s for the answer to an event (upstream, not verified here)
// and then sends it again, so a request that cannot be read, handled and answered within about 3 s is lost anyway, and holding it
// longer only holds memory and a connection. A platform with a longer wait (GitHub's is 10 s, upstream) fits inside them.
const (
	// chatMaxBody caps the body of a request. Slack's event and interaction payloads are a few KiB; past the cap the request is
	// answered 413 and nothing more of it is read. The body is read whole, before the handler is called, because a platform verifies
	// its signature over the raw bytes before it decodes any of them.
	chatMaxBody = 1 << 20
	// chatMaxHeaderBytes caps the request line and the headers. net/http adds a little slack of its own and answers 431 past it.
	chatMaxHeaderBytes = 16 << 10
	// chatReadHeaderTimeout bounds the reading of the request line and the headers, which come in the first packets: a client slower
	// than this is not one whose request could be answered in time.
	chatReadHeaderTimeout = time.Second
	// chatReadTimeout bounds the reading of the whole request, headers and body.
	chatReadTimeout = 2 * time.Second
	// chatHandlerTimeout is the deadline of the context the handler is given, counted from when the body has been read: Slack's
	// wait, past which its answer would arrive after Slack has given up.
	chatHandlerTimeout = 3 * time.Second
	// chatWriteTimeout bounds what follows the headers: the body, the handler and the answer. net/http sets it once the headers are
	// read, so it holds the body's time and the handler's, and a second to write what the handler answers at its deadline.
	chatWriteTimeout = chatReadTimeout + chatHandlerTimeout + time.Second
	// chatIdleTimeout is how long a kept-alive connection may wait for its next request before it is closed.
	chatIdleTimeout = 30 * time.Second
	// chatRetryAfter is the Retry-After of a 503 for a connection that does not run here: a takeover or a restart of the connection
	// takes seconds, not minutes.
	chatRetryAfter = 5 * time.Second
)

// chatPathPrefix begins every route of the chat listener: /chat/<platform>/<namespace>/<name>.
const chatPathPrefix = "/chat/"

// The outcomes of a request to the chat listener, the label of agentkourier_chat_listener_requests_total: a closed set.
const (
	chatServed     = "served"     // a route ran here and its handler was called
	chatNotFound   = "not_found"  // 404: the path is no route's shape
	chatMethod     = "method"     // 405: a route's path with a method other than POST
	chatNoRoute    = "no_route"   // 503: the connection does not run here (another replica leads, or it is down or not served)
	chatTooLarge   = "too_large"  // 413: the body is past chatMaxBody
	chatUnreadable = "unreadable" // 400: the body could not be read whole (the client went away, or chatReadTimeout passed)
)

// chatOutcomes are the outcomes, in the order docs/reference/metrics.md lists them.
var chatOutcomes = []string{chatServed, chatNotFound, chatMethod, chatNoRoute, chatTooLarge, chatUnreadable}

// chatRoutes is the chat listener's route table: the handler of each connection that runs on this replica and takes requests over HTTP
// (chat.HTTPReceiver), by "<platform>/<namespace>/<name>". It belongs to the broker and not to an adapter, so that the routes of every
// platform share one port, and so that a replica with no route for a connection (a standby, or a leader whose connection is starting,
// restarting or down) answers 503, which a platform retries. A runtime registers its route when it starts running and drops it when its
// run ends or it is halted (runtimeHost.run), and close drops every route at once when the replica stops leading (failStop). It is safe
// for concurrent use.
type chatRoutes struct {
	mu     sync.RWMutex
	routes map[string]*chatRoute
	closed bool
	// count is told the outcome of each request (Metrics.ChatListenerRequest).
	count func(outcome string)
	log   *slog.Logger
}

// chatRoute is one registration: dropping it removes the route only if it is still this registration, so that a runtime that restarts and
// registers its successor's route first keeps it.
type chatRoute struct {
	table *chatRoutes
	key   string
	h     http.Handler
}

func newChatRoutes(count func(outcome string), log *slog.Logger) *chatRoutes {
	return &chatRoutes{routes: map[string]*chatRoute{}, count: count, log: log}
}

// chatRouteKey is the key, and the path after chatPathPrefix, of the connection conn ("namespace/name") of platform.
func chatRouteKey(platform, conn string) string { return platform + "/" + conn }

// register makes h the route of the connection conn of platform, in place of any, and returns the registration, or nil once the table
// is closed.
func (t *chatRoutes) register(platform, conn string, h http.Handler) *chatRoute {
	key := chatRouteKey(platform, conn)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	r := &chatRoute{table: t, key: key, h: h}
	t.routes[key] = r
	t.log.Info("the chat listener serves the connection", "connection", conn, "path", chatPathPrefix+key)
	return r
}

// drop removes the route r registered, if it is still the route of its connection.
func (r *chatRoute) drop() {
	t := r.table
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.routes[r.key] == r {
		delete(t.routes, r.key)
	}
}

// close drops every route and refuses any later registration: the replica no longer leads, and nothing it runs may take a request.
func (t *chatRoutes) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	clear(t.routes)
}

// lookup returns the handler of key, or nil.
func (t *chatRoutes) lookup(key string) http.Handler {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if r := t.routes[key]; r != nil {
		return r.h
	}
	return nil
}

// ServeHTTP answers a request to the chat listener. Its checks come in order, and each costs less than the one after it: a path that is
// no route's shape is 404, a method other than POST 405, a connection with no route here 503 with Retry-After (nothing of the body is
// read), a body past chatMaxBody 413, and a body that cannot be read whole 400. Only then is the route's handler called, with the body
// in memory and a context that ends chatHandlerTimeout later.
func (t *chatRoutes) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	key, ok := chatKeyOf(req.URL.Path)
	if !ok {
		t.count(chatNotFound)
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodPost {
		t.count(chatMethod)
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "only POST is served here", http.StatusMethodNotAllowed)
		return
	}
	h := t.lookup(key)
	if h == nil {
		t.count(chatNoRoute)
		w.Header().Set("Retry-After", strconv.Itoa(int(chatRetryAfter/time.Second)))
		http.Error(w, "the connection does not run on this replica", http.StatusServiceUnavailable)
		return
	}
	if req.ContentLength > chatMaxBody {
		t.count(chatTooLarge)
		http.Error(w, "the body is too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, chatMaxBody))
	if err != nil {
		if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
			t.count(chatTooLarge)
			http.Error(w, "the body is too large", http.StatusRequestEntityTooLarge)
			return
		}
		t.count(chatUnreadable)
		http.Error(w, "the body could not be read", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), chatHandlerTimeout)
	defer cancel()
	in := req.WithContext(ctx)
	in.Body, in.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	in.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	t.count(chatServed)
	h.ServeHTTP(w, in)
}

// chatKeyOf is the route key of path, /chat/<platform>/<namespace>/<name> with no segment empty and nothing after it, and false for any
// other path.
func chatKeyOf(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, chatPathPrefix)
	if !ok {
		return "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", false
	}
	for _, p := range parts {
		if p == "" {
			return "", false
		}
	}
	return rest, true
}

// newChatServer is the chat listener's server, with its bounds. net/http reports on the standard logger, which goes through slog's
// default handler, Agent Kourier's, as warnings, as the health server's does.
func newChatServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: chatReadHeaderTimeout, ReadTimeout: chatReadTimeout, WriteTimeout: chatWriteTimeout,
		IdleTimeout: chatIdleTimeout, MaxHeaderBytes: chatMaxHeaderBytes, ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn)}
}

// receiverOf is the handler that p offers for the chat listener (chat.HTTPReceiver), and false when it offers none.
func receiverOf(p chat.Platform) (http.Handler, bool) {
	r, ok := p.(chat.HTTPReceiver)
	if !ok {
		return nil, false
	}
	return r.HTTPHandler()
}

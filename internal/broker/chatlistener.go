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
	// wait, past which its answer would arrive after Slack has given up. A route whose handler asks for longer (chat.HTTPDeadline) is
	// given what it asks for instead.
	chatHandlerTimeout = 3 * time.Second
	// chatAnswerTimeout is the time a handler has to write what it answers at its deadline.
	chatAnswerTimeout = time.Second
	// chatWriteTimeout bounds what follows the headers: the body, the handler and the answer. net/http sets it once the headers are
	// read, so it holds the body's time and the handler's, and chatAnswerTimeout to write what the handler answers at its deadline. A
	// route with a longer deadline has its request's bound moved, once its body is read, to its deadline and chatAnswerTimeout.
	chatWriteTimeout = chatReadTimeout + chatHandlerTimeout + chatAnswerTimeout
	// chatIdleTimeout is how long a kept-alive connection may wait for its next request before it is closed.
	chatIdleTimeout = 30 * time.Second
	// chatRetryAfter is the Retry-After of a 503 for a connection that does not run here, or for a listener that is full: a takeover or
	// a restart of the connection takes seconds, not minutes.
	chatRetryAfter = 5 * time.Second
	// chatMaxInFlight caps the requests whose handlers run at once, across every connection. A request takes its place only once its
	// body has been read whole, so a client that sends its body slowly holds no place, only a chunk of chatBodyBudget. Each platform's
	// handler must acknowledge in milliseconds, handing the request on and answering before the work it asks for is done: that is a
	// requirement on the handlers, which a handler that waits on the work would break. At that pace 32 at once is room for hundreds of
	// requests a second. The one exception is a press whose platform waits for its decision in the request (chat.InlineDecider, Microsoft
	// Teams): it holds its place until the decision, or its route's longer deadline, and such presses hold at most half the places
	// (pressWaitsMax), past which they are answered at once. While every place is taken a request is answered 503 with Retry-After before any of its body is read, and one
	// whose body was read while the last places were taken is answered so once it has been read.
	chatMaxInFlight = 32
	// chatBodyChunk is the size of the buffers a body is read into, one after another, each charged to chatBodyBudget before it is
	// allocated. It is one of Go's allocation size classes, so what is charged is what is allocated. A request being read holds at most
	// one chunk more than it has received: a client that trickles its body holds 4 KiB until chatReadTimeout cuts it.
	chatBodyChunk = 4 << 10
	// chatBodyBudget caps the live body buffers of the requests being read or handled, across every connection, from the first chunk
	// until the handler returns. The buffers are allocated only once they are charged, so the listener's live bodies take at most 32 MiB,
	// an eighth of the chart's default memory limit of 256 MiB, and the list of each body's chunks and their readers about 3% more;
	// buffers given back stay on the heap until the garbage collector frees them. The charge is given back when the handler returns, so a
	// handler must not keep the request's body or its readers after it returns: it copies what it hands on, and that copy is outside the
	// budget. A request whose next chunk does not fit is answered 503 with Retry-After and the rest of its body is not read. A routed
	// request is charged its first chunk as soon as its headers are read, whatever it has sent of its body, so the budget is held by 8192
	// connections that are mid-body, for as long as chatReadTimeout lets them, or by 32 MiB actually sent: starving the listener takes
	// 8192 concurrent connections, not 32. The number of connections, and what net/http holds for each, is outside the budget.
	chatBodyBudget = 32 << 20
	// chatDrainTimeout bounds the wait, at shutdown, for the requests in flight; past it they are cut, unanswered. Slack sends such a
	// request again; Microsoft Teams does not send an invoke again, so a press that waits for its decision is let go as the drain begins
	// (chatRoutes.onDrain) and answered inside it. It is the first part of the intake step, whose bound is 4 s, and is kept to half of
	// it, so that the intakes, which stop after it in the same step, always keep at least 2 s.
	chatDrainTimeout = 2 * time.Second
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
	chatBusy       = "busy"       // 503: chatMaxInFlight handlers were already running, or the body did not fit in chatBodyBudget
)

// chatOutcomes are the outcomes, in the order docs/reference/metrics.md lists them.
var chatOutcomes = []string{chatServed, chatNotFound, chatMethod, chatNoRoute, chatTooLarge, chatUnreadable, chatBusy}

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
	// drains is what runs as the listener begins its drain at shutdown (draining): what holds a request waiting lets it go, so that
	// the request is answered inside the drain. Guarded by mu.
	drains []func()
	// count is told the outcome of each request (Metrics.ChatListenerRequest).
	count func(outcome string)
	log   *slog.Logger
	// slots holds one token for each request whose handler runs, chatMaxInFlight at most.
	slots chan struct{}
	// budget is what is left of chatBodyBudget.
	budget chatBudget
}

// chatBudget is the bytes of body buffer the chat listener may still allocate. It is safe for concurrent use.
type chatBudget struct {
	mu   sync.Mutex
	left int
}

// take charges n bytes to the budget and reports whether they fitted; nothing is charged when they did not.
func (b *chatBudget) take(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.left {
		return false
	}
	b.left -= n
	return true
}

// give returns n bytes that take charged.
func (b *chatBudget) give(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.left += n
}

// chatRoute is one registration: dropping it removes the route only if it is still this registration, so that a runtime that restarts and
// registers its successor's route first keeps it.
type chatRoute struct {
	table *chatRoutes
	key   string
	h     http.Handler
	// timeout is the deadline of the handler's context: chatHandlerTimeout, or the longer one the handler asks for (chat.HTTPDeadline).
	timeout time.Duration
}

// handlerTimeout is the deadline h is given: what it asks for (chat.HTTPDeadline) when that is longer than chatHandlerTimeout, and
// chatHandlerTimeout otherwise.
func handlerTimeout(h http.Handler) time.Duration {
	if d, ok := h.(chat.HTTPDeadline); ok {
		return max(d.HandlerTimeout(), chatHandlerTimeout)
	}
	return chatHandlerTimeout
}

func newChatRoutes(count func(outcome string), log *slog.Logger) *chatRoutes {
	return &chatRoutes{routes: map[string]*chatRoute{}, count: count, log: log, slots: make(chan struct{}, chatMaxInFlight),
		budget: chatBudget{left: chatBodyBudget}}
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
	r := &chatRoute{table: t, key: key, h: h, timeout: handlerTimeout(h)}
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

// onDrain has fn run as the listener begins its drain at shutdown (draining), before it waits for the requests in flight: fn makes a
// handler that waits answer now. It must not block.
func (t *chatRoutes) onDrain(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.drains = append(t.drains, fn)
}

// draining runs what onDrain registered, once: the listener's stop calls it before it stops taking requests and waits for those in
// flight.
func (t *chatRoutes) draining() {
	t.mu.Lock()
	drains := t.drains
	t.drains = nil
	t.mu.Unlock()
	for _, fn := range drains {
		fn()
	}
}

// lookup returns the route of key, or nil.
func (t *chatRoutes) lookup(key string) *chatRoute {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.routes[key]
}

// ServeHTTP answers a request to the chat listener. Its checks come in order, and each costs less than the one after it: a path that is
// no route's shape is 404, a method other than POST 405, a connection with no route here 503 with Retry-After, a declared body past
// chatMaxBody 413, and a listener whose chatMaxInFlight handlers all run 503 with Retry-After. None of those reads the body: each answer
// says Connection: close, so that net/http closes the connection after it instead of first reading and discarding what the client still
// sends (up to 256 KiB, for as long as the client takes). Then the body is read, in chunks charged to chatBodyBudget: a chunk that does not
// fit is 503 with Retry-After and Connection: close, a body past chatMaxBody 413, and one that cannot be read whole 400. Only then does the
// request take a place among the handlers, and it is 503 with Retry-After when the last were taken while it was read: a client slow to
// send its body holds no place. The route's handler is called with the body in memory and a context that ends chatHandlerTimeout later
// (or the route's longer deadline: chat.HTTPDeadline), and the body's chunks are given back to the budget when it returns.
func (t *chatRoutes) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	key, ok := chatKeyOf(req.URL.Path)
	if !ok {
		t.count(chatNotFound)
		refuse(w, http.StatusNotFound, "404 page not found")
		return
	}
	if req.Method != http.MethodPost {
		t.count(chatMethod)
		w.Header().Set("Allow", http.MethodPost)
		refuse(w, http.StatusMethodNotAllowed, "only POST is served here")
		return
	}
	route := t.lookup(key)
	if route == nil {
		t.count(chatNoRoute)
		w.Header().Set("Retry-After", strconv.Itoa(int(chatRetryAfter/time.Second)))
		refuse(w, http.StatusServiceUnavailable, "the connection does not run on this replica")
		return
	}
	if req.ContentLength > chatMaxBody {
		t.count(chatTooLarge)
		refuse(w, http.StatusRequestEntityTooLarge, "the body is too large")
		return
	}
	if len(t.slots) == cap(t.slots) { // spares the read of a body that would be refused; the place itself is taken after the read
		t.busy(w, false)
		return
	}
	chunks, size, charged, err := t.readBody(http.MaxBytesReader(w, req.Body, chatMaxBody))
	defer t.budget.give(charged)
	if err != nil {
		if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
			t.count(chatTooLarge)
			http.Error(w, "the body is too large", http.StatusRequestEntityTooLarge)
			return
		}
		if errors.Is(err, errChatBudget) {
			t.busy(w, false)
			return
		}
		t.count(chatUnreadable)
		http.Error(w, "the body could not be read", http.StatusBadRequest)
		return
	}
	select {
	case t.slots <- struct{}{}:
		defer func() { <-t.slots }()
	default:
		t.busy(w, true)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), route.timeout)
	defer cancel()
	if route.timeout > chatHandlerTimeout {
		// The server's write bound was set, when the headers were read, for a handler with the default deadline: this route's handler
		// gets the same time to answer after its own. A connection that cannot take a deadline any more is cut, as at the bound.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(route.timeout + chatAnswerTimeout))
	}
	in := req.WithContext(ctx)
	in.Body, in.ContentLength = chunkBody(chunks), size
	in.GetBody = func() (io.ReadCloser, error) { return chunkBody(chunks), nil }
	t.count(chatServed)
	route.h.ServeHTTP(w, in)
}

// busy answers 503 with Retry-After, counted as busy. A request whose body was not read whole (bodyRead false) is refused, so that its
// connection is closed rather than read to its end.
func (t *chatRoutes) busy(w http.ResponseWriter, bodyRead bool) {
	t.count(chatBusy)
	w.Header().Set("Retry-After", strconv.Itoa(int(chatRetryAfter/time.Second)))
	if bodyRead {
		http.Error(w, "the chat listener is busy", http.StatusServiceUnavailable)
		return
	}
	refuse(w, http.StatusServiceUnavailable, "the chat listener is busy")
}

// errChatBudget is readBody's error for a chunk that did not fit in chatBodyBudget.
var errChatBudget = errors.New("the body does not fit in the chat listener's budget")

// readBody reads r to its end into chunks of chatBodyChunk, each charged to the budget before it is allocated, and returns them, the
// body's size, and the bytes charged, which the caller gives back whatever the error. Each chunk is filled before the next is charged,
// so a client that sends a byte at a time holds one chunk, not one for each read.
func (t *chatRoutes) readBody(r io.Reader) (chunks [][]byte, size int64, charged int, err error) {
	for {
		if !t.budget.take(chatBodyChunk) {
			return nil, 0, charged, errChatBudget
		}
		charged += chatBodyChunk
		c := make([]byte, chatBodyChunk)
		n := 0
		for n < len(c) && err == nil {
			var m int
			m, err = r.Read(c[n:])
			n += m
		}
		if n > 0 {
			chunks, size = append(chunks, c[:n]), size+int64(n)
		}
		switch {
		case errors.Is(err, io.EOF):
			return chunks, size, charged, nil
		case err != nil:
			return nil, 0, charged, err
		}
	}
}

// chunkBody is a request body that reads chunks in order.
func chunkBody(chunks [][]byte) io.ReadCloser {
	readers := make([]io.Reader, len(chunks))
	for i, c := range chunks {
		readers[i] = bytes.NewReader(c)
	}
	return io.NopCloser(io.MultiReader(readers...))
}

// refuse answers code with text and Connection: close, for a request whose body is not read whole.
func refuse(w http.ResponseWriter, code int, text string) {
	w.Header().Set("Connection", "close")
	http.Error(w, text, code)
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

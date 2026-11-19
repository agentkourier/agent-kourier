package broker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// heard is a handler that records what it was given and answers 200 with "heard".
type heard struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
	dl     []time.Duration // how long each request's context had left when the handler was called
}

func (h *heard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.bodies = append(h.bodies, string(body))
	h.paths = append(h.paths, r.URL.Path)
	if d, ok := r.Context().Deadline(); ok {
		h.dl = append(h.dl, time.Until(d))
	}
	h.mu.Unlock()
	_, _ = io.WriteString(w, "heard")
}

func (h *heard) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bodies)
}

// got is what the handler was given: the bodies, the paths and the time each request's context had left.
func (h *heard) got() (bodies, paths []string, left []time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.bodies), slices.Clone(h.paths), slices.Clone(h.dl)
}

// chatRig is a route table behind a test server, counting through real metrics.
type chatRig struct {
	t      *testing.T
	m      *Metrics
	routes *chatRoutes
	srv    *httptest.Server
}

func newChatRig(t *testing.T) *chatRig {
	t.Helper()
	m := mustMetrics(t, nil)
	routes := newChatRoutes(m, testutil.DiscardLogger())
	srv := httptest.NewUnstartedServer(routes)
	srv.Config = newChatServer(routes)
	srv.Start()
	t.Cleanup(srv.Close)
	return &chatRig{t: t, m: m, routes: routes, srv: srv}
}

// do sends a request and returns its status, its Retry-After and Allow headers and its body.
func (r *chatRig) do(method, path string, body io.Reader) (code int, retryAfter, allow, text string) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.t.Context(), method, r.srv.URL+path, body)
	if err != nil {
		r.t.Fatal(err)
	}
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("Allow"), string(b)
}

// counted is the value of agentkourier_chat_listener_requests_total for outcome, summed over its platforms and connections.
func (r *chatRig) counted(outcome string) int {
	r.t.Helper()
	n := 0
	for k, v := range listenerSeries(r.t, r.m) {
		if k.outcome == outcome {
			n += v
		}
	}
	return n
}

// listenerKey is the labels of a series of agentkourier_chat_listener_requests_total.
type listenerKey struct{ outcome, platform, connection string }

// listenerSeries is every series of agentkourier_chat_listener_requests_total, its value by its labels.
func listenerSeries(t *testing.T, m *Metrics) map[listenerKey]int {
	t.Helper()
	fams, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[listenerKey]int{}
	for _, f := range fams {
		if f.GetName() != "agentkourier_chat_listener_requests_total" {
			continue
		}
		for _, s := range f.GetMetric() {
			var k listenerKey
			for _, l := range s.GetLabel() {
				switch l.GetName() {
				case "outcome":
					k.outcome = l.GetValue()
				case "platform":
					k.platform = l.GetValue()
				case "connection":
					k.connection = l.GetValue()
				default:
					t.Fatalf("agentkourier_chat_listener_requests_total has the label %s", l.GetName())
				}
			}
			out[k] = int(s.GetCounter().GetValue())
		}
	}
	return out
}

// A connection's route is served at /chat/<platform>/<namespace>/<name>, POST only; a connection with no route here is 503 with a
// Retry-After, so that the platform sends it again; a route's path with another method is 405; and every other path is 404. Each is
// counted under its own outcome.
func TestChatRoutesServeARegisteredRouteAndAnswerTheRest(t *testing.T) {
	r := newChatRig(t)
	h := &heard{}
	r.routes.register(v1alpha1.PlatformSlack, "team-a/slack", h)

	if code, _, _, text := r.do(http.MethodPost, "/chat/slack/team-a/slack", strings.NewReader("payload=1")); code != http.StatusOK || text != "heard" {
		t.Errorf("POST to a registered route = %d %q, want 200 from its handler", code, text)
	}
	if bodies, paths, _ := h.got(); !slices.Equal(bodies, []string{"payload=1"}) || !slices.Equal(paths, []string{"/chat/slack/team-a/slack"}) {
		t.Errorf("the handler was given %q at %q, want the body whole at the route's path", bodies, paths)
	}
	for _, path := range []string{"/chat/slack/team-a/other", "/chat/slack/team-b/slack", "/chat/github/team-a/slack"} {
		code, retry, _, _ := r.do(http.MethodPost, path, strings.NewReader("x"))
		if code != http.StatusServiceUnavailable || retry != "5" {
			t.Errorf("POST %s = %d with Retry-After %q, want 503 with 5: no connection runs there", path, code, retry)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead} {
		if code, _, allow, _ := r.do(method, "/chat/slack/team-a/slack", nil); code != http.StatusMethodNotAllowed || allow != http.MethodPost {
			t.Errorf("%s on a route = %d with Allow %q, want 405 with POST", method, code, allow)
		}
	}
	for _, path := range []string{"/", "/metrics", "/healthz", "/chat", "/chat/", "/chat/slack", "/chat/slack/team-a", "/chat/slack/team-a/slack/",
		"/chat/slack/team-a/slack/x", "/chat//team-a/slack", "/chat/slack//slack", "/other/slack/team-a/slack"} {
		if code, _, _, _ := r.do(http.MethodPost, path, nil); code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, code)
		}
	}
	if h.calls() != 1 {
		t.Errorf("the handler was called %d times, want once: only its own route reaches it", h.calls())
	}
	for outcome, want := range map[string]int{chatServed: 1, chatNoRoute: 3, chatMethod: 4, chatNotFound: 12, chatTooLarge: 0, chatUnreadable: 0} {
		if got := r.counted(outcome); got != want {
			t.Errorf("outcome %s counted %d, want %d", outcome, got, want)
		}
	}
}

// A registration is dropped only while it is its connection's: a runtime that restarted and registered its successor before the old run
// ended keeps the successor's route. close drops every route and refuses any after it.
func TestChatRoutesDropOnlyTheirOwnRegistrationAndCloseDropsThemAll(t *testing.T) {
	r := newChatRig(t)
	old, successor, other := &heard{}, &heard{}, &heard{}
	oldReg := r.routes.register(v1alpha1.PlatformSlack, "ns/a", old)
	newReg := r.routes.register(v1alpha1.PlatformSlack, "ns/a", successor)
	r.routes.register(v1alpha1.PlatformSlack, "ns/b", other)

	oldReg.drop()
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/a", nil); code != http.StatusOK || successor.calls() != 1 || old.calls() != 0 {
		t.Fatalf("after the old registration was dropped, POST = %d (successor %d calls, old %d), want the successor's 200", code, successor.calls(), old.calls())
	}
	newReg.drop()
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/a", nil); code != http.StatusServiceUnavailable {
		t.Errorf("after the successor's registration was dropped, POST = %d, want 503", code)
	}

	r.routes.close()
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/b", nil); code != http.StatusServiceUnavailable {
		t.Errorf("after close, POST to a route that was registered = %d, want 503", code)
	}
	if reg := r.routes.register(v1alpha1.PlatformSlack, "ns/c", other); reg != nil {
		t.Error("register after close returned a registration, want nil: a replica that no longer leads takes no route")
	}
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/c", nil); code != http.StatusServiceUnavailable {
		t.Errorf("POST to a route registered after close = %d, want 503", code)
	}
}

// A body past chatMaxBody is answered 413 and never reaches the handler, whether its length is declared or it is sent in chunks; a body of
// exactly the cap reaches it whole. A connection with no route is answered 503 before its body is looked at.
func TestChatRoutesCapTheBody(t *testing.T) {
	r := newChatRig(t)
	h := &heard{}
	r.routes.register(v1alpha1.PlatformSlack, "ns/a", h)
	atCap := strings.Repeat("a", chatMaxBody)
	over := atCap + "b"

	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/a", strings.NewReader(over)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a declared body one byte past the cap = %d, want 413", code)
	}
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/a", io.MultiReader(strings.NewReader(over))); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a chunked body one byte past the cap = %d, want 413", code) // a MultiReader has no length, so the client sends it in chunks
	}
	if h.calls() != 0 {
		t.Fatalf("the handler was called %d times for bodies past the cap, want none", h.calls())
	}
	code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/a", io.MultiReader(strings.NewReader(atCap)))
	if bodies, _, _ := h.got(); code != http.StatusOK || len(bodies) != 1 || bodies[0] != atCap {
		t.Errorf("a body of exactly the cap = %d, and the handler had %d calls; want 200 with the body whole", code, len(bodies))
	}
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/none", strings.NewReader(over)); code != http.StatusServiceUnavailable {
		t.Errorf("a body past the cap for a connection with no route = %d, want 503 first", code)
	}
	if got := r.counted(chatTooLarge); got != 2 {
		t.Errorf("too_large counted %d, want 2", got)
	}
}

// The handler is given a context that ends chatHandlerTimeout after its body was read, and it ends: a handler that waits on it is
// released, and the request ends with it.
func TestChatRoutesGiveTheHandlerADeadline(t *testing.T) {
	t.Parallel()
	r := newChatRig(t)
	h := &heard{}
	r.routes.register(v1alpha1.PlatformSlack, "ns/a", h)
	code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/a", nil)
	_, _, dl := h.got()
	if code != http.StatusOK || len(dl) != 1 {
		t.Fatalf("POST = %d with %d deadlines seen, want 200 and one", code, len(dl))
	}
	if left := dl[0]; left > chatHandlerTimeout || left < chatHandlerTimeout-time.Second {
		t.Errorf("the handler's context had %s left, want about %s", left, chatHandlerTimeout)
	}

	var ended atomic.Value
	r.routes.register(v1alpha1.PlatformSlack, "ns/slow", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
		ended.Store(req.Context().Err())
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	begin := time.Now()
	code, _, _, _ = r.do(http.MethodPost, "/chat/slack/ns/slow", nil)
	took := time.Since(begin)
	if err, _ := ended.Load().(error); !errors.Is(err, context.DeadlineExceeded) || code != http.StatusServiceUnavailable {
		t.Errorf("a handler that waits on its context: %d, its context ended with %v; want its 503 once the deadline passed", code, err)
	}
	if took < chatHandlerTimeout-100*time.Millisecond || took > chatHandlerTimeout+2*time.Second {
		t.Errorf("the slow handler's request took %s, want about %s", took, chatHandlerTimeout)
	}
}

// The server's bounds are the constants, and they fit together: the headers are read within the time of the whole request, and the write
// bound, which net/http starts once the headers are read, holds the body, the handler and a second to answer.
func TestChatServerHasItsBounds(t *testing.T) {
	srv := newChatServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout != chatReadHeaderTimeout || srv.ReadTimeout != chatReadTimeout || srv.WriteTimeout != chatWriteTimeout ||
		srv.IdleTimeout != chatIdleTimeout || srv.MaxHeaderBytes != chatMaxHeaderBytes || srv.ErrorLog == nil {
		t.Errorf("the chat server is %+v, want the chat listener's bounds", srv)
	}
	if chatReadHeaderTimeout > chatReadTimeout || chatWriteTimeout < chatReadTimeout+chatHandlerTimeout {
		t.Errorf("headers %s, request %s, handler %s, write %s: the headers must fit in the request, and the write bound must hold the body and the handler",
			chatReadHeaderTimeout, chatReadTimeout, chatHandlerTimeout, chatWriteTimeout)
	}
	if chatHandlerTimeout > 3*time.Second {
		t.Errorf("the handler's deadline is %s, past Slack's wait of 3 s", chatHandlerTimeout)
	}
}

// A client that sends its headers too slowly, or its body, is cut by the server's own bounds, and its request never reaches a handler. These
// run on the production values, so they take a second or two each.
func TestChatServerCutsASlowClient(t *testing.T) {
	cases := map[string]struct {
		send  string
		bound time.Duration
	}{
		"headers": {send: "POST /chat/slack/ns/a HTTP/1.1\r\nHost: x\r\n", bound: chatReadHeaderTimeout},
		"body":    {send: "POST /chat/slack/ns/a HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\npart", bound: chatReadTimeout},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newChatRig(t)
			h := &heard{}
			r.routes.register(v1alpha1.PlatformSlack, "ns/a", h)
			conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", r.srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			begin := time.Now()
			if _, err := io.WriteString(conn, tc.send); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(tc.bound + 5*time.Second))
			resp, _ := io.ReadAll(bufio.NewReader(conn)) // until the server closes the connection
			took := time.Since(begin)
			if took < tc.bound-100*time.Millisecond || took > tc.bound+2*time.Second {
				t.Errorf("the server closed the connection after %s, want about %s (it answered %q)", took, tc.bound, resp)
			}
			if h.calls() != 0 {
				t.Errorf("a request cut while it was read reached the handler")
			}
		})
	}
}

// receiving is a fake platform that takes requests over HTTP: it offers its handler (when offer says so), and counts the times it was
// asked for it.
type receiving struct {
	*chattest.FakePlatform
	h     http.Handler
	offer bool
	asked *atomic.Int32
}

func (p receiving) HTTPHandler() (http.Handler, bool) {
	p.asked.Add(1)
	return p.h, p.offer
}

var (
	_ chat.HTTPReceiver = receiving{}
	_ chat.HTTPReceiver = fencedPlatform{} // the lease fence passes a platform's handler through
)

// hostRig is a runtime host whose runtimes run on fake platforms, with a chat listener's route table behind a test server.
type hostRig struct {
	*chatRig
	host *runtimeHost
}

func newHostRig(t *testing.T, routes bool) *hostRig {
	t.Helper()
	r := newChatRig(t)
	host := &runtimeHost{log: testutil.DiscardLogger(), metrics: r.m, table: newRuntimeTable(), parent: t.Context()}
	if routes {
		host.routes = r.routes
	}
	return &hostRig{chatRig: r, host: host}
}

// start builds and runs the runtime of the connection ns/name on p, fenced or not, as the leader half does.
func (r *hostRig) start(name string, p chat.Platform, fenced bool) *connRuntime {
	r.t.Helper()
	r.host.open = func(Connection, func(bool)) (builtPlatform, error) {
		if fenced {
			p = fenceChat(p, newLeaseFence(ElectionConfig{}, newLeaseClock(nil), func(string) {}))
		}
		return builtPlatform{Platform: p, closeIdle: func() {}}, nil
	}
	rt, err := r.host.build(Connection{Ref: v1alpha1.ObjectRef{Namespace: "ns", Name: name}, Platform: v1alpha1.PlatformSlack})
	if err != nil {
		r.t.Fatal(err)
	}
	r.host.run(rt, nil, nil)
	return rt
}

// A runtime whose platform offers a handler has its route while it runs, through the lease fence as well, and loses it when it is halted
// (the shutdown, or the supervisor taking the connection away), when it is stopped and its run waited for, and when its run ends by itself
// (the platform rejected its credentials).
func TestRuntimeRouteLastsWhileItsRunDoes(t *testing.T) {
	r := newHostRig(t, true)
	h := &heard{}
	newPlatform := func() receiving {
		return receiving{FakePlatform: chattest.NewFake(), h: h, offer: true, asked: &atomic.Int32{}}
	}

	halted := r.start("halted", newPlatform(), false)
	r.start("fenced", newPlatform(), true)
	stopped := r.start("stopped", newPlatform(), false)
	rejected := r.start("rejected", newPlatform(), false)
	for _, name := range []string{"halted", "fenced", "stopped"} {
		if code, _, _, text := r.do(http.MethodPost, "/chat/slack/ns/"+name, nil); code != http.StatusOK || text != "heard" {
			t.Errorf("the route of %s, which runs = %d %q, want its handler's 200", name, code, text)
		}
	}

	halted.halt()
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/halted", nil); code != http.StatusServiceUnavailable {
		t.Errorf("the route of a halted runtime = %d at once, want 503", code)
	}
	r.host.stopRuntime(t.Context(), stopped)
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/stopped", nil); code != http.StatusServiceUnavailable {
		t.Errorf("the route of a stopped runtime = %d, want 503", code)
	}
	rejected.report("invalid_auth") // as a call through the platform that found the credentials rejected does: the run ends by itself
	rejected.wait(t.Context())
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/rejected", nil); code != http.StatusServiceUnavailable {
		t.Errorf("the route of a runtime whose credentials were rejected = %d, want 503", code)
	}
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/fenced", nil); code != http.StatusOK {
		t.Errorf("the route of the runtime that still runs = %d, want 200: the others' ends must not take it", code)
	}
	r.host.haltAll(t.Context())
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/fenced", nil); code != http.StatusServiceUnavailable {
		t.Errorf("after every runtime was halted, the last route = %d, want 503", code)
	}
}

// A platform that offers no handler (one that takes nothing over HTTP, or a connection of it that does not) has no route, and a broker with
// no chat listener never asks a platform for one.
func TestRuntimeRouteOnlyWhenOfferedAndListened(t *testing.T) {
	r := newHostRig(t, true)
	declines := receiving{FakePlatform: chattest.NewFake(), h: &heard{}, offer: false, asked: &atomic.Int32{}}
	r.start("declines", declines, false)
	r.start("plain", chattest.NewFake(), false)
	for _, name := range []string{"declines", "plain"} {
		if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/"+name, nil); code != http.StatusServiceUnavailable {
			t.Errorf("the route of %s, whose platform offers none = %d, want 503", name, code)
		}
	}
	if declines.asked.Load() != 1 {
		t.Errorf("the platform was asked %d times for its handler, want once as its run began", declines.asked.Load())
	}

	off := newHostRig(t, false)
	offers := receiving{FakePlatform: chattest.NewFake(), h: &heard{}, offer: true, asked: &atomic.Int32{}}
	off.start("offers", offers, false)
	if n := offers.asked.Load(); n != 0 {
		t.Errorf("with no chat listener the platform was asked %d times for its handler, want never", n)
	}
	off.host.haltAll(t.Context())
	r.host.haltAll(t.Context())
}

// A replica that stops leading drops every route before it exits, so that nothing it still runs takes a request in the moment the exit takes.
func TestFailStopDropsTheRoutesBeforeItExits(t *testing.T) {
	r := newChatRig(t)
	r.routes.register(v1alpha1.PlatformSlack, "ns/a", &heard{})
	var atExit int
	f := &failStop{log: testutil.DiscardLogger(), before: r.routes.close, exit: func(int) {
		atExit, _, _, _ = r.do(http.MethodPost, "/chat/slack/ns/a", nil)
	}}

	f.now("the lease was lost")

	if atExit != http.StatusServiceUnavailable {
		t.Errorf("at the exit the route answered %d, want 503", atExit)
	}
}

// Every outcome the listener counts is its own label, and anything else is "other"; a platform outside the listener's set is "other", and
// a connection whose route was never on the listener is "unknown".
func TestChatListenerOutcomesAreBounded(t *testing.T) {
	m := mustMetrics(t, nil)
	if got := samples(t, m); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "agentkourier_chat_listener_") }) {
		t.Errorf("a broker that took no request has chat listener series: %v", got)
	}
	for _, o := range append(slices.Clone(chatOutcomes), "made_up") {
		m.ChatListenerRequest(o, "made_up", "made/up")
	}
	got := listenerSeries(t, m)
	for _, o := range append(slices.Clone(chatOutcomes), otherLabel) {
		if got[listenerKey{o, otherLabel, unknownLabel}] != 1 {
			t.Errorf("outcome %s has no series of its own, under platform other and connection unknown: %v", o, got)
		}
	}
	if len(got) != len(chatOutcomes)+1 {
		t.Errorf("series = %v, want one for each outcome and one for other", got)
	}
	if want := []string{"served", "not_found", "method", "no_route", "too_large", "unreadable", "busy"}; !slices.Equal(chatOutcomes, want) {
		t.Errorf("the outcomes are %q, want %q as docs/reference/metrics.md lists them", chatOutcomes, want)
	}
	if want := []string{"slack"}; !slices.Equal(chatPlatforms, want) {
		t.Errorf("the platforms are %q, want %q as docs/reference/metrics.md lists them", chatPlatforms, want)
	}
}

// errBody is a request body that fails when it is read, as one whose client went away.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("the client went away") }

// fillingBody is a request body that takes every place among the handlers of routes when it is first read, as requests that arrive
// while it is being read do.
type fillingBody struct {
	routes *chatRoutes
	once   sync.Once
	r      io.Reader
}

func (b *fillingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { fillPlaces(b.routes) })
	return b.r.Read(p)
}

// fillPlaces takes every place among the handlers, as chatMaxInFlight requests being handled do.
func fillPlaces(t *chatRoutes) {
	for range cap(t.slots) {
		t.slots <- struct{}{}
	}
}

// Each outcome is counted once, under the platform and the connection of the route its path names. The connection is named once its route
// has been on the listener, so one that is down or restarting is still named; it is "unknown" for one whose route never was, and for a path
// with no route's shape or a method other than POST, which are answered before the route is looked up. The platform is the path's when it
// is one of the listener's, and "other" for any other and for a path that names none.
func TestChatListenerCountsEachOutcomeUnderItsRoute(t *testing.T) {
	post := func(path string) func(*chatRoutes) *http.Request {
		return func(*chatRoutes) *http.Request {
			return httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader("x"))
		}
	}
	unsized := func(body func(*chatRoutes) io.Reader) func(*chatRoutes) *http.Request {
		return func(rt *chatRoutes) *http.Request {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat/slack/ns/a", body(rt))
			req.ContentLength = -1
			return req
		}
	}
	cases := map[string]struct {
		req  func(*chatRoutes) *http.Request
		want listenerKey
	}{
		"served":    {req: post("/chat/slack/ns/a"), want: listenerKey{chatServed, "slack", "ns/a"}},
		"not found": {req: post("/chat/slack/ns"), want: listenerKey{chatNotFound, otherLabel, unknownLabel}},
		"method on a served route": {
			req: func(*chatRoutes) *http.Request {
				return httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/chat/slack/ns/a", nil)
			},
			want: listenerKey{chatMethod, "slack", unknownLabel},
		},
		"no route, its route went":   {req: post("/chat/slack/ns/gone"), want: listenerKey{chatNoRoute, "slack", "ns/gone"}},
		"no route, never a route":    {req: post("/chat/slack/ns/never"), want: listenerKey{chatNoRoute, "slack", unknownLabel}},
		"no route, another platform": {req: post("/chat/github/ns/a"), want: listenerKey{chatNoRoute, otherLabel, unknownLabel}},
		"too large, declared": {
			req: func(*chatRoutes) *http.Request {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat/slack/ns/a", strings.NewReader("x"))
				req.ContentLength = chatMaxBody + 1
				return req
			},
			want: listenerKey{chatTooLarge, "slack", "ns/a"},
		},
		"too large, read": {
			req:  unsized(func(*chatRoutes) io.Reader { return strings.NewReader(strings.Repeat("a", chatMaxBody+1)) }),
			want: listenerKey{chatTooLarge, "slack", "ns/a"},
		},
		"unreadable": {req: unsized(func(*chatRoutes) io.Reader { return errBody{} }), want: listenerKey{chatUnreadable, "slack", "ns/a"}},
		"busy, every place taken": {
			req:  func(rt *chatRoutes) *http.Request { fillPlaces(rt); return post("/chat/slack/ns/a")(rt) },
			want: listenerKey{chatBusy, "slack", "ns/a"},
		},
		"busy, the body does not fit": {
			req:  func(rt *chatRoutes) *http.Request { rt.budget.left = 0; return post("/chat/slack/ns/a")(rt) },
			want: listenerKey{chatBusy, "slack", "ns/a"},
		},
		"busy, the places filled during the read": {
			req:  unsized(func(rt *chatRoutes) io.Reader { return &fillingBody{routes: rt, r: strings.NewReader("x")} }),
			want: listenerKey{chatBusy, "slack", "ns/a"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := mustMetrics(t, nil)
			routes := newChatRoutes(m, testutil.DiscardLogger())
			routes.register(v1alpha1.PlatformSlack, "ns/a", &heard{})
			routes.register(v1alpha1.PlatformSlack, "ns/gone", &heard{}).drop()
			routes.ServeHTTP(httptest.NewRecorder(), tc.req(routes))
			if got, want := listenerSeries(t, m), map[listenerKey]int{tc.want: 1}; !maps.Equal(got, want) {
				t.Errorf("series = %v, want %v", got, want)
			}
		})
	}
}

// The path is the sender's to choose: no part of it becomes a label value unless it names, under its platform, a connection whose route
// was on the listener, so the series cannot grow past the outcomes times the platforms times the connections served, and "unknown".
func TestChatListenerNeverLabelsASenderChosenPath(t *testing.T) {
	m := mustMetrics(t, nil)
	routes := newChatRoutes(m, testutil.DiscardLogger())
	routes.register(v1alpha1.PlatformSlack, "ns/a", &heard{})
	paths := []string{"/chat/slack/ns/b", "/chat/slack/evil/a", "/chat/SLACK/ns/a", "/chat/github/ns/a", "/chat/other/ns/a", "/chat/slack/ns/a/x",
		"/chat/slack/ns/a%2Fx", "/chat/slack/unknown/x", "/chat/" + strings.Repeat("p", 300) + "/ns/a", "/chat/slack/" + strings.Repeat("n", 300) + "/a",
		"/chat//ns/a", "/x/slack/ns/a"}
	for i := range 100 {
		paths = append(paths, "/chat/slack/ns-"+strconv.Itoa(i)+"/name-"+strconv.Itoa(i))
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader("x")))
		}
	}
	got := listenerSeries(t, m)
	for k := range got {
		if !slices.Contains([]string{"ns/a", unknownLabel}, k.connection) || !slices.Contains([]string{"slack", otherLabel}, k.platform) {
			t.Errorf("a series is labelled %+v: a value the sender chose", k)
		}
		if k.connection == "ns/a" && k.platform != "slack" {
			t.Errorf("a series names the served connection under the platform %q, which is not its route's", k.platform)
		}
	}
	if len(got) > len(chatOutcomes)*(len(chatPlatforms)+1)*2 {
		t.Errorf("%d series after %d paths, want them bounded by the outcomes, the platforms and the connections: %v", len(got), len(paths), got)
	}
}

// A connection that is no longer served loses its series of the listener, and a request to its route after that is counted under
// "unknown", not under its name: nothing brings its series back until its route is on the listener again. The other connections keep theirs.
func TestConnectionRemovedDeletesItsListenerSeries(t *testing.T) {
	m := mustMetrics(t, nil)
	routes := newChatRoutes(m, testutil.DiscardLogger())
	regA := routes.register(v1alpha1.PlatformSlack, "ns/a", &heard{})
	routes.register(v1alpha1.PlatformSlack, "ns/b", &heard{})
	serve := func(path string) {
		routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader("x")))
	}
	serve("/chat/slack/ns/a")
	serve("/chat/slack/ns/b")
	regA.drop()
	serve("/chat/slack/ns/a")
	serve("/chat/slack/ns/none")

	m.ConnectionRemoved(t.Context(), "ns/a")

	want := map[listenerKey]int{{chatServed, "slack", "ns/b"}: 1, {chatNoRoute, "slack", unknownLabel}: 1}
	if got := listenerSeries(t, m); !maps.Equal(got, want) {
		t.Errorf("after the removal the series are %v, want %v: ns/a's gone and the rest kept", got, want)
	}
	serve("/chat/slack/ns/a")
	want[listenerKey{chatNoRoute, "slack", unknownLabel}] = 2
	if got := listenerSeries(t, m); !maps.Equal(got, want) {
		t.Errorf("after a request to the removed connection's route the series are %v, want %v: counted as unknown", got, want)
	}
	routes.register(v1alpha1.PlatformSlack, "ns/a", &heard{})
	serve("/chat/slack/ns/a")
	want[listenerKey{chatServed, "slack", "ns/a"}] = 1
	if got := listenerSeries(t, m); !maps.Equal(got, want) {
		t.Errorf("once the connection is served again the series are %v, want %v: named again, from zero", got, want)
	}
}

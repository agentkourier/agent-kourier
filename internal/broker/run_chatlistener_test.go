package broker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// ownRoute is the chat listener's route of the broker's own connection in the test config (api/v1alpha1/testdata).
const ownRoute = "/chat/slack/agent-kourier-system/slack-main"

// offering is a connection's platform with a handler for the chat listener, which no adapter offers yet: it counts the times it is asked.
type offering struct {
	chat.Platform
	h     http.Handler
	asked *atomic.Int32
}

func (p offering) HTTPHandler() (http.Handler, bool) {
	p.asked.Add(1)
	return p.h, true
}

// chatListen gives c a chat listener and has its connections offer h, and returns the listener's address and the count of the times a
// platform was asked for its handler.
func chatListen(t *testing.T, c *RunConfig, h http.Handler) (addr string, asked *atomic.Int32) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c.ChatListener = ln
	asked = &atomic.Int32{}
	c.wrapPlatform = func(p chat.Platform) chat.Platform { return offering{Platform: p, h: h, asked: asked} }
	return ln.Addr().String(), asked
}

// chatPost sends a request to the chat listener at addr and returns its status, or -1 when no connection could be made.
func chatPost(t *testing.T, method, addr, path string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+addr+path, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return -1
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// The leader's own connection has its route on the chat listener while it runs: a POST to it reaches the platform's handler, a connection
// that does not run here is 503, a GET is 405, and the health server's paths are not on the chat port. /metrics, on the health port,
// counts each request by outcome.
func TestRunServesTheChatListenerForTheConnectionsThatRun(t *testing.T) {
	h := &heard{}
	c := startConfig(t)
	addr, asked := chatListen(t, &c, h)
	started := leaderUp(t, &c)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	started()

	code := chatPost(t, http.MethodPost, addr, ownRoute)
	if bodies, _, _ := h.got(); code != http.StatusOK || !slices.Equal(bodies, []string{"payload"}) {
		t.Errorf("POST to the own connection's route = %d, with the handler given %q; want 200 from it with the body", code, bodies)
	}
	if code := chatPost(t, http.MethodPost, addr, "/chat/slack/agent-kourier-system/not-served"); code != http.StatusServiceUnavailable {
		t.Errorf("POST to a connection that does not run here = %d, want 503", code)
	}
	if code := chatPost(t, http.MethodGet, addr, ownRoute); code != http.StatusMethodNotAllowed {
		t.Errorf("GET on the own connection's route = %d, want 405", code)
	}
	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		if code := chatPost(t, http.MethodGet, addr, path); code != http.StatusNotFound {
			t.Errorf("GET %s on the chat listener = %d, want 404: the health server's paths are its own", path, code)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("the platform was asked for its handler %d times, want once as its run began", n)
	}
	r.waitMetric(`agentkourier_chat_listener_requests_total{outcome="served"} 1`)
	r.waitMetric(`agentkourier_chat_listener_requests_total{outcome="no_route"} 1`)
	r.waitMetric(`agentkourier_chat_listener_requests_total{outcome="method"} 1`)
	r.waitMetric(`agentkourier_chat_listener_requests_total{outcome="not_found"} 3`)
}

// At a shutdown the chat listener stops taking requests before the intakes stop, inside the intake step, and the steps are the ones they
// always were: a request in flight is answered while the connection's run still holds, and once the broker has stopped the port takes none.
func TestRunStopsTheChatListenerBeforeTheIntakes(t *testing.T) {
	var runCtx atomic.Pointer[context.Context]
	inFlight, release := make(chan struct{}), make(chan struct{})
	var liveAtAnswer atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(inFlight)
		<-release
		time.Sleep(200 * time.Millisecond) // a halt of the run that came early would be seen within this
		liveAtAnswer.Store((*runCtx.Load()).Err() == nil)
		w.WriteHeader(http.StatusOK)
	})
	slk := &ctxSlack{got: make(chan context.Context, 1)}
	trace := &stepTrace{hook: func(step string) {
		if step == "intake" {
			close(release) // the shutdown has begun: the handler may answer, while the listener waits for it
		}
	}}
	c := startConfig(t)
	c.newSlack = func(slack.Config) slackAPI { return slk }
	c.shutdownTrace = trace.record
	addr, _ := chatListen(t, &c, h)
	started := leaderUp(t, &c)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	ctx := <-slk.got
	runCtx.Store(&ctx)
	started()

	answered := make(chan int, 1)
	go func() { answered <- chatPost(t, http.MethodPost, addr, ownRoute) }()
	<-inFlight
	r.cancel()
	if code := <-answered; code != http.StatusOK || !liveAtAnswer.Load() {
		t.Errorf("the request in flight at the shutdown = %d, with the connection's run live at its answer: %v; want 200 with it live", code, liveAtAnswer.Load())
	}
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after its context ended, want nil", err)
	}
	if want := "intake,interactions,manager,drain,pool,resolver,store,health"; trace.got() != want {
		t.Errorf("Run shut down as %s, want %s", trace.got(), want)
	}
	if code := chatPost(t, http.MethodPost, addr, ownRoute); code != -1 {
		t.Errorf("after the shutdown the chat listener answered %d, want no answer", code)
	}
}

// With no chat listener nothing changes: no platform is asked for a handler, /metrics has no chat listener series, and the shutdown runs
// its steps as it always has.
func TestRunWithoutAChatListenerChangesNothing(t *testing.T) {
	asked := &atomic.Int32{}
	trace := &stepTrace{}
	c := startConfig(t)
	c.wrapPlatform = func(p chat.Platform) chat.Platform { return offering{Platform: p, h: &heard{}, asked: asked} }
	c.shutdownTrace = trace.record
	started := leaderUp(t, &c)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	started()

	if _, body := r.get("/metrics"); strings.Contains(body, "agentkourier_chat_listener_") {
		t.Errorf("/metrics of a broker with no chat listener holds its series:\n%s", grep(body, "agentkourier_chat_listener_"))
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after its context ended, want nil", err)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("a platform was asked for its handler %d times, want never", n)
	}
	if want := "intake,interactions,manager,drain,pool,resolver,store,health"; trace.got() != want {
		t.Errorf("Run shut down as %s, want %s", trace.got(), want)
	}
}

// A chat listener that fails while the broker runs ends Run with an error that names it, as a failed health server does.
func TestRunEndsWhenTheChatListenerFails(t *testing.T) {
	c := startConfig(t)
	chatListen(t, &c, &heard{})
	started := leaderUp(t, &c)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	started()

	_ = c.ChatListener.Close()

	err := r.wait()
	if err == nil || !strings.Contains(err.Error(), "chat listener") || errors.Is(err, ErrWiring) {
		t.Errorf("Run = %v after the chat listener failed, want an error that names it", err)
	}
}

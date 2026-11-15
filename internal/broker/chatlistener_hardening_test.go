package broker

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
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

// answer is what a raw request was answered: its status, its Retry-After, and whether the server closes the connection after it.
type answer struct {
	code       int
	retryAfter string
	closes     bool
}

// rawAnswer sends head, the request line and headers of a request whose body never comes, on a connection of its own, and returns the
// answer and how long it took to come. A server that read the body first would answer only once its read timeout had passed.
func (r *chatRig) rawAnswer(head string) (answer, time.Duration) {
	r.t.Helper()
	conn, err := (&net.Dialer{}).DialContext(r.t.Context(), "tcp", r.srv.Listener.Addr().String())
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.Close() })
	begin := time.Now()
	if _, err := io.WriteString(conn, head+"part of the body"); err != nil {
		r.t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		r.t.Fatalf("no answer to %q: %v", head, err)
	}
	_ = resp.Body.Close()
	return answer{code: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), closes: resp.Close}, time.Since(begin)
}

// An answer that does not read the body (404, 405, 503 for no route, and 413 for a declared length past the cap) comes at once and closes
// the connection: net/http would otherwise read and discard what the client still sends before it answers, as long as the client takes
// (up to the read timeout). A declared 2 MB with no byte of it sent is 413 at once.
func TestChatRoutesAnswerARefusalWithoutReadingTheBody(t *testing.T) {
	r := newChatRig(t)
	r.routes.register(v1alpha1.PlatformSlack, "ns/a", &heard{})
	for name, tc := range map[string]struct {
		head string
		want int
	}{
		"not found": {"POST /other HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\n", http.StatusNotFound},
		"method":    {"PUT /chat/slack/ns/a HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\n", http.StatusMethodNotAllowed},
		"no route":  {"POST /chat/slack/ns/none HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\n", http.StatusServiceUnavailable},
		"too large": {"POST /chat/slack/ns/a HTTP/1.1\r\nHost: x\r\nContent-Length: 2000000\r\n\r\n", http.StatusRequestEntityTooLarge},
	} {
		resp, took := r.rawAnswer(tc.head)
		if resp.code != tc.want || !resp.closes || took > 500*time.Millisecond {
			t.Errorf("%s: %d after %s, closing the connection: %v; want %d at once and closing it", name, resp.code, took, resp.closes, tc.want)
		}
	}
}

// holding is a handler that holds each request until it is released, and says how many it holds.
type holding struct {
	held    atomic.Int32
	release chan struct{}
}

func (b *holding) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	b.held.Add(1)
	defer b.held.Add(-1)
	<-b.release
	w.WriteHeader(http.StatusOK)
}

// At most chatMaxInFlight routed requests are read or handled at once. The next is answered 503 with Retry-After at once, before any of
// its body is read, and counted as busy; once one ends, a request is served again.
func TestChatRoutesCapTheRequestsInHand(t *testing.T) {
	r := newChatRig(t)
	b := &holding{release: make(chan struct{})}
	r.routes.register(v1alpha1.PlatformSlack, "ns/a", b)
	var wg sync.WaitGroup
	for range chatMaxInFlight {
		wg.Go(func() { r.do(http.MethodPost, "/chat/slack/ns/a", strings.NewReader("x")) })
	}
	testutil.Eventually(t, "the handler never held the cap's worth of requests", func() bool { return b.held.Load() == chatMaxInFlight },
		testutil.Within(5*time.Second), testutil.Every(5*time.Millisecond))

	resp, took := r.rawAnswer("POST /chat/slack/ns/a HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\n")
	if resp.code != http.StatusServiceUnavailable || resp.retryAfter != "5" || !resp.closes || took > 500*time.Millisecond {
		t.Errorf("a request past the cap: %d with Retry-After %q after %s; want 503 with 5, at once, closing the connection",
			resp.code, resp.retryAfter, took)
	}
	if got := r.counted(chatBusy); got != 1 {
		t.Errorf("busy counted %d, want 1", got)
	}
	close(b.release)
	wg.Wait()
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/a", strings.NewReader("x")); code != http.StatusOK {
		t.Errorf("once the held requests ended, a request = %d, want 200", code)
	}
}

// stuckRun is a platform whose Run does not return when its context ends until it is released, as a run that is slow to stop.
type stuckRun struct {
	receiving
	release chan struct{}
}

func (p stuckRun) Run(ctx context.Context, _ chat.Sink) error {
	<-ctx.Done()
	<-p.release
	return ctx.Err()
}

// A halted runtime's route goes at halt, not when its run returns: while the run is slow to stop, the route already answers 503.
func TestRuntimeRouteGoesAtHaltWhileTheRunIsSlowToStop(t *testing.T) {
	r := newHostRig(t, true)
	p := stuckRun{receiving: receiving{FakePlatform: chattest.NewFake(), h: &heard{}, offer: true, asked: &atomic.Int32{}}, release: make(chan struct{})}
	rt := r.start("stuck", p, false)
	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/stuck", nil); code != http.StatusOK {
		t.Fatalf("the route of a runtime that runs = %d, want 200", code)
	}

	rt.halt()

	if code, _, _, _ := r.do(http.MethodPost, "/chat/slack/ns/stuck", nil); code != http.StatusServiceUnavailable {
		t.Errorf("the route of a halted runtime whose run has not returned = %d, want 503", code)
	}
	if isClosed(rt.done) {
		t.Error("the run returned before the release: the test did not hold it")
	}
	close(p.release)
	rt.wait(t.Context())
}

// The drain of the chat listener is the first part of the intake step and leaves the intakes, which follow it in the step, at least 2 s
// of the step's bound.
func TestChatDrainLeavesTheIntakesTheirShare(t *testing.T) {
	if left := defaultBounds()[stepIntake] - chatDrainTimeout; left < 2*time.Second {
		t.Errorf("the intake step's bound %s less the drain %s leaves the intakes %s, want at least 2s", defaultBounds()[stepIntake], chatDrainTimeout, left)
	}
}

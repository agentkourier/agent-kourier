package broker

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
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
	conn := r.send(head + "part of the body")
	begin := time.Now()
	return r.answerOn(conn, head), time.Since(begin)
}

// send opens a connection of its own, closed when the test ends, and writes text on it.
func (r *chatRig) send(text string) net.Conn {
	r.t.Helper()
	conn, err := (&net.Dialer{}).DialContext(r.t.Context(), "tcp", r.srv.Listener.Addr().String())
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, text); err != nil {
		r.t.Fatal(err)
	}
	return conn
}

// answerOn reads the answer to the request sent on conn, what.
func (r *chatRig) answerOn(conn net.Conn, what string) answer {
	r.t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		r.t.Fatalf("no answer to %q: %v", what, err)
	}
	_ = resp.Body.Close()
	return answer{code: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), closes: resp.Close}
}

// bodyHeld is the part of the body budget that requests being read or handled hold.
func (r *chatRig) bodyHeld() int {
	b := &r.routes.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	return chatBodyBudget - b.left
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

// At most chatMaxInFlight routed requests are handled at once. While they are, the next is answered 503 with Retry-After at once, before
// any of its body is read, and counted as busy; once one ends, a request is served again.
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

// A client that sends its body slowly holds one chunk of the body budget while it is read, and no place among the handlers: with twice
// as many slow senders mid-body as there are places, a request sent whole is still served, and its chunk is given back once it is.
func TestChatRoutesServeWhileSlowSendersHoldTheirBodies(t *testing.T) {
	r := newChatRig(t)
	h := &heard{}
	r.routes.register(v1alpha1.PlatformSlack, "ns/a", h)
	slow := 2 * chatMaxInFlight
	for range slow {
		r.send("POST /chat/slack/ns/a HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\npart of the body")
	}
	testutil.Eventually(t, "the slow senders were never all being read", func() bool { return r.bodyHeld() == slow*chatBodyChunk },
		testutil.Within(5*time.Second), testutil.Every(5*time.Millisecond))

	if code, _, _, text := r.do(http.MethodPost, "/chat/slack/ns/a", strings.NewReader("payload=1")); code != http.StatusOK || text != "heard" {
		t.Errorf("a whole request while %d slow senders were mid-body = %d %q, want its handler's 200", slow, code, text)
	}
	if held := r.bodyHeld(); held != slow*chatBodyChunk {
		t.Errorf("after the request was served the budget held %d bytes, want the slow senders' %d", held, slow*chatBodyChunk)
	}
	if got := r.counted(chatBusy); got != 0 {
		t.Errorf("busy counted %d, want 0: slow senders take no place", got)
	}
}

// A request whose next chunk does not fit in what is left of the body budget is answered 503 with Retry-After, its connection closed, and
// counted as busy; its handler is not called and its chunks are given back, so a request that fits is served after it.
func TestChatRoutesAnswerBusyPastTheBodyBudget(t *testing.T) {
	r := newChatRig(t)
	h := &heard{}
	r.routes.register(v1alpha1.PlatformSlack, "ns/a", h)
	r.routes.budget.left = 2 * chatBodyChunk // no request has been sent: the server reads it only after the first is

	head := "POST /chat/slack/ns/a HTTP/1.1\r\nHost: x\r\nContent-Length: " + strconv.Itoa(2*chatBodyChunk+1) + "\r\n\r\n"
	resp := r.answerOn(r.send(head+strings.Repeat("a", 2*chatBodyChunk+1)), head)
	if resp.code != http.StatusServiceUnavailable || resp.retryAfter != "5" || !resp.closes {
		t.Errorf("a body of three chunks with two left: %d with Retry-After %q, closing the connection: %v; want 503 with 5, closing it",
			resp.code, resp.retryAfter, resp.closes)
	}
	if h.calls() != 0 || r.counted(chatBusy) != 1 {
		t.Errorf("the handler was called %d times and busy counted %d, want none and 1", h.calls(), r.counted(chatBusy))
	}
	if held := r.bodyHeld(); held != chatBodyBudget-2*chatBodyChunk {
		t.Errorf("after the refusal the requests held %d bytes of the budget, want none", held-(chatBodyBudget-2*chatBodyChunk))
	}
	if code, _, _, text := r.do(http.MethodPost, "/chat/slack/ns/a", strings.NewReader("payload=1")); code != http.StatusOK || text != "heard" {
		t.Errorf("a body of one chunk after the refusal = %d %q, want its handler's 200", code, text)
	}
}

// A request takes its place among the handlers only once its body is read: one whose body was read while the last places were taken is
// answered 503 with Retry-After then, and counted as busy.
func TestChatRoutesAnswerBusyWhenTheHandlersFillDuringTheRead(t *testing.T) {
	r := newChatRig(t)
	b := &holding{release: make(chan struct{})}
	r.routes.register(v1alpha1.PlatformSlack, "ns/a", b)
	head := "POST /chat/slack/ns/a HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n"
	late := r.send(head + "part")
	testutil.Eventually(t, "the late request was never being read", func() bool { return r.bodyHeld() == chatBodyChunk },
		testutil.Within(5*time.Second), testutil.Every(5*time.Millisecond))
	var wg sync.WaitGroup
	defer func() { close(b.release); wg.Wait() }()
	for range chatMaxInFlight {
		wg.Go(func() { r.do(http.MethodPost, "/chat/slack/ns/a", strings.NewReader("x")) })
	}
	testutil.Eventually(t, "the handler never held the cap's worth of requests", func() bool { return b.held.Load() == chatMaxInFlight },
		testutil.Within(5*time.Second), testutil.Every(5*time.Millisecond))

	if _, err := io.WriteString(late, "123456"); err != nil {
		t.Fatal(err)
	}
	if resp := r.answerOn(late, head); resp.code != http.StatusServiceUnavailable || resp.retryAfter != "5" {
		t.Errorf("a request whose body was read while the handlers filled: %d with Retry-After %q, want 503 with 5", resp.code, resp.retryAfter)
	}
	if got := r.counted(chatBusy); got != 1 {
		t.Errorf("busy counted %d, want 1", got)
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

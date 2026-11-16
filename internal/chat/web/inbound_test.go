package web_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

var epoch = time.Unix(1_800_000_000, 0)

// sink is a chat.Sink that keeps what it is given, and the contexts it is given it with.
type sink struct {
	mu    sync.Mutex
	got   []chat.Inbound
	ctxs  []context.Context
	fail  error
	block bool // wait for the context to end, and fail with its error
	in    atomic.Int32
	twice atomic.Bool // two calls were in at once
}

func (s *sink) Message(ctx context.Context, m chat.Inbound) error {
	if s.in.Add(1) > 1 {
		s.twice.Store(true)
	}
	defer s.in.Add(-1)
	s.mu.Lock()
	block, fail := s.block, s.fail
	s.ctxs = append(s.ctxs, ctx)
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if fail != nil {
		return fail
	}
	time.Sleep(time.Millisecond) // long enough for a second call to come in, were the platform to let it
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, m)
	return nil
}

func (s *sink) Interaction(context.Context, chat.Interaction) error { return nil }

func (s *sink) messages() []chat.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]chat.Inbound(nil), s.got...)
}

func newPlatform(t *testing.T, p web.Profile, url string, log *slog.Logger) *web.Platform {
	t.Helper()
	if log == nil {
		log = testutil.DiscardLogger()
	}
	pl, err := web.New(web.Config{Connection: "ns/web", Profile: p, URL: url, Now: func() time.Time { return epoch }, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

type ctxKey struct{}

// running runs p on sk until the test ends, with a value in Run's context that the sink can look for, and returns when it takes
// requests. stop ends the run and waits for it.
func running(t *testing.T, p *web.Platform, sk chat.Sink) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKey{}, "run"))
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, sk) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); _ = <-done }) }
	t.Cleanup(stop)
	testutil.Eventually(t, "the platform to take requests", p.Running)
	return stop
}

func request(method, body string, signed bool) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, "/chat/web/ns/web", strings.NewReader(body))
	if signed {
		req.Header.Set(headerKey, fakeKey)
	}
	return req
}

func serve(t *testing.T, p *web.Platform, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	h, ok := p.HTTPHandler()
	if !ok {
		t.Fatal("the platform has no handler")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

const event = `{"Channel":"ch-1","Thread":"m-0","ID":"m-1","User":"u-1","Text":"hello <!channel>","Mentioned":true}`

func TestTheHandlerTakesAVerifiedEventToTheSink(t *testing.T) {
	p := newPlatform(t, newTestProfile(false), "", nil)
	sk := &sink{}
	running(t, p, sk)

	w := serve(t, p, request(http.MethodPost, event, true))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d (%s), want 202", w.Code, w.Body)
	}
	want := chat.Inbound{At: chat.Ref{Platform: "web", Conn: "ns/web", Channel: "ch-1", Thread: "m-0", ID: "m-1"}, User: "u-1", Text: "hello <!channel>", Mentioned: true}
	if got := sk.messages(); len(got) != 1 || got[0] != want {
		t.Errorf("the sink got %+v, want [%+v]", got, want)
	}
}

func TestTheHandlerRefusesWhatItCannotTake(t *testing.T) {
	prof := newTestProfile(false)
	prof.maxBody = 256
	p := newPlatform(t, prof, "", nil)
	sk := &sink{}
	running(t, p, sk)

	for _, tc := range []struct {
		name string
		req  *http.Request
		want int
	}{
		{"GET", request(http.MethodGet, event, true), http.StatusMethodNotAllowed},
		{"not verified", request(http.MethodPost, event, false), http.StatusUnauthorized},
		{"not the schema", request(http.MethodPost, `{"Channel":"ch-1","URL":"http://evil.example/"}`, true), http.StatusBadRequest},
		{"over the profile's bound", request(http.MethodPost, `{"Text":"`+strings.Repeat("a", 300)+`"}`, true), http.StatusRequestEntityTooLarge},
		{"no channel", request(http.MethodPost, `{"ID":"m-1","User":"u-1"}`, true), http.StatusBadRequest},
		{"no ID", request(http.MethodPost, `{"Channel":"ch-1","User":"u-1"}`, true), http.StatusBadRequest},
		{"a colon in the ID", request(http.MethodPost, `{"Channel":"ch-1","ID":"a:b","User":"u-1"}`, true), http.StatusBadRequest},
		{"a colon in the channel", request(http.MethodPost, `{"Channel":"c:1","ID":"m-1","User":"u-1"}`, true), http.StatusBadRequest},
		{"a colon in the thread", request(http.MethodPost, `{"Channel":"ch-1","Thread":"t:1","ID":"m-1","User":"u-1"}`, true), http.StatusBadRequest},
	} {
		if w := serve(t, p, tc.req); w.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, w.Code, tc.want)
		}
	}
	if got := sk.messages(); len(got) != 0 {
		t.Errorf("the sink got %d messages, want none", len(got))
	}
}

// A request that carries nothing to take, such as a sender's ping, is acknowledged, and nothing reaches the sink.
func TestAnEventWithNothingToTakeIsAcknowledged(t *testing.T) {
	p := newPlatform(t, newTestProfile(false), "", nil)
	sk := &sink{}
	running(t, p, sk)

	if w := serve(t, p, request(http.MethodPost, `{"Ignore":true}`, true)); w.Code != http.StatusAccepted {
		t.Errorf("status %d, want 202", w.Code)
	}
	if got := sk.messages(); len(got) != 0 {
		t.Errorf("the sink got %+v, want nothing", got)
	}
}

// An event that cannot be taken now, because the connection is not running here or the sink did not take it, is answered 503 with
// a Retry-After: the sender sends it again.
func TestTheHandlerAnswers503WhenItCannotTakeTheEventNow(t *testing.T) {
	p := newPlatform(t, newTestProfile(false), "", nil)
	unavailable := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if want := strconv.Itoa(int(web.BusyRetryAfter / time.Second)); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != want {
			t.Errorf("%s: status %d, Retry-After %q; want 503 with a Retry-After of %s", name, w.Code, w.Header().Get("Retry-After"), want)
		}
	}
	unavailable("before Run", serve(t, p, request(http.MethodPost, event, true)))

	sk := &sink{fail: errors.New("the store is busy")}
	stop := running(t, p, sk)
	unavailable("a sink that fails", serve(t, p, request(http.MethodPost, event, true)))

	sk.mu.Lock()
	sk.fail = nil
	sk.mu.Unlock()
	if w := serve(t, p, request(http.MethodPost, event, true)); w.Code != http.StatusAccepted {
		t.Errorf("the retry: status %d, want 202", w.Code)
	}

	h, _ := p.HTTPHandler() // the broker may serve the route a moment after the run ends
	stop()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request(http.MethodPost, event, true))
	unavailable("after Run", w)
	if got := sk.messages(); len(got) != 1 {
		t.Errorf("the sink got %d messages, want the one of the retry", len(got))
	}
}

// The sink is called on a context of Run's, which carries what the broker put there (its telemetry), and not on the request's
// alone, which carries none of it.
func TestTheSinkIsCalledOnRunsContext(t *testing.T) {
	p := newPlatform(t, newTestProfile(false), "", nil)
	sk := &sink{}
	stop := running(t, p, sk)

	serve(t, p, request(http.MethodPost, event, true))

	sk.mu.Lock()
	ctxs := sk.ctxs
	sk.mu.Unlock()
	if len(ctxs) != 1 {
		t.Fatalf("the sink was called %d times, want once", len(ctxs))
	}
	if got := ctxs[0].Value(ctxKey{}); got != "run" {
		t.Errorf("the sink's context has %v, want Run's value", got)
	}
	stop()
	if ctxs[0].Err() == nil {
		t.Error("the sink's context has not ended with Run")
	}
}

// A sink that has not taken the event within the wait is answered 503, and the sink's context ends with the answer, so that it does
// not take the event after the sender was told to send it again.
func TestTheHandlerAnswers503WhenTheSinkDoesNotAnswerInTime(t *testing.T) {
	p := newPlatform(t, newTestProfile(false), "", nil)
	p.SetAckWait(20 * time.Millisecond)
	sk := &sink{block: true}
	running(t, p, sk)

	start := time.Now()
	w := serve(t, p, request(http.MethodPost, event, true))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", w.Code)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the answer took %s, want about the wait", d)
	}
	sk.mu.Lock()
	defer sk.mu.Unlock()
	if len(sk.ctxs) != 1 || sk.ctxs[0].Err() == nil {
		t.Error("the sink's context did not end with the answer")
	}
}

// The events of a connection reach the sink one at a time, in the order the handler takes them (chat.Sink).
func TestOneEventAtATimeReachesTheSink(t *testing.T) {
	p := newPlatform(t, newTestProfile(false), "", nil)
	sk := &sink{}
	running(t, p, sk)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if w := serve(t, p, request(http.MethodPost, event, true)); w.Code != http.StatusAccepted {
				t.Errorf("status %d, want 202", w.Code)
			}
		})
	}
	wg.Wait()

	if sk.twice.Load() {
		t.Error("two events were in the sink at once")
	}
	if got := len(sk.messages()); got != 16 {
		t.Errorf("the sink got %d messages, want 16", got)
	}
}

// Every delivery of an event is handed on, each with the event's own ID: the reply queue drops a repeat by its chat.MessageID, durably.
func TestARepeatedDeliveryKeepsItsMessageID(t *testing.T) {
	p := newPlatform(t, newTestProfile(false), "", nil)
	sk := &sink{}
	running(t, p, sk)

	serve(t, p, request(http.MethodPost, event, true))
	serve(t, p, request(http.MethodPost, event, true))

	got := sk.messages()
	if len(got) != 2 || chat.MessageID(got[0].At) != chat.MessageID(got[1].At) {
		t.Errorf("the sink got %+v, want two messages with one MessageID", got)
	}
}

// What a refused request held, its body and its headers, is never logged: the reason is the platform's or the profile's own words.
func TestTheHandlerLogsNoBodyAndNoSignature(t *testing.T) {
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := newPlatform(t, newTestProfile(false), "", log)
	sk := &sink{}
	running(t, p, sk)

	const secretText = "zq-body-text"
	forged := request(http.MethodPost, `{"Text":"`+secretText+`"}`, false)
	forged.Header.Set(headerKey, "zq-forged-key")
	serve(t, p, forged)
	serve(t, p, request(http.MethodPost, `{"Text":"`+secretText+`","Bad":1}`, true))
	sk.mu.Lock()
	sk.fail = errors.New("the store is busy")
	sk.mu.Unlock()
	serve(t, p, request(http.MethodPost, `{"Channel":"ch-1","ID":"m-1","User":"u-1","Text":"`+secretText+`"}`, true))

	out := buf.String()
	if out == "" {
		t.Fatal("nothing was logged: the test proves nothing")
	}
	for _, leak := range []string{secretText, "zq-forged-key", fakeKey} {
		if strings.Contains(out, leak) {
			t.Errorf("the log holds %q:\n%s", leak, out)
		}
	}
}

// syncBuffer is a bytes.Buffer that a logger may write to from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A handler that loaded the Run just before the Run ended hands its event to no Sink: the Sink of a Run that returned is the broker's
// no longer. A hook holds the handler between the load and the check while the Run ends. The race is run many times, since without
// the check the handler reaches the Sink only when its wait for the order loses to the Run's ended context, about half the time.
func TestAHandlerHandsNoEventToARunThatEnded(t *testing.T) {
	for i := range 32 {
		p := newPlatform(t, newTestProfile(false), "", nil)
		h, _ := p.HTTPHandler()
		loaded, release := make(chan struct{}), make(chan struct{})
		p.SetAfterLoad(func() { close(loaded); <-release })
		sk := &sink{}
		ctx, cancel := context.WithCancel(t.Context())
		ran := make(chan error, 1)
		go func() { ran <- p.Run(ctx, sk) }()
		testutil.Eventually(t, "the platform to take requests", p.Running)

		w := httptest.NewRecorder()
		answered := make(chan struct{})
		go func() { defer close(answered); h.ServeHTTP(w, request(http.MethodPost, event, true)) }()
		<-loaded
		cancel()
		_ = <-ran
		close(release)
		<-answered
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("try %d: answered %d, want 503: the Run had ended", i, w.Code)
		}
		if got := sk.messages(); len(got) != 0 {
			t.Fatalf("try %d: the Sink of an ended Run got %d events", i, len(got))
		}
	}
}

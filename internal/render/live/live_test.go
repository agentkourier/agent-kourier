package live_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	slackgo "github.com/slack-go/slack"
	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

const throttle = 2 * time.Second

// clock is a hand-turned clock: After waits until advance has moved time far enough.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []timer
}

type timer struct {
	at   time.Time
	ch   chan time.Time
	long bool // the final text's deadline, which no test waits on
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, timer{c.now.Add(d), ch, d == 10*time.Minute})
	return ch
}

// due is when the earliest timer fires.
func (c *clock) due() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var earliest time.Time
	for _, tm := range c.timers {
		if !tm.long && (earliest.IsZero() || tm.at.Before(earliest)) {
			earliest = tm.at
		}
	}
	return earliest
}

// waiting is how many timers are pending, but for the final text's deadline.
func (c *clock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, tm := range c.timers {
		if !tm.long {
			n++
		}
	}
	return n
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	keep := c.timers[:0]
	for _, tm := range c.timers {
		if !tm.at.After(c.now) {
			tm.ch <- c.now
		} else {
			keep = append(keep, tm)
		}
	}
	c.timers = keep
}

// recorder is the HTTP transport of the Slack client: it keeps every request's form, and fails
// the first failUpdates chat.update requests.
type recorder struct {
	mu          sync.Mutex
	order       []string // the paths of the requests, in order
	forms       map[string][]url.Values
	failUpdates int
	rateLimited int // the next chat.update requests answered 429, with a Retry-After of 30 s
	hangUpdates int // the next chat.update requests never answer, until their context ends
	losePosts   int // the next chat.postMessage requests reach Slack, and their answers are lost
	// holdStart, if set, makes chat.startStream wait for it to close, as a slow Slack would. A request
	// whose context ends while it waits is still taken by Slack, though its answer is lost.
	holdStart chan struct{}
	held      bool
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(raw))
	form, _ := url.ParseQuery(string(raw))
	r.mu.Lock()
	r.forms[req.URL.Path] = append(r.forms[req.URL.Path], form)
	r.order = append(r.order, req.URL.Path)
	fail := req.URL.Path == "/chat.update" && r.failUpdates > 0
	if fail {
		r.failUpdates--
	}
	lose := req.URL.Path == "/chat.postMessage" && r.losePosts > 0
	if lose {
		r.losePosts--
	}
	hold := r.holdStart
	if req.URL.Path == "/chat.startStream" && hold != nil {
		r.held = true
	} else {
		hold = nil
	}
	limited := req.URL.Path == "/chat.update" && r.rateLimited > 0
	if limited {
		r.rateLimited--
	}
	hang := req.URL.Path == "/chat.update" && r.hangUpdates > 0
	if hang {
		r.hangUpdates--
	}
	r.mu.Unlock()
	if hang {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	if hold != nil {
		select {
		case <-hold:
		case <-req.Context().Done():
			if resp, err := http.DefaultTransport.RoundTrip(req); err == nil {
				_ = resp.Body.Close()
			}
			return nil, req.Context().Err()
		}
	}
	if lose {
		if resp, err := http.DefaultTransport.RoundTrip(req); err == nil {
			_ = resp.Body.Close()
		}
		return nil, errors.New("the connection was lost after the post")
	}
	if limited {
		return &http.Response{StatusCode: 429, Status: "429 Too Many Requests", Header: http.Header{"Retry-After": {"30"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	if fail {
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"internal_error"}`)), Request: req}, nil
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (r *recorder) holding() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held
}

func (r *recorder) orderOf() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.order)
}

func timeoutAfter() <-chan time.Time { return time.After(5 * time.Second) }

func (r *recorder) sent(path string) []url.Values {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]url.Values(nil), r.forms[path]...)
}

// recordFail is the outbox store, which can be made to fail to record a message ref: a crash.
type recordFail struct {
	store.Outbox
	fail atomic.Bool
}

func (s *recordFail) RecordMessageRef(ctx context.Context, id, ref string) error {
	if s.fail.Load() {
		return errors.New("the process died")
	}
	return s.Outbox.RecordMessageRef(ctx, id, ref)
}

// runsAlive says whether a live.Run goroutine is still running.
func runsAlive() bool {
	buf := make([]byte, 1<<20)
	return strings.Contains(string(buf[:runtime.Stack(buf, true)]), "live.(*Run).run")
}

type env struct {
	fake  *fakeslack.Server
	rec   *recorder
	clk   *clock
	store *sqlite.Store
	ob    *outbox.Outbox
	rf    *recordFail
	cfg   live.Config // what live was made from, for a test that needs another Renderer
	live  *live.Renderer
	root  string
	logs  *bytes.Buffer
}

func setup(t *testing.T) *env {
	t.Helper()
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		goleak.VerifyNone(t, before)
	})
	e := &env{fake: fakeslack.New(), rec: &recorder{forms: map[string][]url.Values{}}, clk: &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}, logs: &bytes.Buffer{}}
	t.Cleanup(e.fake.Close)
	_, root, err := slackgo.New("xoxb-fake", slackgo.OptionAPIURL(e.fake.URL()+"/")).PostMessage("C1", slackgo.MsgOptionText("root", false))
	if err != nil {
		t.Fatal(err)
	}
	e.root = root
	client := slack.New(slack.Config{BotToken: secret.New("xoxb-fake"), AppToken: secret.New("xapp-fake"), APIURL: e.fake.URL() + "/", HTTPClient: &http.Client{Transport: e.rec}})
	if e.store, err = sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "live.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.Close() })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	platform := slack.NewPlatform(client, "ns/conn")
	e.ob = outbox.New(outbox.Config{Store: e.store, Platform: platform, Logger: quiet})
	e.rf = &recordFail{Outbox: e.store}
	e.cfg = live.Config{Sender: e.ob, Platform: platform, Store: e.rf, Throttle: throttle, FinalDeadline: 10 * time.Minute, Now: e.clk.Now, After: e.clk.After, Logger: slog.New(slog.NewTextHandler(e.logs, nil))}
	e.live = live.New(e.cfg)
	t.Cleanup(func() { // a run that saw its final goes on without its context: turn the clock for it
		for deadline := time.Now().Add(3 * time.Second); runsAlive() && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			e.clk.advance(throttle)
		}
	})
	return e
}

func (e *env) start(t *testing.T, mode v1alpha1.OutputMode, recovered bool) (*live.Run, context.CancelFunc) {
	return e.startFor(t, live.Turn{Mode: mode, Recovered: recovered})
}

// startFor starts a turn of the test's thread; turn holds the rest.
func (e *env) startFor(t *testing.T, turn live.Turn) (*live.Run, context.CancelFunc) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	turn.Source, turn.Channel, turn.ThreadTS = "turn-1", "C1", e.root
	return e.live.Start(ctx, turn), cancel
}

// reply is the last message in the thread.
func (e *env) reply() fakeslack.Message { th := e.fake.Thread("C1", e.root); return th[len(th)-1] }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// finish turns the clock until the run is done.
func (e *env) finish(t *testing.T, run *live.Run) {
	t.Helper()
	eventually(t, "the run to end", func() bool {
		select {
		case <-run.Done():
			return true
		default:
		}
		if e.clk.waiting() > 0 {
			e.clk.advance(throttle)
		}
		return false
	})
}

func text(id, s string) agent.TextEvent {
	return agent.TextEvent{ArtifactID: a2a.ArtifactID(id), Text: s}
}

func TestDeltasInOneWindowAreOneUpdate(t *testing.T) {
	e := setup(t)
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "Hel"))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(text("a", "Hello"))
	eventually(t, "the update to wait for its window", func() bool { return e.clk.waiting() == 1 })
	for _, s := range []string{"Hello wor", "Hello world"} { // more deltas while it waits
		run.Handle(text("a", s))
	}
	if n := len(e.rec.sent("/chat.update")); n != 0 {
		t.Fatalf("%d updates inside the window; want none", n)
	}
	e.clk.advance(throttle)
	eventually(t, "the latest text", func() bool { return e.reply().Text == "Hello world" })
	if n := len(e.rec.sent("/chat.update")); n != 1 {
		t.Fatalf("%d updates for the window; want one", n)
	}
	// The first post went through the outbox: its row holds the ts, and the message carries its tag.
	row, err := e.store.GetOutbox(t.Context(), outbox.ID("turn-1", "reply"))
	if m := e.reply(); err != nil || row.MessageRef != m.TS || m.Metadata == nil || m.Metadata.EventPayload["outbox_id"] != "turn-1/reply" {
		t.Fatalf("outbox row %+v, %v; message %+v; want the ts recorded and the message tagged", row, err, m)
	}
}

func TestFinalTextIsTheLastUpdateAndToolLinesAreDropped(t *testing.T) {
	e := setup(t)
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "Looking"))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search", Args: map[string]any{"q": "pods"}})
	eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
	e.clk.advance(throttle)
	eventually(t, "the live tool line", func() bool { return strings.Contains(string(e.reply().Blocks), "Calling search") })
	if !strings.Contains(string(e.reply().Blocks), `"context"`) || !strings.Contains(string(e.reply().Blocks), `"plain_text"`) {
		t.Fatalf("blocks %s; want the tool line in a plain_text context block, not the markdown block", e.reply().Blocks)
	}
	run.Handle(agent.ToolResultEvent{CallID: "c1", Name: "search"})
	run.Handle(text("a", "Looking again"))
	eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
	e.clk.advance(throttle)
	eventually(t, "the tool line to go with its result", func() bool {
		return e.reply().Text == "Looking again" && !strings.Contains(string(e.reply().Blocks), "Calling")
	})
	run.Handle(agent.ToolCallEvent{CallID: "c2", Name: "search"}) // still running when the turn ends
	run.Handle(text("a", "Looking is done: all fine"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	if m.Text != "Looking is done: all fine" || !strings.Contains(string(m.Blocks), "all fine") || strings.Contains(string(m.Blocks), "Calling") {
		t.Fatalf("final message %+v; want the full final text, with no tool line", m)
	}
	// Every request is inert, and every update sends its blocks again.
	for _, path := range []string{"/chat.postMessage", "/chat.update"} {
		for _, f := range e.rec.sent(path) {
			if f.Get("unfurl_links") != "false" || f.Get("unfurl_media") != "false" || f.Has("link_names") || f.Get("parse") == "full" {
				t.Errorf("%s request is not inert: %v", path, f)
			}
			if path == "/chat.update" && f.Get("blocks") == "" {
				t.Errorf("an update sent no blocks: %v", f)
			}
		}
	}
}

func TestRecoveredTurnUpdatesTheMessageItPosted(t *testing.T) {
	e := setup(t)
	if _, err := e.ob.Send(t.Context(), outbox.Post{Source: "turn-1", Role: "reply", Channel: "C1", ThreadTS: e.root, Out: chat.Say(chat.Message{Fallback: "before the crash"})}); err != nil {
		t.Fatal(err)
	}
	run, _ := e.start(t, v1alpha1.OutputLive, true)
	run.Handle(text("a", "the whole answer"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if posts := len(e.rec.sent("/chat.postMessage")); posts != 1 || e.reply().Text != "the whole answer" { // the earlier post, and no more
		t.Fatalf("%d posts, message %q; want the earlier message updated to the whole answer, with no second post", posts, e.reply().Text)
	}
}

func TestFailedUpdateIsRetriedAndLosesNoText(t *testing.T) {
	e := setup(t)
	e.rec.failUpdates = 2
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "first"))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(text("a", "SECRETTEXT final answer"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := e.reply().Text; got != "SECRETTEXT final answer" {
		t.Fatalf("message %q after failed updates; want the final text", got)
	}
	if n := len(e.rec.sent("/chat.update")); n != 3 {
		t.Fatalf("%d update attempts; want 2 failures and a success", n)
	}
	if strings.Contains(e.logs.String(), "SECRETTEXT") || !strings.Contains(e.logs.String(), "internal_error") {
		t.Fatalf("log %q; want the failure, without the text", e.logs.String())
	}
}

// Wait is for the shutdown: it returns once every run has ended, so that no render goroutine outlives the
// broker that started it. A run that has seen its final text is still writing it after the turn's
// context has ended, and Wait is what waits for that.
func TestWaitReturnsOnceEveryRunHasEnded(t *testing.T) {
	e := setup(t)
	run, cancel := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "the answer"))
	run.Handle(agent.FinalEvent{})
	cancel() // the turn is over, and its run is still writing the final text
	waited := make(chan error, 1)
	go func() { waited <- e.live.Wait(t.Context()) }()

	e.finish(t, run)

	select {
	case err := <-waited:
		if err != nil {
			t.Errorf("Wait = %v, want nil once the run had ended", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the run ended")
	}
}

func TestWaitGivesUpWhenItsContextEndsAndLeavesTheRunToItsOwn(t *testing.T) {
	e := setup(t)
	run, cancel := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "partial"))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(text("a", "partial, more")) // the run waits for its window and does not end of itself
	ctx, end := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer end()

	err := e.live.Wait(ctx)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait = %v, want the context's deadline, the run being still alive", err)
	}
	cancel()
	<-run.Done()
	if err := e.live.Wait(t.Context()); err != nil {
		t.Errorf("Wait after the run ended = %v, want nil", err)
	}
}

func TestAbandonedTurnEndsWithItsContext(t *testing.T) {
	e := setup(t)
	run, cancel := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "partial"))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(text("a", "partial, more")) // leaves the goroutine waiting for its window
	eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
	cancel()
	select {
	case <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end when its context did")
	}
	run.Handle(text("a", "after the end")) // must not block or panic
}

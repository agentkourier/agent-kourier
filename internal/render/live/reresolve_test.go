package live_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// chatRuntime is the platform of one run of a connection, as the broker gives it out: the fake of an app behind a gate that, once the
// runtime is retired, fails every call at once with an error that wraps chat.ErrConnectionDown (the broker's authWatch does the same),
// and, when the token is revoked, fails an edit as a platform does that rejects the credential. Two runtimes over one fake are one app
// that was restarted: the messages are the app's, whichever runtime made the call.
type chatRuntime struct {
	*chattest.FakePlatform
	conn    string
	retired atomic.Bool
	revoked atomic.Bool
	updates atomic.Int32 // edits that reached the fake
}

func (r *chatRuntime) gate() error {
	if r.retired.Load() {
		return fmt.Errorf("the runtime of %s was retired: %w", r.conn, chat.ErrConnectionDown)
	}
	return nil
}

func (r *chatRuntime) Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error) {
	if err := r.gate(); err != nil {
		return chat.Ref{}, err
	}
	return r.FakePlatform.Post(ctx, t, o, tag)
}

func (r *chatRuntime) Find(ctx context.Context, t chat.Target, tag string, since time.Time) (chat.Ref, bool, error) {
	if err := r.gate(); err != nil {
		return chat.Ref{}, false, err
	}
	return r.FakePlatform.Find(ctx, t, tag, since)
}

func (r *chatRuntime) Update(ctx context.Context, ref chat.Ref, o chat.Outgoing) error {
	if err := r.gate(); err != nil {
		return err
	}
	if r.revoked.Load() {
		return &chattest.FakeError{Verdict: chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"}, Msg: "the token was revoked"}
	}
	err := r.FakePlatform.Update(ctx, ref, o)
	if err == nil {
		r.updates.Add(1)
	}
	return err
}

func (r *chatRuntime) Stream(ctx context.Context, t chat.Target, to, markdown string) (chat.Stream, error) {
	if err := r.gate(); err != nil {
		return nil, err
	}
	s, err := r.FakePlatform.Stream(ctx, t, to, markdown)
	if err != nil {
		return nil, err
	}
	return gated{s, r}, nil
}

func (r *chatRuntime) StreamAt(ref chat.Ref) chat.Stream {
	return gated{r.FakePlatform.StreamAt(ref), r}
}

// RefAt names the connection, as the adapter's Refs do.
func (r *chatRuntime) RefAt(channel, thread, id string) chat.Ref {
	ref := r.FakePlatform.RefAt(channel, thread, id)
	ref.Conn = r.conn
	return ref
}

type gated struct {
	chat.Stream
	r *chatRuntime
}

func (g gated) Append(ctx context.Context, md string) error {
	if err := g.r.gate(); err != nil {
		return err
	}
	return g.Stream.Append(ctx, md)
}

func (g gated) Step(ctx context.Context, st chat.Step) error {
	if err := g.r.gate(); err != nil {
		return err
	}
	return g.Stream.Step(ctx, st)
}

func (g gated) Stop(ctx context.Context, md string, final chat.Message, tag string) error {
	if err := g.r.gate(); err != nil {
		return err
	}
	return g.Stream.Stop(ctx, md, final, tag)
}

// table is the runtimes that run, by connection: the Resolver of the renderer and the outbox, as the broker's runtimeTable is. A
// connection with none is down.
type table struct {
	mu    sync.Mutex
	m     map[string]*chatRuntime
	asked atomic.Int32 // lookups by the renderer
}

func (t *table) set(conn string, r *chatRuntime) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[conn] = r
}

func (t *table) get(conn string) (*chatRuntime, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.m[conn]
	if !ok {
		return nil, fmt.Errorf("%w: %q", chat.ErrConnectionDown, conn)
	}
	return r, nil
}

// Platform implements chat.Resolver. It is the renderer's, and counts.
func (t *table) Platform(conn string) (chat.Platform, error) {
	t.asked.Add(1)
	r, err := t.get(conn)
	if err != nil {
		return nil, err
	}
	return r, nil
}

type tableOutbox struct{ *table }

func (o tableOutbox) Platform(conn string) (outbox.Platform, error) {
	r, err := o.get(conn)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// app is a renderer and an outbox over a table, with one fake behind each runtime of the connection.
type app struct {
	fake  *chattest.FakePlatform
	table *table
	store *sqlite.Store
	live  *live.Renderer
}

const appConn = "ns/team"

// newApp makes an app whose connection runs only if running says so.
func newApp(t *testing.T, cfg live.Config, running bool) (*app, *chatRuntime) {
	t.Helper()
	a := &app{fake: chattest.NewFake(), table: &table{m: map[string]*chatRuntime{}}}
	first := a.newRuntime()
	if running {
		a.table.set(appConn, first)
	}
	a.store = sqlitetest.Open(t)
	quiet := testutil.DiscardLogger()
	ob := newOutbox(t, outbox.Config{Store: a.store, Platforms: tableOutbox{a.table}, Logger: quiet, CallTimeout: time.Second})
	cfg.Sender, cfg.Platforms, cfg.Store, cfg.Logger = ob, a.table, a.store, quiet
	if cfg.Throttle == 0 {
		cfg.Throttle = 2 * time.Millisecond
	}
	a.live = newRenderer(t, cfg)
	return a, first
}

// newRuntime is a new runtime of the app.
func (a *app) newRuntime() *chatRuntime { return &chatRuntime{FakePlatform: a.fake, conn: appConn} }

func (a *app) turn(mode v1alpha1.OutputMode, recipient string) live.Turn {
	return live.Turn{Source: "turn-1", Connection: appConn, Channel: "C1", Thread: "1.1", Mode: mode, Recipient: recipient}
}

func (a *app) end(t *testing.T, run *live.Run) {
	t.Helper()
	select {
	case <-run.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the turn did not end")
	}
}

// only fails the test unless the app holds one message, that shows words and is not a stream still open, and returns it.
func (a *app) only(t *testing.T, words string) chattest.Message {
	t.Helper()
	msgs := a.fake.Messages()
	if len(msgs) != 1 {
		t.Fatalf("the app holds %d messages (%+v); want one: nothing is posted twice", len(msgs), msgs)
	}
	m := msgs[0]
	if m.Streaming {
		t.Errorf("the message is still a stream: %+v", m)
	}
	if shown := m.Out.Message != nil && m.Out.Message.Fallback == words || m.Streamed == words; !shown {
		t.Errorf("the message shows %+v; want %q", m, words)
	}
	return m
}

// TestATurnThatStartsWhileItsConnectionIsDownFinishesThroughTheRuntimeThatComesUp: the turn's first write goes through the outbox,
// which finds the connection's platform at each try, and every write after it is the turn's own. The turn started with no platform,
// so it looks the connection up again after a write fails because the connection is down, and does not count the failures as
// attempts (here 2, and the connection is down for many more tries): its final text replaces the first text that the outbox
// posted once the connection ran, in one message.
func TestATurnThatStartsWhileItsConnectionIsDownFinishesThroughTheRuntimeThatComesUp(t *testing.T) {
	a, rt := newApp(t, live.Config{Attempts: 2, FinalDeadline: 20 * time.Second}, false)
	run := a.live.Start(t.Context(), a.turn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "first words"))
	testutil.Eventually(t, "the first write to be tried", func() bool {
		row, err := a.store.GetOutbox(t.Context(), outbox.ID("turn-1", store.OutboxReply))
		return err == nil && row.Payload != ""
	})
	testutil.Eventually(t, "many tries, far more than Attempts", func() bool { return a.table.asked.Load() > 5 })
	a.table.set(appConn, rt)
	testutil.Eventually(t, "the first words to be posted by the connection that runs", func() bool { return len(a.fake.Messages()) == 1 })

	run.Handle(text("a", "first words and the final words"))
	run.Handle(agent.FinalEvent{})
	a.end(t, run)

	m := a.only(t, "first words and the final words")
	if m.Updates == 0 {
		t.Errorf("the message was not edited (%+v); want the final words to replace the first", m)
	}
	if rt.updates.Load() == 0 {
		t.Error("no edit went through the runtime that came up")
	}
}

// TestATurnThatHoldsARetiredRuntimeEditsThroughTheNewOne: a runtime that the supervisor retired and the connection's new one are two
// platforms over one app. The turn holds the first; its edit fails fast because the connection is down, the connection is looked up
// again, and the edit goes through the new runtime: the same message, edited, and nothing posted a second time.
func TestATurnThatHoldsARetiredRuntimeEditsThroughTheNewOne(t *testing.T) {
	a, old := newApp(t, live.Config{}, true)
	run := a.live.Start(t.Context(), a.turn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "one"))
	testutil.Eventually(t, "the first words to be posted", func() bool { return len(a.fake.Messages()) == 1 })

	old.retired.Store(true)
	fresh := a.newRuntime()
	a.table.set(appConn, fresh)
	run.Handle(text("a", "one and two"))
	run.Handle(agent.FinalEvent{})
	a.end(t, run)

	a.only(t, "one and two")
	if fresh.updates.Load() == 0 || old.updates.Load() != 0 {
		t.Errorf("edits through the new runtime = %d, through the retired one = %d; want the new one alone", fresh.updates.Load(), old.updates.Load())
	}
}

// TestAStreamOfARetiredRuntimeIsEndedAndReplacedThroughTheNewOne: the stream the turn started on a runtime that was retired cannot be
// appended to by the new runtime, whose stream object is blind to what the old one sent, so the turn does what it does for a stream it
// lost: the new runtime stops the stream and edits the message to the whole text. Not a second message, and not a stream left open.
func TestAStreamOfARetiredRuntimeIsEndedAndReplacedThroughTheNewOne(t *testing.T) {
	a, old := newApp(t, live.Config{}, true)
	run := a.live.Start(t.Context(), a.turn(v1alpha1.OutputLive, "U1"))
	run.Handle(text("a", "Hello"))
	testutil.Eventually(t, "the stream to start", func() bool {
		ms := a.fake.Messages()
		return len(ms) == 1 && ms[0].Streaming && ms[0].Streamed == "Hello"
	})

	old.retired.Store(true)
	fresh := a.newRuntime()
	a.table.set(appConn, fresh)
	run.Handle(text("a", "Hello world"))
	run.Handle(agent.FinalEvent{})
	a.end(t, run)

	m := a.only(t, "Hello world")
	if m.Streamed != "Hello" || m.Updates != 1 {
		t.Errorf("the message = %+v; want the stream as it was (Hello) stopped, and one edit to the whole text", m)
	}
}

// TestAStreamIsLeftForTheRuntimeThatComesUpAfterTheConnectionWasRemoved: no runtime replaces the retired one for a while. The turn
// waits for one, as for any write that fails because the connection is down, and finishes through it.
func TestAStreamIsLeftForTheRuntimeThatComesUpAfterTheConnectionWasRemoved(t *testing.T) {
	a, old := newApp(t, live.Config{Attempts: 2, FinalDeadline: 20 * time.Second}, true)
	run := a.live.Start(t.Context(), a.turn(v1alpha1.OutputLive, "U1"))
	run.Handle(text("a", "Hello"))
	testutil.Eventually(t, "the stream to start", func() bool { ms := a.fake.Messages(); return len(ms) == 1 && ms[0].Streaming })

	old.retired.Store(true)
	a.table.mu.Lock()
	delete(a.table.m, appConn)
	a.table.mu.Unlock()
	run.Handle(text("a", "Hello world"))
	run.Handle(agent.FinalEvent{})
	testutil.Eventually(t, "many tries, far more than Attempts", func() bool { return a.table.asked.Load() > 5 })
	select {
	case <-run.Done():
		t.Fatal("the turn gave up while its connection was down, inside the final deadline")
	default:
	}
	a.table.set(appConn, a.newRuntime())
	a.end(t, run)

	a.only(t, "Hello world")
}

// TestATurnWhoseTokenWasRevokedAfterARestartResolvesAgain: the turn holds the platform of a runtime that a restart halted, whose
// token is revoked afterwards. The platform answers with a rejected credential, which no report can end the runtime for, so the turn
// looks the connection up again on the verdict and finishes through the runtime that runs on the new token.
func TestATurnWhoseTokenWasRevokedAfterARestartResolvesAgain(t *testing.T) {
	a, old := newApp(t, live.Config{}, true)
	run := a.live.Start(t.Context(), a.turn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "one"))
	testutil.Eventually(t, "the first words to be posted", func() bool { return len(a.fake.Messages()) == 1 })

	old.revoked.Store(true)
	fresh := a.newRuntime()
	a.table.set(appConn, fresh)
	run.Handle(text("a", "one and two"))
	run.Handle(agent.FinalEvent{})
	a.end(t, run)

	a.only(t, "one and two")
	if fresh.updates.Load() == 0 {
		t.Error("no edit went through the runtime that runs on the new token")
	}
}

// TestATurnThatOnlyLooksAgainWhenAWriteSaysTheConnectionIsDown: a turn on a connection that runs asks for its platform when it
// starts, and not again while its writes work.
func TestATurnThatOnlyLooksAgainWhenAWriteSaysTheConnectionIsDown(t *testing.T) {
	a, _ := newApp(t, live.Config{}, true)
	run := a.live.Start(t.Context(), a.turn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "one"))
	testutil.Eventually(t, "the first words to be posted", func() bool { return len(a.fake.Messages()) == 1 })
	run.Handle(text("a", "one and two"))
	run.Handle(agent.FinalEvent{})
	a.end(t, run)

	a.only(t, "one and two")
	if n := a.table.asked.Load(); n != 1 {
		t.Errorf("the renderer asked for the platform %d times; want once, when the turn started", n)
	}
}

// TestARecoveredTurnWhoseMessageWasNamedWhileItsConnectionWasDownEditsItOnceItRuns: the turn ended with nothing to show and found the
// message an earlier process left in the outbox row, while no platform ran its connection, so it named the message without the
// platform's name. Once the connection runs the Ref is made again by its platform, and the message is stopped and edited.
func TestARecoveredTurnWhoseMessageWasNamedWhileItsConnectionWasDownEditsItOnceItRuns(t *testing.T) {
	a, rt := newApp(t, live.Config{FinalDeadline: 20 * time.Second}, false)
	// An earlier process streamed the reply and recorded where it is.
	ref, err := rt.Stream(t.Context(), chat.Target{Conn: appConn, Channel: "C1", Thread: "1.1"}, "U1", "Hello")
	if err != nil {
		t.Fatal(err)
	}
	row := store.OutboxEntry{ID: outbox.ID("turn-1", store.OutboxReply), EventID: "turn-1", Role: store.OutboxReply, Connection: appConn, Channel: "C1", Thread: "1.1", CreatedAt: time.Now()}
	if _, err := a.store.CreateOutbox(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := a.store.RecordMessageRef(t.Context(), row.ID, ref.Ref().ID); err != nil {
		t.Fatal(err)
	}

	turn := a.turn(v1alpha1.OutputLive, "U1")
	turn.Recovered = true
	run := a.live.Start(t.Context(), turn)
	run.Handle(agent.FinalEvent{}) // the turn ended with nothing to show
	testutil.Eventually(t, "the turn to have tried while the connection is down", func() bool { return a.table.asked.Load() >= 2 })
	a.table.set(appConn, rt)
	a.end(t, run)

	msgs := a.fake.Messages()
	if len(msgs) != 1 || msgs[0].Streaming || msgs[0].Updates != 1 {
		t.Errorf("the app holds %+v; want the one message stopped and edited once", msgs)
	}
}

// TestATurnWhoseTokenWasRevokedWaitsOutTheGapOfTheRestart: the old runtime is out of the table and the new one is not in it yet, so the
// lookup that follows the rejected credential says the connection is down. That is a wait for the connection, as any down is, and not
// an attempt: the final text outlasts more tries than Attempts, and goes out through the runtime that comes up.
func TestATurnWhoseTokenWasRevokedWaitsOutTheGapOfTheRestart(t *testing.T) {
	a, old := newApp(t, live.Config{Attempts: 2, FinalDeadline: 20 * time.Second}, true)
	run := a.live.Start(t.Context(), a.turn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "one"))
	testutil.Eventually(t, "the first words to be posted", func() bool { return len(a.fake.Messages()) == 1 })

	old.revoked.Store(true)
	a.table.mu.Lock()
	delete(a.table.m, appConn)
	a.table.mu.Unlock()
	run.Handle(text("a", "one and two"))
	run.Handle(agent.FinalEvent{})
	testutil.Eventually(t, "many tries, far more than Attempts", func() bool { return a.table.asked.Load() > 5 })
	select {
	case <-run.Done():
		t.Fatal("the turn gave up in the gap of a restart, inside the final deadline")
	default:
	}
	fresh := a.newRuntime()
	a.table.set(appConn, fresh)
	a.end(t, run)

	a.only(t, "one and two")
	if fresh.updates.Load() == 0 {
		t.Error("no edit went through the runtime that runs on the new token")
	}
}

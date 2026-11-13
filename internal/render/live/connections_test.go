package live_test

import (
	"sync"
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

const (
	connA = "ns/a"
	connB = "ns/b"
)

// apps is the platforms of the connections a Renderer and an outbox run, which a test changes: a connection comes by being set.
type apps struct {
	mu sync.Mutex
	m  map[string]*chattest.FakePlatform
}

func (a *apps) set(conn string, p *chattest.FakePlatform) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[conn] = p
}

func (a *apps) get(conn string) (*chattest.FakePlatform, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return chat.PlatformSet[*chattest.FakePlatform](a.m).Platform(conn)
}

type renderPlatforms struct{ *apps }

func (r renderPlatforms) Platform(conn string) (chat.Platform, error) { return r.get(conn) }

type outboxPlatforms struct{ *apps }

func (o outboxPlatforms) Platform(conn string) (outbox.Platform, error) { return o.get(conn) }

// twoConnections is a Renderer and an outbox over two fake platforms, one for each of connA and connB.
type twoConnections struct {
	a, b  *chattest.FakePlatform
	apps  *apps
	store *sqlite.Store
	ob    *outbox.Outbox
	live  *live.Renderer
}

func newTwoConnections(t *testing.T, cfg live.Config) *twoConnections {
	t.Helper()
	w := &twoConnections{a: chattest.NewFake(), b: chattest.NewFake()}
	w.apps = &apps{m: map[string]*chattest.FakePlatform{connA: w.a, connB: w.b}}
	w.store = sqlitetest.Open(t)
	quiet := testutil.DiscardLogger()
	w.ob = newOutbox(t, outbox.Config{Store: w.store, Platforms: outboxPlatforms{w.apps}, Logger: quiet, CallTimeout: time.Second})
	cfg.Sender, cfg.Platforms, cfg.Store, cfg.Logger = w.ob, renderPlatforms{w.apps}, w.store, quiet
	if cfg.Throttle == 0 {
		cfg.Throttle = time.Millisecond
	}
	w.live = newRenderer(t, cfg)
	return w
}

// run renders a turn of conn with the text and the end of the turn, and waits for the run to end.
func (w *twoConnections) run(t *testing.T, conn string, turn live.Turn, words string) {
	t.Helper()
	turn.Connection, turn.Channel, turn.Thread = conn, "C1", "1.1"
	if turn.Source == "" {
		turn.Source = "turn-" + conn
	}
	run := w.live.Start(t.Context(), turn)
	run.Handle(text("a", words))
	run.Handle(agent.FinalEvent{})
	select {
	case <-run.Done():
	case <-time.After(30 * time.Second):
		t.Fatalf("the turn of %s did not end", conn)
	}
}

// shows says whether the fake holds one message of connection conn, in the thread, that says words, as the text of the
// message, or as the markdown of a stream.
func shows(p *chattest.FakePlatform, conn, words string) (n int) {
	for _, m := range p.Messages() {
		if m.Ref.Conn == conn && m.Ref.Channel == "C1" && (m.Streamed == words || m.Out.Message != nil && m.Out.Message.Fallback == words) {
			n++
		}
	}
	return n
}

// TestEachTurnIsRenderedByTheAppOfItsConnection: the streams, the posts of the reply through the outbox, and the edits of
// two turns of two connections each reach the platform of their own connection and no other.
func TestEachTurnIsRenderedByTheAppOfItsConnection(t *testing.T) {
	for name, turn := range map[string]live.Turn{
		"a stream":                    {Mode: v1alpha1.OutputLive, Recipient: "U1"},
		"a message that is updated":   {Mode: v1alpha1.OutputLive},
		"a message posted at the end": {Mode: v1alpha1.OutputFinal},
	} {
		t.Run(name, func(t *testing.T) {
			w := newTwoConnections(t, live.Config{})
			w.run(t, connA, turn, "words of a")
			w.run(t, connB, turn, "words of b")
			if shows(w.a, connA, "words of a") != 1 || shows(w.b, connB, "words of b") != 1 {
				t.Errorf("a holds %+v and b holds %+v; want each to hold its own turn's text, once", w.a.Messages(), w.b.Messages())
			}
			if len(w.a.Messages()) != 1 || len(w.b.Messages()) != 1 {
				t.Errorf("a holds %d messages and b holds %d; want one each", len(w.a.Messages()), len(w.b.Messages()))
			}
			for _, row := range []string{"turn-" + connA, "turn-" + connB} {
				got, err := w.store.GetOutbox(t.Context(), outbox.ID(row, store.OutboxReply))
				if want := map[string]string{"turn-" + connA: connA, "turn-" + connB: connB}[row]; err != nil || got.Connection != want {
					t.Errorf("the outbox row of %s = %+v, %v; want connection %s", row, got, err, want)
				}
			}
		})
	}
}

// TestATurnOfAConnectionThatIsDownIsWrittenWhenItRuns: with no platform running the turn's connection, the run's writes fail, are not
// counted as attempts (Attempts is 2), and are tried again until its final deadline; its reply is in the outbox, waiting, and goes out
// through the connection's platform once it runs.
func TestATurnOfAConnectionThatIsDownIsWrittenWhenItRuns(t *testing.T) {
	w := newTwoConnections(t, live.Config{FinalDeadline: 300 * time.Millisecond, Attempts: 2})
	w.apps.mu.Lock()
	delete(w.apps.m, connB)
	w.apps.mu.Unlock()

	w.run(t, connB, live.Turn{Mode: v1alpha1.OutputLive}, "waiting words")
	if len(w.a.Messages()) != 0 {
		t.Fatalf("a holds %+v; want nothing: the turn is b's", w.a.Messages())
	}
	row, err := w.store.GetOutbox(t.Context(), outbox.ID("turn-"+connB, store.OutboxReply))
	if err != nil || row.Connection != connB || row.MessageRef != "" || row.Payload == "" {
		t.Fatalf("the row of the reply = %+v, %v; want it written for b, unposted, with its text", row, err)
	}

	w.apps.set(connB, w.b)
	if err := w.ob.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile once b runs: %v", err)
	}
	if shows(w.b, connB, "waiting words") != 1 || len(w.a.Messages()) != 0 {
		t.Errorf("a holds %+v and b holds %+v; want the reply on b alone", w.a.Messages(), w.b.Messages())
	}
}

// TestAConnectionThatCannotShowBusyDoesNotSilenceAnother: a platform that says it cannot mark a thread as busy
// (chat.ErrNoBusy) is not asked again by the turns of its own connection, and says nothing of another connection's.
func TestAConnectionThatCannotShowBusyDoesNotSilenceAnother(t *testing.T) {
	w := newTwoConnections(t, live.Config{})
	w.a.FailNext("MarkBusy", chat.ErrNoBusy)
	activity := live.Turn{Mode: v1alpha1.OutputLive, Progress: v1alpha1.ProgressActivity}

	w.run(t, connA, activity, "one")
	second := activity
	second.Source = "turn-a-2"
	w.run(t, connA, second, "two")
	w.run(t, connB, activity, "three")

	if got := len(w.a.BusyCalls()); got != 1 {
		t.Errorf("a was asked to mark the thread %d times; want once: it said it cannot", got)
	}
	if got := len(w.b.BusyCalls()); got == 0 {
		t.Error("b was never asked to mark the thread: a's answer silenced it")
	}
}

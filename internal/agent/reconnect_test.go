package agent

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// quick shortens the reconnect pause.
var quick = Config{ReconnectDelay: time.Millisecond, InactivityTimeout: time.Minute, UnaryTimeout: 10 * time.Second}

// resumeAfterFirstTick serves WP-02's reconnect case: the stream dies after "tick 1", GetTask
// shows the task WORKING with that text, and SubscribeToTask carries the rest. hang keeps
// the first stream open and silent instead of dropping it.
func resumeAfterFirstTick(t *testing.T, hang bool) func(rpc, http.ResponseWriter) {
	return func(call rpc, w http.ResponseWriter) {
		switch call.Method {
		case "SendStreamingMessage":
			sse(t, w, "reconnect/01-stream-dropped.response.sse", -1)
			if hang {
				<-call.Done
				return
			}
			drop()
		case "GetTask":
			reply(t, w, "reconnect/02-gettask-after-drop.response.json")
		case "SubscribeToTask":
			sse(t, w, "reconnect/03-subscribe.response.sse", -1)
		}
	}
}

// finalText checks that a send ended COMPLETED with the whole text, delivered without repeats.
func finalText(t *testing.T, got []Event) {
	t.Helper()
	seen := map[string]bool{}
	for _, s := range texts(got) {
		if seen[s] {
			t.Errorf("text %q delivered twice", s)
		}
		seen[s] = true
	}
	final, ok := got[len(got)-1].(FinalEvent)
	if !ok || final.Task.Status.State != a2a.TaskStateCompleted || final.Task.Artifacts[0].Parts[0].Text() != ticks {
		t.Fatalf("last event = %#v, want the COMPLETED task with %q", got[len(got)-1], ticks)
	}
	if all := texts(got); all[len(all)-1] != ticks {
		t.Errorf("last text = %q, want %q", all[len(all)-1], ticks)
	}
}

// The wire tests check that a real HTTP stream is dropped or cancelled and resumed as the
// recorded calls show. The timing-dependent behavior is tested below, on a fake clock.
func TestReconnectAfterDropContinuesWithoutDuplicates(t *testing.T) {
	fa := newFakeAgent(t, resumeAfterFirstTick(t, false))
	h, got := collector()
	if err := fa.agentWith(t, quick).Send(t.Context(), message("go slowly"), h); err != nil {
		t.Fatal(err)
	}
	finalText(t, *got)
	for _, m := range []string{"SendStreamingMessage", "GetTask", "SubscribeToTask"} {
		if n := fa.count(m); n != 1 {
			t.Errorf("%s called %d times, want once", m, n)
		}
	}
}

func TestWatchdogCancelsASilentHTTPStreamAndReconnects(t *testing.T) {
	fa := newFakeAgent(t, resumeAfterFirstTick(t, true))
	cfg := quick
	cfg.InactivityTimeout = time.Second // the stream never speaks again, so the window only sets the delay
	h, got := collector()
	if err := fa.agentWith(t, cfg).Send(t.Context(), message("go slowly"), h); err != nil {
		t.Fatal(err)
	}
	finalText(t, *got)
	select {
	case <-fa.callAt(0).Done:
	case <-time.After(5 * time.Second):
		t.Error("the silent stream was never cancelled")
	}
}

// The watchdog measures silence, not the length of the stream.
func TestWatchdogResetsOnEveryEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan item)
		m := newMemConn(t, []chan item{ch}, nil)
		s := sendAsync(t, memAgent(t, m, nil))
		for range 6 {
			time.Sleep(6 * time.Second) // 36 s in all, far past the 10 s window
			s.feed(ch, item{ev: delta("x")})
		}
		s.feed(ch, item{ev: task(a2a.TaskStateCompleted, "xxxxxx")})
		if err := s.result(); err != nil {
			t.Fatal(err)
		}
		if got, want := m.calls(), []string{"send", "send ended"}; !slices.Equal(got, want) {
			t.Errorf("calls = %q, want %q: the watchdog fired on a stream that was never silent", got, want)
		}
	})
}

// Silence past the window cancels the stream, and only then is the task reconciled: GetTask,
// then SubscribeToTask (spec 8.3), ReconnectDelay after the cut.
func TestWatchdogCancelsASilentStreamThenReconciles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send, sub := make(chan item), make(chan item)
		m := newMemConn(t, []chan item{send}, []chan item{sub}, getResult{task: task(a2a.TaskStateWorking, "tick 1 ")})
		s := sendAsync(t, memAgent(t, m, nil))
		s.feed(send, item{ev: working()})
		s.feed(send, item{ev: delta("tick 1 ")})
		time.Sleep(11 * time.Second) // silence past the window
		s.feed(sub, item{ev: delta("tick 2")})
		s.feed(sub, item{ev: task(a2a.TaskStateCompleted, "tick 1 tick 2")})
		if err := s.result(); err != nil {
			t.Fatal(err)
		}
		want := []string{"send", "send ended", "get t1", "subscribe t1", "subscribe ended"}
		if got := m.calls(); !slices.Equal(got, want) {
			t.Errorf("calls = %q, want %q", got, want)
		}
		if got, want := texts(s.events()), []string{"tick 1 ", "tick 1 tick 2"}; !slices.Equal(got, want) {
			t.Errorf("texts = %q, want %q", got, want)
		}
		sent, ok1 := m.at("send")
		cut, ok2 := m.at("send ended")
		got, ok3 := m.at("get t1")
		if !ok1 || !ok2 || !ok3 { // the calls check above has already failed
			return
		}
		if d := cut.Sub(sent); d != 10*time.Second { // the events came at once: the silence is the window
			t.Errorf("the stream lived %v, want the 10 s window", d)
		}
		if d := got.Sub(cut); d != time.Second {
			t.Errorf("GetTask came %v after the cut, want ReconnectDelay (1s)", d)
		}
	})
}

// If GetTask finds the task over, there is nothing to subscribe to.
func TestReconnectStopsAtAFinishedTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := make(chan item)
		m := newMemConn(t, []chan item{send}, nil, getResult{task: task(a2a.TaskStateCompleted, "ab")})
		s := sendAsync(t, memAgent(t, m, nil))
		s.feed(send, item{ev: delta("a")})
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		if err := s.result(); err != nil {
			t.Fatal(err)
		}
		if got, want := m.calls(), []string{"send", "send ended", "get t1"}; !slices.Equal(got, want) {
			t.Errorf("calls = %q, want %q", got, want)
		}
		if _, ok := s.events()[len(s.events())-1].(FinalEvent); !ok || !slices.Equal(texts(s.events()), []string{"a", "ab"}) {
			t.Errorf("events = %#v, want the text a then ab, and the final task", s.events())
		}
	})
}

// MaxReconnects counts failures in a row: a stream that changed something starts the count over.
func TestReconnectBudgetResetsWhenAStreamChangesSomething(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send, sub1, sub2 := make(chan item), make(chan item), make(chan item)
		m := newMemConn(t, []chan item{send}, []chan item{sub1, sub2}, getResult{task: task(a2a.TaskStateWorking, "a")})
		s := sendAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 1 }))
		s.feed(send, item{ev: delta("a")})
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		s.feed(sub1, item{ev: delta("b")})
		s.feed(sub1, item{err: io.ErrUnexpectedEOF})
		s.feed(sub2, item{ev: task(a2a.TaskStateCompleted, "abc")})
		if err := s.result(); err != nil {
			t.Fatalf("Send: %v (two drops with progress between them fit a budget of one)", err)
		}
		if got, want := texts(s.events()), []string{"a", "ab", "abc"}; !slices.Equal(got, want) {
			t.Errorf("texts = %q, want %q", got, want)
		}
	})
}

func TestReconnectGivesUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := make(chan item)
		m := newMemConn(t, []chan item{send}, nil, getResult{err: errors.New("down")})
		s := sendAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 2 }))
		s.feed(send, item{ev: working()})
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		if err := s.result(); !errors.Is(err, ErrStreamLost) {
			t.Errorf("err = %v, want ErrStreamLost", err)
		}
		if n := countOf(m.calls(), "get t1"); n != 2 {
			t.Errorf("%d GetTask calls, want 2, one per reconnect", n)
		}
	})
}

package agent

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
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

func TestWatchdogCancelsASilentStreamAndReconnects(t *testing.T) {
	fa := newFakeAgent(t, resumeAfterFirstTick(t, true))
	cfg := quick
	cfg.InactivityTimeout = 150 * time.Millisecond
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
	fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) {
		for _, ev := range sseEvents(t, "livestream/01-slow-stream.response.sse") {
			write(w, ev)
			time.Sleep(60 * time.Millisecond) // 14 events, far longer than the window
		}
	})
	cfg := quick
	cfg.InactivityTimeout = 250 * time.Millisecond
	h, got := collector()
	if err := fa.agentWith(t, cfg).Send(t.Context(), message("go slowly"), h); err != nil {
		t.Fatal(err)
	}
	if finalText(t, *got); fa.count("GetTask") != 0 {
		t.Error("the watchdog fired on a stream that was never silent")
	}
}

// If GetTask finds the task over, there is nothing to subscribe to.
func TestReconnectStopsAtAFinishedTask(t *testing.T) {
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		switch call.Method {
		case "SendStreamingMessage":
			sse(t, w, "reconnect/01-stream-dropped.response.sse", -1)
			drop()
		case "GetTask":
			reply(t, w, "reconnect/04-gettask-final.response.json")
		}
	})
	h, got := collector()
	if err := fa.agentWith(t, quick).Send(t.Context(), message("go slowly"), h); err != nil {
		t.Fatal(err)
	}
	if finalText(t, *got); fa.count("SubscribeToTask") != 0 {
		t.Error("subscribed to a finished task")
	}
}

// MaxReconnects counts failures in a row: a stream that made progress starts the count over.
func TestReconnectBudgetResetsOnProgress(t *testing.T) {
	var subscribes atomic.Int32
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		switch {
		case call.Method == "SubscribeToTask" && subscribes.Add(1) == 1:
			sse(t, w, "reconnect/03-subscribe.response.sse", 3) // the snapshot and two deltas
			drop()
		default:
			resumeAfterFirstTick(t, false)(call, w)
		}
	})
	cfg := quick
	cfg.MaxReconnects = 1
	h, got := collector()
	if err := fa.agentWith(t, cfg).Send(t.Context(), message("go slowly"), h); err != nil {
		t.Fatalf("Send: %v (two drops with progress between them fit a budget of one)", err)
	}
	// The recorded snapshot is older than what the first resume delivered, so it replaces
	// newer text; only the end state is comparable.
	if all := texts(*got); all[len(all)-1] != ticks {
		t.Errorf("last text = %q, want %q", all[len(all)-1], ticks)
	}
}

func TestReconnectGivesUp(t *testing.T) {
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		if call.Method == "SendStreamingMessage" {
			sse(t, w, "reconnect/01-stream-dropped.response.sse", -1)
			drop()
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	cfg := quick
	cfg.MaxReconnects = 2
	err := fa.agentWith(t, cfg).Send(t.Context(), message("go slowly"), nil)
	if !errors.Is(err, ErrStreamLost) {
		t.Errorf("err = %v, want ErrStreamLost", err)
	}
	if n := fa.count("GetTask"); n != 2 {
		t.Errorf("GetTask called %d times, want 2, one per reconnect", n)
	}
}

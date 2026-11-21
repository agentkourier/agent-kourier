package adapter

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// clock is a settable time for the executor.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClockedExec(t *testing.T, fake *fakeBackend, ttl time.Duration) (*Executor, *clock, a2asrv.RequestHandler) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	exec := &Executor{Backend: fake, Store: newStore(t, c.now), IdleTTL: ttl, now: c.now}
	return exec, c, NewRequestHandler(exec)
}

func TestIdleSessionIsArchivedAfterTTL(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	first, _ := turn(t, h, userText("", "one"))

	clk.advance(time.Hour)
	exec.reap(context.Background())
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived at exactly the TTL: %v", got)
	}
	clk.advance(time.Second)
	exec.reap(context.Background())
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_1"}) {
		t.Fatalf("archived = %v, want [sesn_1]", got)
	}

	// The context's next message starts a new session.
	turn(t, h, userText(first.ContextID, "two"))
	if n := fake.sessionCount(); n != 2 {
		t.Errorf("sessions created = %d, want 2", n)
	}
}

func TestActivityRestartsTheIdleClock(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	first, _ := turn(t, h, userText("", "one"))
	clk.advance(50 * time.Minute)
	turn(t, h, userText(first.ContextID, "two"))
	clk.advance(50 * time.Minute)
	exec.reap(context.Background())
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v 50 minutes after its last turn", got)
	}
}

func TestZeroTTLNeverArchives(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, 0)
	turn(t, h, userText("", "one"))
	clk.advance(10000 * time.Hour)
	exec.reap(context.Background())
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v with no TTL", got)
	}
}

func TestRunningTurnIsNotArchived(t *testing.T) {
	// The agent starts and then says nothing: the turn stays open until the test ends it.
	fake := newFake(script(say("thinking")))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	working := make(chan struct{})
	done := make(chan a2a.TaskState, 1)
	go func() {
		var state a2a.TaskState
		once := sync.Once{}
		for ev, err := range h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: userText("", "long")}) {
			if err != nil {
				t.Errorf("stream error: %v", err)
				break
			}
			if _, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
				once.Do(func() { close(working) })
			}
			if s, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
				state = s.Status.State
			}
		}
		done <- state
	}()
	select {
	case <-working:
	case <-ctx.Done():
		t.Fatal("turn never started")
	}

	clk.advance(24 * time.Hour)
	exec.reap(context.Background())
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v during a turn", got)
	}

	fake.publish("sesn_1", idle(stopEndTurn))
	if s := <-done; s != a2a.TaskStateCompleted {
		t.Fatalf("turn ended %s, want completed", s)
	}
	// The turn's end is its last activity.
	clk.advance(59 * time.Minute)
	exec.reap(context.Background())
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v 59 minutes after the turn ended", got)
	}
}

func TestFailedArchiveIsRetried(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	turn(t, h, userText("", "one"))
	clk.advance(2 * time.Hour)
	fake.archiveErr = errors.New("503")
	exec.reap(context.Background())
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived = %v after a failed call", got)
	}
	exec.reap(context.Background())
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_1"}) {
		t.Fatalf("archived = %v, want the retry to archive sesn_1", got)
	}
}

func TestRestoreMapsContextsToSessions(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	now := clk.now()
	fake.seed("sesn_old", "ctx-1", now.Add(-10*time.Minute))
	fake.seed("sesn_new", "ctx-1", now.Add(-5*time.Minute)) // a second session for the same context: the newer one wins
	fake.seed("sesn_other", "ctx-2", now.Add(-time.Minute))
	fake.seed("sesn_foreign", "", now) // not the adapter's: no context metadata

	if err := exec.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_old"}) {
		t.Fatalf("archived = %v, want the superseded sesn_old only", got)
	}

	turn(t, h, userText("ctx-1", "again"))
	sent := fake.sentEvents()
	if len(sent) != 1 || sent[0].session != "sesn_new" {
		t.Fatalf("sent = %+v, want the message in sesn_new", sent)
	}
	if n := fake.sessionCount(); n != 0 {
		t.Errorf("sessions created = %d, want 0", n)
	}
}

func TestRestoredSessionAgesFromItsLastActivity(t *testing.T) {
	fake := newFake(nil)
	exec, clk, _ := newClockedExec(t, fake, time.Hour)
	fake.seed("sesn_stale", "ctx-1", clk.now().Add(-2*time.Hour))
	fake.seed("sesn_fresh", "ctx-2", clk.now().Add(-10*time.Minute))
	fake.seed("sesn_unknown", "ctx-3", time.Time{}) // no activity time: counted from the restore, not from year 1
	if err := exec.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	exec.reap(context.Background())
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_stale"}) {
		t.Fatalf("archived = %v, want [sesn_stale]", got)
	}
}

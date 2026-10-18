package live_test

import (
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
)

// These tests are about the busy mark of a thread (spec 8.3): on the update path, in a progress mode that shows activity,
// the renderer marks the thread when the turn starts, marks it again after each write it makes anyway, and clears it when the
// turn ends however it ends. They run over a FakePlatform, which models a platform that drops the mark when a message is
// posted in the thread.

// busyEnv is a renderer over a FakePlatform, with the outbox over it too, so that the first post of a turn lands there.
type busyEnv struct {
	*env
	fake *chattest.FakePlatform
	at   chat.Target
}

func setupBusy(t *testing.T) *busyEnv {
	t.Helper()
	e := setup(t)
	fake := chattest.NewFake()
	e.cfg.Platform = fake
	e.cfg.Sender = newOutbox(t, outbox.Config{Store: e.store, Platform: fake, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	e.live = newRenderer(t, e.cfg)
	return &busyEnv{env: e, fake: fake, at: chat.Target{Channel: "C1", Thread: e.root}}
}

// start starts an alert-thread turn in the progress mode.
func (b *busyEnv) start(t *testing.T, mode v1alpha1.ProgressMode) *live.Run {
	t.Helper()
	return b.progressRun(t, mode, "")
}

func (b *busyEnv) marks() []bool {
	var out []bool
	for _, c := range b.fake.BusyCalls() {
		out = append(out, c.Busy)
	}
	return out
}

func (b *busyEnv) waitMarks(t *testing.T, want ...bool) {
	t.Helper()
	eventually(t, fmt.Sprintf("the marks %v", want), func() bool { return slices.Equal(b.marks(), want) })
}

func TestTheThreadIsMarkedWhenTheTurnStartsAndClearedWhenItEnds(t *testing.T) {
	for _, mode := range []v1alpha1.ProgressMode{sa, activity} {
		t.Run(string(mode), func(t *testing.T) {
			b := setupBusy(t)
			run := b.start(t, mode)
			b.waitMarks(t, true)
			if !b.fake.Busy(b.at) || len(b.fake.Messages()) != 0 {
				t.Fatalf("busy %v with %d messages; want the thread marked before the turn has posted anything", b.fake.Busy(b.at), len(b.fake.Messages()))
			}
			run.Handle(text("a", "The answer."))
			b.waitMarks(t, true, true) // the start, and the refresh after the post that took the mark off
			run.Handle(agent.FinalEvent{})
			b.finish(t, run)
			b.waitMarks(t, true, true, false) // and the clear
			if b.fake.Busy(b.at) {
				t.Error("the thread is still marked after the turn")
			}
			if m := b.fake.Messages(); len(m) != 1 {
				t.Errorf("%d messages, want the one reply", len(m))
			}
		})
	}
}

// A post in the thread takes the mark off, so each write that the run makes is followed by a mark: and a write that is not
// made, because the state shows what is already there, is followed by none. No mark comes of a clock alone.
func TestEachWriteIsFollowedByAMarkAndNothingElseMarks(t *testing.T) {
	b := setupBusy(t)
	run := b.start(t, sa)
	b.waitMarks(t, true)

	b.elapse(30 * time.Second) // a quiet turn: its first sign of work is the Working… line at the delay, which is a write
	eventually(t, "the Working… line", func() bool { return len(b.fake.Messages()) == 1 })
	b.waitMarks(t, true, true)
	posted := b.fake.Messages()[0].Updates

	b.elapse(5 * time.Minute) // nothing happens, and nothing is written or marked
	if got := b.marks(); !slices.Equal(got, []bool{true, true}) || b.fake.Messages()[0].Updates != posted {
		t.Fatalf("marks %v and %d updates after a quiet five minutes; want no more of either", got, b.fake.Messages()[0].Updates-posted)
	}

	run.Handle(text("a", "Hel"))
	b.until(t, "the text", 3*time.Second, func() bool { return len(b.marks()) == 3 })
	if !b.fake.Busy(b.at) {
		t.Error("the thread is not marked after a write")
	}
	run.Handle(text("a", "Hel"))   // the same state: no write
	run.Handle(text("a", "Hello")) // a new one
	b.until(t, "the second text", 3*time.Second, func() bool { return len(b.marks()) == 4 })
	run.Handle(agent.FinalEvent{})
	b.finish(t, run)
	b.waitMarks(t, true, true, true, true, false)
	if b.fake.Busy(b.at) {
		t.Error("the thread is still marked after the turn")
	}
}

// The mark is cleared on every way a turn can end.
func TestTheMarkIsClearedOnEveryEndOfATurn(t *testing.T) {
	failed := &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}
	paused := &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
	for _, tc := range []struct {
		name string
		do   func(run *live.Run, cancel func())
		// posted is whether the turn had a message to show: a failed task with no output posts nothing.
		posted bool
	}{
		{"completed", func(run *live.Run, _ func()) { run.Handle(text("a", "Done.")); run.Handle(agent.FinalEvent{}) }, true},
		{"failed with no output", func(run *live.Run, _ func()) { run.Handle(agent.FinalEvent{Task: failed}) }, false},
		{"failed after some output", func(run *live.Run, _ func()) {
			run.Handle(text("a", "Part."))
			run.Handle(agent.FinalEvent{Task: failed})
		}, true},
		{"paused for input", func(run *live.Run, _ func()) {
			run.Handle(text("a", "Which one?"))
			run.Handle(agent.FinalEvent{Task: paused})
		}, true},
		{"cancelled before any output", func(_ *live.Run, cancel func()) { cancel() }, false},
		{"cancelled after the first post", func(run *live.Run, cancel func()) { run.Handle(text("a", "Part.")); cancel() }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := setupBusy(t)
			run, cancel := b.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa})
			b.waitMarks(t, true)
			tc.do(run, cancel)
			b.finish(t, run)
			if got := b.marks(); got[len(got)-1] || b.fake.Busy(b.at) {
				t.Errorf("marks %v, busy %v; want the last call to be the clear, and the thread unmarked", got, b.fake.Busy(b.at))
			}
			if got := len(b.fake.Messages()) == 1; got != tc.posted {
				t.Errorf("posted %v, want %v", got, tc.posted)
			}
		})
	}
}

// Without the progress modes that show activity, on the stream path, or on a platform that cannot, the thread is never marked.
func TestTheThreadIsNeverMarkedWhereItIsNotWanted(t *testing.T) {
	for _, tc := range []struct {
		name string
		turn live.Turn
		caps func(chat.Caps) chat.Caps
	}{
		{"the steps mode", live.Turn{Mode: v1alpha1.OutputLive, Progress: v1alpha1.ProgressSteps}, nil},
		{"no mode", live.Turn{Mode: v1alpha1.OutputLive}, nil},
		{"output final, which ignores the mode", live.Turn{Mode: v1alpha1.OutputFinal, Progress: sa}, nil},
		{"a chat thread, which streams", live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: "U1"}, nil},
		{"a chat thread in the activity mode", live.Turn{Mode: v1alpha1.OutputLive, Progress: activity, Recipient: "U1"}, nil},
		{"a platform that does not mark threads", live.Turn{Mode: v1alpha1.OutputLive, Progress: sa}, func(c chat.Caps) chat.Caps { c.Busy = false; return c }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := setupBusy(t)
			if tc.caps != nil {
				b.fake.SetCaps(tc.caps(b.fake.Capabilities()))
			}
			run, _ := b.startFor(t, tc.turn)
			run.Handle(call("c1", "search", map[string]any{"q": "up"}))
			run.Handle(result("c1", map[string]any{"output": "x"}))
			run.Handle(text("a", "Done."))
			run.Handle(agent.FinalEvent{})
			b.finish(t, run)
			if got := b.fake.BusyCalls(); len(got) != 0 {
				t.Errorf("MarkBusy was called %d times: %+v; want never", len(got), got)
			}
			if len(b.fake.Messages()) != 1 {
				t.Errorf("%d messages, want the reply", len(b.fake.Messages()))
			}
		})
	}
}

// A platform that says it cannot show the mark is not asked again, by this turn or by any later one of the Renderer, and the
// turn is delivered as if it had not been asked. The platform reported why, so the renderer logs nothing.
func TestARefusedMarkTurnsItOffForTheRenderer(t *testing.T) {
	b := setupBusy(t)
	b.fake.FailNext("MarkBusy", fmt.Errorf("slack: refused: %w", chat.ErrNoBusy))
	run := b.start(t, sa)
	b.waitMarks(t, true)
	run.Handle(text("a", "Part."))
	b.until(t, "the post", 3*time.Second, func() bool { return len(b.fake.Messages()) == 1 })
	run.Handle(text("a", "Part, then the rest."))
	run.Handle(agent.FinalEvent{})
	b.finish(t, run)
	if got := b.marks(); !slices.Equal(got, []bool{true}) {
		t.Errorf("marks %v; want the one that was refused, and no refresh and no clear", got)
	}
	if m := b.fake.Messages(); len(m) != 1 || m[0].Out.Message == nil || !strings.Contains(m[0].Out.Message.Fallback, "the rest") {
		t.Errorf("messages %+v; want the whole reply", m)
	}
	if b.logs.Len() != 0 {
		t.Errorf("log %q; want nothing: the platform says why it cannot", b.logs.String())
	}

	again, _ := b.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa})
	again.Handle(agent.FinalEvent{})
	b.finish(t, again)
	if got := b.marks(); !slices.Equal(got, []bool{true}) {
		t.Errorf("marks %v after a second turn; want none beyond the refused one", got)
	}
}

// Any other error is the call's own: it is logged once, the turn goes on without marking, and it still clears the mark that
// the failed call may have set.
func TestAFailedMarkIsLoggedOnceAndTheTurnGoesOn(t *testing.T) {
	b := setupBusy(t)
	b.fake.FailNext("MarkBusy", fmt.Errorf("the connection broke"))
	run := b.start(t, sa)
	b.waitMarks(t, true)
	run.Handle(text("a", "Done."))
	run.Handle(agent.FinalEvent{})
	b.finish(t, run)
	b.waitMarks(t, true, false) // the failed mark, no refresh after the post, and the clear
	if n := strings.Count(b.logs.String(), "busy"); n != 1 {
		t.Errorf("log %q; want one line about the mark", b.logs.String())
	}
	if m := b.fake.Messages(); len(m) != 1 {
		t.Errorf("%d messages, want the reply", len(m))
	}
}

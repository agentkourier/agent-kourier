package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// ending is the script of a turn that says text (in one artifact, none if empty) and ends in state, with
// statusText as its status message.
func ending(state a2a.TaskState, text, statusText string) func(context.Context, int, agent.Handler) error {
	return func(_ context.Context, n int, h agent.Handler) error {
		id := accepted(h, n)
		if text != "" {
			h(agent.TextEvent{ArtifactID: "a1", Text: text})
		}
		task := &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: state}}
		if statusText != "" {
			task.Status.Message = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(statusText))
		}
		h(agent.StateEvent{TaskID: id, ContextID: "c1", State: state})
		h(agent.FinalEvent{Task: task})
		return nil
	}
}

// providerError is what a model provider's error can look like: a masked key, whose last characters are
// the key's. No redaction recognises it, so no part of an agent's status text may reach the thread.
const providerError = "Incorrect API key provided: sk-proj-********************wxyz. HTTP 401"

// A task that reaches a terminal state leaves the thread something to read, once, whatever the agent said
// (spec 8.2, 11: events are never dropped silently). A task that failed, was rejected or was cancelled
// is told, also when it said something first; a completed task is told only when it said nothing to show.
// The notice is the courier's own words: the agent's status message is untrusted and cannot be redacted
// reliably (providerError), so it reaches neither the thread nor the log.
func TestTerminalTaskTellsTheThread(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script func(context.Context, int, agent.Handler) error
		want   NoticeKind // "" for no notice
	}{
		{"failed, with a status message and no output", ending(a2a.TaskStateFailed, "", providerError), NoticeTaskFailed},
		{"failed, with no message either", ending(a2a.TaskStateFailed, "", ""), NoticeTaskFailed},
		{"failed after some output", ending(a2a.TaskStateFailed, "partial answer", providerError), NoticeTaskFailed},
		{"rejected", ending(a2a.TaskStateRejected, "", providerError), NoticeTaskRejected},
		{"canceled", ending(a2a.TaskStateCanceled, "", ""), NoticeTaskCanceled},
		{"canceled after some output", ending(a2a.TaskStateCanceled, "partial answer", ""), NoticeTaskCanceled},
		{"completed with no output", ending(a2a.TaskStateCompleted, "", "the answer, in a status message"), NoticeNoOutput},
		{"completed with output that is white space and hidden characters", ending(a2a.TaskStateCompleted, " \n\t\u200b\u2800\u3164\x00 ", ""), NoticeNoOutput},
		{"completed with output", ending(a2a.TaskStateCompleted, "the answer", ""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.agent.script = tc.script
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			waitFinal(t, sink)
			eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
			f.retired(t)
			sink.mu.Lock()
			defer sink.mu.Unlock()
			if tc.want == "" {
				if len(sink.notices) != 0 {
					t.Errorf("notices = %+v; want none", sink.notices)
				}
				return
			}
			want := Notice{ID: "task:t1/" + string(tc.want), Kind: tc.want, MessageRef: "1"}
			if len(sink.notices) != 1 || sink.notices[0] != want {
				t.Errorf("notices = %+v; want exactly %+v: no detail, for the agent's text is not the courier's to show", sink.notices, want)
			}
		})
	}
}

// The notice is named after the task that ended, so the turn that sees the end more than once raises one
// post: after a stream lost and followed, and after a restart that follows the task again, whatever turn
// row the follower holds.
func TestTerminalTaskNoticeIsOneAcrossFollowAndRestart(t *testing.T) {
	failedID := "task:t1/" + string(NoticeTaskFailed)
	t.Run("a stream lost before the end, then followed", func(t *testing.T) {
		f := newFixture(t)
		sink := f.live()
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
		f.agent.script = lost(errors.New("connection reset"))
		f.agent.get = working(t0)
		f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error {
			return ending(a2a.TaskStateFailed, "", providerError)(nil, 0, h)
		}
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeTaskFailed)) >= 1 })
		f.retired(t)
		if got := sink.noticesOf(NoticeTaskFailed); len(got) != 1 || got[0].ID != failedID {
			t.Errorf("notices = %+v; want one, %s", got, failedID)
		}
	})
	t.Run("a restart that follows the task, which no turn row names", func(t *testing.T) {
		f, sink := restarted(t)
		f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error {
			return ending(a2a.TaskStateFailed, "", providerError)(nil, 0, h)
		}
		f.recover(t)
		eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeTaskFailed)) >= 1 })
		f.retired(t)
		if got := sink.noticesOf(NoticeTaskFailed); len(got) != 1 || got[0].ID != failedID {
			t.Errorf("notices = %+v; want one, %s: the same post as the one the turn that sent it would have raised", got, failedID)
		}
	})
}

// logRecorder is a slog.Handler that keeps what it is given, attributes included.
type logRecorder struct {
	mu   *sync.Mutex
	recs *[]logRecord
	pre  map[string]any
}

type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

func newLogRecorder() *logRecorder {
	return &logRecorder{mu: &sync.Mutex{}, recs: &[]logRecord{}, pre: map[string]any{}}
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	for k, v := range l.pre {
		rec.attrs[k] = v
	}
	r.Attrs(func(a slog.Attr) bool { rec.attrs[a.Key] = a.Value.Any(); return true })
	l.mu.Lock()
	*l.recs = append(*l.recs, rec)
	l.mu.Unlock()
	return nil
}

func (l *logRecorder) WithAttrs(as []slog.Attr) slog.Handler {
	pre := map[string]any{}
	for k, v := range l.pre {
		pre[k] = v
	}
	for _, a := range as {
		pre[a.Key] = a.Value.Any()
	}
	return &logRecorder{mu: l.mu, recs: l.recs, pre: pre}
}

func (l *logRecorder) WithGroup(string) slog.Handler { return l }

// lines returns the records with the message msg.
func (l *logRecorder) lines(msg string) (out []logRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range *l.recs {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}

// everything is all that was logged, as one string.
func (l *logRecorder) everything() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, r := range *l.recs {
		fmt.Fprintf(&b, "%s %v\n", r.msg, r.attrs)
	}
	return b.String()
}

// turnEnds records the outcomes the manager reports.
type turnEnds struct {
	mu   sync.Mutex
	list []string
}

func (e *turnEnds) add(binding string, outcome TurnOutcome) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, binding+" "+string(outcome))
}

func (e *turnEnds) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

// logged is a fixture whose manager logs to a recorder and reports its turn outcomes to ends.
func logged(t *testing.T) (*fixture, *fakeSink, *logRecorder, *turnEnds) {
	t.Helper()
	f := newFixture(t)
	logs, ends := newLogRecorder(), &turnEnds{}
	f.m.cfg.Logger, f.m.cfg.OnTurnEnd = slog.New(logs), ends.add
	return f, f.live(), logs, ends
}

// A turn logs one line when it starts and one when it ends, at Info, with where it ran, how it ended and
// how long it took, and nothing that was said: not the person's message, not the agent's output or status
// message (R2). The outcome is counted once.
func TestTurnLogsItsStartAndEnd(t *testing.T) {
	f, sink, logs, ends := logged(t)
	f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
		f.now.Add(int64(5 * time.Second)) // the agent works for five seconds
		return ending(a2a.TaskStateFailed, "agent output FAKE-OUTPUT", providerError)(ctx, n, h)
	}
	in := inbound("1")
	in.Text = "my password is FAKE-PROMPT"
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) == 1 })
	f.retired(t)

	starts, endings := logs.lines("session turn started"), logs.lines("session turn ended")
	if len(starts) != 1 || len(endings) != 1 {
		t.Fatalf("%d start lines, %d end lines; want one of each", len(starts), len(endings))
	}
	where := map[string]any{"binding": "ns/b1", "connection": "slack", "channel": "C1", "thread": "1700.01", "message": in.MessageID}
	for name, rec := range map[string]logRecord{"start": starts[0], "end": endings[0]} {
		if rec.level != slog.LevelInfo {
			t.Errorf("the %s line is at %v; want Info", name, rec.level)
		}
		for k, v := range where {
			if rec.attrs[k] != v {
				t.Errorf("the %s line has %s = %v; want %v", name, k, rec.attrs[k], v)
			}
		}
	}
	end := endings[0].attrs
	if end["outcome"] != "failed" || end["duration_ms"] != int64(5000) || end["task"] != "t1" || end["session"] != "c1" || end["output"] != true {
		t.Errorf("the end line = %v; want outcome failed after 5000 ms, task t1, session c1, and output true", end)
	}
	for _, secret := range []string{"FAKE-PROMPT", "FAKE-OUTPUT", "sk-proj", "wxyz", "HTTP 401"} {
		if all := logs.everything(); strings.Contains(all, secret) {
			t.Errorf("the log holds %q; it must hold neither what was said nor the agent's status text:\n%s", secret, all)
		}
	}
	if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 failed" {
		t.Errorf("outcomes reported = %v; want one, ns/b1 failed", got)
	}
}

// A turn is one start and one end however many times its send is tried or its stream followed, and a turn
// a restart finds again says it is not new.
func TestTurnLogsOneStartForRetriesAndFollows(t *testing.T) {
	t.Run("a send that is tried again", func(t *testing.T) {
		f, sink, logs, ends := logged(t)
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
		f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
			if n < 2 {
				return fmt.Errorf("%w: the agent is down", agent.ErrNotSent)
			}
			return ending(a2a.TaskStateCompleted, "done", "")(ctx, n, h)
		}
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
		eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) == 1 })
		if starts := logs.lines("session turn started"); len(starts) != 1 {
			t.Errorf("%d start lines for three sends of one turn; want one", len(starts))
		}
		if end := logs.lines("session turn ended")[0].attrs; end["outcome"] != "completed" || end["output"] != true {
			t.Errorf("the end line = %v; want completed, with output", end)
		}
		if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 completed" {
			t.Errorf("outcomes reported = %v; want one, ns/b1 completed", got)
		}
	})
	t.Run("a stream lost and followed", func(t *testing.T) {
		f, sink, logs, _ := logged(t)
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
		f.agent.script = lost(errors.New("connection reset"))
		f.agent.get = working(t0)
		f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error {
			return ending(a2a.TaskStateCompleted, "done", "")(nil, 0, h)
		}
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
		eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) == 1 })
		if starts := logs.lines("session turn started"); len(starts) != 1 {
			t.Errorf("%d start lines for a send and a follow of one turn; want one", len(starts))
		}
	})
	t.Run("a task found again after a restart", func(t *testing.T) {
		f, sink := restarted(t)
		logs, ends := newLogRecorder(), &turnEnds{}
		f.m.cfg.Logger, f.m.cfg.OnTurnEnd = slog.New(logs), ends.add
		f.recover(t)
		waitFinal(t, sink)
		eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) == 1 })
		starts := logs.lines("session turn started")
		if len(starts) != 1 || starts[0].attrs["recovered"] != true {
			t.Errorf("start lines = %v; want one, marked recovered", starts)
		}
	})
}

// A turn that ends without a task says how, with the same line: the class of the error, never its text.
func TestTurnLogsTheClassOfAFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  func(context.Context, int, agent.Handler) error
		outcome TurnOutcome
	}{
		{"credentials refused", func(context.Context, int, agent.Handler) error {
			return fmt.Errorf("%w: %w: FAKE-ERROR-TEXT", agent.ErrNotSent, agent.ErrCredentialsRejected)
		}, TurnCredentialsRejected},
		{"identity refused", func(context.Context, int, agent.Handler) error {
			return fmt.Errorf("send message: %w: FAKE-ERROR-TEXT", agent.ErrForbidden)
		}, TurnForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink, logs, ends := logged(t)
			f.agent.script = tc.script
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
			eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) == 1 })
			f.retired(t)
			if starts := logs.lines("session turn started"); len(starts) != 1 {
				t.Errorf("%d start lines; want one", len(starts))
			}
			end := logs.lines("session turn ended")[0].attrs
			if end["outcome"] != string(tc.outcome) || end["task"] != nil {
				t.Errorf("the end line = %v; want outcome %s and no task", end, tc.outcome)
			}
			if lines := fmt.Sprint(logs.lines("session turn started"), logs.lines("session turn ended")); strings.Contains(lines, "FAKE-ERROR-TEXT") {
				t.Errorf("the turn lines hold the text of the error: %s", lines)
			}
			if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 "+string(tc.outcome) {
				t.Errorf("outcomes reported = %v; want one, ns/b1 %s", got, tc.outcome)
			}
		})
	}
	t.Run("an agent that stays down", func(t *testing.T) {
		f, sink, logs, _ := logged(t)
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
		f.agent.script = func(_ context.Context, n int, _ agent.Handler) error {
			if n == 1 {
				f.now.Store(t0.Add(16 * time.Minute).UnixNano())
			}
			return fmt.Errorf("%w: the agent is down", agent.ErrNotSent)
		}
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
		eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) == 1 })
		if end := logs.lines("session turn ended")[0].attrs; end["outcome"] != string(TurnUnreachable) || end["duration_ms"] != int64(16*60*1000) {
			t.Errorf("the end line = %v; want outcome unreachable after the 16 minutes of trying", end)
		}
	})
}

// The stub agent over the real client and the kagent dialect: a task that fails with a model error, one
// the agent rejects, and one a person stops each tell the thread once. The stub's failed task has
// the reason in its status message, as kagent's does, and the thread does not see it.
func TestStubTerminalTasksTellTheThread(t *testing.T) {
	for _, tc := range []struct {
		name string
		step stubagent.Step
		want NoticeKind
	}{
		{"failed", stubagent.Fail(providerError), NoticeTaskFailed},
		{"rejected", stubagent.Reject(providerError), NoticeTaskRejected},
		{"completed with nothing to say", stubagent.Text(), NoticeNoOutput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newStubEnv(t)
			logs := newLogRecorder()
			e.m.cfg.Logger = slog.New(logs)
			e.stub.Next(tc.step)
			if _, err := e.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			e.sink.finalTask(t, 1)
			eventually(t, "the task to be cleared", func() bool { return e.taskID() == "" })
			e.retired(t)
			e.sink.mu.Lock()
			defer e.sink.mu.Unlock()
			if len(e.sink.notices) != 1 || e.sink.notices[0].Kind != tc.want || e.sink.notices[0].Detail != "" {
				t.Errorf("notices = %+v; want one %s, with no detail", e.sink.notices, tc.want)
			}
			if all := logs.everything(); strings.Contains(all, "wxyz") {
				t.Errorf("the log holds the agent's status text:\n%s", all)
			}
		})
	}
	t.Run("stopped by a person", func(t *testing.T) {
		e := newStubEnv(t)
		e.stub.Next(stubagent.Hold("never"), stubagent.Text("never said"))
		if _, err := e.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the task to run", func() bool { return e.taskID() != "" })
		if did, err := e.m.Stop(t.Context(), key, a2a.TaskID(e.taskID()), "U9"); err != nil || !did {
			t.Fatalf("Stop = %v, %v", did, err)
		}
		e.sink.finalTask(t, 1)
		eventually(t, "the notice", func() bool { return len(e.sink.noticesOf(NoticeTaskCanceled)) == 1 })
		e.retired(t)
		if got := e.sink.noticesOf(NoticeTaskCanceled); len(got) != 1 {
			t.Errorf("canceled notices = %+v; want one", got)
		}
	})
}

// Every way a turn leaves the runner logs its end with the class of it, and reports it once: a Binding that
// left the config, a session the agent lost, a task presumed wedged, and a pause that ended unanswered.
func TestTurnLogsHowTheOtherEndingsClassify(t *testing.T) {
	// ended waits for the end line, and checks that it is the only one and says want.
	ended := func(t *testing.T, logs *logRecorder, ends *turnEnds, want TurnOutcome) {
		t.Helper()
		eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) >= 1 })
		lines := logs.lines("session turn ended")
		if len(lines) != 1 || lines[0].attrs["outcome"] != string(want) {
			t.Errorf("end lines = %v; want one, outcome %s", lines, want)
		}
		if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 "+string(want) {
			t.Errorf("outcomes reported = %v; want one, ns/b1 %s", got, want)
		}
	}
	t.Run("a Binding that left the config", func(t *testing.T) {
		f, _, logs, ends := logged(t)
		f.m.wake = func(Key) {} // queue the turn first, run it after the Binding is gone
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/gone", LastActivity: t0}); err != nil {
			t.Fatal(err)
		}
		f.m.start(key)
		eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) >= 1 })
		if lines := logs.lines("session turn ended"); len(lines) != 1 || lines[0].attrs["outcome"] != string(TurnBindingGone) || lines[0].attrs["binding"] != "ns/gone" {
			t.Errorf("end lines = %v; want one, binding_gone, for ns/gone", lines)
		}
		if got := ends.get(); len(got) != 1 || got[0] != "ns/gone binding_gone" {
			t.Errorf("outcomes reported = %v; want one, ns/gone binding_gone", got)
		}
	})
	t.Run("a session the agent lost", func(t *testing.T) {
		f, _, logs, ends := logged(t)
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
		f.agent.script = lost(errors.New("connection reset"))
		f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return nil, a2a.NewError(a2a.ErrTaskNotFound, "gone") }
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		ended(t, logs, ends, TurnSessionGone)
	})
	t.Run("a task presumed wedged", func(t *testing.T) {
		logs, ends := newLogRecorder(), &turnEnds{}
		_, sink := wedgedFixture(t, func(f *fixture) { f.m.cfg.Logger, f.m.cfg.OnTurnEnd = slog.New(logs), ends.add }, "1")
		eventually(t, "the wedged notice", func() bool { return len(sink.noticesOf(NoticeWedged)) == 1 })
		ended(t, logs, ends, TurnWedged)
	})
	t.Run("a pause cancelled", func(t *testing.T) {
		logs, ends := newLogRecorder(), &turnEnds{}
		f := newFixture(t)
		f.m.cfg.Logger, f.m.cfg.OnTurnEnd = slog.New(logs), ends.add
		sink := f.live()
		f.agent.script = pauseFirst
		f.agent.get = func(id a2a.TaskID) (*a2a.Task, error) { return working(t0)(id) }
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
		eventually(t, "the runner to park", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return f.m.convos[key].parked == "t1" })
		if err := f.m.Cancel(t.Context(), key, "t1", "timeout"); err != nil {
			t.Fatal(err)
		}
		ended(t, logs, ends, TurnPauseEnded)
	})
}

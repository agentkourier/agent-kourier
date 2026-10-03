package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// A turn that fails after the task was named, with an error that is no lost stream (a refused identity on
// the follow, a permanent 4xx, a stream that ends with no final task), is not followed again and is not left
// silent: the thread is told in fixed words, the end is logged with its class, and the outcome is counted once.
// The task stays recorded, for reconciliation.
func TestTurnThatFailsForAGoodAfterTheTaskIsNamedIsTold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(f *fixture)
		detail  string
		outcome TurnOutcome
	}{
		{"a follow the agent refuses", func(f *fixture) {
			f.agent.script = lost(errors.New("connection reset"))
			f.agent.get = working(t0)
			f.agent.follow = func(a2a.TaskID, agent.Handler) error { return fmt.Errorf("follow task t1: %w", agent.ErrForbidden) }
		}, "the agent refused to continue the task", TurnRefused},
		{"a send that fails once the task is named", func(f *fixture) {
			f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
				accepted(h, n)
				return fmt.Errorf("send message: %w", agent.ErrForbidden)
			}
		}, "the agent refused to continue the task", TurnRefused},
		{"a stream that ends with no final task", func(f *fixture) {
			f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
				accepted(h, n)
				return nil
			}
		}, "the agent's reply ended before the task did", TurnNoFinal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink, logs, ends := logged(t)
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
			tc.setup(f)
			in := inbound("1")
			if _, err := f.submit(in); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) >= 1 })
			eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) >= 1 })
			f.retired(t)
			wantID := in.MessageID + "/failed"
			if tc.outcome == TurnRefused {
				wantID = taskNoticeID(key, "t1", NoticeFailed) // the refusal is named after the task, so a recovery raises the same post
			}
			if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != tc.detail || got[0].ID != wantID {
				t.Errorf("failed notices = %+v; want one with the detail %q", got, tc.detail)
			}
			lines := logs.lines("session turn ended")
			if len(lines) != 1 || lines[0].attrs["outcome"] != string(tc.outcome) || lines[0].attrs["task"] != "t1" {
				t.Errorf("end lines = %v; want one, outcome %s, task t1", lines, tc.outcome)
			}
			if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 "+string(tc.outcome) {
				t.Errorf("outcomes reported = %v; want one, ns/b1 %s", got, tc.outcome)
			}
			if _, _, follows := f.agent.counts(); follows > 1 {
				t.Errorf("%d follows; want the refused one only: it is not followed again", follows)
			}
			if f.taskID() != "t1" {
				t.Errorf("task %q recorded; want t1 kept, for reconciliation", f.taskID())
			}
		})
	}
	t.Run("a shutdown that cuts the stream tells nobody", func(t *testing.T) {
		f, sink, logs, ends := logged(t)
		f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
			accepted(h, n)
			<-ctx.Done()
			return fmt.Errorf("send message: %w", ctx.Err())
		}
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the task to be recorded", func() bool { return f.taskID() == "t1" })
		if err := f.close(); err != nil {
			t.Fatal(err)
		}
		if len(sink.notices) != 0 || len(logs.lines("session turn ended")) != 0 || len(ends.get()) != 0 {
			t.Errorf("notices %+v, end lines %v, outcomes %v; want none: the task is still running, and recorded", sink.notices, logs.lines("session turn ended"), ends.get())
		}
	})
}

// A notice about a task is named after the thread too: the outbox keeps one row per ID, and a backend's task
// IDs may repeat across threads (the stub agent's counter does), so the second thread's notice must not be
// taken for the first's. One fact in one thread is still one ID, whoever raises it.
func TestTaskNoticeIDNamesTheThread(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	other := Key{Connection: "slack", Channel: "C2", Thread: "1700.01"}
	f.m.taskNotice(t.Context(), key, "t1", NoticeTaskFailed, "")
	f.m.taskNotice(t.Context(), key, "t1", NoticeTaskFailed, "")
	f.m.taskNotice(t.Context(), other, "t1", NoticeTaskFailed, "")
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.notices) != 3 || sink.notices[0].ID != sink.notices[1].ID || sink.notices[0].ID == sink.notices[2].ID {
		t.Errorf("notice IDs = %+v; want one ID for one fact in a thread, and another for the same task ID in another thread", sink.notices)
	}
}

// What a turn said before its stream was lost counts after the follow, whose snapshot may not replay it: the
// renderer keeps the message it made, so a completed task is not told "finished without any output".
func TestTextSaidBeforeALostStreamCountsAfterTheFollow(t *testing.T) {
	f, sink, _, _ := logged(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		id := accepted(h, n)
		h(agent.TextEvent{ArtifactID: "a1", Text: "already shown"})
		return &agent.StreamLostError{TaskID: id, ContextID: "c1", Err: errors.New("connection reset")}
	}
	f.agent.get = working(t0)
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error {
		h(agent.StateEvent{TaskID: "t1", ContextID: "c1", State: a2a.TaskStateCompleted}) // a snapshot, with no text replayed
		h(agent.FinalEvent{Task: &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
		return nil
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	f.retired(t)
	if _, _, follows := f.agent.counts(); follows != 1 {
		t.Fatalf("%d follows; want the one that ends the turn", follows)
	}
	if len(sink.notices) != 0 {
		t.Errorf("notices = %+v; want none: the turn said something before the stream was lost", sink.notices)
	}
}

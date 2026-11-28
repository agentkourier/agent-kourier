package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// Every stranded-task failure notice (rejected credentials on follow, refused follow, and uncancellable wedged task)
// carries the recorded TaskID so the renderer attaches a Stop button.
func TestStrandedFailureNoticesCarryTaskID(t *testing.T) {
	t.Run("credentials rejected on follow", func(t *testing.T) {
		f, sink := restarted(t)
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
		f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
			return nil, fmt.Errorf("get task t1: %w", agent.ErrCredentialsRejected)
		}
		f.recover(t)
		testutil.Eventually(t, "notice emitted", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
		f.retired(t)
		if got := sink.noticesOf(NoticeFailed); got[0].TaskID != "t1" {
			t.Errorf("notice TaskID = %q; want t1", got[0].TaskID)
		}
	})

	t.Run("refused follow", func(t *testing.T) {
		f, sink := restarted(t)
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
		f.agent.get = working(t0)
		f.agent.follow = func(a2a.TaskID, agent.Handler) error {
			return fmt.Errorf("follow task t1: %w", errors.New("400 bad request"))
		}
		f.recover(t)
		testutil.Eventually(t, "notice emitted", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
		f.retired(t)
		if got := sink.noticesOf(NoticeFailed); got[0].TaskID != "t1" {
			t.Errorf("notice TaskID = %q; want t1", got[0].TaskID)
		}
	})

	t.Run("uncancellable wedged task", func(t *testing.T) {
		f, sink := restarted(t)
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
		f.m.cfg.MaxWorkingAge = time.Second
		f.now.Store(t0.Add(time.Hour).UnixNano())
		f.agent.get = working(t0)
		f.agent.cancelErr = func(int) error { return fmt.Errorf("cancel t1: %w", agent.ErrForbidden) }
		f.recover(t)
		testutil.Eventually(t, "notice emitted", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
		f.retired(t)
		if got := sink.noticesOf(NoticeFailed); got[0].TaskID != "t1" {
			t.Errorf("notice TaskID = %q; want t1", got[0].TaskID)
		}
		if _, err := f.m.Stop(context.Background(), key, "t1", "U1"); err == nil {
			t.Errorf("Stop with forbidden cancel succeeded unexpectedly")
		}
	})
}

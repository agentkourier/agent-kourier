package launcher

import (
	"context"
	"encoding/hex"
	"errors"
	"iter"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	kit "github.com/agentkourier/agent-kourier/addons/kit/store"
)

func (e *launcher) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		deadline := e.now().Add(e.cfg.timeout)
		submitted := a2a.NewSubmittedTask(ec, ec.Message)
		if !yield(submitted, nil) {
			return
		}
		id := result.NewID()
		task, _, err := e.taskForTurn(ctx, ec, id, deadline)
		if err != nil {
			yield(failedTask(submitted, "AX could not prepare the Task.", ""), nil)
			return
		}
		record := store.Launch{
			TaskID: string(ec.TaskID), Name: task.Metadata.Name, Fingerprint: fingerprint(task.Spec),
			ResultID: id, Deadline: deadline.UnixNano(),
		}
		waitCtx, stop := context.WithDeadline(ctx, deadline)
		defer stop()
		registered, registerErr := e.register(waitCtx, record)
		if !registered {
			if ctx.Err() != nil {
				return
			}
			if l, err := e.st.Launch(ctx, record.TaskID); err == nil && l.Decision == store.Canceled {
				yield(terminalTask(submitted, l), nil)
			} else if errors.Is(registerErr, context.DeadlineExceeded) || !e.now().Before(deadline) {
				yield(failedTask(submitted, "The AX Task deadline elapsed.", "DEADLINE_EXCEEDED"), nil)
			} else {
				yield(failedTask(submitted, "AX could not register the Task.", ""), nil)
			}
			return
		}

		wakeup := make(chan struct{}, 1)
		e.waiters.Store(record.TaskID, wakeup)
		defer e.waiters.Delete(record.TaskID)
		// A result can settle before Resume replies. Drain the bounded AX
		// call independently so cleanup can use its definitive response.
		e.launchMu.Lock()
		if e.life.Err() != nil {
			e.launchMu.Unlock()
			return
		}
		launchCtx, cancelLaunch := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		stopLaunch := context.AfterFunc(e.life, cancelLaunch) //nolint:contextcheck // the add-on lifetime also bounds the detached RPC
		e.active.Store(record.TaskID, struct{}{})
		e.launches.Go(func() {
			defer cancelLaunch()
			defer stopLaunch()
			defer e.active.Delete(record.TaskID)
			e.launch(launchCtx, record, task)
		})
		e.launchMu.Unlock()
		e.follow(ctx, submitted, record, deadline, wakeup, yield)
	}
}

func (e *launcher) register(ctx context.Context, l store.Launch) (bool, error) {
	// The SDK queues the submitted event before persisting it. The INSERT is
	// conditional on that snapshot being present and still eligible.
	for {
		ok, err := e.st.Register(ctx, l)
		if ok || err != nil {
			return ok, err
		}
		if _, err = e.st.Launch(ctx, l.TaskID); err == nil {
			return false, nil
		} else if !errors.Is(err, kit.ErrNoRows) {
			return false, err
		}
		if !sleep(ctx, 10*time.Millisecond) {
			return false, ctx.Err()
		}
	}
}

func (e *launcher) follow(ctx context.Context, task *a2a.Task, record store.Launch, deadline time.Time, wakeup <-chan struct{}, yield func(a2a.Event, error) bool) {
	pollAt := e.now()
	var queued, running bool
	for ctx.Err() == nil {
		now := e.now()
		if !now.Before(deadline) {
			if _, err := e.st.Decide(ctx, record.TaskID, store.TimedOut); err != nil {
				yield(nil, errors.New("could not record AX deadline"))
				return
			}
		}
		l, err := e.st.Launch(ctx, record.TaskID)
		if err != nil {
			yield(nil, errors.New("could not read AX launch"))
			return
		}
		if l.Decision != "" {
			if l.Receipt == "" || beforeSettlement(ctx, task) {
				yield(terminalTask(task, l), nil)
			}
			return
		}
		if !now.Before(pollAt) && l.ResumeSent != 0 {
			pollAt = now.Add(e.cfg.poll)
			pollCtx, cancel := context.WithDeadline(ctx, minTime(deadline, now.Add(2*time.Second)))
			observed, err := e.get(pollCtx, l.Name)
			cancel()
			if err == nil && !e.owns(observed, l) {
				_, _ = e.st.Decide(ctx, l.TaskID, store.Failed)
				continue
			}
			if observed != nil && err == nil {
				if missingActor(observed) || provisioningFailed(observed) || stoppedWithoutResult(observed, l) {
					_, _ = e.st.Decide(ctx, l.TaskID, store.Failed)
					continue
				}
				progress := ""
				switch observed.GetStatus().GetState() {
				case ax.TaskState_TASK_STATE_QUEUED:
					if !queued {
						queued = true
						progress = "AX sandbox queued."
					}
				case ax.TaskState_TASK_STATE_RUNNING:
					if !running {
						running = true
						progress = "AX sandbox running."
					}
				case ax.TaskState_TASK_STATE_CRASHED, ax.TaskState_TASK_STATE_DELETING:
					_, _ = e.st.Decide(ctx, l.TaskID, store.Failed)
					continue
				}
				if progress != "" && !yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking,
					a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart(progress))), nil) {
					return
				}
			}
			if code(err) == codes.NotFound {
				_, _ = e.st.Decide(ctx, l.TaskID, store.Failed)
			}
		}
		timer := time.NewTimer(min(100*time.Millisecond, e.cfg.poll))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wakeup:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func stoppedWithoutResult(task *ax.Task, launch store.Launch) bool {
	// A suspended Task before Resume replies is expected. Decide rechecks
	// the durable decision atomically if a result arrived during this Get.
	return launch.ResumeSent != 0 && launch.DrainUntil == 0 && task.GetStatus().GetState() == ax.TaskState_TASK_STATE_SUSPENDED
}

func (e *launcher) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	// CancelAdmission already committed the decision before entering the SDK.
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func (e *launcher) Cleanup(ctx context.Context, ec *a2asrv.ExecutorContext, res a2a.SendMessageResult, _ error) {
	task, ok := res.(*a2a.Task)
	if !ok || !task.Status.State.Terminal() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	l, err := e.st.Launch(ctx, string(ec.TaskID))
	if err != nil || l.Clean != 0 {
		return
	}
	at := e.now()
	if l.Decision == store.CommandFail && task.Status.State == a2a.TaskStateFailed {
		at = at.Add(e.cfg.keep)
	}
	e.diagnostic(ctx, l, "pending")
	if err := e.st.ScheduleCleanup(ctx, l.TaskID, at); err != nil {
		e.diagnostic(ctx, l, "schedule_retry")
	}
}

func terminalTask(task *a2a.Task, l store.Launch) *a2a.Task {
	switch l.Decision {
	case store.Succeeded:
		snapshot := *task
		snapshot.Status = a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil).Status
		output, _ := hex.DecodeString(l.Output)
		text := string(output)
		if l.Truncated != 0 {
			text += "\n\n[AX output truncated]"
		}
		if strings.TrimSpace(text) != "" {
			snapshot.Artifacts = []*a2a.Artifact{{ID: "ax-result", Name: "AX result", Parts: []*a2a.Part{a2a.NewTextPart(text)}}}
		}
		return &snapshot
	case store.Canceled:
		snapshot := *task
		snapshot.Status = a2a.NewStatusUpdateEvent(task, a2a.TaskStateCanceled, nil).Status
		return &snapshot
	case store.TimedOut:
		return failedTask(task, "The AX Task deadline elapsed.", "DEADLINE_EXCEEDED")
	case store.CommandFail:
		return failedTask(task, "The AX command failed.", "")
	default:
		return failedTask(task, "The AX sandbox could not complete the turn.", "")
	}
}

func failedTask(task *a2a.Task, text, code string) *a2a.Task {
	snapshot := *task
	message := a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart(text))
	if code != "" {
		message.SetMeta("code", code)
	}
	snapshot.Status = a2a.NewStatusUpdateEvent(task, a2a.TaskStateFailed, message).Status
	return &snapshot
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

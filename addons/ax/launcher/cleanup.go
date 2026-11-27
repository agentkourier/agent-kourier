package launcher

import (
	"context"
	"errors"
	"time"

	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/agentkourier/agent-kourier/addons/ax/store"
	kit "github.com/agentkourier/agent-kourier/addons/kit/store"
)

const (
	maxCleanupInterval  = 5 * time.Second
	orphanSweepInterval = time.Minute
)

func (e *launcher) RunLeader(ctx context.Context) {
	cleanup := time.NewTicker(min(e.cfg.poll, maxCleanupInterval))
	defer cleanup.Stop()
	sweep := time.NewTicker(orphanSweepInterval)
	defer sweep.Stop()
	e.cleanPending(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-cleanup.C:
			e.cleanPending(ctx)
		case <-sweep.C:
			e.sweep(ctx)
		}
	}
}

func (e *launcher) cleanPending(ctx context.Context) {
	if err := e.st.ScheduleSettled(ctx, e.cfg.keep); err != nil {
		if ctx.Err() == nil {
			e.log.WarnContext(ctx, "could not schedule AX cleanup", "error", err)
		}
		return
	}
	pending, err := e.st.PendingCleanup(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.log.WarnContext(ctx, "could not read pending AX cleanup", "error", err)
		}
		return
	}
	now := e.now().UnixNano()
	var retained int
	for _, launch := range pending {
		if launch.Suspended != 0 && launch.CleanupAt > now {
			retained++
		}
	}
	e.cleanup.Set(float64(len(pending) - retained))
	e.retained.Set(float64(retained))
	for _, l := range pending {
		if ctx.Err() != nil {
			return
		}
		if _, active := e.active.Load(l.TaskID); active {
			continue
		}
		now := e.now().UnixNano()
		if l.DrainUntil > now || (l.Suspended != 0 && l.CleanupAt > now) {
			continue
		}
		e.cleanLaunch(ctx, l, now)
	}
}

func (e *launcher) cleanLaunch(ctx context.Context, l store.Launch, now int64) {
	task, err := e.get(ctx, l.Name)
	if code(err) == codes.NotFound {
		e.recordCleanup(ctx, l, "absent", e.st.MarkClean(ctx, l.TaskID))
		return
	}
	if err != nil {
		e.cleanupRetry(ctx, l, "lookup_retry", err)
		return
	}
	if !e.owns(task, l) {
		e.recordCleanup(ctx, l, "ownership_changed", e.st.MarkForeign(ctx, l.TaskID))
		return
	}
	if l.ActorUID != "" && task.GetStatus().GetActorUid() == "" && !missingActor(task) {
		// AX could not resolve the actor. Keep checking, unless deletion is
		// already pending and its actor has disappeared as a result.
		if task.GetMetadata().GetDeleteTime() != nil {
			e.diagnostic(ctx, l, "delete_pending")
		} else {
			e.diagnostic(ctx, l, "actor_uid_retry")
		}
		return
	}
	if l.UID == "" {
		l.UID, l.ActorUID = task.Metadata.Uid, task.GetStatus().GetActorUid()
		if ok, err := e.st.Bind(ctx, l.TaskID, l.UID, l.ActorUID); !ok || err != nil {
			e.cleanupRetry(ctx, l, "ownership_retry", err)
			return
		}
	}
	if err := e.st.CheckFence(ctx); err != nil {
		e.cleanupRetry(ctx, l, "fence_retry", err)
		return
	}
	if l.CleanupAt > now {
		_, err := call(ctx, e, "suspend", func(ctx context.Context) (*ax.Task, error) {
			return e.client.SuspendTask(ctx, &ax.SuspendTaskRequest{Atespace: e.cfg.atespace, Name: l.Name})
		})
		switch code(err) {
		case codes.OK:
			e.recordCleanup(ctx, l, "suspended", e.st.MarkSuspended(ctx, l.TaskID))
		case codes.NotFound:
			e.recordCleanup(ctx, l, "absent", e.st.MarkClean(ctx, l.TaskID))
		default:
			e.cleanupRetry(ctx, l, "suspend_retry", err)
		}
		return
	}
	switch err := e.delete(ctx, l.Name); code(err) {
	case codes.NotFound:
		e.recordCleanup(ctx, l, "absent", e.st.MarkClean(ctx, l.TaskID))
	case codes.OK:
		// Confirm task removal with Get. A delete_time alone can also mean
		// AX recorded a failed deletion, which the next sweep must retry.
		e.diagnostic(ctx, l, "delete_requested")
	default:
		e.cleanupRetry(ctx, l, "delete_retry", err)
	}
}

func (e *launcher) recordCleanup(ctx context.Context, l store.Launch, disposition string, err error) {
	if err != nil {
		e.cleanupRetry(ctx, l, disposition+"_record_retry", err)
		return
	}
	e.diagnostic(ctx, l, disposition)
}

func (e *launcher) cleanupRetry(ctx context.Context, l store.Launch, disposition string, err error) {
	// AX error details can come from the runner. Record the gRPC status
	// without its message, which can contain Task data.
	if _, ok := status.FromError(err); ok && err != nil {
		err = errors.New(code(err).String())
	}
	e.log.WarnContext(ctx, "retry AX cleanup", "a2a_task", l.TaskID, "ax_task", l.Name,
		"ax_uid", l.UID, "cleanup", disposition, "error", err)
}

func (e *launcher) diagnostic(ctx context.Context, l store.Launch, disposition string) {
	attrs := []any{"a2a_task", l.TaskID, "ax_task", l.Name, "ax_uid", l.UID,
		"decision", l.Decision, "cleanup", disposition}
	if l.ExitCode >= 0 {
		attrs = append(attrs, "exit_code", l.ExitCode)
	}
	e.log.InfoContext(ctx, "AX Task lifecycle", attrs...)
}

func (e *launcher) delete(ctx context.Context, name string) error {
	_, err := call(ctx, e, "delete", func(ctx context.Context) (*emptypb.Empty, error) {
		return e.client.DeleteTask(ctx, &ax.DeleteTaskRequest{Atespace: e.cfg.atespace, Name: name})
	})
	return err
}

func (e *launcher) sweep(ctx context.Context) {
	const pageSize = 100
	cutoff := e.now().Add(-maxDeadline - unknownRPCTime)
	page := ""
	for ctx.Err() == nil {
		list, err := call(ctx, e, "list", func(ctx context.Context) (*ax.ListTasksResponse, error) {
			return e.client.ListTasks(ctx, &ax.ListTasksRequest{Atespace: e.cfg.atespace, PageSize: pageSize, PageToken: page})
		})
		if err != nil {
			return
		}
		for _, task := range list.GetTasks() {
			if !e.scoped(task) {
				continue
			}
			e.cleanLateTask(ctx, task, cutoff)
		}
		page = list.GetNextPageToken()
		if page == "" {
			break
		}
	}
	if err := e.st.PruneReceipts(ctx, cutoff); err != nil && ctx.Err() == nil {
		e.log.WarnContext(ctx, "could not prune AX receipts", "error", err)
	}
}

func (e *launcher) cleanLateTask(ctx context.Context, task *ax.Task, cutoff time.Time) {
	m := task.GetMetadata()
	l, err := e.st.LaunchByName(ctx, m.Name)
	switch {
	case err == nil:
		if l.Clean == store.CleanDone {
			// Get again immediately before a write. A replacement between List
			// and Get must be rejected by its UID and fingerprint.
			e.cleanLaunch(ctx, l, e.now().UnixNano())
		}
	case errors.Is(err, kit.ErrNoRows):
		if m.GetCreateTime() == nil || !m.CreateTime.AsTime().Before(cutoff) {
			return
		}
		current, err := e.get(ctx, m.Name)
		if err != nil || !e.scoped(current) || current.GetMetadata().GetUid() != m.Uid ||
			current.GetStatus().GetActorUid() != task.GetStatus().GetActorUid() {
			return
		}
		if err := e.st.CheckFence(ctx); err != nil {
			return
		}
		err = e.delete(ctx, m.Name)
		e.log.InfoContext(ctx, "requested deletion of an orphaned AX Task", "ax_task", m.Name, "ax_uid", m.Uid, "code", code(err))
	}
}

func missingActor(task *ax.Task) bool {
	for _, c := range task.GetStatus().GetConditions() {
		if c.GetType() == ax.ConditionReady && c.GetReason() == ax.ReasonActorNotFound {
			return true
		}
	}
	return false
}

package launcher

import (
	"context"
	"errors"
	"strings"
	"time"

	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agentkourier/agent-kourier/addons/ax/store"
)

const (
	rpcTimeout        = 25 * time.Second
	unknownRPCTime    = 2 * time.Minute
	maxResumeAttempts = 5
)

func (e *launcher) launch(ctx context.Context, l store.Launch, task *ax.Task) {
	if !e.create(ctx, l, task) || !e.resume(ctx, l) {
		if ctx.Err() == nil {
			_, _ = e.st.Decide(ctx, l.TaskID, store.Failed)
		}
	}
}

func (e *launcher) create(ctx context.Context, l store.Launch, task *ax.Task) bool {
	for ctx.Err() == nil {
		dispatched, err := e.st.Dispatch(ctx, l.TaskID, false, e.now().Add(rpcTimeout+unknownRPCTime))
		if err != nil || !dispatched {
			return false
		}
		created, err := call(ctx, e, "create", func(ctx context.Context) (*ax.Task, error) {
			return e.client.CreateTask(ctx, &ax.CreateTaskRequest{Task: task})
		})
		if !unknown(err) {
			_ = e.st.Drained(ctx, l.TaskID)
		}
		if err != nil {
			ambiguous := unknown(err)
			if !ambiguous && code(err) != codes.FailedPrecondition && code(err) != codes.AlreadyExists {
				return false
			}
			// If Create reached the server, it may have persisted an immutable
			// Task even when the response was lost.
			created, err = e.get(ctx, l.Name)
			if err != nil {
				if (unknown(err) || ambiguous && code(err) == codes.NotFound) && sleep(ctx, min(e.cfg.poll, time.Second)) {
					continue
				}
				return false
			}
		}
		if !e.owns(created, l) || created.GetMetadata().GetUid() == "" {
			return false
		}
		ok, err := e.st.Bind(ctx, l.TaskID, created.Metadata.Uid, created.GetStatus().GetActorUid())
		return ok && err == nil
	}
	return false
}

func (e *launcher) resume(ctx context.Context, intent store.Launch) bool {
	for dispatched := 0; dispatched < maxResumeAttempts && ctx.Err() == nil; {
		l, err := e.st.Launch(ctx, intent.TaskID)
		if err != nil || l.Decision != "" || l.Invocation != "" {
			return err == nil
		}
		observed, err := e.get(ctx, l.Name)
		if err != nil {
			if unknown(err) && sleep(ctx, min(e.cfg.poll, time.Second)) {
				continue
			}
			return false
		}
		if !e.owns(observed, l) || missingActor(observed) {
			return false
		}
		actor := observed.GetStatus().GetActorUid()
		if actor == "" {
			if !sleep(ctx, min(e.cfg.poll, time.Second)) {
				return false
			}
			continue
		}
		ok, err := e.st.Bind(ctx, l.TaskID, observed.Metadata.Uid, actor)
		if err != nil || !ok {
			return false
		}
		ok, err = e.st.Dispatch(ctx, l.TaskID, true, e.now().Add(rpcTimeout+unknownRPCTime))
		if err != nil || !ok {
			return err == nil
		}
		dispatched++
		_, err = call(ctx, e, "resume", func(ctx context.Context) (*ax.Task, error) {
			return e.client.ResumeTask(ctx, &ax.ResumeTaskRequest{Atespace: e.cfg.atespace, Name: l.Name})
		})
		if !unknown(err) {
			_ = e.st.Drained(ctx, l.TaskID)
		}
		if err == nil {
			return true // AX acknowledged runner delivery
		}
		latest, readErr := e.st.Launch(ctx, l.TaskID)
		if readErr == nil && (latest.Invocation != "" || latest.Decision != "") {
			return true
		}
		if !unknown(err) || !sleep(ctx, min(e.cfg.poll, time.Second)) {
			return false
		}
	}
	return false
}

func (e *launcher) get(ctx context.Context, name string) (*ax.Task, error) {
	return call(ctx, e, "get", func(ctx context.Context) (*ax.Task, error) {
		return e.client.GetTask(ctx, &ax.GetTaskRequest{Atespace: e.cfg.atespace, Name: name})
	})
}

func (e *launcher) scoped(task *ax.Task) bool {
	m := task.GetMetadata()
	turn := m.GetLabels()[turnLabel]
	return m.GetUid() != "" && m.GetAtespace() == e.cfg.atespace && m.GetLabels()[nameLabel] == e.cfg.identity &&
		len(turn) == 32 && m.GetName() == taskName(e.cfg, turn)
}

func (e *launcher) owns(task *ax.Task, l store.Launch) bool {
	if !e.scoped(task) || task.GetMetadata().GetName() != l.Name || fingerprint(task.GetSpec()) != l.Fingerprint {
		return false
	}
	uid, actor := task.GetMetadata().GetUid(), task.GetStatus().GetActorUid()
	return (l.UID == "" || l.UID == uid) && (l.ActorUID == "" || actor == "" || l.ActorUID == actor)
}

func call[T any](ctx context.Context, e *launcher, method string, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	started := time.Now()
	value, err := fn(ctx)
	e.rpc.WithLabelValues(method, code(err).String()).Observe(time.Since(started).Seconds())
	return value, err
}

func code(err error) codes.Code {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Code()
	}
	return status.Code(err)
}

func unknown(err error) bool {
	switch code(err) {
	case codes.Canceled, codes.DeadlineExceeded, codes.Unavailable, codes.Aborted, codes.ResourceExhausted, codes.Internal, codes.Unknown:
		// rc.2 can wrap the runner's 409 inside an internal gRPC error.
		return !strings.Contains(status.Convert(err).Message(), "runner is running another task")
	default:
		return false
	}
}

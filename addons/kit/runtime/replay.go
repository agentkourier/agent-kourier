package runtime

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// replayHandler is the request handler in front of a2a-go's. A message whose ID the store has recorded joins that
// message's task instead of starting another turn, after a restart too: Agent Kourier resends a message with the same
// ID when it cannot tell whether the first send arrived. The store records a message's ID in the transaction that
// stores its task (a new task's history holds the message, and an answer is added to its task's history before the
// turn runs), so the handler holds a send of one ID, and only of that ID, from its lookup until the turn's first event,
// also when its caller hangs up before that (follow): a resend that races it in this process waits and finds the task.
//
// It also marks a message's context active before the message is accepted (touch), refuses an answer the backend's
// ResumeValidator does not accept before the turn starts, and follows a task only for its own caller.
type replayHandler struct {
	a2asrv.RequestHandler
	store     Store
	tel       *telemetry.Telemetry
	validator ResumeValidator // nil: every answer reaches the backend
	life      context.Context // ends with the add-on's stop, which ends a stream that outlived its request

	mu      sync.Mutex
	sending map[string]chan struct{} // by message ID: a send between its lookup and its first event, closed at its end
}

// claim waits until no other send of the message ID is between its lookup and its first event, then makes this send
// that one. The returned func ends the claim; it may be called more than once.
func (h *replayHandler) claim(ctx context.Context, messageID string) (func(), error) {
	for {
		h.mu.Lock()
		pending, ok := h.sending[messageID]
		if !ok {
			done := make(chan struct{})
			if h.sending == nil {
				h.sending = map[string]chan struct{}{}
			}
			h.sending[messageID] = done
			h.mu.Unlock()
			return sync.OnceFunc(func() {
				h.mu.Lock()
				delete(h.sending, messageID)
				h.mu.Unlock()
				close(done)
			}), nil
		}
		h.mu.Unlock()
		select {
		case <-pending:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// SendStreamingMessage implements a2asrv.RequestHandler.
func (h *replayHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if req == nil || req.Message == nil || req.Message.ID == "" {
			yield(nil, a2a.ErrInvalidParams)
			return
		}
		if err := h.touch(ctx, req.Message); err != nil {
			yield(nil, err)
			return
		}
		unclaim, err := h.claim(ctx, req.Message.ID)
		if err != nil {
			yield(nil, err)
			return
		}
		defer unclaim()
		old, err := h.store.MessageTask(ctx, req.Message.ID)
		if err != nil {
			yield(nil, err)
			return
		}
		if old != nil {
			unclaim()
			h.tel.MessageReplay()
			h.rejoin(ctx, req.Message, old, yield)
			return
		}
		if err := h.checkResume(ctx, req.Message); err != nil {
			yield(nil, err)
			return
		}
		h.follow(ctx, req, unclaim, yield)
	}
}

// follow streams the turn of req, holding its message ID's claim until the turn's first event, which a2a-go sends once
// it has stored the task and so the message ID, or the end of the execution without one. A caller who hangs up before
// that does not end the claim: the stream goes on, detached from the request, until that event, or the add-on's stop,
// so that a resend waits and joins the one turn rather than sending the message to the backend again. Once the event
// has come, the request's end ends the stream.
func (h *replayHandler) follow(ctx context.Context, req *a2a.SendMessageRequest, unclaim func(), yield func(a2a.Event, error) bool) {
	inner, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	defer context.AfterFunc(h.life, cancel)() //nolint:contextcheck // the add-on's stop ends a stream that outlived its request
	first := true
	for ev, err := range h.RequestHandler.SendStreamingMessage(inner, req) {
		if first {
			first = false
			unclaim()
			defer context.AfterFunc(ctx, cancel)()
		}
		if !yield(ev, err) {
			return
		}
	}
}

// rejoin answers a resent message with old, the task its first send started or joined: the task as it stands if it
// has settled or paused, also when it does so before the stream is followed, and otherwise the rest of its stream. A resend that names another context or task than old's
// is refused.
func (h *replayHandler) rejoin(ctx context.Context, msg *a2a.Message, old *a2a.Task, yield func(a2a.Event, error) bool) {
	if (msg.ContextID != "" && msg.ContextID != old.ContextID) || (msg.TaskID != "" && msg.TaskID != old.ID) {
		yield(nil, a2a.ErrInvalidParams)
		return
	}
	if settledOrPaused(old) {
		yield(old, nil)
		return
	}
	first := true
	for ev, err := range h.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: old.ID}) {
		if first && err != nil {
			// The turn may have ended since old was read, and a2a-go follows no settled task: answer it as it stands.
			if now, getErr := h.GetTask(ctx, &a2a.GetTaskRequest{ID: old.ID}); getErr == nil && settledOrPaused(now) {
				yield(now, nil)
				return
			}
		}
		first = false
		if !yield(ev, err) {
			return
		}
	}
}

// settledOrPaused reports whether no turn of task runs: it has ended, or it waits for an answer.
func settledOrPaused(task *a2a.Task) bool {
	return task.Status.State.Terminal() || task.Status.State == a2a.TaskStateInputRequired
}

// checkResume refuses a message for a paused task that the backend's ResumeValidator does not accept, with
// a2a.ErrInvalidParams. A message for a task the caller does not have, or one that is not paused, is the inner
// handler's to answer.
func (h *replayHandler) checkResume(ctx context.Context, msg *a2a.Message) error {
	if h.validator == nil || msg.TaskID == "" {
		return nil
	}
	task, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: msg.TaskID})
	if err != nil || task.Status.State != a2a.TaskStateInputRequired {
		return nil
	}
	if err := h.validator.ValidateResume(ctx, task, msg); err != nil {
		if !errors.Is(err, a2a.ErrInvalidParams) {
			err = fmt.Errorf("%w: %w", a2a.ErrInvalidParams, err)
		}
		return err
	}
	return nil
}

// touch marks the context a message is for active, in a transaction that commits before the message reaches the
// turn: the message's context, or else its task's; a message for neither starts a new context, which has nothing to
// keep. DeleteIdleContexts deletes a context only if it is still idle when its delete runs, in its own transaction, so
// a sweep whose delete comes after the touch keeps the context. One window remains: a sweep whose delete came first.
// The touch waits for it to commit and records the context anew, and the turn finds what the backend kept gone and
// starts afresh (a message naming a deleted task gets task not found), as it would have had it come a moment later.
func (h *replayHandler) touch(ctx context.Context, msg *a2a.Message) error {
	contextID := msg.ContextID
	if contextID == "" && msg.TaskID != "" {
		task, err := h.store.Get(ctx, msg.TaskID)
		switch {
		case errors.Is(err, a2a.ErrTaskNotFound):
			return nil // the handler refuses the message
		case err != nil:
			return err
		}
		contextID = task.Task.ContextID
	}
	if contextID == "" {
		return nil
	}
	return h.store.TouchContext(ctx, contextID)
}

// SubscribeToTask follows a task of the call's caller. a2a-go finds a running execution by task ID alone, with no
// owner check, and streams it even when the store does not return the task to the caller, so the task is loaded for
// the caller first: another caller's task is not found.
func (h *replayHandler) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if req == nil {
			yield(nil, a2a.ErrInvalidParams)
			return
		}
		if _, err := h.store.Get(ctx, req.ID); err != nil {
			yield(nil, err)
			return
		}
		for event, err := range h.RequestHandler.SubscribeToTask(ctx, req) {
			if !yield(event, err) {
				return
			}
		}
	}
}

// SendMessage implements a2asrv.RequestHandler: the streaming send, answered with its task as it ends. The task is the
// one its events name: a turn that resumes a task may yield no *a2a.Task, only updates of it.
func (h *replayHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	var id a2a.TaskID
	for event, err := range h.SendStreamingMessage(ctx, req) {
		if err != nil {
			return nil, err
		}
		switch e := event.(type) {
		case *a2a.Task:
			id = e.ID
		case *a2a.TaskStatusUpdateEvent:
			id = e.TaskID
		case *a2a.TaskArtifactUpdateEvent:
			id = e.TaskID
		}
	}
	if id == "" {
		return nil, errors.New("task result missing")
	}
	return h.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
}

package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// ErrNotPaused means the conversation has no task to end or answer: none is recorded, or the
// conversation is not paused on the task named.
var ErrNotPaused = errors.New("session: the conversation is not paused on that task")

// Stop is the thread's stop control (spec 8.2): it cancels the task taskID, the one the control
// was rendered for, and the queued replies then go on. It reports false, without calling the
// agent, when that is not the task recorded for the thread, so an old button cannot stop a newer
// task. The caller has already checked that by may press the button (D12). A running task's stream
// ends cancelled, and the runner then posts NoticeTaskCanceled; a paused task has no stream, so Stop ends the pause and
// tells the thread. It works after a restart. A failed cancel is classified as a wedged task's is
// (see cancelActive). Stop calls the agent before it returns, so an adapter runs it off Slack's
// 3-second acknowledgement path: enqueue the control, and act on it from the queue.
func (m *Manager) Stop(ctx context.Context, key Key, taskID a2a.TaskID, by string) (stopped bool, err error) {
	done, paused, err := m.cancelActive(ctx, key, taskID, by, false)
	if err != nil || !done {
		return false, err
	}
	if paused {
		m.taskNotice(ctx, key, taskID, NoticeStopped, "")
	}
	return true, nil
}

// Cancel cancels the task taskID a conversation is paused on and ends the pause, for the
// interaction timeout and an unusable pause (spec 8.4): the queued replies go on. ErrNotPaused
// means the recorded task is not taskID, or it is not paused: a timeout that fires late, for a
// pause that has ended, must leave the newer task alone. It is audited, naming no person, and
// tells the thread nothing, which the caller does. It works after a restart. reason is for the
// log. Like Stop, it calls the agent before it returns: run it off any acknowledgement path.
func (m *Manager) Cancel(ctx context.Context, key Key, taskID a2a.TaskID, reason string) error {
	done, _, err := m.cancelActive(ctx, key, taskID, "", true)
	switch {
	case err != nil:
		return err
	case !done:
		return ErrNotPaused
	}
	m.cfg.Logger.Info("cancelled a paused task", "key", key, "task", taskID, "reason", reason)
	return nil
}

// cancelAttempts bounds how often a synchronous control retries a cancel that may pass.
const cancelAttempts = 3

// cancelKind says what a failed CancelTask means, for the runner's wedged-task cancel and the
// controls alike.
type cancelKind int

const (
	cancelDone        cancelKind = iota // it worked
	cancelGone                          // the agent no longer knows the task (-32001)
	cancelEnded                         // the task ended in the meantime (-32002)
	cancelCredentials                   // the front door refused the credentials
	cancelForbidden                     // the agent refused the identity (-31403)
	cancelTransient                     // anything else: it may pass
)

func classifyCancel(err error) cancelKind {
	switch {
	case err == nil:
		return cancelDone
	case errors.Is(err, a2a.ErrTaskNotFound):
		return cancelGone
	case errors.Is(err, a2a.ErrTaskNotCancelable):
		return cancelEnded
	case errors.Is(err, agent.ErrCredentialsRejected):
		return cancelCredentials
	case errors.Is(err, agent.ErrForbidden):
		return cancelForbidden
	}
	return cancelTransient
}

// cancelActive cancels the session's recorded task, which must be id, and audits the decision
// once. A cancel that failed is classified: a task that ended meanwhile counts as cancelled once
// GetTask says so; a task the agent no longer knows resets the session; a refused credential
// goes to the identity Source; a refused identity is audited; anything else is retried a few
// times, then reported. Where the task is over, the pause ends if the runner is parked on it, or
// the record is cleared and the queue woken if no runner drives it. done is false when id is not
// the recorded task, and, with needPause, when it is not paused.
func (m *Manager) cancelActive(ctx context.Context, key Key, id a2a.TaskID, by string, needPause bool) (done, paused bool, err error) {
	sess, err := m.cfg.Store.GetSession(ctx, key, readAll, m.cfg.Now())
	if errors.Is(err, store.ErrNotFound) || (err == nil && (id == "" || sess.TaskID != string(id))) {
		return false, false, nil
	} else if err != nil {
		return false, false, fmt.Errorf("read session: %w", err)
	}
	if needPause {
		m.mu.Lock()
		c := m.convos[key]
		notPaused := c != nil && (c.parked != id || c.released)
		m.mu.Unlock()
		if notPaused { // the runner streams it, or waits on something else
			return false, false, nil
		}
	}
	backend, err := m.cfg.Bindings.Backend(sess.Binding)
	if err != nil {
		return false, false, fmt.Errorf("session Binding %q: %w", sess.Binding, err)
	}
	var cerr error
	for i := range cancelAttempts {
		if _, cerr = backend.Agent.CancelTask(ctx, id); classifyCancel(cerr) != cancelTransient {
			break
		}
		if i+1 < cancelAttempts {
			select {
			case <-m.cfg.After(m.backoff(i + 1)):
			case <-ctx.Done():
				return false, false, ctx.Err()
			}
		}
	}
	actor := audit.Actor{User: by, Binding: sess.Binding, Identity: backend.Identity}
	call := audit.Call{Op: audit.OpCancel, TaskID: id, ContextID: sess.ContextID, Err: cerr}
	kind := classifyCancel(cerr)
	if kind == cancelCredentials {
		m.reject(ctx, key, backend, actor, call) // the identity Source writes the row
	} else {
		m.auditCall(ctx, key, actor, call)
	}
	switch kind {
	case cancelEnded:
		if task, gerr := backend.Agent.GetTask(ctx, id); gerr != nil || !task.Status.State.Terminal() {
			return false, false, fmt.Errorf("cancel task %s: %w", id, cerr)
		}
	case cancelGone:
		if err := m.cfg.Store.ResetSession(ctx, key); err != nil {
			return false, false, fmt.Errorf("reset session: %w", err)
		}
		m.goneNotice(ctx, key, id, "")
		_ = m.endPause(ctx, key, id) // a parked runner is released; the record is already cleared
		return true, false, nil
	case cancelDone:
	default:
		return false, false, fmt.Errorf("cancel task %s: %w", id, cerr)
	}
	switch err := m.endPause(ctx, key, id); {
	case err == nil:
		return true, true, nil
	case errors.Is(err, ErrNotParked): // it streams: its stream ends cancelled
		return true, false, nil
	default:
		return true, false, err
	}
}

// Answer sends msg, an answer to the paused task msg.TaskID (built by the dialect, validated,
// with the Slack user as attribution), through the conversation's runner, which streams what
// follows to the sink under the turn that paused. It returns once the runner has it: ErrNotPaused
// means the conversation is not paused on that task, or it was already answered. How the send
// ends, a rejection or a busy task included, reaches the thread as a notice, and the queued
// replies go on when the task ends.
func (m *Manager) Answer(ctx context.Context, key Key, msg *a2a.Message) error {
	if msg == nil || msg.TaskID == "" || msg.ID == "" {
		return fmt.Errorf("answer: a message with an ID and a task is required: %w", store.ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.convos[key]
	if c == nil || c.parked != msg.TaskID || c.released {
		return ErrNotPaused
	}
	c.released = true
	parent, state := telemetry.Capture(ctx)
	c.answers <- answerWork{msg: msg, parent: parent, state: state} // one per park, and the channel holds one: never blocks
	return nil
}

// SetTextReplyHandler installs the interaction callback before chat intake starts.
func (m *Manager) SetTextReplyHandler(h func(context.Context, Key, store.Reply, a2a.Task) (bool, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.TextReply = h
	for _, c := range m.convos {
		select {
		case c.queued <- struct{}{}:
		default:
		}
	}
}

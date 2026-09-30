package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// ErrNotPaused means the conversation has no task to end or answer: none is recorded, or the
// conversation is not paused on the task named.
var ErrNotPaused = errors.New("session: the conversation is not paused on that task")

// Stop is the thread's stop control (spec 8.2): it cancels the task that runs or waits for
// input, and the queued replies then go on. It reports false, without calling the agent, when
// the thread has no active task. The caller has already checked that by may press the button
// (D12). A running task's stream ends cancelled, which the sink renders; a paused task has no
// stream, so Stop ends the pause and tells the thread. It works after a restart, on the task
// the session recorded. Call it for a pause only through the interaction's owner: WP-33
// resolves the interaction itself.
func (m *Manager) Stop(ctx context.Context, key Key, by string) (stopped bool, err error) {
	id, paused, err := m.cancelActive(ctx, key, by)
	if err != nil || id == "" {
		return false, err
	}
	if paused && m.cfg.Sink != nil {
		m.cfg.Sink.Notice(ctx, key, Notice{ID: "task:" + string(id) + "/stopped", Kind: NoticeStopped})
	}
	return true, nil
}

// Cancel cancels the task a conversation is paused on and ends the pause, for the interaction
// timeout and an unusable pause (spec 8.4): the queued replies go on. It is audited, naming no
// person, and tells the thread nothing, which the caller does. It works after a restart.
// ErrNotPaused means no task is recorded. reason is for the log.
func (m *Manager) Cancel(ctx context.Context, key Key, reason string) error {
	id, _, err := m.cancelActive(ctx, key, "")
	if err == nil && id == "" {
		return ErrNotPaused
	}
	if err == nil {
		m.cfg.Logger.Info("cancelled a paused task", "key", key, "task", id, "reason", reason)
	}
	return err
}

// cancelActive cancels the session's recorded task, audits the call, and ends the pause if the
// runner is parked on it. It returns the task, or "" when none is recorded, and whether a pause
// was ended. A call that finds the runner streaming, or busy, leaves the rest to the stream,
// which ends cancelled.
func (m *Manager) cancelActive(ctx context.Context, key Key, by string) (a2a.TaskID, bool, error) {
	sess, err := m.cfg.Store.GetSession(ctx, key, readAll, m.cfg.Now())
	if errors.Is(err, store.ErrNotFound) || (err == nil && sess.TaskID == "") {
		return "", false, nil
	} else if err != nil {
		return "", false, fmt.Errorf("read session: %w", err)
	}
	backend, err := m.cfg.Bindings.Backend(sess.Binding)
	if err != nil {
		return "", false, fmt.Errorf("session Binding %q: %w", sess.Binding, err)
	}
	id := a2a.TaskID(sess.TaskID)
	_, cerr := backend.Agent.CancelTask(ctx, id)
	if m.cfg.Audit != nil {
		call := audit.Call{Op: audit.OpCancel, TaskID: id, ContextID: sess.ContextID, Err: cerr}
		if aerr := m.cfg.Audit.AgentCall(context.WithoutCancel(ctx), audit.Actor{User: by, Binding: sess.Binding, Identity: backend.Identity}, call); aerr != nil {
			m.cfg.Logger.Error("audit write failed", "key", key, "error", aerr)
		}
	}
	if cerr != nil {
		return "", false, fmt.Errorf("cancel task %s: %w", id, cerr)
	}
	switch err := m.endPause(ctx, key, id); {
	case err == nil:
		return id, true, nil
	case errors.Is(err, ErrNotParked): // it streams: its stream ends cancelled
		return id, false, nil
	default:
		return id, false, err
	}
}

// Answer sends msg, an answer to the paused task msg.TaskID (built by the dialect, validated,
// with the Slack user as attribution), through the conversation's runner, which streams what
// follows to the sink under the turn that paused. It returns once the runner has it: ErrNotPaused
// means the conversation is not paused on that task, or it was already answered. How the send
// ends, a rejection or a busy task included, reaches the thread as a notice, and the queued
// replies go on when the task ends.
func (m *Manager) Answer(_ context.Context, key Key, msg *a2a.Message) error {
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
	c.answers <- msg // one per park, and the channel holds one: never blocks
	return nil
}

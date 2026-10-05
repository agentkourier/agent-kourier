package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// Recover reconciles the conversations a restart found in the store (spec 8.2, 8.3): every
// session with an active task, and every one with a queued reply. It returns at once, after two
// lists; each conversation is taken up by its own goroutine, which a backend that is down does
// not stop (it is asked again, with a backoff). Call it at startup, before anything wakes the manager;
// a wake that gets to a conversation first has the recovered task attached to its runner, which
// follows it before it sends anything. A second call, once the task is taken up, starts nothing new.
//
// A conversation with a recorded task is followed from where the task stands: the agent says
// whether it ended (its output is rendered, marked recovered), waits for input (the pause is shown
// again and the runner parks), runs (it is followed), has run too long without word (R17: it is
// cancelled), or is gone (the session is reset). Sessions past their threadTTL, and ones whose
// Binding left the config, are left exactly as they are. A queued turn of a conversation with a
// context may have been sent before the crash, so it is looked up before it is sent.
func (m *Manager) Recover(ctx context.Context) error {
	active, err := m.cfg.Store.ListActiveSessions(ctx)
	if err != nil {
		return fmt.Errorf("list active sessions: %w", err)
	}
	queued, err := m.cfg.Store.ListQueuedSessions(ctx)
	if err != nil {
		return fmt.Errorf("list queued sessions: %w", err)
	}
	seen := map[Key]bool{}
	var keys []Key
	for _, s := range active {
		keys, seen[s.Key] = append(keys, s.Key), true
	}
	for _, k := range queued {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("session: the manager is closed")
	}
	for _, k := range keys {
		m.wg.Add(1)
		go m.recoverKey(k) //nolint:contextcheck // the goroutine belongs to the manager, and outlives the caller
	}
	return nil
}

// recoverKey takes up one conversation.
func (m *Manager) recoverKey(key Key) {
	defer m.wg.Done()
	log := m.cfg.Logger.With("key", key)
	sess, err := m.cfg.Store.GetSession(m.ctx, key, readAll, m.cfg.Now())
	if err != nil {
		log.Error("recover: read session", "error", err)
		return
	}
	backend, err := m.cfg.Bindings.Backend(sess.Binding)
	switch {
	case errors.Is(err, ErrUnknownBinding):
		log.Warn("recover: skipping a session whose Binding is not configured", "skip", "unknown_binding", "binding", sess.Binding, "error", err)
		return
	case err != nil: // a dialect it cannot build, a snapshot not loaded yet: not the same as a Binding that left
		log.Error("recover: skipping a session whose Binding has no usable backend", "skip", "backend_error", "binding", sess.Binding, "error", err)
		return
	}
	if _, err := m.cfg.Store.GetSession(m.ctx, key, backend.ThreadTTL, m.cfg.Now()); errors.Is(err, store.ErrExpired) {
		log.Info("recover: skipping an expired session", "skip", "expired")
		return
	} else if err != nil {
		log.Error("recover: read session", "error", err)
		return
	}
	head, herr := m.cfg.Store.NextReply(m.ctx, key)
	if sess.TaskID == "" {
		m.start(key)
		return
	}
	// A task is recorded. The runner follows it first, before anything queued is sent (adopt). If the head
	// is that task's own turn (a crash between recording the task and marking the turn delivered),
	// the task's history says so: the turn is then the task's, marked delivered by following it, and
	// never sent again.
	turnRow := store.Reply{ID: recoveredPrefix + sess.TaskID, Key: key}
	headIsTask := false
	if herr == nil && sess.ContextID != "" {
		for attempt := 1; ; attempt++ { // asked again until it answers: a fallback would render t's output twice
			task, _, err := backend.Agent.FindTask(m.ctx, sess.ContextID, head.ID)
			if err == nil {
				if task != nil && string(task.ID) == sess.TaskID {
					turnRow, headIsTask = head, true
					// The crash may have come before this turn's reaction was taken off, and nothing else
					// would now do it: the runner never takes the row up again, it follows the task.
					m.mu.Lock()
					m.marked[head.ID] = struct{}{}
					m.mu.Unlock()
				}
				break
			}
			select {
			case <-m.cfg.After(m.backoff(attempt)):
			case <-m.ctx.Done():
				return
			}
		}
	}
	t := &tstate{key: key, r: turnRow, sess: sess, b: backend,
		follow: true, taskID: a2a.TaskID(sess.TaskID), recovered: true}
	if !headIsTask && sess.TurnTask == sess.TaskID { // no turn of the queue is the task's: the session says whose output it is, if it does for this task
		t.began = sess.Turn // (a turn recorded for another task is a binary's that did not know of turns: it starts tasks and leaves it)
	}
	telemetry.Count(m.ctx, "courier_session_recovery_total", "adopted")
	m.adopt(key, t)
}

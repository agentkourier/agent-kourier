package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// convo is a conversation that has a runner, which drains its queue one turn at a time (D21).
type convo struct{ again bool } // again: a wake arrived while the runner was busy

// outcome is how one turn left the conversation: nothing queued, the task ended, or the task
// waits for input (spec 8.4).
type outcome int

const (
	idle outcome = iota
	ended
	paused
)

// start is the wake hook: it makes sure a runner is draining key's queue.
func (m *Manager) start(key Key) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if c, ok := m.convos[key]; ok {
		c.again = true
		return
	}
	m.convos[key] = &convo{}
	m.wg.Add(1)
	go m.run(key)
}

// retire ends key's runner unless a wake arrived since it last looked at the queue.
func (m *Manager) retire(key Key) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.convos[key]; c.again {
		c.again = false
		return false
	}
	delete(m.convos, key)
	return true
}

// run drains key's queue. A turn that fails stays queued, for the next wake to retry.
func (m *Manager) run(key Key) {
	defer m.wg.Done()
	for m.ctx.Err() == nil {
		out, err := m.turn(key)
		if err != nil && m.ctx.Err() == nil {
			m.cfg.Logger.Error("session turn failed", "key", key, "error", err)
		}
		switch {
		case out == paused:
			<-m.ctx.Done() // held until Close; Answer and Cancel add the ways out
		case (out == idle || err != nil) && m.retire(key):
			return
		}
	}
}

// turn sends the oldest queued turn of key and follows it until its task ends or pauses.
// Store writes ignore a shutdown, so an accepted turn is never left unrecorded.
func (m *Manager) turn(key Key) (outcome, error) {
	sctx := context.WithoutCancel(m.ctx)
	r, err := m.cfg.Store.NextReply(sctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return idle, nil
	} else if err != nil {
		return idle, fmt.Errorf("read queue: %w", err)
	}
	sess, err := m.cfg.Store.GetSession(sctx, key, readAll, m.cfg.Now())
	if err != nil {
		return idle, fmt.Errorf("read session: %w", err)
	}
	backend, err := m.cfg.Bindings.Backend(sess.Binding)
	if err != nil {
		return idle, fmt.Errorf("session Binding %q: %w", sess.Binding, err)
	}
	h := func(agent.Event) {}
	if m.cfg.Sink != nil {
		if sh := m.cfg.Sink.Turn(m.ctx, TurnRef{key, r.ID}); sh != nil {
			h = sh
		}
	}
	var final *a2a.Task
	named := false
	err = backend.Agent.Send(m.ctx, message(r, sess), func(e agent.Event) {
		switch ev := e.(type) {
		case agent.StateEvent:
			if !named { // the agent has named the task: record it before anyone renders it
				named = true
				if err := m.accept(sctx, r, ev); err != nil {
					m.cfg.Logger.Error("record accepted turn", "message", r.ID, "error", err)
				}
			}
		case agent.FinalEvent:
			final = ev.Task
		}
		h(e)
	})
	switch {
	case err != nil:
		return ended, fmt.Errorf("send turn %s: %w", r.ID, err)
	case final == nil:
		return ended, fmt.Errorf("send turn %s: the stream ended with no final task", r.ID)
	case !final.Status.State.Terminal():
		return paused, nil
	}
	return ended, m.end(sctx, key, backend.ThreadTTL)
}

// message builds the A2A message of a turn: exactly one text part (R18), the queue's messageId
// and the session's contextId, which is empty on a first turn.
func message(r store.Reply, sess store.Session) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(r.Text))
	msg.ID, msg.ContextID = r.ID, sess.ContextID
	return msg
}

// accept records that the agent took the turn (spec 8.2): the session gets its contextId and
// task, then the turn leaves the queue. In that order, so a crash in between leaves a task
// recovery can find, never one nobody knows about. It does not rewrite the session row, so it
// cannot undo a Submit's touch.
func (m *Manager) accept(ctx context.Context, r store.Reply, ev agent.StateEvent) error {
	if err := m.cfg.Store.SetSessionTask(ctx, r.Key, ev.ContextID, string(ev.TaskID)); err != nil {
		return err
	}
	return m.cfg.Store.MarkReplyDelivered(ctx, r.ID, m.cfg.Now())
}

// end clears the task of a conversation whose task ended, and touches the session, but only
// after reading it: an expired session is never touched back to life (D28).
func (m *Manager) end(ctx context.Context, key Key, ttl time.Duration) error {
	if err := m.cfg.Store.SetSessionTask(ctx, key, "", ""); err != nil {
		return err
	}
	switch _, err := m.cfg.Store.GetSession(ctx, key, ttl, m.cfg.Now()); {
	case errors.Is(err, store.ErrExpired):
		return nil
	case err != nil:
		return fmt.Errorf("read session: %w", err)
	}
	return m.cfg.Store.TouchSession(ctx, key, m.cfg.Now())
}

// Close stops the runners and waits for them, until ctx ends. It does not cancel any task at
// the agent: a turn the agent accepted stays recorded, for restart reconciliation.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.stop()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("close session manager: %w", ctx.Err())
	}
}

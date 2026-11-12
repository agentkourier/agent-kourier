package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// ErrNotParked is what endPause returns for a call it dropped: the conversation is not paused on
// that task, or the pause was already ended.
var ErrNotParked = errors.New("session: no pause of that task to end")

// endPause tells the runner of key that the pause of taskID ended (answered and finished, timed
// out, or cancelled): it clears the task and sends what was held. A call that arrives while
// the runner is not parked on taskID, or repeats one already made, is dropped with ErrNotParked:
// it must never release the next pause. (A pause ended from inside the renderer's handler, in
// a stream that then fails, is wiped with the park: if the task followed turns out still to wait
// for input, the runner parks again and waits for the pause's own timeout. Rare, and bounded.)
// With no runner (after a restart), a call for the task
// recorded in the session clears it and starts one, without checking that the task was
// paused, so callers call it only for a pause.
func (m *Manager) endPause(ctx context.Context, key Key, taskID a2a.TaskID) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrNotParked // the runners are gone, or going
	}
	if c := m.convos[key]; c != nil {
		switch {
		case taskID == "" || c.released || c.driving == taskID: // a duplicate, or the task streams
			m.mu.Unlock()
			return ErrNotParked
		case c.parked == taskID:
			c.released = true
			c.resume <- struct{}{} // one release per park, and the channel holds one: never blocks
			m.mu.Unlock()
			return nil
		}
	}
	m.mu.Unlock()
	// No runner, or one that does not drive this task (a turn held behind a task recorded before a
	// restart): clear the record if it is still the task's, and wake the queue.
	sess, err := m.cfg.Store.GetSession(ctx, key, store.AnyAge, m.cfg.Now())
	if err != nil {
		return err
	}
	if sess.TaskID != string(taskID) || taskID == "" {
		return ErrNotParked // nothing recorded for that task
	}
	if err := m.cfg.Store.SetSessionTask(ctx, key, "", "", store.TaskTurn{}); err != nil {
		return fmt.Errorf("end pause of task %s: %w", taskID, err)
	}
	m.start(key) //nolint:contextcheck // the runner belongs to the manager, and outlives the caller
	return nil
}

// parked waits while the conversation's task waits for input, and returns what happened: the
// pause ended (cancelled, timed out: see endPause), so the task is cleared and the held replies
// go out; or an answer was sent, whose stream is followed to where it ends or pauses again.
func (m *Manager) parked(key Key, c *convo) (outcome, error) {
	telemetry.AddGauge(m.ctx, "agentkourier_sessions_paused", 1)
	defer telemetry.AddGauge(m.ctx, "agentkourier_sessions_paused", -1)
	select {
	case <-c.resume:
		return m.releasePause(key, c)
	default:
	}
	if c.pendingAnswer != nil {
		return m.answer(c, *c.pendingAnswer)
	}
	select {
	case msg := <-c.answers:
		return m.answer(c, msg)
	default:
	}
	// Persist consumption before sending a decided answer, including after a write retry.
	if err := m.flushTextReply(c); err != nil {
		return m.waitTextReply(key, c, err)
	}
	select {
	case msg := <-c.answers:
		return m.answer(c, msg)
	default:
		m.discardAnswer(c)
	}
	if c.paused != nil && TakesTextReplies(c.paused.b.Dialect) {
		r, err := m.cfg.Store.NextReplyAfter(m.ctx, key, c.textAfter)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return m.waitTextReply(key, c, err)
		}
		if err == nil {
			task, err := c.paused.b.Agent.GetTask(m.ctx, c.paused.taskID)
			if errors.Is(err, a2a.ErrTaskNotFound) {
				return m.releasePause(key, c)
			}
			if dialect.AnswerTooLarge(err) && m.ctx.Err() == nil {
				return m.pauseTooLarge(c, err) // the same answer comes again: not a read to try after every backoff
			}
			if err != nil {
				if errors.Is(err, agent.ErrCredentialsRejected) && !c.textRejected {
					c.textRejected = true
					m.rejected(c.paused, err)
				}
				return m.waitTextReply(key, c, err)
			}
			if task.Status.State.Terminal() {
				return m.releasePause(key, c)
			}
			if task.Status.State != a2a.TaskStateInputRequired {
				if !running(task) {
					return m.waitTextReply(key, c, nil)
				}
				if c.since.IsZero() {
					c.since = m.cfg.Now()
				}
				c.follow = c.paused
				m.unpark(c)
				return m.resume(c)
			}
			m.mu.Lock()
			c.pendingReply = r
			m.mu.Unlock()
			verdict, err := m.cfg.TextReply(telemetry.Restore(m.ctx, r.TraceParent, r.TraceState), key, r, *task)
			if err != nil {
				m.clearPendingReply(c)
				return m.waitTextReply(key, c, err)
			}
			switch verdict {
			case TextReplyNotAnswer:
				m.clearPendingReply(c)
				c.textAfter = r.Sequence
				return paused, nil
			case TextReplyWait:
				m.clearPendingReply(c)
				return m.waitTextReply(key, c, nil)
			case TextReplyConsumed:
			default:
				return m.waitTextReply(key, c, fmt.Errorf("text reply %s: invalid outcome %d without a retry cause", r.ID, verdict))
			}
			select {
			case msg := <-c.answers:
				return m.answer(c, msg)
			default:
				if err := m.flushTextReply(c); err != nil {
					return m.waitTextReply(key, c, err)
				}
				// Drain rejected replies even when their enqueue wakes coalesced.
				m.discardAnswer(c)
				return paused, nil
			}
		}
	}
	select {
	case <-c.queued:
		return paused, nil
	case <-c.resume:
		return m.releasePause(key, c)
	case msg := <-c.answers:
		return m.answer(c, msg)
	case <-m.ctx.Done():
		return ended, nil
	}
}

// pauseTooLarge ends the pause of a task that cannot be read for its size, which the read for a person's text reply found: the
// turn that paused ends as any whose task is too large does (lost): the thread is told, the task is cancelled and, once it is, ended.
// The pause is released without the turn being counted again as one whose pause ended, and the reply that was read stays queued: turn takes
// it up again, an ordinary reply to go out as a turn of its own, and one admitted as an answer to the question of the pause (AdmitAnswer) to
// be reconciled with a question that is no longer open, which is the handler's to say, as for any pause that ended.
func (m *Manager) pauseTooLarge(c *convo, err error) (outcome, error) {
	over := c.paused
	out, lerr := m.lost(over, err)
	m.mu.Lock()
	c.parked, c.released, c.paused = "", false, nil
	select {
	case <-c.resume:
	default:
	}
	select {
	case <-c.answers:
	default:
	}
	m.mu.Unlock()
	return out, lerr
}

// releasePause clears a pause once, without requeueing its end signal.
func (m *Manager) releasePause(key Key, c *convo) (outcome, error) {
	m.mu.Lock()
	c.released = true
	m.mu.Unlock()
	// The park, and its release, stay until the record is cleared: a duplicate endPause in between
	// must still be refused, not find a runner that is neither parked nor driving.
	if err := m.end(context.WithoutCancel(m.ctx), key, c.ttl); err != nil {
		m.cfg.Logger.Error("end paused task", "key", key, "error", err)
	}
	m.mu.Lock()
	over := c.paused // the turn that paused
	c.parked, c.released, c.paused = "", false, nil
	select {
	case <-c.resume:
	default:
	}
	select {
	case <-c.answers:
	default:
	}
	m.mu.Unlock()
	if over != nil {
		m.discardAnswer(c)
		m.finish(over, TurnPauseEnded)
	}
	return ended, nil
}

// flushTextReply retries only the queue write; the decision and answer are not repeated.
func (m *Manager) flushTextReply(c *convo) error {
	m.mu.Lock()
	r := c.pendingReply
	m.mu.Unlock()
	if r.ID == "" {
		return nil
	}
	if err := m.cfg.Store.MarkReplyDelivered(context.WithoutCancel(m.ctx), r.ID, m.cfg.Now()); err != nil {
		return err
	}
	m.mu.Lock()
	if _, fresh := m.fresh[r.ID]; !fresh {
		m.marked[r.ID] = struct{}{}
	}
	delete(m.fresh, r.ID)
	c.pendingReply = store.Reply{}
	m.mu.Unlock()
	m.unqueue(&tstate{key: r.Key, r: r})
	c.answerReply = r
	c.textTries, c.tries = 0, 0
	return nil
}

func (m *Manager) clearPendingReply(c *convo) {
	m.mu.Lock()
	c.pendingReply = store.Reply{}
	m.mu.Unlock()
}

// waitTextReply bounds retries and handles cancellation without sending an unrecorded answer.
func (m *Manager) waitTextReply(key Key, c *convo, err error) (outcome, error) {
	c.textTries++
	answers := c.answers
	m.mu.Lock()
	pending := c.pendingReply.ID != "" || c.pendingAnswer != nil
	m.mu.Unlock()
	if pending {
		// A decided text answer must wait until its queue delivery write succeeds.
		answers = nil
	}
	select {
	case work := <-answers:
		return m.answer(c, work)
	case <-c.resume:
		return m.releasePause(key, c)
	case <-m.cfg.After(m.backoff(c.textTries)):
		return paused, err
	case <-m.ctx.Done():
		return ended, m.ctx.Err()
	}
}

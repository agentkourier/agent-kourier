package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// answer sends an answer to the paused task and streams what follows to the sink, under the
// turn that paused (the renderer shows one message). The pause is over while it runs, so a
// second answer, or an endPause for the task, is dropped.
type answerWork struct {
	msg           *a2a.Message
	parent, state string
	reply         store.Reply
}

func (m *Manager) answer(c *convo, work answerWork) (outcome, error) {
	if c.pendingAnswer == nil {
		c.pendingAnswer = &work
	} else {
		work = *c.pendingAnswer
	}
	msg := work.msg
	t := *c.paused
	t.answer, t.nid, t.taskID, t.follow, t.takes = true, msg.ID, msg.TaskID, false, false // the answer's output is a message of its own
	if work.reply.ID == "" {
		m.mu.Lock()
		work.reply = c.pendingReply
		m.mu.Unlock()
	}
	t.ackReply = work.reply
	t.r.TraceParent, t.r.TraceState = work.parent, work.state
	t.texted = false // the answer's output is a message of its own: what the question said is not part of it
	if msg.ContextID == "" {
		msg.ContextID = t.sess.ContextID
	}
	if work.reply.ID != "" {
		// Record the answer's output and its acknowledgment source before settling the incoming reply. If
		// the process stops after either write, recovery still owns the original source and can remove it.
		if err := m.cfg.Store.SetSessionTask(context.WithoutCancel(m.ctx), t.key, msg.ContextID, string(msg.TaskID), t.outputTurn()); err != nil {
			return m.waitTextReply(t.key, c, fmt.Errorf("record the answer's turn: %w", err))
		}
		if err := m.cfg.Store.MarkReplyDelivered(context.WithoutCancel(m.ctx), work.reply.ID, m.cfg.Now()); err != nil {
			return m.waitTextReply(t.key, c, err)
		}
		m.mu.Lock()
		if _, fresh := m.fresh[work.reply.ID]; !fresh {
			m.marked[work.reply.ID] = struct{}{}
		}
		delete(m.fresh, work.reply.ID)
		c.pendingReply = store.Reply{}
		c.answerReply = store.Reply{}
		m.mu.Unlock()
		m.unqueue(&tstate{key: work.reply.Key, r: work.reply})
	}
	m.mu.Lock()
	c.pendingAnswer = nil
	c.parked, c.released, c.paused, c.driving = "", false, nil, msg.TaskID // no gap between parked and driving
	m.mu.Unlock()
	return m.drive(&t, func(ctx context.Context, h agent.Handler) error {
		// The answer's turn is recorded before the agent is asked, and not when it first names the task: a process that ends
		// between the two would otherwise leave the session naming the turn that paused, and the recovery would stop that
		// turn's message and replace it with the continuation. An answer that is then refused leaves the task paused under
		// the answer's turn, whose worst case is a new message.
		if err := m.cfg.Store.SetSessionTask(context.WithoutCancel(ctx), t.key, msg.ContextID, string(msg.TaskID), t.outputTurn()); err != nil {
			return fmt.Errorf("record the answer's turn: %w", err)
		}
		return t.b.Agent.Answer(ctx, msg, h)
	})
}

// unanswered handles an answer that failed before the agent named a task again. A rejected one
// wedged its task, which Agent.Answer has already cancelled (R17). A busy one, or one refused as not ready, is read
// (see readAnswered). One whose reply was too large ends the turn (tooLarge). Any other failure leaves the task paused,
// so the person can answer again.
func (m *Manager) unanswered(t *tstate, err error) (outcome, error) {
	switch {
	case errors.Is(err, agent.ErrAnswerRejected):
		m.notice(t, NoticeAnswerRejected, "")
		m.audit(t, audit.Call{Op: audit.OpAnswer, TaskID: t.taskID, ContextID: t.sess.ContextID, MessageID: t.nid, Err: err})
		if errors.Is(err, agent.ErrCancelFailed) {
			// The wedge persists, and would refuse every send for ever: keep the task recorded and
			// cancel it again, classified and bounded, as any wedged task is.
			t.c.follow, t.follow, t.c.cancelNow = t, true, true
			return m.wedged(t)
		}
		// Agent.Answer cancelled the wedged task itself: record that call, made by no person.
		m.auditCall(m.ctx, t.key, audit.Actor{Binding: t.sess.Binding, Identity: t.b.Identity},
			audit.Call{Op: audit.OpCancel, TaskID: t.taskID, ContextID: t.sess.ContextID, MessageID: t.nid})
		m.finish(t, TurnAnswerRejected)
		return ended, m.end(context.WithoutCancel(m.ctx), t.key, t.c.ttl)
	case dialect.AnswerTooLarge(err) && m.ctx.Err() == nil:
		// Before ErrOutcomeUnknown, which it comes with when no task was named: the agent would send the same reply to the
		// same answer again, whatever the task is doing now. So the task is not read, to be parked again for a person to answer
		// into the same reply or followed into it: the turn ends, with a notice, and the task is cancelled, as when the
		// reply is too large after the task is named. Whether the agent took the answer is not known, and a reply that was not read
		// cannot say: a Message that is too large reads the same as anything else that is, though a small one (ErrMessageReply) is
		// known not to continue the task. So the cancel is not a claim that the answer was refused: the thread is told that the
		// agent may have acted on it (tooLargeAnswerDetail), and a task left open could be answered only into the same reply.
		return m.tooLarge(t, err)
	case errors.Is(err, agent.ErrBusy):
		return m.readAnswered(t, err, "")
	case errors.Is(err, agent.ErrMessageReply): // the agent replied, and not with the task: one that still waits did not take it
		return m.readAnswered(t, nil, answerMessageDetail)
	case errors.Is(err, agent.ErrOutcomeUnknown):
		return m.readAnswered(t, nil, "")
	case errors.Is(err, agent.ErrCredentialsRejected):
		m.rejected(t, err)
	}
	return m.repark(t, "the answer could not be delivered"), nil
}

// Details of the notice an answer that the agent refused gets, by what it said (see readAnswered). Agent Kourier does
// not send the answer again: the thread is told, and the person answers again if the pause is open.
const (
	answerBusyDetail     = "the agent is busy with the task, and did not take the answer"
	answerNotReadyDetail = "the agent's session was not ready, and did not take the answer"
	answerMessageDetail  = "the agent replied with a message that did not continue the task, so the answer was not taken"
)

// readAnswered reads the task an answer may or may not have reached: refused is the -32004 it got (busy,
// wedged, or ended; ErrNotReady besides ErrBusy for a session that is not ready), or nil when its outcome is
// unknown (it may have been delivered). An ended task drops the answer; a WORKING one past maxWorkingAge that
// refused it is cancelled (R17); any other WORKING one is followed, so its end is seen and the queue goes on (the
// thread is told when it refused the answer, and why); one still waiting for input reopens the pause, and tells
// the thread the same. waiting is what the thread is told when the outcome was unknown and the task still waits,
// empty for nothing: the agent may never have had the answer, but one that replied with a Message did.
func (m *Manager) readAnswered(t *tstate, refused error, waiting string) (outcome, error) {
	busy := refused != nil
	detail := answerBusyDetail
	if errors.Is(refused, agent.ErrNotReady) {
		detail = answerNotReadyDetail
	}
	task, err := t.b.Agent.GetTask(m.ctx, t.taskID)
	switch {
	case dialect.AnswerTooLarge(err) && m.ctx.Err() == nil:
		return m.lost(t, err) // the same answer comes again: the turn ends as any whose task is too large does
	case err != nil && !busy:
		// The outcome of the answer was unknown, and the task cannot be read to tell: the agent may have taken it.
		return m.repark(t, answerUnknownDetail), nil
	case err != nil:
		return m.repark(t, "the answer could not be delivered"), nil
	case task.Status.State.Terminal():
		m.notice(t, NoticeAnswerDropped, "")
		m.finish(t, stateOutcome(task.Status.State))
		return ended, m.end(context.WithoutCancel(m.ctx), t.key, t.c.ttl)
	case running(task):
		if busy && m.wedgedByAge(t.c, task) {
			return m.wedged(t)
		}
		if busy {
			m.notice(t, NoticeFailed, detail)
		}
		t.c.follow, t.follow = t, true
		return m.resume(t.c)
	}
	if !busy {
		detail = waiting // an outcome that was unknown, and the task still waits
	}
	return m.repark(t, detail), nil
}

// repark reopens the pause before notifying the answer watcher, so its cancel can be accepted.
// It also settles the failed answer's acknowledgment and restores the paused task's metadata.
func (m *Manager) repark(t *tstate, detail string) outcome {
	if t.answer && t.ackReply.Acknowledgment != "" {
		m.unacknowledgeReply(t.key, t.ackReply, t.ackReply.ID)
		t.ackReply = store.Reply{}
	}

	m.mu.Lock()
	// The runner stops driving the task as it parks on it, under the one lock, as a pause found in a stream does (no gap between
	// parked and driving): drive's deferred m.drives(c, "") comes only after the watcher is told, and a Stop that lands in between
	// finds the task driven, is refused by endPause as for a task that streams, and leaves the runner parked on a task that was cancelled.
	t.c.parked, t.c.released, t.c.driving = t.taskID, false, ""
	pt := *t
	pt.answer, pt.nid = false, ""
	t.c.paused = &pt
	m.mu.Unlock()
	if t.answer {
		if err := m.cfg.Store.SetSessionTask(context.WithoutCancel(m.ctx), t.key, t.sess.ContextID, string(t.taskID), pt.outputTurn()); err != nil {
			m.cfg.Logger.WarnContext(m.ctx, "session: could not restore paused acknowledgment metadata", "message", t.r.ID, "error", err)
		}
	}
	told := m.cfg.AnswerWatcher != nil && t.nid != "" && m.cfg.AnswerWatcher.AnswerReopened(m.ctx, t.key, t.taskID, t.nid)
	if detail != "" && !told {
		m.notice(t, NoticeFailed, detail)
	}

	return paused
}

// answerUnknownDetail is what the thread is told when the outcome of an answer was unknown and the task cannot be read to tell.
const answerUnknownDetail = "Agent Kourier could not tell whether the agent took the answer"

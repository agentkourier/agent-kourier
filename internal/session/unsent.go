package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// lookupIncompleteDetail is what the thread is told when the turn was not sent, because the agent's list of tasks could not be read in
// full, whatever the reason (a page past the bound at every size, the walk's deadline or page limit, a server that did not answer at the
// size it was lowered to), and so Agent Kourier cannot tell whether the agent already has the turn. It says what happened, that the agent
// may be running it, and what a person does. It must be shown whole: a notice's detail is cut at render.MaxNoticeDetail units, which a test
// holds every fixed detail to. It names no backend, no size and nothing the agent sent.
const lookupIncompleteDetail = "Agent Kourier could not tell if the agent already started this message, as its task list could not be read in full, so it did not send it. The agent may be running it. To run it, post it again."

// unverified ends a turn that is not sent, since the agent's list of tasks could not be read in full, and so it is not known whether the agent has the
// turn: the thread is told (lookupIncompleteDetail), the lookup is audited as failed with lookup_incomplete (and its cause, such as
// response_too_large), the turn is logged as failed with the error and counted as TurnLookupIncomplete, which an operator can alert on.
// Nothing is sent, and nothing is asked again.
func (m *Manager) unverified(t *tstate, cause error) (outcome, error) {
	m.audit(t, audit.Call{Op: audit.OpFindTask, ContextID: t.sess.ContextID, MessageID: t.r.ID, Err: cause})
	return m.fail(t, TurnLookupIncomplete, lookupIncompleteDetail, cause)
}

// unsent handles a send that failed before the agent named a task, by what the error says
// about how far the message got (see the agent package's errors).
func (m *Manager) unsent(t *tstate, err error) (outcome, error) {
	switch {
	case errors.Is(err, agent.ErrCredentialsRejected): // before ErrNotSent, which it comes with
		m.rejected(t, err)
		return m.fail(t, TurnCredentialsRejected, "the agent's front door refused the Binding's credentials", err)
	case errors.Is(err, agent.ErrForbidden):
		m.audit(t, t.call(err))
		return m.fail(t, TurnForbidden, "the agent refused the Binding's identity for this session", err)
	case errors.Is(err, agent.ErrAgentForbidden): // before ErrNotSent, which it comes with: the same credentials get the same 403
		m.audit(t, t.call(err))
		return m.fail(t, TurnForbidden, "the agent was not found, or does not permit the Binding's identity", err)
	case errors.Is(err, agent.ErrBusy): // some task of the session runs, or it is not ready: try again later, never give up
		if errors.Is(err, agent.ErrNotReady) {
			// A session that is starting or in a lifecycle operation holds no turn ahead of this one, so the
			// reply is not queued behind anything and gets no reaction, however the first mention of a thread
			// that starts one looks.
			m.waiting(t, NoticeNotReady)
		} else {
			if !t.c.busyNoted && !t.b.AcknowledgmentConfigured && t.r.Acknowledgment == "" { // the acknowledgment already marks the reply while it waits
				t.c.busyNoted = true
				m.mu.Lock()
				m.marked[t.r.ID] = struct{}{}
				m.mu.Unlock()
				m.notice(t, NoticeQueued, "")
			}
			m.waiting(t, NoticeStillBusy)
		}
		t.c.tries++
		if t.c.unreached < 2 { // the agent answered, so a silent retry pending is no longer a second failure in a row; once told, the run keeps its one notice
			t.c.unreached = 0
		}
		t.c.since = time.Time{} // the agent answered: its unreachable time starts over, and a turn that then fails to send is given up on as usual
		return blocked, fmt.Errorf("send turn %s: %w", t.r.ID, err)
	case dialect.AnswerTooLarge(err):
		// Before ErrOutcomeUnknown, which it comes with when no task was named: the agent would send the same reply to the
		// same message again, so a resend gets nowhere, and each one would make it run the message once more. No task is
		// named, so none can be cancelled.
		return m.fail(t, TurnResponseTooLarge, tooLargeDetail, err)
	case errors.Is(err, agent.ErrOutcomeUnknown): // the agent may have taken it: looked up before any resend
		m.mu.Lock()
		m.attempted[t.key] = attempt{id: t.r.ID, unknown: true}
		m.mu.Unlock()
		return m.retry(t, err)
	case errors.Is(err, agent.ErrNotSent): // nothing left, so nothing new to look up: safe to retry
		m.mu.Lock()
		if !m.attempted[t.key].unknown { // but an earlier unknown outcome still needs its lookup
			delete(m.attempted, t.key)
		}
		m.mu.Unlock()
		return m.retry(t, err)
	}
	// Any other failure before a task was named, a JSON-RPC error that is not busy such as -32603 among them, says nothing of whether the
	// agent took the message: it may have started the task and failed on its way back. The outcome is unknown, so that a lookup that cannot be
	// answered fails the turn and does not send it again.
	m.mu.Lock()
	m.attempted[t.key] = attempt{id: t.r.ID, unknown: true}
	m.mu.Unlock()
	return ended, fmt.Errorf("send turn %s: %w", t.r.ID, err)
}

// waitNoticeAfter is how long the head turn may get -32004, busy or not ready, before the thread is told it waits.
// A reply to a task Agent Kourier recorded is held without a send, and its reaction says so; this is for the turn that
// is sent again and again into a session that does not take it, such as a task that nobody recorded, or a
// controller that crash-loops, which would otherwise show nothing past the reaction. A couple of minutes is
// past a session that is merely starting, and well inside the time an operator takes to notice a stuck one.
const waitNoticeAfter = 2 * time.Minute

// waiting notes that the head turn got -32004 once more, and, once it has got it for waitNoticeAfter, tells the
// thread of the kind once for the run of trouble: the turn is still sent after every backoff, and never dropped
// (spec 8.2). The notice is named after the turn, so a repeat after a restart is one post.
func (m *Manager) waiting(t *tstate, kind NoticeKind) {
	c, now := t.c, m.cfg.Now()
	if c.waitSince.IsZero() {
		c.waitSince = now
	}
	if !c.waitNoted && now.Sub(c.waitSince) >= waitNoticeAfter {
		c.waitNoted = true
		m.notice(t, kind, "")
	}
}

// retry waits and tries the head turn again. It gives up, failing the turn, after SendGiveUp of
// failures, unless the turn is one never to give up on. The first failure is retried silently, for the
// first call of a new pod can fail where the next one works; when the second fails too, it tells the
// thread, once for the run of trouble (spec 11). The count is in memory, so a restart starts it over:
// at most one more silent retry, and a notice named after the turn, which the sink posts once.
func (m *Manager) retry(t *tstate, err error) (outcome, error) {
	c := t.c
	if c.since.IsZero() {
		c.since = m.cfg.Now()
	}
	if !c.patient && m.cfg.Now().Sub(c.since) >= m.cfg.SendGiveUp {
		return m.fail(t, TurnUnreachable, "the agent could not be reached", err)
	}
	c.tries++
	if c.unreached++; c.unreached == 2 {
		m.notice(t, NoticeRetrying, "")
	}
	return blocked, fmt.Errorf("send turn %s: %w", t.r.ID, err)
}

// duplicates reports a turn the agent ran more than once: the thread is told, and the audit
// log records every holder.
func (m *Manager) duplicates(t *tstate, holders []a2a.TaskID) {
	m.notice(t, NoticeDuplicateRun, "")
	m.audit(t, audit.Call{Op: audit.OpFindTask, ContextID: t.sess.ContextID, MessageID: t.r.ID, Holders: holders})
}

// fail ends the turn without a task: it is not sent again, the thread is told why, and the
// queue goes on. If marking the turn delivered fails, the turn stays queued and the next attempt
// is a real second call, with its own audit row: bounded, and never silent. class is what kind of
// failure it was, for the end line of the turn.
func (m *Manager) fail(t *tstate, class TurnOutcome, why string, cause error) (outcome, error) {
	m.drives(t.c, "") // it stops driving before it says so: a Stop that answers the notice must not find it driving
	t.c.settle()
	m.mu.Lock()
	delete(m.attempted, t.key) // it will not be sent again
	delete(m.fresh, t.r.ID)
	m.mu.Unlock()
	m.notice(t, NoticeFailed, why)
	m.cfg.Logger.Warn("session turn failed", "key", t.key, "binding", t.sess.Binding, "message", t.r.ID, "reason", why, "error", cause)
	if err := m.cfg.Store.MarkReplyDelivered(context.WithoutCancel(m.ctx), t.r.ID, m.cfg.Now()); err != nil {
		return ended, err
	}
	m.finish(t, class)
	return ended, nil
}

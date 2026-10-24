package session

import (
	"context"
	"slices"
	"strings"
	"unicode"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TurnOutcome is how a turn ended: the state its task reached, or the class of what stopped it. It is
// logged and counted, so the set is closed and holds no text from the agent or the error.
type TurnOutcome string

// The outcomes of a turn.
const (
	// TurnCompleted, TurnFailed, TurnRejected and TurnCanceled are the terminal states of the turn's task.
	TurnCompleted TurnOutcome = "completed"
	TurnFailed    TurnOutcome = "failed"
	TurnRejected  TurnOutcome = "rejected"
	TurnCanceled  TurnOutcome = "canceled"
	// TurnCredentialsRejected is a turn the agent's front door refused the Binding's credentials of.
	TurnCredentialsRejected TurnOutcome = "credentials_rejected"
	// TurnForbidden is a turn the agent refused the Binding's identity for.
	TurnForbidden TurnOutcome = "forbidden"
	// TurnUnreachable is a turn given up on after SendGiveUp of an agent that could not be reached.
	TurnUnreachable TurnOutcome = "unreachable"
	// TurnSessionGone is a turn whose task or session the agent no longer has.
	TurnSessionGone TurnOutcome = "session_gone"
	// TurnBindingGone is a turn dropped because its Binding left the config.
	TurnBindingGone TurnOutcome = "binding_gone"
	// TurnWedged is a turn whose task ran past maxWorkingAge and was cancelled (R17).
	TurnWedged TurnOutcome = "wedged"
	// TurnUncancellable is a turn whose wedged task could not be cancelled.
	TurnUncancellable TurnOutcome = "uncancellable"
	// TurnPauseEnded is a turn whose pause ended without an answer: the interaction was cancelled or timed out.
	TurnPauseEnded TurnOutcome = "pause_ended"
	// TurnAnswerRejected is a turn whose answer the agent rejected, which cancelled its task (R17).
	TurnAnswerRejected TurnOutcome = "answer_rejected"
	// TurnRefused is a turn that was not followed to its end because the agent answered the follow, or the
	// stream failed, with an error that is no lost stream, such as a refused identity or a permanent 4xx.
	TurnRefused TurnOutcome = "refused"
	// TurnNoFinal is a turn whose stream ended cleanly with no final task, which a real client never does.
	TurnNoFinal TurnOutcome = "no_final"
)

// TurnOutcomes returns every outcome a turn can have.
func TurnOutcomes() []TurnOutcome {
	return []TurnOutcome{TurnCompleted, TurnFailed, TurnRejected, TurnCanceled, TurnCredentialsRejected, TurnForbidden, TurnUnreachable,
		TurnSessionGone, TurnBindingGone, TurnWedged, TurnUncancellable, TurnPauseEnded, TurnAnswerRejected, TurnRefused, TurnNoFinal}
}

// Valid reports whether o is one of TurnOutcomes.
func (o TurnOutcome) Valid() bool { return slices.Contains(TurnOutcomes(), o) }

// stateOutcome is the outcome of a task that reached the terminal state s.
func stateOutcome(s a2a.TaskState) TurnOutcome {
	switch s {
	case a2a.TaskStateCompleted:
		return TurnCompleted
	case a2a.TaskStateRejected:
		return TurnRejected
	case a2a.TaskStateCanceled:
		return TurnCanceled
	}
	return TurnFailed // failed, and a terminal state this version does not know: the task did not complete
}

// unqueue asks the sink to take the queued reaction off the turn t's message, if the turn may carry one (see
// Manager.marked), once: when the agent has the turn, or, for a turn that never reaches the agent, when it ends.
// A turn that was never marked raises nothing.
func (m *Manager) unqueue(t *tstate) {
	m.mu.Lock()
	_, marked := m.marked[t.r.ID]
	delete(m.marked, t.r.ID)
	m.mu.Unlock()
	if !marked && (t.r.Acknowledgment != "" || t.ackReply.Acknowledgment != "") {
		// The acknowledgment reaction covers both waiting and processing. The legacy queued
		// marker must not compete with it or remove the same emoji early.
		return
	}
	if marked {
		m.cfg.Sink.Notice(m.ctx, t.key, Notice{ID: queuedID(t.r.ID), Kind: NoticeDequeued, MessageRef: t.r.MessageRef})
	}
}

// begin logs the start of the turn t, once however often it is sent or followed: its first send, or, for a
// turn a restart found again, the first time it is followed (marked recovered).
func (m *Manager) begin(t *tstate) {
	m.mu.Lock()
	_, begun := m.begun[t.r.ID]
	if !begun {
		m.begun[t.r.ID] = m.cfg.Now()
	}
	m.mu.Unlock()
	if !begun {
		m.cfg.Logger.InfoContext(m.turnContext(t), "session turn started", m.turnAttrs(t)...)
	}
}

// finish logs the end of the turn t and reports its outcome. extra is logged with it. Call it once, where
// the turn leaves the runner for good: a turn that stays queued, or recorded for a retry, has not ended.
func (m *Manager) finish(t *tstate, outcome TurnOutcome, extra ...any) {
	m.mu.Lock()
	start, begun := m.begun[t.r.ID]
	delete(m.begun, t.r.ID)
	m.mu.Unlock()
	m.unqueue(t) // however it ended, the reply waits no more: most turns have been unqueued, when they were sent
	m.unacknowledge(t)
	if t.c != nil && t.c.left != nil && t.c.left.r.ID == t.r.ID { // it ended before an attempt took over what it handed on
		m.endLeft(t.c)
	}
	attrs := append(m.turnAttrs(t), "outcome", string(outcome))
	if begun {
		attrs = append(attrs, "duration_ms", m.cfg.Now().Sub(start).Milliseconds())
	}
	m.cfg.OnTurnEnd(t.sess.Binding, outcome)
	m.cfg.Logger.InfoContext(m.turnContext(t), "session turn ended", append(attrs, extra...)...) // after the report: whoever sees the line finds it counted
}

// unacknowledge removes every durable acknowledgment source held by the turn. An answer keeps the
// paused question in t.r and its own accepted reply in t.ackReply, so both are settled when the
// continued task reaches a terminal end. The outbox makes repeated removals idempotent.
func (m *Manager) unacknowledge(t *tstate) {
	seen := map[string]struct{}{}
	for i, r := range []store.Reply{t.r, t.ackReply} {
		if r.Acknowledgment == "" || r.MessageRef == "" {
			continue
		}
		sourceID := r.ID
		if i == 0 && t.ackID != "" {
			sourceID = t.ackID
		}
		if sourceID == "" {
			continue
		}
		if _, ok := seen[sourceID]; ok {
			continue
		}
		seen[sourceID] = struct{}{}
		m.unacknowledgeReply(t.key, r, sourceID)
	}
}

// unacknowledgeReply queues the removal of one source's reaction. The source ID is supplied separately because a recovered
// task can be represented by a synthetic queue row while its reaction still belongs to the original reply.
func (m *Manager) unacknowledgeReply(key Key, r store.Reply, sourceID string) {
	if sourceID == "" || r.MessageRef == "" || r.Acknowledgment == "" {
		return
	}
	m.cfg.Sink.Notice(m.ctx, key, Notice{ID: acknowledgmentID(sourceID), Kind: NoticeUnacknowledged, MessageRef: r.MessageRef, Reaction: r.Acknowledgment})
}

// discardAnswer settles a consumed text reply when the interaction policy did not enqueue a continuation. A consumed
// reply is still a valid accepted message, so its temporary acknowledgment must not depend on a later task ending.
func (m *Manager) discardAnswer(c *convo) {
	r := c.answerReply
	c.answerReply = store.Reply{}
	if r.ID != "" {
		m.unacknowledgeReply(r.Key, r, r.ID)
	}
}

// turnAttrs says where the turn t runs. It names ids and places only, never what anyone wrote.
func (m *Manager) turnAttrs(t *tstate) []any {
	attrs := []any{"binding", t.sess.Binding, "connection", t.key.Connection, "channel", t.key.Channel, "thread", t.key.Thread, "message", t.r.ID}
	if t.sess.ContextID != "" {
		attrs = append(attrs, "session", t.sess.ContextID)
	}
	if t.taskID != "" {
		attrs = append(attrs, "task", string(t.taskID))
	}
	if t.recovered {
		attrs = append(attrs, "recovered", true)
	}
	return attrs
}

// taskEnded handles a turn whose task reached a terminal state: the thread is told what it would
// otherwise not read, and the end of the turn is logged. A failed, rejected or cancelled task is told always
// (a half answer is no sign that the task failed); a completed one only if it had nothing to show: t.texted
// says whether the turn said any text. A failed task's bounded status text is inspected only for a known category;
// its text and metadata never reach a Notice or log (spec 8.4, R2).
func (m *Manager) taskEnded(t *tstate, task *a2a.Task) {
	output := t.texted
	var kind NoticeKind
	var detail string
	var logExtra []any
	switch state := task.Status.State; {
	case state == a2a.TaskStateCompleted && !output:
		kind = NoticeNoOutput
	case state == a2a.TaskStateFailed:
		kind = NoticeTaskFailed
		category := classifyTaskFailure(task.Status.Message)
		reference := failureReference(t.key, task.ID)
		detail = failureDetail(category, reference)
		logExtra = []any{"failure_category", string(category), "failure_ref", reference}
	case state == a2a.TaskStateRejected:
		kind = NoticeTaskRejected
	case state == a2a.TaskStateCanceled:
		kind = NoticeTaskCanceled
	}
	switch {
	case kind == "":
	case task.ID == "": // the agent replied with a Message: no task to name the notice after, so it is the turn's
		m.notice(t, kind, detail)
	case kind == NoticeTaskFailed:
		m.cfg.Sink.Notice(m.ctx, t.key, Notice{ID: taskNoticeID(t.key, task.ID, kind), Kind: kind, MessageRef: t.r.MessageRef, Detail: detail})
	default:
		m.taskNotice(m.ctx, t.key, task.ID, kind, t.r.MessageRef)
	}
	m.finish(t, stateOutcome(task.Status.State), append([]any{"output", output}, logExtra...)...)
}

// visible reports whether s has anything to read: some character that is not white space once the invisible
// characters are gone. It uses redact.Normalize, the one deletion set that chat.Clean, which the renderer shows text
// through, uses too (the chat package imports this one, so Clean cannot be called here).
func visible(s string) bool {
	return strings.TrimFunc(redact.Normalize(s), unicode.IsSpace) != ""
}

func (m *Manager) turnContext(t *tstate) context.Context {
	if t.traceCtx != nil {
		return t.traceCtx
	}
	return m.ctx
}

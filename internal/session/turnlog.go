package session

import (
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
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
)

// TurnOutcomes returns every outcome a turn can have.
func TurnOutcomes() []TurnOutcome {
	return []TurnOutcome{TurnCompleted, TurnFailed, TurnRejected, TurnCanceled, TurnCredentialsRejected, TurnForbidden, TurnUnreachable,
		TurnSessionGone, TurnBindingGone, TurnWedged, TurnUncancellable, TurnPauseEnded, TurnAnswerRejected}
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
		m.cfg.Logger.Info("session turn started", m.turnAttrs(t)...)
	}
}

// finish logs the end of the turn t and reports its outcome. extra is logged with it. Call it once, where
// the turn leaves the runner for good: a turn that stays queued, or recorded for a retry, has not ended.
func (m *Manager) finish(t *tstate, outcome TurnOutcome, extra ...any) {
	m.mu.Lock()
	start, begun := m.begun[t.r.ID]
	delete(m.begun, t.r.ID)
	m.mu.Unlock()
	attrs := append(m.turnAttrs(t), "outcome", string(outcome))
	if begun {
		attrs = append(attrs, "duration_ms", m.cfg.Now().Sub(start).Milliseconds())
	}
	m.cfg.Logger.Info("session turn ended", append(attrs, extra...)...)
	if m.cfg.OnTurnEnd != nil {
		m.cfg.OnTurnEnd(t.sess.Binding, outcome)
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
// (a half answer is no sign that the task failed); a completed one only if it had nothing to show. output
// says whether the turn said any text. The agent's status message is not shown: it is untrusted, and a
// provider's error can hold a fragment of a key that no redaction knows (spec 8.4, R2).
func (m *Manager) taskEnded(t *tstate, task *a2a.Task, output bool) {
	var kind NoticeKind
	switch state := task.Status.State; {
	case state == a2a.TaskStateCompleted && !output:
		kind = NoticeNoOutput
	case state == a2a.TaskStateFailed:
		kind = NoticeTaskFailed
	case state == a2a.TaskStateRejected:
		kind = NoticeTaskRejected
	case state == a2a.TaskStateCanceled:
		kind = NoticeTaskCanceled
	}
	if kind != "" {
		m.taskNotice(m.ctx, t.key, task.ID, kind, t.r.MessageRef)
	}
	m.finish(t, stateOutcome(task.Status.State), "output", output)
}

// visible reports whether s has anything to read: some character that is not white space, a control or
// format character, or a blank filler. It follows chat.Clean, which the renderer shows text through (the chat
// package imports this one, so it cannot be called here).
func visible(s string) bool {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == utf8.RuneError && n == 1: // invalid UTF-8, which Clean deletes
		case unicode.IsSpace(r), r == 'ㅤ', r == '⠀', unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs):
		default:
			return true
		}
	}
	return false
}

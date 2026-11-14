package session

import (
	"cmp"
	"context"

	"github.com/agentkourier/agent-kourier/internal/audit"
)

// actor is who an audit row of this turn names.
func (t *tstate) actor() audit.Actor {
	a := audit.Actor{Binding: t.sess.Binding, Identity: t.b.Identity}
	if t.r.UserID != SystemUser && !t.system {
		a.User = t.r.UserID
	}
	return a
}

func (t *tstate) call(err error) audit.Call {
	c := audit.Call{Op: cmp.Or(t.op, audit.OpSend), TaskID: t.taskID, ContextID: t.sess.ContextID, MessageID: t.r.ID, Err: err}
	if t.system {
		c.MessageID = ""
	}
	return c
}

// rejected passes a refused credential to the identity Source, which re-reads the token and
// writes the audit row. Its own failure is logged: it is where an operator learns the token
// cannot be read.
func (m *Manager) rejected(t *tstate, err error) {
	m.reject(m.ctx, t.key, t.b, t.actor(), t.call(err))
}

// reject passes a refused credential to the identity Source, which writes the row, or, with no
// Source, writes it here: never dropped. The Source's own failure is logged.
func (m *Manager) reject(ctx context.Context, key Key, b Backend, actor audit.Actor, call audit.Call) {
	if b.Rejected == nil {
		m.auditCall(ctx, key, actor, call)
		return
	}
	if rerr := b.Rejected(context.WithoutCancel(ctx), actor, call); rerr != nil {
		m.cfg.Logger.Error("credential rejection bookkeeping", "key", key, "error", rerr)
	}
}

// audit records a call outcome the manager alone knows about. A failed write is logged, and
// never blocks the turn (spec 8.6 step 7).
func (m *Manager) audit(t *tstate, call audit.Call) { m.auditCall(m.ctx, t.key, t.actor(), call) }

// auditCall writes a call outcome. A failed write is logged, and never blocks anything (8.6 step 7).
func (m *Manager) auditCall(ctx context.Context, key Key, actor audit.Actor, call audit.Call) {
	if m.cfg.Audit == nil {
		return
	}
	if aerr := m.cfg.Audit.AgentCall(context.WithoutCancel(ctx), actor, call); aerr != nil {
		m.cfg.Logger.Error("audit write failed", "key", key, "error", aerr)
	}
}

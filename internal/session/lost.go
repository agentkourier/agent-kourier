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
	"github.com/agentkourier/agent-kourier/internal/store"
)

// lost handles a failure after the agent named the task, or of a follow. A refused credential
// is checked before a lost stream, because a reconnect the front door refused is both: it is
// not followed in a loop. A lost stream is followed again after a backoff.
func (m *Manager) lost(t *tstate, err error) (outcome, error) {
	var sl *agent.StreamLostError
	switch {
	case errors.Is(err, a2a.ErrTaskNotFound):
		return m.gone(t)
	case errors.Is(err, agent.ErrCredentialsRejected):
		m.rejected(t, err)
		t.c.follow = nil  // the task stays recorded, for reconciliation
		m.drives(t.c, "") // no longer driven, before the thread is told
		m.notice(t, NoticeFailed, "the agent's front door refused the Binding's credentials")
		m.finish(t, TurnCredentialsRejected)
		return ended, fmt.Errorf("follow turn %s: %w", t.r.ID, err)
	case errors.Is(err, agent.ErrAgentForbidden):
		// A card 403 on the reconnect: the agent is gone, or no longer permits the identity. Checked before a lost
		// stream, which it comes as, so it is not followed in a loop; it is refused like any other answer below.
	case dialect.AnswerTooLarge(err) && m.ctx.Err() == nil:
		// Checked before a lost stream: the agent would send the same reply again, so following it again gets nowhere.
		return m.tooLarge(t, err)
	case errors.As(err, &sl):
		if t.c.since.IsZero() {
			t.c.since = m.cfg.Now()
		}
		t.c.follow, t.taskID, t.follow = t, sl.TaskID, true
		m.drives(t.c, sl.TaskID) // it is followed again: still the task driven
		t.c.tries++
		return blocked, fmt.Errorf("follow turn %s: %w", t.r.ID, err)
	case m.ctx.Err() != nil: // a shutdown cut the stream: the task stays recorded, and nobody is told
		return ended, fmt.Errorf("send turn %s: %w", t.r.ID, err)
	}
	// Any other answer or failure after the task was named, such as a refused identity on the follow or a
	// permanent 4xx: it is no lost stream, so it is not followed again, and the turn is not left silent.
	// The task stays recorded, for reconciliation.
	t.c.follow = nil
	m.drives(t.c, "")
	// Named after the task and the thread, not the turn row: a restart that follows the task again raises the same post.
	detail, id := "the agent refused to continue the task", taskNoticeID(t.key, t.taskID, NoticeFailed)
	if errors.Is(err, dialect.ErrCardTooLarge) { // no connection was made: nothing was refused, and nothing was read of the task
		detail, id = cardTooLargeDetail, taskNoticeID(t.key, t.taskID, noticeCardTooLarge)
	}
	m.cfg.Sink.Notice(m.ctx, t.key, Notice{ID: id, Kind: NoticeFailed, MessageRef: t.r.MessageRef, Detail: detail})
	m.finish(t, TurnRefused)
	return ended, fmt.Errorf("send turn %s: %w", t.r.ID, err)
}

// tooLargeDetail is what the thread is told when what the agent sent passed what Agent Kourier reads or holds
// (dialect.ErrResponseTooLarge): one event, an answer to a call, the reply of the turn, or the tasks of a list. It is fixed,
// and true for each of them, a reply of sixteen bytes after tool results that add up to megabytes included: it names no
// backend, no size and nothing the agent sent.
const tooLargeDetail = "the agent sent more than Agent Kourier can read or hold, so the turn stopped"

// tooLargeAnswerDetail is tooLargeDetail for the turn of an answer to a paused task, with what else is true of it: the task is the one
// answered, and it is being cancelled; and the agent may have acted on the answer, which Agent Kourier cannot tell, since what the agent
// sent after it was not read (it may have taken the answer, or replied to it with something too large, a message among them). It must be
// shown whole (render.MaxNoticeDetail): a test holds it to that.
const tooLargeAnswerDetail = "what the agent sent after the answer was more than Agent Kourier can read or hold, so the turn stopped and its task is being cancelled. The agent may have acted on the answer."

// cardTooLargeDetail is what the thread is told when a task cannot be followed because the connection to the agent could not be opened: its
// agent card is past the bound. Nothing was refused and nothing was read of the task, which stays at the agent, recorded and followed by no one
// (the outcome is TurnRefused, as for any connection that fails on a follow). Nothing follows it again by itself: once the card is fixed, a restart
// takes it up (recovery reads it and follows it). No control of the thread does: Manager.Stop has no caller in production, and the Slack adapter
// handles no stop. The sentence says restart, and no more.
const cardTooLargeDetail = "Agent Kourier could not connect to the agent: its agent card is too large to read, so the turn stopped. The task stays at the agent. Once the card is fixed, restart Agent Kourier."

// cancelTimeout bounds the cancel of a task whose reply was too large, as the agent's own bound on a cancel
// (agent.Config.UnaryTimeout) does: this is the backstop for an AgentAPI that does not keep to it.
const cancelTimeout = 30 * time.Second

// tooLarge handles a task whose reply, after it was named, passed what Agent Kourier reads or holds (or an answer's, before it was
// named again: the task is the one answered). A reply that is one Message and no task names none (t.taskID is empty): there is
// nothing to cancel, so the turn ends at once, and the notice is named after the turn, since a thread's second such turn would
// otherwise share its name. The agent refused nothing:
// the broker stopped reading. The turn ends, the thread is told in fixed words (what it was already shown stays), and the
// task is cancelled, so that it does not run on at the agent with nobody reading it. The turn's own queue row, if the task
// came from one that nothing marked (recovery found the head to be the task's own, in the gap after a crash), is marked
// delivered, or the head would be looked up, followed and ended again, with a second notice. When the cancel took, the turn
// is over and the cause is logged, and the queue behind it goes on: a runner that is handed an error retires. A cancel that fails
// does not change what the thread is told: the task stays recorded, for reconciliation, as after a refusal, and the error is
// returned. A restart follows it again, meets the same, and cancels again (a thread control that cancels it, Manager.Stop, has no caller in
// production yet).
func (m *Manager) tooLarge(t *tstate, err error) (outcome, error) {
	t.c.follow = nil
	m.drives(t.c, "")
	detail := tooLargeDetail
	if t.answer {
		detail = tooLargeAnswerDetail
	}
	m.cfg.Sink.Notice(m.ctx, t.key, Notice{ID: tooLargeNoticeID(t), Kind: NoticeFailed, MessageRef: t.r.MessageRef, Detail: detail})
	if merr := m.cfg.Store.MarkReplyDelivered(context.WithoutCancel(m.ctx), t.r.ID, m.cfg.Now()); merr != nil && !errors.Is(merr, store.ErrNotFound) {
		m.cfg.Logger.WarnContext(m.ctx, "session: could not mark the turn of a task whose reply was too large delivered", "key", t.key, "turn", t.r.ID, "error", merr)
	}
	var endErr error
	cancelled := t.taskID == "" || m.cancelOversized(t)
	if cancelled {
		t.c.settle()
		endErr = m.end(context.WithoutCancel(m.ctx), t.key, t.c.ttl)
	}
	m.finish(t, TurnResponseTooLarge)
	cause := fmt.Errorf("send turn %s: %w", t.r.ID, err)
	if cancelled && endErr == nil {
		m.cfg.Logger.WarnContext(m.ctx, "session turn stopped: what the agent sent was more than Agent Kourier can read or hold", "key", t.key, "binding", t.sess.Binding, "turn", t.r.ID, "task", string(t.taskID), "error", cause)
		return ended, nil
	}
	return ended, errors.Join(cause, endErr)
}

// cancelOversized cancels the task whose reply was too large, once, within cancelTimeout, and reports whether the task is
// over at the agent (cancelled, already ended, or gone), so that it need not stay recorded. The call is audited as
// Agent Kourier's own, under no person and no turn; a refused credential goes to the identity Source, which writes the row.
func (m *Manager) cancelOversized(t *tstate) bool {
	ctx, stop := context.WithTimeout(m.ctx, m.cancelTimeout)
	defer stop()
	_, err := t.b.Agent.CancelTask(ctx, t.taskID)
	t.op, t.system = audit.OpCancel, true
	defer func() { t.op, t.system = "", false }()
	kind := classifyCancel(err)
	if kind == cancelCredentials {
		m.rejected(t, err)
	} else {
		m.audit(t, t.call(err))
	}
	m.warnUnverifiedCancel(t.key, t.sess.Binding, t.taskID, err)
	switch kind {
	case cancelDone, cancelEnded, cancelGone:
		return true
	}
	m.cfg.Logger.WarnContext(m.ctx, "session: could not cancel a task whose reply was too large; it stays recorded", "key", t.key, "binding", t.sess.Binding, "task", string(t.taskID), "error", err)
	return false
}

// gone handles a session the agent no longer has: the thread is told, and the record of it is
// reset, context and task together, so the next turn starts a fresh session. The thread stays
// owned. UNCONFIRMED: what kagent answers for a task or session it dropped (spec 8.2).
func (m *Manager) gone(t *tstate) (outcome, error) {
	if err := m.cfg.Store.ResetSession(context.WithoutCancel(m.ctx), t.key); err != nil {
		// Not reset, so not told and not moved on: the task is followed again, which finds it gone
		// again and retries the reset, or the session would keep its dead context for ever.
		t.c.follow, t.follow = t, true
		t.c.tries++
		return blocked, fmt.Errorf("reset session: %w", err)
	}
	m.goneNotice(m.ctx, t.key, t.taskID, t.r.MessageRef) // named after the task, so a repeat, or a control's, is one
	t.c.follow = nil
	t.c.settle()
	m.finish(t, TurnSessionGone)
	return ended, nil
}

// resume follows the task of a turn whose stream was lost. A task WORKING for longer than
// MaxWorkingAge (the agent's timestamp of the state, on Agent Kourier's clock), or one whose state
// cannot be read for that long, is presumed wedged and cancelled (R17); otherwise the stream
// is picked up again.
func (m *Manager) resume(c *convo) (out outcome, err error) {
	t := c.follow
	m.drives(c, t.taskID)
	defer func() {
		if out != blocked { // an exit that leaves the task recorded must not leave it looking driven
			m.drives(c, "")
			m.endHanded(t)
		}
	}()
	if c.cancelNow { // the wedge is known, so there is nothing to read first
		return m.wedged(t)
	}
	task, err := t.b.Agent.GetTask(m.ctx, t.taskID)
	switch {
	case dialect.AnswerTooLarge(err):
		// The same answer comes again: not a read to retry until the task counts as wedged. A task that a restart found was shown by
		// the process before it, and nothing here has given the sink a turn to end that message with: it is, as for a task that
		// is gone, once the turn is over.
		t.handed = t.handed || t.recovered
		return m.lost(t, err)
	case errors.Is(err, dialect.ErrCardTooLarge):
		// Every call of a real Agent meets the card, this read first: it is no read to retry until the task counts as wedged, and a thread
		// that is told nothing for an hour. lost tells it (cardTooLargeDetail). As for a task too large to read, the sink is given a turn.
		t.handed = t.handed || t.recovered
		return m.lost(t, err)
	case errors.Is(err, agent.ErrCredentialsRejected), errors.Is(err, agent.ErrAgentForbidden), errors.Is(err, a2a.ErrTaskNotFound):
		return m.lost(t, err)
	case err == nil && m.wedgedByAge(c, task), err != nil && m.cfg.Now().Sub(c.since) > m.cfg.MaxWorkingAge:
		return m.wedged(t)
	case err != nil:
		c.tries++
		return blocked, fmt.Errorf("get task %s: %w", t.taskID, err)
	}
	// Follow reads the task again (a second unary call, cheap) and then subscribes to it.
	return m.drive(t, func(ctx context.Context, h agent.Handler) error { return t.b.Agent.Follow(ctx, t.taskID, h) })
}

// endHanded ends the turn that a lost stream, or a send of unknown outcome, handed its message on to, for an attempt that
// ended without a sink turn of its own: the agent no longer had the task, it was cancelled as wedged, the follow was refused
// before it began, or the turn was given up on or dropped before it was sent again, and the thread was told so. The sink is
// given that turn, recovered, on a context that is over at once with no ErrTurnContinues, so that it ends what was handed on
// as it ends any turn with no final event (a stopped stream, no Working… left). A shutdown leaves it to recovery, and an
// attempt that drove the task had its own turn.
func (m *Manager) endHanded(t *tstate) {
	if !t.handed || m.ctx.Err() != nil {
		return
	}
	t.handed = false
	ctx, end := context.WithCancel(context.WithoutCancel(m.ctx))
	end()
	ref := t.sinkRef()
	ref.Recovered = true
	m.cfg.Sink.Turn(ctx, ref)
}

// endLeft ends what the head turn's send of unknown outcome handed on (convo.left), for a turn that ends, or a runner
// that retires, without another attempt of it taking that over.
func (m *Manager) endLeft(c *convo) {
	if l := c.left; l != nil {
		c.left = nil
		m.endHanded(l)
	}
}

// wedgedByAge reports whether a WORKING task has gone too long without life (R17): it counts
// from the later of the task's status timestamp and the last event Agent Kourier saw of it, so a
// healthy task that streamed for hours and then dropped is followed. A state with no timestamp
// counts from the last event Agent Kourier saw, else from the loss; a timestamp in the future protects
// the task only until it has passed. What a follow replays from its snapshot is not new life.
func (m *Manager) wedgedByAge(c *convo, task *a2a.Task) bool {
	if !running(task) {
		return false
	}
	var alive time.Time // the latest sign of life: the status timestamp, or what Agent Kourier saw
	if ts := task.Status.Timestamp; ts != nil {
		alive = *ts
	}
	if c.life.task == task.ID && c.lastSeen.After(alive) {
		alive = c.lastSeen
	}
	if alive.IsZero() {
		alive = c.since // no timestamp and nothing seen: count from the loss
	}
	return !alive.IsZero() && m.cfg.Now().Sub(alive) > m.cfg.MaxWorkingAge
}

// running reports whether a task is being worked on: not ended, and not waiting for input.
func running(t *a2a.Task) bool {
	s := t.Status.State
	return !s.Terminal() && s != a2a.TaskStateInputRequired && s != a2a.TaskStateAuthRequired
}

// wedged cancels a task presumed wedged (R17). The decision is audited once, by how it ends, not
// once per attempt. The cancel's error says what to do: a task the agent no longer knows resets
// the session; one that cannot be cancelled has ended, so it is read once more and its end
// rendered; a refused credential or identity fails; anything else is retried until SendGiveUp,
// then reported. Every way out lets the queue go on, except where the task stays recorded and
// the turns behind it stay held until a restart's reconciliation releases
// the session (Manager.Stop would too, but nothing calls it in production yet).
func (m *Manager) wedged(t *tstate) (outcome, error) {
	c := t.c
	if c.cancelFrom.IsZero() {
		c.cancelFrom = m.cfg.Now()
	}
	_, err := t.b.Agent.CancelTask(m.ctx, t.taskID)
	t.op, t.system = audit.OpCancel, true // Agent Kourier's own cancel: not the paused turn's user, not its message
	defer func() { t.op, t.system = "", false }()
	kind := classifyCancel(err)
	m.warnUnverifiedCancel(t.key, t.sess.Binding, t.taskID, err)
	switch {
	case kind == cancelDone:
		m.audit(t, t.call(err)) // err is nil, or an answer too large to read, which the row says
		m.notice(t, NoticeWedged, "")
		m.finish(t, TurnWedged)
		c.follow = nil
		c.settle()
		return ended, m.end(context.WithoutCancel(m.ctx), t.key, c.ttl)
	case kind == cancelGone:
		m.audit(t, t.call(err))
		return m.gone(t)
	case kind == cancelEnded: // it ended in the meantime: show how
		m.audit(t, t.call(err))
		c.cancelFrom = time.Time{}
		return m.drive(t, func(ctx context.Context, h agent.Handler) error { return t.b.Agent.Follow(ctx, t.taskID, h) })
	case kind == cancelCredentials:
		return m.lost(t, err) // the identity Source writes the row
	case kind == cancelForbidden, m.cfg.Now().Sub(c.cancelFrom) >= m.cfg.SendGiveUp:
		m.audit(t, t.call(err))
		c.follow = nil
		return m.fail(t, TurnUncancellable, "the wedged task could not be cancelled; restart agent-kourier to reconcile it", err)
	}
	c.tries++
	return blocked, fmt.Errorf("cancel wedged task %s: %w", t.taskID, err)
}

package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// convo is a conversation that has a runner, which drains its queue one turn at a time (D21).
// Only the runner reads and writes tries and ttl; the Manager's mutex guards the rest.
type convo struct {
	again         bool            // a wake arrived while the runner was busy
	tries         int             // sends of the head turn that failed in a way to retry, in a row
	since         time.Time       // when the first of them failed, for SendGiveUp
	unreached     int             // sends or lookups of the head turn that failed to reach the agent, in a row: the second tells the thread
	busyNoted     bool            // the thread was told the reply is queued behind a task nobody recorded
	waitSince     time.Time       // when the head turn first got -32004 in this run of trouble, for waitNoticeAfter
	waitNoted     bool            // the thread was told the turn has waited long (NoticeStillBusy or NoticeNotReady)
	lastSeen      time.Time       // when Agent Kourier last saw NEW life of task life.task: the later of this and the status timestamp is its age (R17)
	life          life            // what Agent Kourier already holds of that task, so a replayed snapshot is not life
	cancelFrom    time.Time       // when the first attempt to cancel a wedged task was made, for SendGiveUp
	cancelNow     bool            // the task is known to be wedged: cancel it at once, without waiting for its age
	patient       bool            // the turn is never given up on: the agent may have taken it
	follow        *tstate         // the turn whose task is followed again, after its stream was lost
	paused        *tstate         // the turn whose task waits for input, while it does
	answers       chan answerWork // answers to the paused task, for the parked runner to send
	attach        *tstate         // a recovered task for the runner to take up; guarded by the Manager's mutex
	ttl           time.Duration   // the Binding's threadTTL, for end
	parked        a2a.TaskID      // the task the runner waits on, while it waits for input
	driving       a2a.TaskID      // the task the runner streams or follows, between named and ended
	released      bool            // the pause ended, and the runner has not yet moved on
	resume        chan struct{}   // tells the parked runner that the pause ended
	textRejected  bool            // credential rejection was reported for this pause
	textTries     int
	textAfter     int64         // queue cursor for the current question; skipped replies stay queued
	pendingReply  store.Reply   // decided reply whose delivery write must succeed before its answer is sent
	answerReply   store.Reply   // the accepted answer whose acknowledgment ends with the continued task
	pendingAnswer *answerWork   // an answer whose source is being durably recorded or delivered
	queued        chan struct{} // wakes a parked text-question runner
	window        windowText    // the thread context of the head turn, so that a resend does not ask for it again
}

// windowText is the thread context (Config.ThreadContext) of one turn, kept for as long as the turn is the head of its
// conversation: a send that got -32004 is sent again, and must carry what the first carried without the store or the
// hook being asked again. The runner alone reads and writes it, and the next turn replaces it.
type windowText struct {
	id      string // the turn's ID; empty before the first turn that asks
	none    bool   // the turn has no turn before it, so no window
	after   string // the message of the turn before it: the window opens after it
	asked   bool   // the hook was called, and section is its answer, which may be ""
	section string
}

// outcome is how one turn left the conversation: nothing queued, the task ended, or the task
// waits for input (spec 8.4), or cannot be sent yet.
type outcome int

const (
	idle outcome = iota
	ended
	paused
	blocked // the agent is busy with a task Agent Kourier did not record, or the lookup failed
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
		select {
		case c.queued <- struct{}{}:
		default:
		}
		return
	}
	c := m.newConvo(key)
	m.wg.Add(1)
	go m.run(key, c)
}

// adopt puts the recorded task t, found by Recover, in front of key's runner, which follows it
// before anything queued is sent. A conversation with no runner gets one; a conversation whose runner
// a wake got to first has the task attached, for the runner to take on its next pass.
func (m *Manager) adopt(key Key, t *tstate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if c, ok := m.convos[key]; ok {
		// A runner that already drives or waits on a task has the recorded one; only an idle runner,
		// or one holding a turn behind it, takes it up.
		if c.attach == nil && c.parked == "" && c.driving == "" {
			t.c, c.attach = c, t
		}
		c.again = true
		return
	}
	c := m.newConvo(key)
	t.c, c.attach = c, t
	m.wg.Add(1)
	go m.run(key, c)
}

// newConvo registers a conversation's state. The caller holds m.mu.
func (m *Manager) newConvo(key Key) *convo {
	c := &convo{resume: make(chan struct{}, 1), answers: make(chan answerWork, 1), queued: make(chan struct{}, 1)}
	m.convos[key] = c
	telemetry.AddGauge(m.ctx, "agentkourier_sessions_active", 1)
	return c
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
	sess, err := m.cfg.Store.GetSession(ctx, key, readAll, m.cfg.Now())
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

// backoff is the pause before the next resend after tries in a row: RetryBase, doubling, up
// to RetryMax.
func (m *Manager) backoff(tries int) time.Duration {
	d := m.cfg.RetryBase
	for i := 1; i < tries && d < m.cfg.RetryMax; i++ {
		d *= 2
	}
	return min(d, m.cfg.RetryMax)
}

// run drains key's queue. A turn that fails stays queued, for the next wake to retry; one that
// is blocked is retried after a backoff, and never dropped.
func (m *Manager) run(key Key, c *convo) {
	defer m.wg.Done()
	defer telemetry.AddGauge(m.ctx, "agentkourier_sessions_active", -1)
	for m.ctx.Err() == nil {
		out, err := m.turn(key, c)
		for m.log(key, c, out, err); out == paused && m.ctx.Err() == nil; m.log(key, c, out, err) {
			out, err = m.parked(key, c)
		}
		switch {
		case out == blocked:
			select {
			case <-m.cfg.After(m.backoff(c.tries)):
			case <-m.ctx.Done():
			}
		case (out == idle || err != nil) && m.retire(key):
			return
		}
	}
}

// log reports the error of a turn. A hold is expected and only informs, and so is a send the agent refused as
// busy or not ready (-32004) that is sent again after a backoff: it names the reason and the attempt, and
// leaves out the error, which holds nothing the reason does not say. Anything else is a failure.
func (m *Manager) log(key Key, c *convo, out outcome, err error) {
	switch {
	case errors.Is(err, errHeld):
		m.cfg.Logger.Info("session turn held", "key", key, "reason", err)
	case err == nil || m.ctx.Err() != nil:
	case out == blocked && errors.Is(err, agent.ErrBusy):
		reason := "busy"
		if errors.Is(err, agent.ErrNotReady) {
			reason = "not_ready"
		}
		m.cfg.Logger.Info("session turn waiting", "key", key, "reason", reason, "attempt", c.tries)
	default:
		m.cfg.Logger.Error("session turn failed", "key", key, "error", err)
	}
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

// turn sends the oldest queued turn of key and follows it until its task ends or pauses.
// Store writes ignore a shutdown, so an accepted turn is never left unrecorded.
func (m *Manager) turn(key Key, c *convo) (outcome, error) {
	if err := m.flushTextReply(c); err != nil {
		c.tries++
		return blocked, err
	}
	m.mu.Lock()
	var taken *tstate
	if a := c.attach; a != nil { // Recover found a task for this conversation
		c.attach = nil
		if c.follow == nil {
			c.follow, c.ttl, c.since = a, a.b.ThreadTTL, m.cfg.Now()
			c.driving = a.taskID // from now on the runner drives it: no second attachment in the gap before resume
			taken = a
		}
	}
	m.mu.Unlock()
	if taken != nil && m.superseded(taken) { // it ended, or was stopped, since Recover read the session
		m.mu.Lock()
		c.follow, c.driving = nil, ""
		m.mu.Unlock()
	}
	if c.follow != nil { // a stream was lost: the task is followed before anything else is sent
		return m.resume(c)
	}
	sctx := context.WithoutCancel(m.ctx)
	r, err := m.cfg.Store.NextReply(sctx, key)
	if errors.Is(err, store.ErrNotFound) { // empty: nothing sent is waiting for a resend
		m.mu.Lock()
		delete(m.attempted, key)
		m.mu.Unlock()
		return idle, nil
	} else if err != nil {
		return idle, fmt.Errorf("read queue: %w", err)
	}
	sctx = telemetry.Restore(sctx, r.TraceParent, r.TraceState)
	m.mu.Lock()
	if _, fresh := m.fresh[r.ID]; !fresh { // an earlier process may have marked it queued, and nothing here knows
		m.marked[r.ID] = struct{}{} // so it is taken off when the turn is sent or ends, which does no harm if it is not there
	}
	m.mu.Unlock()
	sess, err := m.cfg.Store.GetSession(sctx, key, readAll, m.cfg.Now())
	if err != nil {
		return idle, fmt.Errorf("read session: %w", err)
	}
	backend, err := m.cfg.Bindings.Backend(sess.Binding)
	if errors.Is(err, ErrUnknownBinding) { // it left the config since the turn was queued: no retry loop
		t := &tstate{key: key, c: c, r: r, sess: sess}
		m.notice(t, NoticeBindingGone, "")
		// Delivered_at on this row means dropped, not sent: recovery and audit readers must not
		// take it for a turn the agent received.
		m.mu.Lock()
		delete(m.fresh, r.ID)
		m.mu.Unlock()
		if err := m.cfg.Store.MarkReplyDelivered(sctx, r.ID, m.cfg.Now()); err != nil {
			return ended, err // still queued: dropped again on the next pass
		}
		m.finish(t, TurnBindingGone)
		return ended, nil
	} else if err != nil {
		return idle, fmt.Errorf("session Binding %q: %w", sess.Binding, err)
	}
	// A claimed answer left by a crash, or one queued pinned by the intake, must be reconciled with its
	// question, never sent as an ordinary turn, whatever the Binding's dialect is by now.
	if Queued(r).Kind == AdmitAnswer {
		verdict, err := m.cfg.TextReply(telemetry.Restore(m.ctx, r.TraceParent, r.TraceState), key, r, a2a.Task{})
		if err != nil || verdict != TextReplyConsumed {
			c.tries++
			if err == nil && verdict != TextReplyWait {
				err = fmt.Errorf("pinned text reply %s: invalid outcome %d", r.ID, verdict)
			}
			return blocked, err
		}
		m.mu.Lock()
		c.pendingReply = r
		m.mu.Unlock()
		if err := m.flushTextReply(c); err != nil {
			c.tries++
			return blocked, err
		}
		c.tries = 0
		m.discardAnswer(c)
		return ended, nil
	}
	c.ttl = backend.ThreadTTL
	t := &tstate{key: key, c: c, r: r, sess: sess, b: backend}
	send := func(ctx context.Context, h agent.Handler) error {
		// Only a send carries the thread's context: the turn that is followed was sent before, with its own.
		msg := m.message(m.withThreadContext(ctx, c, r, sess.Binding, backend), sess, backend)
		if err := testhooks.BeforeSend(ctx, msg.ID); err != nil {
			return err
		}
		return backend.Agent.Send(ctx, msg, h)
	}
	sends := true // false once the turn is found on the agent, to be followed and not sent
	hold := m.held(key, r.ID, sess)
	m.mu.Lock()
	prev := m.attempted[key]
	_, fresh := m.fresh[r.ID]
	// A turn this process sent before, or one it did not enqueue itself (a process that died may have
	// sent it), is a resend: looked up first, however the wake got here.
	resend := prev.id == r.ID || !fresh
	m.attempted[key] = attempt{id: r.ID, unknown: prev.id == r.ID && prev.unknown}
	m.mu.Unlock()
	if hold != nil || resend {
		// The agent may already have this turn, and a resend into an existing context is not
		// deduplicated, so look it up first, whatever failed the first time: found, it is followed
		// (a duplicate run is reported); absent, it is sent. A lookup that fails means nothing is
		// known, so nothing is sent (spec 8.2). A first turn has no context to look in, and its
		// resend is deduplicated.
		if sess.ContextID != "" {
			task, holders, err := backend.Agent.FindTask(telemetry.Restore(m.ctx, r.TraceParent, r.TraceState), sess.ContextID, r.ID)
			switch {
			case errors.Is(err, agent.ErrLookupUnsupported):
				// The agent has no task list (A2A 0.3, kagent 0.9.x): this turn cannot be looked up, and asking
				// again changes nothing. It is treated as absent, as a lookup that found nothing is: a duplicate
				// run is possible after a resend, a turn that waits for ever is not.
				m.cfg.Logger.Warn("session turn not looked up: the agent cannot list tasks", "key", key, "turn", r.ID)
				if sess.TaskID != "" { // a task of the session runs: this turn is behind it
					return idle, hold
				}
			case err != nil:
				c.patient = c.patient || hold != nil // a turn the agent took is never failed
				return m.retry(t, fmt.Errorf("look up turn %s: %w", r.ID, err))
			case task != nil:
				if len(holders) > 1 {
					m.duplicates(t, holders)
				}
				send, sends = func(ctx context.Context, h agent.Handler) error { return backend.Agent.Follow(ctx, task.ID, h) }, false
			case sess.TaskID != "": // absent, and a task of the session runs: this turn is behind it
				return idle, hold
			}
		} else if sess.TaskID != "" {
			return idle, hold
		}
		m.release(key, r.ID) // reconciled: followed, or the agent never had it
	}
	if sends {
		if err := m.readWindow(sctx, c, r, backend); err != nil { // like any store failure here: wait and try again, with no notice
			c.tries++
			return blocked, err
		}
	}
	return m.drive(t, send)
}

// wantsThreadContext reports whether the turn r takes the thread's context (Config.ThreadContext): a person's turn, in
// a Binding with chat.threadReplies: mention. A reply that was admitted as an answer to a question is no turn (Queued).
func (m *Manager) wantsThreadContext(r store.Reply, b Backend) bool {
	return b.ThreadReplies == v1alpha1.ThreadRepliesMention && r.UserID != SystemUser && Queued(r).Kind == AdmitTurn
}

// readWindow finds where the window of the turn r opens, which is the message of the row before it in the session's
// queue, delivered or not, and keeps it for the turn. It is read from stored rows, so it is the same on every attempt,
// after a restart and after a failover. A turn with no row before it has no window. It runs before the turn is sent,
// where a store that fails waits like any other failure of the runner: the turn is never sent without its context,
// which it could not be sent again with, and nothing tells the thread the agent cannot be reached.
func (m *Manager) readWindow(ctx context.Context, c *convo, r store.Reply, b Backend) error {
	if !m.wantsThreadContext(r, b) || c.window.id == r.ID {
		return nil
	}
	prev, err := m.cfg.Store.PreviousReply(ctx, r.Key, r.Sequence)
	switch {
	case errors.Is(err, store.ErrNotFound): // the first turn of the thread: nothing came before it
		c.window = windowText{id: r.ID, none: true}
	case err != nil:
		return fmt.Errorf("read the turn before %s: %w", r.ID, err)
	default:
		c.window = windowText{id: r.ID, after: prev.MessageRef}
	}
	return nil
}

// withThreadContext returns the turn r with the thread's context after the person's text (R18: one text part, the
// dialect's attribution in front of it), or r as it was queued. The hook is called once for the turn, for the window
// from readWindow's message to this turn's, and what it returned is kept, so a resend after -32004 does not ask again.
func (m *Manager) withThreadContext(ctx context.Context, c *convo, r store.Reply, binding string, b Backend) store.Reply {
	if !m.wantsThreadContext(r, b) || c.window.id != r.ID || c.window.none {
		return r
	}
	if !c.window.asked {
		c.window.section, c.window.asked = m.cfg.ThreadContext(ctx, r.Key, binding, c.window.after, r.MessageRef), true
	}
	if c.window.section != "" {
		r.Text += "\n\n" + c.window.section
	}
	return r
}

// superseded reports whether the session no longer records the task t, which Recover read some
// time ago: following it now would render its end again, and clear whatever the session holds
// by then. A read that fails does not drop the task: following it is the safe side.
func (m *Manager) superseded(t *tstate) bool {
	sess, err := m.cfg.Store.GetSession(context.WithoutCancel(m.ctx), t.key, readAll, m.cfg.Now())
	return errors.Is(err, store.ErrNotFound) || (err == nil && sess.TaskID != string(t.taskID))
}

// settle forgets the retry state of the head turn: it was taken, failed for good, or ended.
func (c *convo) settle() {
	c.tries, c.since, c.patient, c.unreached, c.busyNoted, c.cancelFrom, c.cancelNow = 0, time.Time{}, false, 0, false, time.Time{}, false
	c.waitSince, c.waitNoted = time.Time{}, false
}

// drives records the task the runner is streaming or following, for endPause and the controls.
func (m *Manager) drives(c *convo, id a2a.TaskID) {
	m.mu.Lock()
	c.driving = id
	m.mu.Unlock()
}

// unpark forgets a pause the runner turned out not to be parked on, and any signal for it.
func (m *Manager) unpark(c *convo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.parked, c.released, c.paused = "", false, nil
	select {
	case <-c.resume:
	default:
	}
}

// life is what Agent Kourier holds of one task: its state, the text of each artifact, and the tool
// calls and results seen. An event that adds nothing to it (the replay of a snapshot) shows no
// sign that the task is alive.
type life struct {
	task  a2a.TaskID
	state a2a.TaskState
	text  map[a2a.ArtifactID]string
	tools map[string]bool
}

// renews records e, and reports whether it was new: a first look at a task, a change of state,
// text that differs from what was held, or a tool call or result not seen before. A reply with no
// task (a Message) is a first look each time, so no reply's text outlives its turn.
func (l *life) renews(e agent.Event) bool {
	switch ev := e.(type) {
	case agent.StateEvent:
		if ev.TaskID != l.task || ev.TaskID == "" {
			*l = life{task: ev.TaskID, state: ev.State, text: map[a2a.ArtifactID]string{}, tools: map[string]bool{}}
			return true
		}
		old := l.state
		l.state = ev.State
		return ev.State != old
	case agent.TextEvent:
		old, had := l.text[ev.ArtifactID]
		if l.text != nil {
			l.text[ev.ArtifactID] = ev.Text
		}
		return !had || old != ev.Text
	case agent.ToolCallEvent:
		return l.tool("call " + ev.CallID)
	case agent.ToolResultEvent:
		return l.tool("result " + ev.CallID)
	}
	return false
}

func (l *life) tool(key string) bool {
	if l.tools == nil {
		return true // a tool event before any state: nothing is held yet
	}
	seen := l.tools[key]
	l.tools[key] = true
	return !seen
}

// tstate is the turn being run: its queue row, the session it runs in, and its Binding.
type tstate struct {
	follow    bool            // the attempt follows a task instead of sending
	answer    bool            // the attempt answers the paused task
	nid       string          // what a notice of this attempt is named after, if not the turn
	taskID    a2a.TaskID      // the task the agent named, or the one followed or answered
	op        audit.Op        // the call audit rows name, when not a send
	traceCtx  context.Context // current attempt, for correlated lifecycle logs
	system    bool            // Agent Kourier made the call itself: no person, no turn
	recovered bool            // the task was found again after a restart
	began     store.TaskTurn  // the turn the session recorded for the recovered task, whose output the renderer continues; zero if none was
	texted    bool            // the turn said something to read: kept across a stream lost and followed, for the renderer keeps the message
	key       Key
	c         *convo
	r         store.Reply
	ackReply  store.Reply // an answer's own reply, when r is the paused question it continues
	ackID     string      // the original source ID when r is a synthetic recovered task row
	sess      store.Session
	b         Backend
}

// outputTurn is the turn the output of this attempt belongs to, which the task is recorded with and the renderer is told: an
// answer's continuation is the answer's own (its output is a message of its own, never the paused turn's), a task found again
// is the turn the session recorded for it, so that the message that turn began is found, and any other is the turn itself.
func (t *tstate) outputTurn() store.TaskTurn {
	ack := t.r
	if t.ackReply.Acknowledgment != "" {
		ack = t.ackReply
	}
	ackID := cmp.Or(t.ackID, t.began.AcknowledgmentID, t.began.ID, t.r.ID)
	if t.ackReply.Acknowledgment != "" {
		ackID = t.ackReply.ID
	}
	ackEmoji := cmp.Or(ack.Acknowledgment, t.r.Acknowledgment, t.began.Acknowledgment)
	if ackEmoji == "" {
		ackID = ""
	}
	return store.TaskTurn{ID: cmp.Or(t.nid, t.began.ID, t.r.ID), User: cmp.Or(ack.UserID, t.r.UserID, t.began.User), MessageRef: cmp.Or(ack.MessageRef, t.r.MessageRef, t.began.MessageRef), Acknowledgment: ackEmoji, AcknowledgmentID: ackID}
}

// feedbackOf is the feedback buttons the final message of a turn written by user carries (TurnRef.Feedback).
func feedbackOf(user string, b Backend) store.FeedbackKind {
	switch {
	case user == SystemUser:
		return store.FeedbackInvestigation
	case user != "" && b.Feedback:
		return store.FeedbackAnswer
	}
	return ""
}

// drive runs one attempt at a turn, a Send or a Follow, streaming its events to the sink and
// recording the task as soon as the agent names it.
func (m *Manager) drive(t *tstate, send func(context.Context, agent.Handler) error) (out outcome, err error) {
	key, c, r := t.key, t.c, t.r
	defer func() {
		if out != paused { // the pause event may have parked the runner before the stream failed
			m.unpark(c)
		}
		if out != blocked { // a lost stream is still followed: the task stays the one driven
			m.drives(c, "")
		}
	}()
	parentCtx := telemetry.Restore(m.ctx, r.TraceParent, r.TraceState)
	if t.recovered && !t.answer {
		parentCtx = m.ctx
	}
	parentCtx, op := telemetry.Start(parentCtx, "agentkourier.turn.attempt", attribute.String("agentkourier.binding", t.sess.Binding), attribute.String("agentkourier.turn.id", r.ID), attribute.Bool("agentkourier.recovered", t.recovered && !t.answer), attribute.Int("agentkourier.retry.count", c.tries), attribute.String("agentkourier.backend", t.b.Name), attribute.String("agentkourier.agent.name", t.b.AgentName), attribute.String("agentkourier.dialect", t.b.Dialect))
	if t.recovered && !t.answer {
		telemetry.Link(parentCtx, t.sess.TraceParent, t.sess.TraceState)
	}
	t.traceCtx = parentCtx
	defer func() {
		switch {
		case errors.Is(err, errHeld):
			op.FinishOutcome(nil, "held")
		case errors.Is(err, agent.ErrBusy):
			op.FinishOutcome(nil, "busy")
		default:
			op.FinishOutcome(err, turnAttemptOutcome(out, err))
		}
	}()
	sctx := context.WithoutCancel(parentCtx)
	m.mu.Lock()
	_, begun := m.begun[r.ID]
	m.mu.Unlock()
	if !begun && !r.EnqueuedAt.IsZero() {
		telemetry.Observe(m.ctx, "agentkourier_turn_queue_wait_seconds", m.cfg.Now().Sub(r.EnqueuedAt), t.sess.Binding)
	}
	m.begin(t)
	h := func(agent.Event) {}
	// The sink's context ends when the send returns, not with the manager: a turn that ends with
	// no FinalEvent frees what the sink started for it. A sink that must settle a finished turn
	// does so on a context of its own (the renderer's bounded flush).
	tctx, endTurn := context.WithCancel(parentCtx)
	defer endTurn()
	// An answer's output is a message of its own, named after the answer: it must not overwrite
	// the output of the turn that paused. A task found again after a restart, or followed again
	// after a lost stream, continues the message it had.
	shown := t.outputTurn()
	ref := TurnRef{Key: key, Binding: t.sess.Binding, MessageID: shown.ID, UserID: shown.User, Output: t.b.Output, Recovered: t.recovered || t.follow, Restarted: t.recovered, Feedback: feedbackOf(shown.User, t.b), ThreadReplies: t.b.ThreadReplies}
	if sh := m.cfg.Sink.Turn(tctx, ref); sh != nil {
		h = sh
	}
	var final *a2a.Task
	var recordErr error
	named := false // the agent took the turn: it named the task, or replied with a Message and no task
	var pausedTaskID, pausedTurnID string
	err = send(tctx, func(e agent.Event) {
		paused := false
		if c.life.renews(e) { // only what is new counts: a follow replays the snapshot it began with
			c.lastSeen = m.cfg.Now()
		}
		switch ev := e.(type) {
		case agent.StateEvent:
			trace.SpanFromContext(tctx).SetAttributes(attribute.String("agentkourier.task.id", string(ev.TaskID)), attribute.String("agentkourier.context.id", ev.ContextID), attribute.String("agentkourier.task.state", string(ev.State)))
			t.taskID = ev.TaskID
			m.drives(c, ev.TaskID)
			if !named { // the agent has taken the turn: record it before anyone renders it
				named = true
				c.settle()
				if t.sess.ContextID != "" && ev.ContextID != "" && ev.ContextID != t.sess.ContextID {
					// The agent answered in another context: it opened a new session for the old id.
					// UNCONFIRMED against kagent. The new context is recorded below.
					m.notice(t, NoticeSessionGone, "")
				}
				if t.sess.ContextID == "" {
					t.sess.ContextID = ev.ContextID // a first turn learns its context here
				}
				recordErr = m.accept(sctx, r, ev, t.outputTurn())
				if t.recovered && errors.Is(recordErr, store.ErrNotFound) {
					recordErr = nil // the turn of a recovered task may have no queue row to mark
				}
				if recordErr == nil {
					m.mu.Lock()
					if m.attempted[key].id == r.ID { // this turn's own record: taken, so never a resend
						delete(m.attempted, key)
					}
					delete(m.fresh, r.ID)
					m.mu.Unlock()
				}
				m.unqueue(t) // the agent has it, so the reply waits no more, however the recording went
			}
		case agent.TextEvent:
			t.texted = t.texted || visible(ev.Text)
		case agent.FinalEvent:
			final = ev.Task
			if !final.Status.State.Terminal() {
				pausedTaskID = string(final.ID)
				pausedTurnID = t.outputTurn().ID
				// The pause is rendered inside this handler, and whoever renders it may end it at
				// once: the runner must already be parked on this task.
				m.mu.Lock()
				c.textAfter, c.textTries, c.textRejected = 0, 0, false
				c.parked, c.paused, c.driving = final.ID, t, "" // a pause is not a stream
				m.mu.Unlock()
				paused = true
			}
		}
		h(e)
		if paused {
			// The agent has posted its question or prompt, so the source message no longer needs
			// to advertise that it is being picked up. Clear the in-memory copy as well: an answer
			// continues the paused task and owns its own acknowledgment.
			m.unacknowledge(t)
			t.r.Acknowledgment = ""
			t.ackReply.Acknowledgment = ""
			t.began.Acknowledgment = ""
			t.began.AcknowledgmentID = ""
			t.ackID = ""
			if pausedTaskID != "" && pausedTurnID != "" {
				// A task-owned update cannot restore task or context if Stop reset the session
				// while the rendered pause was being handled. The turn ID also protects a newer
				// continuation of the same agent task.
				if err := m.cfg.Store.ClearSessionAcknowledgment(context.WithoutCancel(sctx), t.key, pausedTaskID, pausedTurnID); err != nil {
					m.cfg.Logger.WarnContext(sctx, "session: could not clear paused acknowledgment metadata", "message", t.r.ID, "error", err)
				}
			}
		}
	})
	if err == nil {
		c.follow = nil
	}
	switch {
	case recordErr != nil: // the agent has the turn and we cannot say so: never send it again
		telemetry.Count(parentCtx, "agentkourier_session_recovery_total", "record_failed")
		m.hold(key, r.ID)
		return ended, fmt.Errorf("record accepted turn %s: %w", r.ID, recordErr)
	case err != nil && t.answer && errors.Is(err, agent.ErrAnswerRejected): // it says so after the task is named again
		return m.unanswered(t, err)
	case err != nil && (named || t.follow):
		return m.lost(t, err)
	case err != nil && t.answer:
		return m.unanswered(t, err)
	case err != nil:
		return m.unsent(t, err)
	case !named: // the stream ended clean but named no task: the agent may have run the turn
		m.hold(key, r.ID)
		return ended, fmt.Errorf("send turn %s: the agent named no task", r.ID)
	case final == nil:
		// A real client never returns nil without a final event (Agent.Send and Follow return nil only once
		// the task has settled), but the AgentAPI does not promise it: the thread is told, not left waiting.
		m.notice(t, NoticeFailed, "the agent's reply ended before the task did")
		m.finish(t, TurnNoFinal)
		return ended, fmt.Errorf("send turn %s: the stream ended with no final task", r.ID)
	case !final.Status.State.Terminal():
		return paused, nil
	}
	m.taskEnded(t, final)
	return ended, m.end(sctx, key, c.ttl)
}

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
	t.answer, t.nid, t.taskID, t.follow = true, msg.ID, msg.TaskID, false
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
// (see readAnswered). Any other failure leaves the task paused, so the person can answer again.
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
	t.c.parked, t.c.released = t.taskID, false
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
	m.cfg.Sink.Notice(m.ctx, t.key, Notice{ID: taskNoticeID(t.key, t.taskID, NoticeFailed), Kind: NoticeFailed, MessageRef: t.r.MessageRef, Detail: "the agent refused to continue the task"})
	m.finish(t, TurnRefused)
	return ended, fmt.Errorf("send turn %s: %w", t.r.ID, err)
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

// goneNotice tells the thread the agent no longer has the task's session. Its ID is the task's and the thread's, so
// the runner and a control that both find it gone post one notice.
func (m *Manager) goneNotice(ctx context.Context, key Key, id a2a.TaskID, ts string) {
	m.taskNotice(ctx, key, id, NoticeSessionGone, ts)
}

// taskNotice tells the thread something about the task id. The notice is named after the task and its
// kind, so that every path that finds the same fact, whichever turn row it holds, posts it once.
func (m *Manager) taskNotice(ctx context.Context, key Key, id a2a.TaskID, kind NoticeKind, ts string) {
	m.cfg.Sink.Notice(ctx, key, Notice{ID: taskNoticeID(key, id, kind), Kind: kind, MessageRef: ts})
}

// taskNoticeID names the notice of kind about the task id in the thread key. The thread is part of it
// because the outbox keeps one row per ID, and a backend's task IDs may repeat across threads (the stub
// agent's do): a notice named after the task alone would be dropped in the second thread.
func taskNoticeID(key Key, id a2a.TaskID, kind NoticeKind) string {
	return "task:" + key.Connection + ":" + key.Channel + ":" + key.Thread + ":" + string(id) + "/" + string(kind)
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
		}
	}()
	if c.cancelNow { // the wedge is known, so there is nothing to read first
		return m.wedged(t)
	}
	task, err := t.b.Agent.GetTask(m.ctx, t.taskID)
	switch {
	case errors.Is(err, agent.ErrCredentialsRejected), errors.Is(err, a2a.ErrTaskNotFound):
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
// the turns behind it stay held until the Stop control or a restart's reconciliation releases
// the session.
func (m *Manager) wedged(t *tstate) (outcome, error) {
	c := t.c
	if c.cancelFrom.IsZero() {
		c.cancelFrom = m.cfg.Now()
	}
	_, err := t.b.Agent.CancelTask(m.ctx, t.taskID)
	t.op, t.system = audit.OpCancel, true // Agent Kourier's own cancel: not the paused turn's user, not its message
	defer func() { t.op, t.system = "", false }()
	kind := classifyCancel(err)
	switch {
	case kind == cancelDone:
		m.audit(t, t.call(nil))
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
		return m.fail(t, TurnUncancellable, "the wedged task could not be cancelled; stop it from the thread, or restart agent-kourier to reconcile it", err)
	}
	c.tries++
	return blocked, fmt.Errorf("cancel wedged task %s: %w", t.taskID, err)
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
	m.cfg.Logger.Warn("session turn failed", "key", t.key, "message", t.r.ID, "reason", why, "error", cause)
	if err := m.cfg.Store.MarkReplyDelivered(context.WithoutCancel(m.ctx), t.r.ID, m.cfg.Now()); err != nil {
		return ended, err
	}
	m.finish(t, class)
	return ended, nil
}

func (m *Manager) notice(t *tstate, kind NoticeKind, detail string) {
	switch kind {
	case NoticeWedged, NoticeDuplicateRun, NoticeRetrying:
		telemetry.Count(m.ctx, "agentkourier_session_recovery_total", string(kind))
	}
	m.cfg.Sink.Notice(m.ctx, t.key, Notice{ID: cmp.Or(t.nid, t.r.ID) + "/" + string(kind), Kind: kind, MessageRef: t.r.MessageRef, Detail: detail})
}

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

// message builds the A2A message of a turn: exactly one text part (R18), the queue's messageId
// and the session's contextId, which is empty on a first turn. A turn a person wrote carries
// who (D14): the user the platform resolves, else the ID alone, and the Binding's platform either way. The
// dialect puts it into that one part. A turn nobody wrote (an alert's prompt) carries none.
func (m *Manager) message(r store.Reply, sess store.Session, b Backend) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(r.Text))
	msg.ID, msg.ContextID = r.ID, sess.ContextID
	if r.UserID == SystemUser || b.Attribute == nil {
		return msg
	}
	user := dialect.User{ID: r.UserID}
	switch u, err := m.cfg.Users.ResolveUser(m.ctx, r.UserID); {
	case err != nil:
		m.cfg.Logger.Warn("resolve user for attribution, using the ID alone", "user", r.UserID, "error", err)
	default:
		user = u
		user.ID = r.UserID // the platform's answer never renames the author
	}
	user.Platform = b.Platform // the Binding's ChatConnection names it, whatever the lookup returned
	b.Attribute(msg, user)
	return msg
}

// errHeld marks a turn that is not sent until reconciliation lifts the hold: an expected state,
// so it is logged at Info.
var errHeld = errors.New("session: turn held")

// held refuses to send while a task of the session is recorded (kagent runs one at a time, and
// a turn resent into its context would run twice), and a turn whose acceptance could not be
// recorded. Reconciliation (FindTask first, spec 8.2) lifts both.
func (m *Manager) held(key Key, id string, sess store.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case sess.TaskID != "":
		return fmt.Errorf("task %s of the session is still active, not sending %s: %w", sess.TaskID, id, errHeld)
	case m.unrecorded[key] == id:
		return fmt.Errorf("the agent took turn %s but it was never recorded, not sending it again: %w", id, errHeld)
	}
	return nil
}

// release lifts the hold on turn id: it was reconciled.
func (m *Manager) release(key Key, id string) {
	m.mu.Lock()
	if m.unrecorded[key] == id {
		delete(m.unrecorded, key)
	}
	m.mu.Unlock()
}

// hold makes no wake send turn id again, until reconciliation lifts it.
func (m *Manager) hold(key Key, id string) {
	m.mu.Lock()
	m.unrecorded[key] = id
	m.mu.Unlock()
}

// recordAttempts bounds accept's retries, recordDelay spaces them.
const (
	recordAttempts = 3
	recordDelay    = 10 * time.Millisecond
)

// accept records that the agent took the turn (spec 8.2): the session gets its contextId and
// task, then the turn leaves the queue. In that order, so a crash in between leaves a task
// recovery can find, never one nobody knows about. A reply with no task (a Message, spec 6.3)
// records the contextId alone, and there is nothing for recovery to find: a crash after both writes
// and before the reply is posted loses the reply, and one between them sends the turn again. Both writes are idempotent, so the pair is
// retried, a bounded number of times. It does not rewrite the session row, so it cannot undo
// a Submit's touch.
func (m *Manager) accept(ctx context.Context, r store.Reply, ev agent.StateEvent, turn store.TaskTurn) (err error) {
	for i := range recordAttempts {
		if i > 0 {
			time.Sleep(recordDelay)
		}
		if err = m.cfg.Store.SetSessionTask(ctx, r.Key, ev.ContextID, string(ev.TaskID), turn); err == nil {
			if err = m.cfg.Store.MarkReplyDelivered(ctx, r.ID, m.cfg.Now()); err == nil {
				return nil
			}
		}
	}
	return err
}

// end clears the task of a conversation whose task ended, and touches the session, but only
// after reading it: an expired session is never touched back to life (D28).
func (m *Manager) end(ctx context.Context, key Key, ttl time.Duration) error {
	if err := m.cfg.Store.SetSessionTask(ctx, key, "", "", store.TaskTurn{}); err != nil {
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

func turnAttemptOutcome(out outcome, err error) string {
	if err != nil {
		return telemetry.ErrorClass(err)
	}
	switch out {
	case paused:
		return "input_required"
	case blocked:
		return "blocked"
	case idle:
		return "idle"
	default:
		return "ended"
	}
}

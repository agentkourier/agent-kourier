package session

import (
	"cmp"
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

// convo is a conversation that has a runner, which drains its queue one turn at a time (D21).
// Only the runner reads and writes tries and ttl; the Manager's mutex guards the rest.
type convo struct {
	again      bool              // a wake arrived while the runner was busy
	tries      int               // sends of the head turn that failed in a way to retry, in a row
	since      time.Time         // when the first of them failed, for SendGiveUp
	notified   bool              // the thread was told courier is retrying, for this run of trouble
	busyNoted  bool              // the thread was told the reply is queued behind a task nobody recorded
	lastSeen   time.Time         // when courier last saw NEW life of task life.task: the later of this and the status timestamp is its age (R17)
	life       life              // what courier already holds of that task, so a replayed snapshot is not life
	cancelFrom time.Time         // when the first attempt to cancel a wedged task was made, for SendGiveUp
	patient    bool              // the turn is never given up on: the agent may have taken it
	follow     *tstate           // the turn whose task is followed again, after its stream was lost
	paused     *tstate           // the turn whose task waits for input, while it does
	answers    chan *a2a.Message // answers to the paused task, for the parked runner to send
	ttl        time.Duration     // the Binding's threadTTL, for end
	parked     a2a.TaskID        // the task the runner waits on, while it waits for input
	released   bool              // the pause ended, and the runner has not yet moved on
	resume     chan struct{}     // tells the parked runner that the pause ended
}

// outcome is how one turn left the conversation: nothing queued, the task ended, or the task
// waits for input (spec 8.4), or cannot be sent yet.
type outcome int

const (
	idle outcome = iota
	ended
	paused
	blocked // the agent is busy with a task courier did not record, or the lookup failed
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
	c := &convo{resume: make(chan struct{}, 1), answers: make(chan *a2a.Message, 1)}
	m.convos[key] = c
	m.wg.Add(1)
	go m.run(key, c)
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
	c, running := m.convos[key], m.convos[key] != nil
	if m.closed {
		m.mu.Unlock()
		return ErrNotParked // the runners are gone, or going
	}
	if running {
		defer m.mu.Unlock()
		if taskID == "" || c.parked != taskID || c.released {
			return ErrNotParked
		}
		c.released = true
		c.resume <- struct{}{} // one release per park, and the channel holds one: never blocks
		return nil
	}
	m.mu.Unlock()
	sess, err := m.cfg.Store.GetSession(ctx, key, readAll, m.cfg.Now())
	if err != nil {
		return err
	}
	if sess.TaskID != string(taskID) || taskID == "" {
		return ErrNotParked // nothing recorded for that task
	}
	if err := m.cfg.Store.SetSessionTask(ctx, key, "", ""); err != nil {
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
	for m.ctx.Err() == nil {
		out, err := m.turn(key, c)
		for m.log(key, err); out == paused && m.ctx.Err() == nil; m.log(key, err) {
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

// log reports the error of a turn: a hold is expected and only informs.
func (m *Manager) log(key Key, err error) {
	switch {
	case errors.Is(err, errHeld):
		m.cfg.Logger.Info("session turn held", "key", key, "reason", err)
	case err != nil && m.ctx.Err() == nil:
		m.cfg.Logger.Error("session turn failed", "key", key, "error", err)
	}
}

// parked waits while the conversation's task waits for input, and returns what happened: the
// pause ended (cancelled, timed out: see endPause), so the task is cleared and the held replies
// go out; or an answer was sent, whose stream is followed to where it ends or pauses again.
func (m *Manager) parked(key Key, c *convo) (outcome, error) {
	select {
	case <-c.resume:
		m.mu.Lock()
		c.parked, c.released, c.paused = "", false, nil
		m.mu.Unlock()
		if err := m.end(context.WithoutCancel(m.ctx), key, c.ttl); err != nil {
			m.cfg.Logger.Error("end paused task", "key", key, "error", err)
		}
		return ended, nil
	case msg := <-c.answers:
		return m.answer(c, msg)
	case <-m.ctx.Done():
		return ended, nil
	}
}

// turn sends the oldest queued turn of key and follows it until its task ends or pauses.
// Store writes ignore a shutdown, so an accepted turn is never left unrecorded.
func (m *Manager) turn(key Key, c *convo) (outcome, error) {
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
		return ended, m.cfg.Store.MarkReplyDelivered(sctx, r.ID, m.cfg.Now())
	} else if err != nil {
		return idle, fmt.Errorf("session Binding %q: %w", sess.Binding, err)
	}
	c.ttl = backend.ThreadTTL
	t := &tstate{key: key, c: c, r: r, sess: sess, b: backend}
	send := func(h agent.Handler) error { return backend.Agent.Send(m.ctx, m.message(r, sess, backend), h) }
	hold := m.held(key, r.ID, sess)
	m.mu.Lock()
	prev := m.attempted[key]
	resend := prev.id == r.ID // sent before, and not known to have been taken
	m.attempted[key] = attempt{id: r.ID, unknown: resend && prev.unknown}
	m.mu.Unlock()
	if hold != nil || resend {
		// The agent may already have this turn, and a resend into an existing context is not
		// deduplicated, so look it up first, whatever failed the first time: found, it is followed
		// (a duplicate run is reported); absent, it is sent. A lookup that fails means nothing is
		// known, so nothing is sent (spec 8.2). A first turn has no context to look in, and its
		// resend is deduplicated.
		if sess.ContextID != "" {
			task, holders, err := backend.Agent.FindTask(m.ctx, sess.ContextID, r.ID)
			switch {
			case err != nil:
				c.patient = c.patient || hold != nil // a turn the agent took is never failed
				return m.retry(t, fmt.Errorf("look up turn %s: %w", r.ID, err))
			case task != nil:
				if len(holders) > 1 {
					m.duplicates(t, holders)
				}
				send = func(h agent.Handler) error { return backend.Agent.Follow(m.ctx, task.ID, h) }
			case sess.TaskID != "": // absent, and a task of the session runs: this turn is behind it
				return idle, hold
			}
		} else if sess.TaskID != "" {
			return idle, hold
		}
		m.release(key, r.ID) // reconciled: followed, or the agent never had it
	}
	return m.drive(t, send)
}

// settle forgets the retry state of the head turn: it was taken, failed for good, or ended.
func (c *convo) settle() {
	c.tries, c.since, c.patient, c.notified, c.busyNoted, c.cancelFrom = 0, time.Time{}, false, false, false, time.Time{}
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

// life is what courier holds of one task: its state, the text of each artifact, and the tool
// calls and results seen. An event that adds nothing to it (the replay of a snapshot) shows no
// sign that the task is alive.
type life struct {
	task  a2a.TaskID
	state a2a.TaskState
	text  map[a2a.ArtifactID]string
	tools map[string]bool
}

// renews records e, and reports whether it was new: a first look at a task, a change of state,
// text that differs from what was held, or a tool call or result not seen before.
func (l *life) renews(e agent.Event) bool {
	switch ev := e.(type) {
	case agent.StateEvent:
		if ev.TaskID != l.task {
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
	follow bool       // the attempt follows a task instead of sending
	answer bool       // the attempt answers the paused task
	nid    string     // what a notice of this attempt is named after, if not the turn
	taskID a2a.TaskID // the task the agent named, or the one followed or answered
	op     audit.Op   // the call audit rows name, when not a send
	key    Key
	c      *convo
	r      store.Reply
	sess   store.Session
	b      Backend
}

// drive runs one attempt at a turn, a Send or a Follow, streaming its events to the sink and
// recording the task as soon as the agent names it.
func (m *Manager) drive(t *tstate, send func(agent.Handler) error) (out outcome, err error) {
	key, c, r := t.key, t.c, t.r
	defer func() {
		if out != paused { // the pause event may have parked the runner before the stream failed
			m.unpark(c)
		}
	}()
	sctx := context.WithoutCancel(m.ctx)
	h := func(agent.Event) {}
	if m.cfg.Sink != nil {
		if sh := m.cfg.Sink.Turn(m.ctx, TurnRef{key, r.ID}); sh != nil {
			h = sh
		}
	}
	var final *a2a.Task
	var recordErr error
	named := false
	err = send(func(e agent.Event) {
		if c.life.renews(e) { // only what is new counts: a follow replays the snapshot it began with
			c.lastSeen = m.cfg.Now()
		}
		switch ev := e.(type) {
		case agent.StateEvent:
			t.taskID = ev.TaskID
			if !named { // the agent has named the task: record it before anyone renders it
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
				if recordErr = m.accept(sctx, r, ev); recordErr == nil {
					m.mu.Lock()
					delete(m.attempted, key) // taken and recorded: never a resend
					m.mu.Unlock()
				}
			}
		case agent.FinalEvent:
			final = ev.Task
			if !final.Status.State.Terminal() {
				// The pause is rendered inside this handler, and whoever renders it may end it at
				// once: the runner must already be parked on this task.
				m.mu.Lock()
				c.parked, c.paused = final.ID, t
				m.mu.Unlock()
			}
		}
		h(e)
	})
	if err == nil {
		c.follow = nil
	}
	switch {
	case recordErr != nil: // the agent has the turn and we cannot say so: never send it again
		m.hold(key, r.ID)
		return ended, fmt.Errorf("record accepted turn %s: %w", r.ID, recordErr)
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
		return ended, fmt.Errorf("send turn %s: the stream ended with no final task", r.ID)
	case !final.Status.State.Terminal():
		return paused, nil
	}
	return ended, m.end(sctx, key, c.ttl)
}

// answer sends an answer to the paused task and streams what follows to the sink, under the
// turn that paused (the renderer shows one message). The pause is over while it runs, so a
// second answer, or an endPause for the task, is dropped.
func (m *Manager) answer(c *convo, msg *a2a.Message) (outcome, error) {
	t := *c.paused
	m.mu.Lock()
	c.parked, c.released, c.paused = "", false, nil
	m.mu.Unlock()
	t.answer, t.nid, t.taskID, t.follow = true, msg.ID, msg.TaskID, false
	if msg.ContextID == "" {
		msg.ContextID = t.sess.ContextID
	}
	return m.drive(&t, func(h agent.Handler) error { return t.b.Agent.Answer(m.ctx, msg, h) })
}

// unanswered handles an answer that failed before the agent named a task again. A rejected one
// wedged its task, which Agent.Answer has already cancelled (R17). A busy one is read (see
// busyAnswer). Any other failure leaves the task paused, so the person can answer again.
func (m *Manager) unanswered(t *tstate, err error) (outcome, error) {
	call := audit.Call{Op: audit.OpAnswer, TaskID: t.taskID, ContextID: t.sess.ContextID, MessageID: t.nid, Err: err}
	switch {
	case errors.Is(err, agent.ErrAnswerRejected):
		m.notice(t, NoticeAnswerRejected, "")
		m.audit(t, call)
		return ended, m.end(context.WithoutCancel(m.ctx), t.key, t.c.ttl)
	case errors.Is(err, agent.ErrBusy):
		return m.busyAnswer(t)
	case errors.Is(err, agent.ErrCredentialsRejected):
		m.rejected(t, err)
	}
	m.notice(t, NoticeFailed, "the answer could not be delivered")
	return m.repark(t), nil
}

// busyAnswer reads the task an answer got -32004 for, which is busy, wedged, or ended: an
// ended one drops the answer; a WORKING one past maxWorkingAge is cancelled (R17); any other
// is busy with something else, and stays paused for another try.
func (m *Manager) busyAnswer(t *tstate) (outcome, error) {
	task, err := t.b.Agent.GetTask(m.ctx, t.taskID)
	switch {
	case err == nil && task.Status.State.Terminal():
		m.notice(t, NoticeAnswerDropped, "")
		return ended, m.end(context.WithoutCancel(m.ctx), t.key, t.c.ttl)
	case err == nil && running(task) && task.Status.Timestamp != nil && m.cfg.Now().Sub(*task.Status.Timestamp) > m.cfg.MaxWorkingAge:
		return m.wedged(t)
	}
	m.notice(t, NoticeFailed, "the agent is busy with the task, and did not take the answer")
	return m.repark(t), nil
}

// repark reopens the pause of the task an answer was not delivered to.
func (m *Manager) repark(t *tstate) outcome {
	m.mu.Lock()
	t.c.parked, t.c.released = t.taskID, false
	pt := *t
	pt.answer, pt.nid = false, ""
	t.c.paused = &pt
	m.mu.Unlock()
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
		t.c.follow = nil // the task stays recorded, for reconciliation
		m.notice(t, NoticeFailed, "the agent's front door refused the Binding's credentials")
		return ended, fmt.Errorf("follow turn %s: %w", t.r.ID, err)
	case errors.As(err, &sl):
		if t.c.since.IsZero() {
			t.c.since = m.cfg.Now()
		}
		t.c.follow, t.taskID, t.follow = t, sl.TaskID, true
		t.c.tries++
		return blocked, fmt.Errorf("follow turn %s: %w", t.r.ID, err)
	}
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
	m.notice(t, NoticeSessionGone, "") // its ID names the turn, so a repeat after a retry is harmless
	t.c.follow = nil
	t.c.settle()
	return ended, nil
}

// resume follows the task of a turn whose stream was lost. A task WORKING for longer than
// MaxWorkingAge (the agent's timestamp of the state, on courier's clock), or one whose state
// cannot be read for that long, is presumed wedged and cancelled (R17); otherwise the stream
// is picked up again.
func (m *Manager) resume(c *convo) (outcome, error) {
	t := c.follow
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
	return m.drive(t, func(h agent.Handler) error { return t.b.Agent.Follow(m.ctx, t.taskID, h) })
}

// wedgedByAge reports whether a WORKING task has gone too long without life (R17): it counts
// from the later of the task's status timestamp and the last event courier saw of it, so a
// healthy task that streamed for hours and then dropped is followed. A state with no timestamp
// counts from the last event courier saw, else from the loss; a timestamp in the future protects
// the task only until it has passed. What a follow replays from its snapshot is not new life.
func (m *Manager) wedgedByAge(c *convo, task *a2a.Task) bool {
	if !running(task) {
		return false
	}
	var alive time.Time // the latest sign of life: the status timestamp, or what courier saw
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
	t.op = audit.OpCancel
	defer func() { t.op = "" }()
	switch {
	case err == nil:
		m.audit(t, t.call(nil))
		m.notice(t, NoticeWedged, "")
		c.follow = nil
		c.settle()
		return ended, m.end(context.WithoutCancel(m.ctx), t.key, c.ttl)
	case errors.Is(err, a2a.ErrTaskNotFound):
		m.audit(t, t.call(err))
		return m.gone(t)
	case errors.Is(err, a2a.ErrTaskNotCancelable): // it ended in the meantime: show how
		m.audit(t, t.call(err))
		c.cancelFrom = time.Time{}
		return m.drive(t, func(h agent.Handler) error { return t.b.Agent.Follow(m.ctx, t.taskID, h) })
	case errors.Is(err, agent.ErrCredentialsRejected):
		return m.lost(t, err) // the identity Source writes the row
	case errors.Is(err, agent.ErrForbidden), m.cfg.Now().Sub(c.cancelFrom) >= m.cfg.SendGiveUp:
		m.audit(t, t.call(err))
		c.follow = nil
		return m.fail(t, "the wedged task could not be cancelled; stop it from the thread, or restart courier to reconcile it", err)
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
		return m.fail(t, "the agent's front door refused the Binding's credentials", err)
	case errors.Is(err, agent.ErrForbidden):
		m.audit(t, t.call(err))
		return m.fail(t, "the agent refused the Binding's identity for this session", err)
	case errors.Is(err, agent.ErrBusy): // some task of the session runs: try again later, never give up
		if !t.c.busyNoted { // Submit saw no task recorded, so it did not mark the reply queued: do it now
			t.c.busyNoted = true
			m.notice(t, NoticeQueued, "")
		}
		t.c.tries++
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

// retry waits and tries the head turn again. It gives up, failing the turn, after SendGiveUp of
// failures, unless the turn is one never to give up on. The first time, it tells the thread.
func (m *Manager) retry(t *tstate, err error) (outcome, error) {
	c := t.c
	if c.since.IsZero() {
		c.since = m.cfg.Now()
	}
	if !c.patient && m.cfg.Now().Sub(c.since) >= m.cfg.SendGiveUp {
		return m.fail(t, "the agent could not be reached", err)
	}
	c.tries++
	if !c.notified {
		c.notified = true
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
// is a real second call, with its own audit row: bounded, and never silent.
func (m *Manager) fail(t *tstate, why string, cause error) (outcome, error) {
	t.c.settle()
	m.mu.Lock()
	delete(m.attempted, t.key) // it will not be sent again
	m.mu.Unlock()
	m.notice(t, NoticeFailed, why)
	m.cfg.Logger.Warn("session turn failed", "key", t.key, "message", t.r.ID, "reason", why, "error", cause)
	return ended, m.cfg.Store.MarkReplyDelivered(context.WithoutCancel(m.ctx), t.r.ID, m.cfg.Now())
}

func (m *Manager) notice(t *tstate, kind NoticeKind, detail string) {
	if m.cfg.Sink != nil {
		m.cfg.Sink.Notice(m.ctx, t.key, Notice{ID: cmp.Or(t.nid, t.r.ID) + "/" + string(kind), Kind: kind, MessageTS: t.r.MessageTS, Detail: detail})
	}
}

// actor is who an audit row of this turn names.
func (t *tstate) actor() audit.Actor {
	a := audit.Actor{Binding: t.sess.Binding, Identity: t.b.Identity}
	if t.r.UserID != SystemUser {
		a.User = t.r.UserID
	}
	return a
}

func (t *tstate) call(err error) audit.Call {
	return audit.Call{Op: cmp.Or(t.op, audit.OpSend), TaskID: t.taskID, ContextID: t.sess.ContextID, MessageID: t.r.ID, Err: err}
}

// rejected passes a refused credential to the identity Source, which re-reads the token and
// writes the audit row. Its own failure is logged: it is where an operator learns the token
// cannot be read.
func (m *Manager) rejected(t *tstate, err error) {
	if t.b.Rejected == nil { // no identity Source to write the row: write it here, never drop it
		m.audit(t, t.call(err))
		return
	}
	if rerr := t.b.Rejected(context.WithoutCancel(m.ctx), t.actor(), t.call(err)); rerr != nil {
		m.cfg.Logger.Error("credential rejection bookkeeping", "key", t.key, "error", rerr)
	}
}

// audit records a call outcome the manager alone knows about. A failed write is logged, and
// never blocks the turn (spec 8.6 step 7).
func (m *Manager) audit(t *tstate, call audit.Call) {
	if m.cfg.Audit == nil {
		return
	}
	if aerr := m.cfg.Audit.AgentCall(context.WithoutCancel(m.ctx), t.actor(), call); aerr != nil {
		m.cfg.Logger.Error("audit write failed", "key", t.key, "error", aerr)
	}
}

// message builds the A2A message of a turn: exactly one text part (R18), the queue's messageId
// and the session's contextId, which is empty on a first turn. A turn a person wrote carries
// who (D14): the user the platform resolves, else the ID alone. The dialect puts it into that
// one part. A turn nobody wrote (an alert's prompt) carries none.
func (m *Manager) message(r store.Reply, sess store.Session, b Backend) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(r.Text))
	msg.ID, msg.ContextID = r.ID, sess.ContextID
	if r.UserID == SystemUser || b.Attribute == nil {
		return msg
	}
	user := dialect.User{ID: r.UserID}
	if m.cfg.Users != nil {
		switch u, err := m.cfg.Users.ResolveUser(m.ctx, r.UserID); {
		case err != nil:
			m.cfg.Logger.Warn("resolve user for attribution, using the ID alone", "user", r.UserID, "error", err)
		default:
			user = u
			user.ID = r.UserID // the platform's answer never renames the author
		}
	}
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
// recovery can find, never one nobody knows about. Both writes are idempotent, so the pair is
// retried, a bounded number of times. It does not rewrite the session row, so it cannot undo
// a Submit's touch.
func (m *Manager) accept(ctx context.Context, r store.Reply, ev agent.StateEvent) (err error) {
	for i := range recordAttempts {
		if i > 0 {
			time.Sleep(recordDelay)
		}
		if err = m.cfg.Store.SetSessionTask(ctx, r.Key, ev.ContextID, string(ev.TaskID)); err == nil {
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

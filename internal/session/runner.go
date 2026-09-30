package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// convo is a conversation that has a runner, which drains its queue one turn at a time (D21).
// Only the runner reads and writes tries and ttl; the Manager's mutex guards the rest.
type convo struct {
	again    bool          // a wake arrived while the runner was busy
	tries    int           // sends of the head turn that got -32004 or a failed lookup, in a row
	ttl      time.Duration // the Binding's threadTTL, for end
	parked   a2a.TaskID    // the task the runner waits on, while it waits for input
	released bool          // the pause ended, and the runner has not yet moved on
	resume   chan struct{} // tells the parked runner that the pause ended
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
	c := &convo{resume: make(chan struct{}, 1)}
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

// endPause tells the runner of key that the pause of taskID ended (answered and finished, timed
// out, or cancelled): it clears the task and sends what was held. A call that arrives while
// the runner is not parked on taskID, or repeats one already made, is dropped: it must never
// release the next pause. With no runner (after a restart), a call for the task recorded in
// the session clears it and starts one.
func (m *Manager) endPause(ctx context.Context, key Key, taskID a2a.TaskID) error {
	m.mu.Lock()
	c, running, closed := m.convos[key], m.convos[key] != nil, m.closed
	if running && taskID != "" && c.parked == taskID && !c.released {
		c.released = true
		c.resume <- struct{}{} // one release per park, and the channel holds one: never blocks
	}
	m.mu.Unlock()
	if running || closed {
		return nil
	}
	sess, err := m.cfg.Store.GetSession(ctx, key, readAll, m.cfg.Now())
	if err != nil || sess.TaskID != string(taskID) || taskID == "" {
		return err // nothing recorded for that task
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
		switch {
		case errors.Is(err, errHeld):
			m.cfg.Logger.Info("session turn held", "key", key, "reason", err)
		case err != nil && m.ctx.Err() == nil:
			m.cfg.Logger.Error("session turn failed", "key", key, "error", err)
		}
		switch {
		case out == paused:
			select {
			case <-c.resume:
				m.mu.Lock()
				c.parked, c.released = "", false
				m.mu.Unlock()
				if err := m.end(context.WithoutCancel(m.ctx), key, c.ttl); err != nil {
					m.cfg.Logger.Error("end paused task", "key", key, "error", err)
				}
			case <-m.ctx.Done():
			}
		case out == blocked:
			select {
			case <-time.After(m.backoff(c.tries)):
			case <-m.ctx.Done():
			}
		case (out == idle || err != nil) && m.retire(key):
			return
		}
	}
}

// turn sends the oldest queued turn of key and follows it until its task ends or pauses.
// Store writes ignore a shutdown, so an accepted turn is never left unrecorded.
func (m *Manager) turn(key Key, c *convo) (outcome, error) {
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
	if err := m.held(key, r.ID, sess); err != nil {
		return idle, err
	}
	c.ttl = backend.ThreadTTL
	send := func(h agent.Handler) error { return backend.Agent.Send(m.ctx, m.message(r, sess, backend), h) }
	m.mu.Lock()
	resend := m.attempted[key] == r.ID // sent before, and not known to have been taken
	m.attempted[key] = r.ID
	m.mu.Unlock()
	if resend && sess.ContextID != "" {
		// A resend into an existing context is not deduplicated by the agent, so look the turn up
		// first, whatever failed the first time: if the agent recorded it after all, follow that
		// task instead (spec 8.2).
		task, _, err := backend.Agent.FindTask(m.ctx, sess.ContextID, r.ID)
		if err != nil {
			c.tries++
			return blocked, fmt.Errorf("look up turn %s: %w", r.ID, err)
		}
		if task != nil {
			send = func(h agent.Handler) error { return backend.Agent.Follow(m.ctx, task.ID, h) }
		}
	}
	return m.drive(key, c, r, send)
}

// drive runs one attempt at a turn, a Send or a Follow, streaming its events to the sink and
// recording the task as soon as the agent names it.
func (m *Manager) drive(key Key, c *convo, r store.Reply, send func(agent.Handler) error) (outcome, error) {
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
	err := send(func(e agent.Event) {
		switch ev := e.(type) {
		case agent.StateEvent:
			if !named { // the agent has named the task: record it before anyone renders it
				named, c.tries = true, 0
				if recordErr = m.accept(sctx, r, ev); recordErr == nil {
					m.mu.Lock()
					delete(m.attempted, key) // taken and recorded: never a resend
					m.mu.Unlock()
				}
			}
		case agent.FinalEvent:
			final = ev.Task
		}
		h(e)
	})
	switch {
	case recordErr != nil: // the agent has the turn and we cannot say so: never send it again
		m.hold(key, r.ID)
		return ended, fmt.Errorf("record accepted turn %s: %w", r.ID, recordErr)
	case !named && errors.Is(err, agent.ErrBusy): // some task of the session runs: try again later
		c.tries++
		return blocked, fmt.Errorf("send turn %s: %w", r.ID, err)
	case err != nil:
		return ended, fmt.Errorf("send turn %s: %w", r.ID, err)
	case !named: // the stream ended clean but named no task: the agent may have run the turn
		m.hold(key, r.ID)
		return ended, fmt.Errorf("send turn %s: the agent named no task", r.ID)
	case final == nil:
		return ended, fmt.Errorf("send turn %s: the stream ended with no final task", r.ID)
	case !final.Status.State.Terminal():
		m.mu.Lock()
		c.parked = final.ID
		m.mu.Unlock()
		return paused, nil
	}
	return ended, m.end(sctx, key, c.ttl)
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

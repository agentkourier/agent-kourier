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
	"github.com/agentkourier/agent-kourier/internal/backoff"
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
	left          *tstate         // the head turn whose send's outcome is unknown: what it showed waits for its next attempt
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

// backoff is the pause before the next resend after tries in a row: RetryBase, doubling, up
// to RetryMax.
func (m *Manager) backoff(tries int) time.Duration {
	return backoff.Doubling(m.cfg.RetryBase, m.cfg.RetryMax, tries)
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
		case out == idle || err != nil:
			// A runner that retires takes over nothing more: what a send handed on is ended before it can retire, so that no later
			// runner's turn is started ahead of the end. One that goes on, for a wake came, tries the head turn again at once.
			m.mu.Lock()
			again := c.again
			m.mu.Unlock()
			if !again {
				m.endLeft(c)
			}
			if m.retire(key) {
				return
			}
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
	_, fresh := m.fresh[r.ID]
	m.mu.Unlock()
	if !fresh {
		// The intake writes the row before it marks the turn fresh, both under its lock: wait out an intake that
		// may be between the two, or a turn this process just queued is taken for one an earlier process sent.
		m.intake.Lock()
		m.intake.Unlock() //nolint:staticcheck // SA2001: an empty critical section is the point, a barrier
		m.mu.Lock()
		if _, fresh = m.fresh[r.ID]; !fresh { // an earlier process may have marked it queued, and nothing here knows
			m.marked[r.ID] = struct{}{} // so it is taken off when the turn is sent or ends, which does no harm if it is not there
		}
		m.mu.Unlock()
	}
	sess, err := m.cfg.Store.GetSession(sctx, key, store.AnyAge, m.cfg.Now())
	if err != nil {
		return idle, fmt.Errorf("read session: %w", err)
	}
	backend, err := m.sessionBackend(sess)
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
		msg := m.message(withMentionPrompt(m.withThreadContext(ctx, c, r, sess.Binding, backend), sess, backend), sess, backend)
		if err := testhooks.BeforeSend(ctx, msg.ID); err != nil {
			return err
		}
		return backend.Agent.Send(ctx, msg, h)
	}
	sends := true // false once the turn is found on the agent, to be followed and not sent
	hold := m.held(key, r.ID, sess)
	m.mu.Lock()
	prev := m.attempted[key]
	_, fresh = m.fresh[r.ID]
	// A turn this process sent before, or one it did not enqueue itself (a process that died may have
	// sent it), is a resend: looked up first, however the wake got here.
	resend := prev.id == r.ID || !fresh
	m.attempted[key] = attempt{id: r.ID, unknown: prev.id == r.ID && prev.unknown}
	m.mu.Unlock()
	// The first attempt here at a turn that an earlier process queued continues what that process may have left for it, which
	// no attempt here has taken over or ended: the renderer takes it over, or ends it. A later attempt here has only what this
	// process showed, which it ended itself, or handed on (convo.left).
	first := !fresh && prev.id != r.ID
	t.takes = first
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
				m.cfg.Logger.Warn("session turn not looked up: the agent cannot list tasks", "key", key, "binding", sess.Binding, "turn", r.ID)
				if sess.TaskID != "" { // a task of the session runs: this turn is behind it
					return idle, hold
				}
			case errors.Is(err, agent.ErrLookupIncomplete):
				// The agent can list tasks, but the list could not be read in full (a page is more than Agent Kourier reads at any
				// size it asked for, the walk took too long or ran on, or a server refused the size it was lowered to). It is not
				// asked again: a page that is too large or a token that comes again comes again, and a walk that took too long
				// may end sooner another time but nothing says it will. Whether the agent has this turn is not known.
				if sess.TaskID != "" { // a task of the session runs: this turn is behind it, and asked again when that ends
					return idle, hold
				}
				if fresh && !prev.unknown && hold == nil {
					// This process queued the turn, and no attempt of its left the agent's answer unknown: every one was refused as
					// busy or not ready (-32004), or never left (not sent; held behind a task, as a turn never sent is). The agent does
					// not have it, so there is nothing to find, and sending it is no blind resend. Any other failure before a task
					// was named (a JSON-RPC error such as -32603, say) is recorded as unknown by unsent, a turn that an earlier
					// process queued is not fresh, and one the agent took and that was not recorded (hold) is not this either.
					m.cfg.Logger.Warn("session turn not looked up: the agent's list of tasks could not be read in full, and the agent has not taken the turn", "key", key, "binding", sess.Binding, "turn", r.ID, "error", err)
					break
				}
				if first && c.left == nil {
					t.handed, c.left = true, t // as a failed lookup does: what an earlier process left is ended with the turn
				}
				return m.unverified(t, err)
			case err != nil:
				c.patient = c.patient || hold != nil // a turn the agent took is never failed
				if first && c.left == nil {
					// An earlier process may have left a message that no attempt here has taken over: a turn given up on before
					// it is sent or followed ends it (finish).
					t.handed, c.left = true, t
				}
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

// wantsThreadContext reports whether the turn r takes the thread's context (Config.ThreadContext): a turn marked to carry the
// whole thread, or a person's turn in a Binding with chat.threadReplies: mention. A reply that was admitted as an answer to a
// question is no turn (Queued).
func (m *Manager) wantsThreadContext(r store.Reply, b Backend) bool {
	return Queued(r).Kind == AdmitTurn && (r.WholeThread || (b.ThreadReplies == v1alpha1.ThreadRepliesMention && r.UserID != SystemUser))
}

// readWindow finds where the window of the turn r opens, which is the message of the row before it in the session's
// queue, delivered or not, and keeps it for the turn. A turn that carries the thread reads back to the last that did: a turn
// between them carried no bot's post. It is read from stored rows, so it is the same on every attempt,
// after a restart and after a failover. A turn with no row before it has no window. It runs before the turn is sent,
// where a store that fails waits like any other failure of the runner: the turn is never sent without its context,
// which it could not be sent again with, and nothing tells the thread the agent cannot be reached.
func (m *Manager) readWindow(ctx context.Context, c *convo, r store.Reply, b Backend) error {
	if !m.wantsThreadContext(r, b) || c.window.id == r.ID {
		return nil
	}
	previous := m.cfg.Store.PreviousReply
	if r.WholeThread {
		previous = m.cfg.Store.PreviousWholeReply
	}
	prev, err := previous(ctx, r.Key, r.Sequence)
	switch {
	case errors.Is(err, store.ErrNotFound): // no turn came before it, or none that carried the thread
		c.window = windowText{id: r.ID, none: !r.WholeThread} // a turn that carries the thread has all of it to read
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
		c.window.section, c.window.asked = m.cfg.ThreadContext(ctx, r.Key, binding, c.window.after, r.MessageRef, r.WholeThread), true
	}
	if c.window.section != "" {
		r.Text += "\n\n" + c.window.section
	}
	return r
}

// withMentionPrompt returns the turn r with the line that tells the agent the names it may mention after everything else, on
// a session's first turn (one text part, as R18 has it): the agent keeps it from there, so the later turns do not say it
// again. It is Agent Kourier's own text, after any untrusted block. A Binding that declares nobody has no line.
func withMentionPrompt(r store.Reply, sess store.Session, b Backend) store.Reply {
	if b.MentionPrompt != "" && sess.ContextID == "" {
		r.Text += "\n\n" + b.MentionPrompt
	}
	return r
}

// superseded reports whether the session no longer records the task t, which Recover read some
// time ago: following it now would render its end again, and clear whatever the session holds
// by then. A read that fails does not drop the task: following it is the safe side.
func (m *Manager) superseded(t *tstate) bool {
	sess, err := m.cfg.Store.GetSession(context.WithoutCancel(m.ctx), t.key, store.AnyAge, m.cfg.Now())
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
	said  string // the working update last seen: its ID and text
}

// renews records e, and reports whether it was new: a first look at a task, a change of state,
// text that differs from what was held, a tool call or result not seen before, or a live working
// update (agent.ProgressEvent, which a snapshot never makes), unless it opens a stream and is the one last seen: a follow reads
// the task afresh, so a status that a server repeats to each follow comes as an event each time. Later on a stream an update is
// the agent at work, whatever it says. A reply with no task (a Message) is a
// first look each time, so no reply's text outlives its turn.
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
	case agent.ProgressEvent:
		said := ev.ID + "\x00" + ev.Text
		old := l.said
		l.said = said
		return !ev.Opening || said != old
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
	handed    bool            // the last drive ended with the task followed again, or the turn tried again after an unknown outcome: what it showed waits for the next attempt's sink turn
	takes     bool            // the attempt continues what an earlier attempt of the turn may have shown: a resend, or what convo.left handed on
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
	mention := cmp.Or(t.began.Mention, t.startOf()) // an answer leaves it: the task is still the start's, whoever's message the turn names now
	return store.TaskTurn{ID: cmp.Or(t.nid, t.began.ID, t.r.ID), User: cmp.Or(ack.UserID, t.r.UserID, t.began.User), MessageRef: cmp.Or(ack.MessageRef, t.r.MessageRef, t.began.MessageRef), Acknowledgment: ackEmoji, AcknowledgmentID: ackID, Mention: mention}
}

// startOf is the message of the trigger's start that the turn r is, for a start that holds its place in the trigger's limits for
// its own turn alone (store.TaskTurn.Mention): a bot's mention, which carries the thread, or the person's message at the root
// of a session that a personMessage trigger began. Any other turn, an alert's included, is none, and says "".
func (t *tstate) startOf() string {
	if t.r.WholeThread || (t.sess.Origin.Source == store.OriginPersonMessage && t.r.MessageRef == t.key.Thread) {
		return t.r.MessageRef
	}
	return ""
}

// sinkRef is the TurnRef the sink is given for this attempt. An answer's output is a message of its own, named after the
// answer: it must not overwrite the output of the turn that paused. A task found again after a restart, followed again
// after a lost stream, or looked up or sent again after a send of unknown outcome, continues the message it had.
func (t *tstate) sinkRef() TurnRef {
	shown := t.outputTurn()
	return TurnRef{Key: t.key, Binding: t.sess.Binding, MessageID: shown.ID, UserID: shown.User, Output: t.b.Output, Progress: t.b.Progress, Recovered: t.recovered || t.follow || t.takes, Restarted: t.recovered, Feedback: feedbackOf(shown, t.b), ThreadReplies: t.b.ThreadReplies, Mentions: t.b.Mentions, MaxMentions: t.b.MaxMentions}
}

// feedbackOf is the feedback buttons the final message of a turn carries (TurnRef.Feedback): an investigation's always, and an
// answer's on a Binding with feedback.enabled. A turn nobody wrote is an investigation. A bot's mention is also nobody's, but it is a
// question put by another agent: it is answered like a person's, which the recorded turn can tell after a restart too.
func feedbackOf(turn store.TaskTurn, b Backend) store.FeedbackKind {
	switch {
	case turn.User == SystemUser && turn.Mention == "":
		return store.FeedbackInvestigation
	case turn.User != "" && b.Feedback:
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
	// The send's context ends with the manager. The sink's context ends when the drive returns, and
	// only then: a turn that ends with no FinalEvent frees what the sink started for it, with
	// ErrTurnContinues as the cause when the task goes on (a lost stream, followed again, a send of
	// unknown outcome, looked up and followed or sent again, or a shutdown, after which recovery
	// follows it), so that the renderer leaves the message to the later turn rather than ending it.
	// A sink that must settle a finished turn does so on a context of its own (the renderer's
	// bounded flush).
	tctx, endSend := context.WithCancel(parentCtx)
	defer endSend()
	sinkCtx, endTurn := context.WithCancelCause(context.WithoutCancel(parentCtx))
	defer func() {
		var cause error
		// A blocked send that is tried again goes on only while the agent may have the turn: an outcome that was unknown
		// stays so through a resend that never left. The next attempt, a follow or a resend, is given the turn recovered and
		// takes the message over, or ends it. A turn the agent never had (not sent, busy, not ready) is ended.
		m.mu.Lock()
		unknown := m.attempted[key].id == r.ID && m.attempted[key].unknown
		m.mu.Unlock()
		t.handed = out == blocked && (t.follow || unknown)
		if t.handed && !t.follow { // a follow's own resume ends what it was handed
			c.left = t
		}
		if t.handed || m.ctx.Err() != nil {
			cause = ErrTurnContinues
		}
		endTurn(cause)
	}()
	if l := c.left; l != nil { // an earlier attempt handed its message on
		c.left = nil
		if l.outputTurn().ID == t.outputTurn().ID {
			t.takes = true
		} else {
			m.endHanded(l)
		}
	}
	if sh := m.cfg.Sink.Turn(sinkCtx, t.sinkRef()); sh != nil {
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
	switch u, err := m.cfg.Users.ResolveUser(m.ctx, r.Key.Connection, r.UserID); {
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

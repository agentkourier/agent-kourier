package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// recoveredPrefix starts the message ID recovery gives a task whose turn it cannot name: no turn
// of a person or an alert may begin with it.
const recoveredPrefix = "task:"

// SystemUser is the user a turn is filed under when nobody wrote it (an alert's first
// prompt): the store requires one.
const SystemUser = "courier:system"

// Disposition says what Submit did with a turn.
type Disposition string

// The dispositions of a Submit. Created and Accepted both mean the turn is queued. A
// redelivery after a partial failure finds the session the first attempt made, and the
// mention/message pair may split the same way, so a turn that created the session can be
// reported as Accepted: never hang a one-time side effect on Created.
const (
	Created     Disposition = "created"      // a new session was made and the turn queued on it
	Accepted    Disposition = "accepted"     // the session is live and the turn queued on it
	NotOwned    Disposition = "not_owned"    // courier owns no such thread: ignore the turn (spec 8.2)
	BindingGone Disposition = "binding_gone" // owned, but its Binding left the config: nothing written; the caller posts the notice
	Expired     Disposition = "expired"      // owned but past threadTTL: nothing written; the caller posts the notice
	// Unmentioned is a reply the Binding does not take: owned, but the Binding's threadReplies is mention
	// and the reply does not mention courier (D38). Nothing is written or queued, the session is not
	// touched (threadTTL is not extended), and nobody is told, whether the thread is live or expired. A typed
	// answer to a pending question that takes text replies is no such reply: it is Accepted, queued pinned to the
	// question.
	Unmentioned Disposition = "unmentioned"
)

// Inbound is one turn from a person or an alert.
type Inbound struct {
	Key Key
	// Binding is used only to create a session: an existing one keeps the Binding it was
	// created under (D14, 8.5).
	Binding    string
	MessageID  string // the turn's A2A messageId and queue ID; keep it across redeliveries
	MessageRef string // the platform's ID of the message, for the reaction on a queued reply
	UserID     string // the platform's ID of the user who wrote the turn; empty when nobody did
	Text       string
	// Mentioned says the turn mentions courier, or is in a direct message, where no mention is needed:
	// the adapter decides. A Binding whose threadReplies is mention takes a reply in a thread it owns
	// only if this is set (or Create is).
	Mentioned bool
	// Create lets the turn make the session if there is none: an @mention, a DM, an alert's
	// first prompt. A plain thread reply leaves it false.
	Create bool
	// Alert is the name of the alert the thread is started for, which the session keeps for the feedback on its
	// messages (spec 8.7). Like Binding it is used only to create a session. The caller has bounded it: it is
	// text out of an alert.
	Alert string
}

// Result is the outcome of a Submit. Queued means the turn has to wait for a task that is
// active, or for turns ahead of it: the caller marks it with a reaction (the sink is told too).
type Result struct {
	Disposition Disposition
	Queued      bool
}

// readAll is a threadTTL no session outlives: the real one is the session's Binding's.
const readAll = 100 * 365 * 24 * time.Hour

// Submit is the durable intake of a turn, called by the chat adapter before it acknowledges
// the event (spec 6.1), and by the alert path. It decides who owns
// the thread, writes the session row for a new one, and queues the turn (idempotently, by
// MessageID), all before it wakes the runner, so a crash loses nothing and the redelivered
// event repeats what is left. It does not wait for the agent. An expired session is
// returned as Expired and left as it was, and a reply that a mention-only Binding does not
// take as Unmentioned, with nothing written.
func (m *Manager) Submit(ctx context.Context, in Inbound) (Result, error) {
	if in.Key.Connection == "" || in.Key.Channel == "" || in.Key.Thread == "" || in.MessageID == "" || in.MessageRef == "" || in.Text == "" || in.UserID == SystemUser || strings.HasPrefix(in.MessageID, recoveredPrefix) || (in.Create && in.Binding == "") {
		return Result{}, fmt.Errorf("submit %q: the key, MessageID, MessageRef and Text are required, and a Binding to create; UserID is never %q, and MessageID never starts %q: %w", in.MessageID, SystemUser, recoveredPrefix, store.ErrInvalid)
	}
	res, err := m.queue(ctx, in)
	if err != nil || (res.Disposition != Created && res.Disposition != Accepted) {
		return res, err
	}
	// Both outside the intake lock: a slow wake or notice must not hold up other turns. The notice comes first, so
	// that the runner never sends the turn, and takes its reaction off, before the reaction was asked for (the
	// sink is made to cope with that order too, but this is the cheap half of it).
	if res.Queued && m.markPending(in.MessageID) {
		m.cfg.Sink.Notice(ctx, in.Key, Notice{ID: queuedID(in.MessageID), Kind: NoticeQueued, MessageRef: in.MessageRef})
	}
	m.wake(in.Key)
	return res, nil
}

// queuedID is the ID of the notice that asks for the queued reaction of the turn id, and of the one that
// takes it off.
func queuedID(id string) string { return id + "/queued" }

// markPending remembers that the turn id may carry the queued reaction (see Manager.marked), and reports
// whether the reaction is still wanted: it is while this process has the turn to send (fresh). A turn that was
// sent in the meantime, or that a duplicate of its event names after it ended, is not waiting, and must not be
// marked, for nothing would ever unmark it.
func (m *Manager) markPending(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, pending := m.fresh[id]; !pending {
		return false
	}
	m.marked[id] = struct{}{}
	return true
}

// queue is Submit's durable half, under the intake lock. A live session is touched before the
// turn is queued, so the decision that it is live sticks: if the enqueue then fails, or the
// process dies between the two writes, the retry may come after threadTTL and must still
// find the session live (a touch nobody used is harmless).
func (m *Manager) queue(ctx context.Context, in Inbound) (Result, error) {
	m.intake.Lock()
	defer m.intake.Unlock()
	now := m.cfg.Now()
	res, pin, err := m.admit(ctx, in, now)
	if err != nil || (res.Disposition != Created && res.Disposition != Accepted) {
		return res, err
	}
	user := in.UserID
	if user == "" {
		user = SystemUser
	}
	// A duplicate, or a turn already delivered, is not queued again, and still wakes the runner.
	parent, state := telemetry.Capture(ctx)
	enqueued, err := m.cfg.Store.EnqueueReply(ctx, store.Reply{ID: in.MessageID, Key: in.Key, UserID: user, MessageRef: in.MessageRef, InteractionID: pin, Text: in.Text, EnqueuedAt: now, TraceParent: parent, TraceState: state})
	if err != nil {
		return Result{}, fmt.Errorf("queue turn %s: %w", in.MessageID, err)
	}
	if !enqueued {
		telemetry.Count(ctx, "courier_intake_total", "reply_queue", "duplicate")
	}
	if enqueued { // this process made the row, so nothing has sent the turn yet
		m.mu.Lock()
		m.fresh[in.MessageID] = struct{}{}
		m.mu.Unlock()
	}
	// It waits if turns are ahead of it, or a task of the session is active or paused. This is
	// advisory: it races the runner, which may take the turn a moment later.
	head, herr := m.cfg.Store.NextReply(ctx, in.Key)
	sess, serr := m.cfg.Store.GetSession(ctx, in.Key, readAll, now)
	res.Queued = (herr == nil && head.ID != in.MessageID) || (serr == nil && sess.TaskID != "")
	return res, nil
}

// admit reads the session and, for a new thread that may be created, writes its row, or, for
// a live one, touches it. A reply the Binding does not take (threadReplies mention, no mention)
// is returned Unmentioned without a touch, unless it is the answer to a question that takes text replies:
// that one is Accepted, and pin is the question, which the reply is enqueued pinned to (D38).
func (m *Manager) admit(ctx context.Context, in Inbound, now time.Time) (Result, string, error) {
	sess, err := m.cfg.Store.GetSession(ctx, in.Key, readAll, now)
	switch {
	case errors.Is(err, store.ErrNotFound) && !in.Create:
		return Result{Disposition: NotOwned}, "", nil
	case errors.Is(err, store.ErrNotFound):
		backend, err := m.cfg.Bindings.Backend(in.Binding)
		if err != nil {
			return Result{}, "", fmt.Errorf("create session %+v under Binding %q: %w", in.Key, in.Binding, err)
		}
		// Before the turn is queued or anything is sent (spec 8.2); the agent's first
		// response fills in the contextId.
		if err := m.cfg.Store.UpsertSession(ctx, store.Session{Key: in.Key, Binding: in.Binding, Alert: in.Alert, Agent: backend.AgentName, LastActivity: now}); err != nil {
			return Result{}, "", fmt.Errorf("create session %+v: %w", in.Key, err)
		}
		return Result{Disposition: Created}, "", nil
	case err != nil:
		return Result{}, "", fmt.Errorf("read session %+v: %w", in.Key, err)
	}
	backend, err := m.cfg.Bindings.Backend(sess.Binding)
	if errors.Is(err, ErrUnknownBinding) { // expiry cannot be judged without it, so old or not
		return Result{Disposition: BindingGone}, "", nil
	} else if err != nil {
		return Result{}, "", fmt.Errorf("session %+v: Binding %q: %w", in.Key, sess.Binding, err)
	}
	// In mention mode a reply that does not mention courier is not taken (D38). A Create turn is no reply:
	// a mention, a DM, a trigger's start and an alert's prompt are held to nothing.
	gated := backend.ThreadReplies == v1alpha1.ThreadRepliesMention && !in.Mentioned && !in.Create
	// The store owns the expiry rule: ask it again, with the session's own threadTTL.
	switch _, err := m.cfg.Store.GetSession(ctx, in.Key, backend.ThreadTTL, now); {
	case errors.Is(err, store.ErrExpired) && gated:
		return m.unmentioned(ctx, sess.Binding), "", nil // no expiry notice for a reply that was not asking for anything
	case errors.Is(err, store.ErrExpired):
		return Result{Disposition: Expired}, "", nil
	case err != nil:
		return Result{}, "", fmt.Errorf("read session %+v: %w", in.Key, err)
	}
	pin := ""
	if gated {
		// Not a turn, but possibly an answer: a reply in a thread whose agent waits on a question that takes text
		// replies is that question's, mention or not, and is queued pinned to it, so that it is decided like a
		// button's press and can never be sent as a turn. Only a dialect that has such questions is asked.
		if !backend.TextReplies {
			return m.unmentioned(ctx, sess.Binding), "", nil
		}
		q, err := m.pendingTextQuestion(ctx, in.Key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return m.unmentioned(ctx, sess.Binding), "", nil
		case err != nil:
			return Result{}, "", fmt.Errorf("read the pending question of %+v: %w", in.Key, err)
		// A question stays pending until it is answered, refused or swept at its deadline, whether or not the agent
		// still waits on it: one whose task is not the session's (the pause ended, or a new task began) takes no
		// answer, and neither does one whose prompt is not posted (nobody has seen it, and a reply pinned to it
		// would hold the queue until it is). What is typed meanwhile is chatter.
		case q.TaskID != sess.TaskID || q.MessageRef == "":
			return m.unmentioned(ctx, sess.Binding), "", nil
		}
		pin = q.ID
	}
	if err := m.cfg.Store.TouchSession(ctx, in.Key, now); err != nil {
		return Result{}, "", fmt.Errorf("touch session %+v: %w", in.Key, err)
	}
	return Result{Disposition: Accepted}, pin, nil
}

// unmentioned is the result for a reply the Binding's mention mode turns away. It writes nothing; it counts the
// reply for the Binding, which is how a thread that people talk in without waking the agent shows up.
func (m *Manager) unmentioned(ctx context.Context, binding string) Result {
	telemetry.Count(ctx, "courier_thread_unmentioned_total", binding)
	return Result{Disposition: Unmentioned}
}

// pendingTextQuestion is the question of the thread that a typed reply answers, or store.ErrNotFound.
func (m *Manager) pendingTextQuestion(ctx context.Context, key Key) (store.Interaction, error) {
	return m.cfg.Questions.PendingTextQuestion(ctx, key)
}

package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
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
	// Create lets the turn make the session if there is none: an @mention, a DM, an alert's
	// first prompt. A plain thread reply leaves it false.
	Create bool
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
// returned as Expired and left as it was.
func (m *Manager) Submit(ctx context.Context, in Inbound) (Result, error) {
	if in.Key.Connection == "" || in.Key.Channel == "" || in.Key.Thread == "" || in.MessageID == "" || in.MessageRef == "" || in.Text == "" || in.UserID == SystemUser || strings.HasPrefix(in.MessageID, recoveredPrefix) || (in.Create && in.Binding == "") {
		return Result{}, fmt.Errorf("submit %q: the key, MessageID, MessageRef and Text are required, and a Binding to create; UserID is never %q, and MessageID never starts %q: %w", in.MessageID, SystemUser, recoveredPrefix, store.ErrInvalid)
	}
	res, err := m.queue(ctx, in)
	if err != nil || (res.Disposition != Created && res.Disposition != Accepted) {
		return res, err
	}
	m.wake(in.Key) // outside the intake lock: a slow wake must not hold up other turns
	if res.Queued && m.cfg.Sink != nil {
		m.cfg.Sink.Notice(ctx, in.Key, Notice{ID: in.MessageID + "/queued", Kind: NoticeQueued, MessageRef: in.MessageRef})
	}
	return res, nil
}

// queue is Submit's durable half, under the intake lock. A live session is touched before the
// turn is queued, so the decision that it is live sticks: if the enqueue then fails, or the
// process dies between the two writes, the retry may come after threadTTL and must still
// find the session live (a touch nobody used is harmless).
func (m *Manager) queue(ctx context.Context, in Inbound) (Result, error) {
	m.intake.Lock()
	defer m.intake.Unlock()
	now := m.cfg.Now()
	res, err := m.admit(ctx, in, now)
	if err != nil || (res.Disposition != Created && res.Disposition != Accepted) {
		return res, err
	}
	user := in.UserID
	if user == "" {
		user = SystemUser
	}
	// A duplicate, or a turn already delivered, is not queued again, and still wakes the runner.
	enqueued, err := m.cfg.Store.EnqueueReply(ctx, store.Reply{ID: in.MessageID, Key: in.Key, UserID: user, MessageRef: in.MessageRef, Text: in.Text, EnqueuedAt: now})
	if err != nil {
		return Result{}, fmt.Errorf("queue turn %s: %w", in.MessageID, err)
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
// a live one, touches it.
func (m *Manager) admit(ctx context.Context, in Inbound, now time.Time) (Result, error) {
	sess, err := m.cfg.Store.GetSession(ctx, in.Key, readAll, now)
	switch {
	case errors.Is(err, store.ErrNotFound) && !in.Create:
		return Result{Disposition: NotOwned}, nil
	case errors.Is(err, store.ErrNotFound):
		if _, err := m.cfg.Bindings.Backend(in.Binding); err != nil {
			return Result{}, fmt.Errorf("create session %+v under Binding %q: %w", in.Key, in.Binding, err)
		}
		// Before the turn is queued or anything is sent (spec 8.2); the agent's first
		// response fills in the contextId.
		if err := m.cfg.Store.UpsertSession(ctx, store.Session{Key: in.Key, Binding: in.Binding, LastActivity: now}); err != nil {
			return Result{}, fmt.Errorf("create session %+v: %w", in.Key, err)
		}
		return Result{Disposition: Created}, nil
	case err != nil:
		return Result{}, fmt.Errorf("read session %+v: %w", in.Key, err)
	}
	backend, err := m.cfg.Bindings.Backend(sess.Binding)
	if errors.Is(err, ErrUnknownBinding) { // expiry cannot be judged without it, so old or not
		return Result{Disposition: BindingGone}, nil
	} else if err != nil {
		return Result{}, fmt.Errorf("session %+v: Binding %q: %w", in.Key, sess.Binding, err)
	}
	// The store owns the expiry rule: ask it again, with the session's own threadTTL.
	switch _, err := m.cfg.Store.GetSession(ctx, in.Key, backend.ThreadTTL, now); {
	case errors.Is(err, store.ErrExpired):
		return Result{Disposition: Expired}, nil
	case err != nil:
		return Result{}, fmt.Errorf("read session %+v: %w", in.Key, err)
	}
	if err := m.cfg.Store.TouchSession(ctx, in.Key, now); err != nil {
		return Result{}, fmt.Errorf("touch session %+v: %w", in.Key, err)
	}
	return Result{Disposition: Accepted}, nil
}

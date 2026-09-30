package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// SystemUser is the user a turn is filed under when nobody wrote it (an alert's first
// prompt): the store requires one.
const SystemUser = "courier:system"

// Disposition says what Submit did with a turn.
type Disposition string

// The dispositions of a Submit.
const (
	Created  Disposition = "created"   // a new session was made and the turn queued on it
	Accepted Disposition = "accepted"  // the session is live and the turn queued on it
	NotOwned Disposition = "not_owned" // courier owns no such thread: ignore the turn (spec 8.2)
	Expired  Disposition = "expired"   // owned but past threadTTL: nothing written; the caller posts the notice
)

// Inbound is one turn from a person or an alert.
type Inbound struct {
	Key Key
	// Binding is used only to create a session: an existing one keeps the Binding it was
	// created under (D14, 8.5).
	Binding   string
	MessageID string // the turn's A2A messageId and queue ID; keep it across redeliveries
	MessageTS string // the Slack ts of the message, for the reaction on a queued reply
	UserID    string // the Slack user who wrote the turn; empty when nobody did
	Text      string
	// Create lets the turn make the session if there is none: an @mention, a DM, an alert's
	// first prompt. A plain thread reply leaves it false.
	Create bool
}

// Result is the outcome of a Submit.
type Result struct{ Disposition Disposition }

// readAll is a threadTTL no session outlives: the real one is the session's Binding's.
const readAll = 100 * 365 * 24 * time.Hour

// Submit is the durable intake of a turn, called from the queue handler. It decides who owns
// the thread, writes the session row for a new one, and queues the turn (idempotently, by
// MessageID), all before it wakes the runner, so a crash loses nothing and the redelivered
// event repeats what is left. It does not wait for the agent. An expired session is
// returned as Expired and left as it was.
func (m *Manager) Submit(ctx context.Context, in Inbound) (Result, error) {
	if in.Key.Connection == "" || in.Key.Channel == "" || in.Key.ThreadTS == "" || in.MessageID == "" || in.MessageTS == "" || in.Text == "" || (in.Create && in.Binding == "") {
		return Result{}, fmt.Errorf("submit %q: the key, MessageID, MessageTS and Text are required, and a Binding to create: %w", in.MessageID, store.ErrInvalid)
	}
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
	if _, err := m.cfg.Store.EnqueueReply(ctx, store.Reply{ID: in.MessageID, Key: in.Key, UserID: user, MessageTS: in.MessageTS, Text: in.Text, EnqueuedAt: now}); err != nil {
		return Result{}, fmt.Errorf("queue turn %s: %w", in.MessageID, err)
	}
	if res.Disposition == Accepted {
		if err := m.cfg.Store.TouchSession(ctx, in.Key, now); err != nil {
			return Result{}, fmt.Errorf("touch session %+v: %w", in.Key, err)
		}
	}
	m.wake(in.Key)
	return res, nil
}

// admit reads the session and, for a new thread that may be created, writes its row.
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
	if err != nil {
		return Result{}, fmt.Errorf("session %+v: Binding %q: %w", in.Key, sess.Binding, err)
	}
	// The store owns the expiry rule: ask it again, with the session's own threadTTL.
	switch _, err := m.cfg.Store.GetSession(ctx, in.Key, backend.ThreadTTL, now); {
	case errors.Is(err, store.ErrExpired):
		return Result{Disposition: Expired}, nil
	case err != nil:
		return Result{}, fmt.Errorf("read session %+v: %w", in.Key, err)
	}
	return Result{Disposition: Accepted}, nil
}

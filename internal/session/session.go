// Package session is courier's session manager: it maps a chat conversation to an agent
// session, queues the thread's replies durably, and runs one turn at a time against the
// agent (spec 8.2, 8.3, D21, D28). The durable intake here never talks to the agent.
package session

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// Key is the conversation key: (connection, channel, thread_ts).
type Key = store.ConversationKey

// ErrUnknownBinding means the config has no Binding of that name: a new session named one
// that does not exist, or an existing session's Binding was removed.
var ErrUnknownBinding = errors.New("session: unknown Binding")

// Store is what the manager persists through.
type Store interface {
	store.Sessions
	store.ReplyQueue
}

// Backend is what the manager needs to know about one Binding.
type Backend struct {
	// ThreadTTL is how long after its last activity a thread keeps its session (D28).
	ThreadTTL time.Duration
}

// Bindings resolves a Binding by its namespace/name.
type Bindings interface {
	// Backend returns the Binding's backend, or an error wrapping ErrUnknownBinding.
	Backend(name string) (Backend, error)
}

// Config configures a Manager. Store and Bindings are required; Now defaults to time.Now.
type Config struct {
	Store    Store
	Bindings Bindings
	Now      func() time.Time
}

// Manager owns the sessions of every conversation. It is safe for concurrent use.
type Manager struct {
	cfg  Config
	wake func(Key) // tells the runner that Key has a turn waiting
	// intake serializes Submit: UpsertSession replaces a whole row, so two Submits that
	// both found no session must not both write one.
	intake sync.Mutex
}

// New returns a Manager over cfg.
func New(cfg Config) (*Manager, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("session: Store is required")
	case cfg.Bindings == nil:
		return nil, errors.New("session: Bindings is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{cfg: cfg, wake: func(Key) {}}, nil
}

// SlackMessageID derives the A2A messageId of a Slack message from where it is, not from
// the event that delivered it: an @mention arrives as two events with different IDs, and
// both must be one turn (spec 8.2). An alert's first prompt passes its own stable ID.
func SlackMessageID(connection, channel, ts string) string {
	return fmt.Sprintf("slack:%s:%s:%s", connection, channel, ts)
}

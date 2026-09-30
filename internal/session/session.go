// Package session is courier's session manager: it maps a chat conversation to an agent
// session, queues the thread's replies durably, and runs one turn at a time against the
// agent (spec 8.2, 8.3, D21, D28). The durable intake here never talks to the agent.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
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
	store.SessionTasks
}

// AgentAPI is the part of *agent.Agent the manager uses.
type AgentAPI interface {
	Send(ctx context.Context, msg *a2a.Message, h agent.Handler) error
	Follow(ctx context.Context, id a2a.TaskID, h agent.Handler) error
	FindTask(ctx context.Context, contextID, messageID string) (*a2a.Task, []a2a.TaskID, error)
}

// Backend is what the manager needs to know about one Binding.
type Backend struct {
	// ThreadTTL is how long after its last activity a thread keeps its session (D28).
	ThreadTTL time.Duration
	// Agent runs the Binding's turns. Its credentials resolve on every call.
	Agent AgentAPI
	// Attribute records the acting user on a turn: the dialect's Attribute. Nil means a turn
	// carries no attribution.
	Attribute func(msg *a2a.Message, actor dialect.User)
}

// Bindings resolves a Binding by its namespace/name.
type Bindings interface {
	// Backend returns the Binding's backend, or an error wrapping ErrUnknownBinding.
	Backend(name string) (Backend, error)
}

// Users resolves a chat user for attribution (the chat platform's ResolveUser).
type Users interface {
	ResolveUser(ctx context.Context, id string) (dialect.User, error)
}

// TurnRef names the turn a Sink is asked to render.
type TurnRef struct {
	Key       Key
	MessageID string
}

// NoticeKind is what a Notice tells the thread.
type NoticeKind string

// NoticeQueued asks for the reaction that marks a reply waiting for the active task (spec 8.2).
const NoticeQueued NoticeKind = "queued"

// Notice is something for the thread that no turn's output says. ID is stable, so a renderer
// that posts through the outbox posts it once.
type Notice struct {
	ID        string
	Kind      NoticeKind
	MessageTS string // the Slack message the notice is about
}

// Sink receives the output of turns; the renderer (WP-32) implements it. Turn returns the
// handler for one turn's events, FinalEvent included, or nil. The handler runs on the goroutine
// that reads the agent's stream, so it must not block: a slow renderer stalls the read, and
// can trip the inactivity watchdog into a needless reconnect. Notice must not block for long.
type Sink interface {
	Turn(ctx context.Context, t TurnRef) agent.Handler
	Notice(ctx context.Context, key Key, n Notice)
}

// Config configures a Manager. Store and Bindings are required. A nil Users attributes a turn
// by the user's ID alone, and a nil Sink drops the output.
type Config struct {
	Store    Store
	Bindings Bindings
	Users    Users
	Sink     Sink
	Now      func() time.Time
	Logger   *slog.Logger
	// RetryBase and RetryMax are the first pause before a resend, which doubles up to RetryMax.
	// They default to 1 s and 60 s (spec 8.2).
	RetryBase, RetryMax time.Duration
}

// Manager owns the sessions of every conversation. It is safe for concurrent use.
type Manager struct {
	cfg  Config
	wake func(Key)       // tells the runner that Key has a turn waiting
	ctx  context.Context // bounds every turn; Close cancels it
	stop context.CancelFunc
	wg   sync.WaitGroup // the runners

	mu     sync.Mutex
	convos map[Key]*convo // conversations with a runner
	// unrecorded holds, per conversation, the turn the agent took but that could not be
	// recorded, so that no wake sends it again.
	unrecorded map[Key]string
	closed     bool // set by Close: no runner starts after it, so wg.Add cannot race wg.Wait, and no convos entry leaks
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
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = time.Minute
	}
	m := &Manager{cfg: cfg, convos: map[Key]*convo{}, unrecorded: map[Key]string{}}
	m.ctx, m.stop = context.WithCancel(context.Background())
	m.wake = m.start
	return m, nil
}

// SlackMessageID derives the A2A messageId of a Slack message from where it is, not from
// the event that delivered it: an @mention arrives as two events with different IDs, and
// both must be one turn (spec 8.2). An alert's first prompt passes its own stable ID.
func SlackMessageID(connection, channel, ts string) string {
	return fmt.Sprintf("slack:%s:%s:%s", connection, channel, ts)
}

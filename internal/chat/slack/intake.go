package slack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Submitter is the session manager's intake, which *session.Manager satisfies. Its Submit is
// the persist step of a chat event (spec 6.1): a local write, idempotent by messageId.
type Submitter interface {
	Submit(ctx context.Context, in session.Inbound) (session.Result, error)
}

// IntakeConfig configures an Intake. Every field is required.
type IntakeConfig struct {
	// Connection is the ChatConnection this adapter serves, with a namespace and a name. It
	// scopes every conversation key and messageId.
	Connection v1alpha1.ObjectRef
	// Snapshot returns the current routing table, as config.Reloader.Current does.
	Snapshot  func() *config.Snapshot
	Submitter Submitter
	Logger    *slog.Logger // slog.Default() if nil
}

// Intake is a Sink that applies spec 8.2's trigger table: it turns each message into a turn
// for the session manager, or ignores it. It is safe for concurrent use, though Run calls it
// from one goroutine.
type Intake struct {
	cfg  IntakeConfig
	conn string // the connection as it appears in keys: namespace/name
	log  *slog.Logger
	seen recent
}

// NewIntake returns an Intake over cfg.
func NewIntake(cfg IntakeConfig) *Intake {
	return &Intake{cfg: cfg, conn: cfg.Connection.Namespace + "/" + cfg.Connection.Name, log: cmp.Or(cfg.Logger, slog.Default())}
}

// Message submits m as a turn, unless the table says to ignore it:
//   - A mention in a channel bound to a Binding may start a session: a new thread, or one
//     anchored to the thread it is in (the session manager decides whether courier owns it).
//   - Any other message in a thread continues the thread's session if courier owns it;
//     the session manager says NotOwned otherwise, and it is ignored.
//   - A message outside a thread that does not start one is ignored without asking.
//
// It returns an error only when Submit failed in a way a retry may cure, so Run leaves the
// event unacknowledged. An invalid turn is logged and dropped; no retry could mend it.
// A message is remembered once a turn is queued for it, so the second event of a mention
// (app_mention and message) is skipped; an event that queued nothing may be tried again.
func (in *Intake) Message(ctx context.Context, m Message) error {
	seen := [2]string{m.Channel, m.TS}
	if in.seen.has(seen) {
		return nil
	}
	req := session.Inbound{
		Key:       session.Key{Connection: in.conn, Channel: m.Channel, ThreadTS: cmp.Or(m.ThreadTS, m.TS)},
		MessageID: session.SlackMessageID(in.conn, m.Channel, m.TS),
		MessageTS: m.TS, UserID: m.User, Text: m.Text,
	}
	if m.Mentioned {
		req.Binding, req.Create = in.binding(m.Channel)
	}
	if !req.Create && m.ThreadTS == "" {
		return nil
	}
	res, err := in.cfg.Submitter.Submit(ctx, req)
	switch {
	case errors.Is(err, store.ErrInvalid):
		in.log.WarnContext(ctx, "slack: message dropped as invalid", "channel", m.Channel, "ts", m.TS, "err", err)
		return nil
	case err != nil:
		return fmt.Errorf("submit %s: %w", req.MessageID, err)
	}
	switch res.Disposition {
	case session.Created, session.Accepted:
		in.seen.add(seen)
	case session.Expired, session.BindingGone: // WP-32 posts the notice; until then the user sees nothing
		in.log.WarnContext(ctx, "slack: reply in a thread courier cannot continue", "channel", m.Channel, "thread", req.Key.ThreadTS, "disposition", res.Disposition)
	}
	return nil
}

// binding returns the namespace/name of the Binding that owns channel on this connection.
func (in *Intake) binding(channel string) (name string, ok bool) {
	snap := in.cfg.Snapshot()
	if snap == nil {
		return "", false
	}
	rb, ok := snap.BindingForChannel(in.cfg.Connection.Namespace, in.cfg.Connection.Name, channel)
	if !ok {
		return "", false
	}
	return rb.Binding.Namespace + "/" + rb.Binding.Name, true
}

// recentMax bounds each generation of recent.
const recentMax = 1024

// recent remembers the messages seen lately, by (channel, ts). It keeps two generations of at
// most recentMax each, and forgets the older when the newer fills: bounded memory, and
// a message stays remembered for at least recentMax others, far longer than the milliseconds
// between a mention's two events. A redelivery after that is Submit's to make idempotent.
type recent struct {
	mu       sync.Mutex
	cur, old map[[2]string]struct{}
}

func (r *recent) has(k [2]string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, inCur := r.cur[k]
	_, inOld := r.old[k]
	return inCur || inOld
}

func (r *recent) add(k [2]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil || len(r.cur) >= recentMax {
		r.old, r.cur = r.cur, map[[2]string]struct{}{}
	}
	r.cur[k] = struct{}{}
}

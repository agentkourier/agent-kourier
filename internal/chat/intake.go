package chat

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

// IntakeConfig configures an Intake. Every field but Logger is required.
type IntakeConfig struct {
	// Connection is the ChatConnection this adapter serves, with a namespace and a name. It
	// scopes every conversation key and messageId.
	Connection v1alpha1.ObjectRef
	// Snapshot returns the current routing table, as config.Reloader.Current does.
	Snapshot  func() *config.Snapshot
	Submitter Submitter
	// Interactions takes the presses of buttons. Nil drops them, acknowledged.
	Interactions func(ctx context.Context, i Interaction) error
	// Notice, if set, tells a thread that courier cannot continue it: session.NoticeExpired for a
	// thread past its threadTTL, session.NoticeBindingGone for one whose Binding left the config.
	// The notice ID is the message's ID and a suffix, so a notice for the same message is the same
	// row for the outbox. It queues and does not call the platform (live.Sink.QueueNotice). Its error means
	// the notice was not written: Message returns it, so the platform leaves the event unacknowledged and
	// delivers it again, and the message is not remembered, so the redelivery asks again. The same ID
	// makes that safe. Nil only logs them.
	Notice func(ctx context.Context, key session.Key, n session.Notice) error
	Logger *slog.Logger // slog.Default() if nil
}

// Intake is the Sink that applies spec 8.2's trigger table, the same for every platform: it turns each
// Inbound message into a turn for the session manager, or ignores it, and passes presses on. The adapter
// has decided what is a mention and who is who, so nothing here knows a platform's syntax. It is safe for
// concurrent use, though a platform's Run calls it from one goroutine.
type Intake struct {
	cfg  IntakeConfig
	conn string // the connection as it appears in keys: namespace/name
	log  *slog.Logger
	seen recent
}

var _ Sink = (*Intake)(nil)

// NewIntake returns an Intake over cfg.
func NewIntake(cfg IntakeConfig) *Intake {
	return &Intake{cfg: cfg, conn: v1alpha1.NamespacedName(cfg.Connection.Namespace, cfg.Connection.Name), log: cmp.Or(cfg.Logger, slog.Default())}
}

// Message submits m as a turn, unless the table says to ignore it:
//   - A mention in a channel bound to a Binding may start a session: a new thread, or one
//     anchored to the thread it is in (the session manager decides whether courier owns it).
//   - Any other message in a thread continues the thread's session if courier owns it;
//     the session manager says NotOwned otherwise, and it is ignored.
//   - A message outside a thread that does not start one is ignored without asking.
//
// It returns an error only when Submit failed in a way a retry may cure, so the platform leaves the
// event unacknowledged. An invalid turn is logged and dropped; no retry could mend it.
// A message is remembered once a turn is queued for it, or a notice written for it (see
// IntakeConfig.Notice), so the second event of a mention (a platform may deliver it twice) is skipped; an
// event that queued nothing, or whose notice failed to write, may be tried again.
func (in *Intake) Message(ctx context.Context, m Inbound) error {
	seen := [2]string{m.At.Channel, m.At.ID}
	if in.seen.has(seen) {
		return nil
	}
	req := session.Inbound{
		Key:        session.Key{Connection: in.conn, Channel: m.At.Channel, Thread: cmp.Or(m.At.Thread, m.At.ID)},
		MessageID:  MessageID(Ref{Platform: m.At.Platform, Conn: in.conn, Channel: m.At.Channel, ID: m.At.ID}),
		MessageRef: m.At.ID, UserID: m.User, Text: m.Text,
	}
	if m.Mentioned {
		req.Binding, req.Create = in.binding(m.At.Channel)
	}
	if !req.Create && m.At.Thread == "" {
		return nil
	}
	res, err := in.cfg.Submitter.Submit(ctx, req)
	switch {
	case errors.Is(err, store.ErrInvalid):
		in.log.WarnContext(ctx, "chat: message dropped as invalid", "channel", m.At.Channel, "message", m.At.ID, "err", err)
		return nil
	case err != nil:
		return fmt.Errorf("submit %s: %w", req.MessageID, err)
	}
	switch res.Disposition {
	case session.Created, session.Accepted:
		in.seen.add(seen)
	case session.Expired, session.BindingGone:
		in.log.WarnContext(ctx, "chat: reply in a thread courier cannot continue", "channel", m.At.Channel, "thread", req.Key.Thread, "disposition", res.Disposition)
		if in.cfg.Notice != nil {
			kind, suffix := session.NoticeExpired, "/expired"
			if res.Disposition == session.BindingGone {
				kind, suffix = session.NoticeBindingGone, "/binding_gone"
			}
			if err := in.cfg.Notice(ctx, req.Key, session.Notice{ID: req.MessageID + suffix, Kind: kind, MessageRef: m.At.ID}); err != nil {
				return fmt.Errorf("notice %s%s: %w", req.MessageID, suffix, err)
			}
			in.seen.add(seen) // the mention's other event is the same message, and the same notice
		}
	}
	return nil
}

// Interaction passes a press to IntakeConfig.Interactions, and returns its error.
func (in *Intake) Interaction(ctx context.Context, i Interaction) error {
	if in.cfg.Interactions == nil {
		in.log.WarnContext(ctx, "chat: press dropped; no handler", "channel", i.At.Channel, "message", i.At.ID)
		return nil
	}
	return in.cfg.Interactions(ctx, i)
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
	return v1alpha1.NamespacedName(rb.Binding.Namespace, rb.Binding.Name), true
}

// recentMax bounds each generation of recent.
const recentMax = 1024

// recent remembers the messages seen lately, by (channel, ID). It keeps two generations of at
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

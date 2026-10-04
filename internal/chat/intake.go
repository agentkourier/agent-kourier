package chat

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

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

// IntakeConfig configures an Intake. Connection, Snapshot and Submitter are required; the rest are optional.
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
	// OnTrigger, if set, takes a bot message that a botMessage trigger matched, after the Intake has
	// logged and counted it. Its error means it was not taken: Message returns it, so the platform leaves
	// the event unacknowledged and delivers it again, and the message is not remembered.
	//
	// STUB(CT-3, CT-4): this is where the match goes onward, and CT-2 starts no session from it. CT-3
	// maps the key to its thread and applies the cooldown and the close rule; CT-4 renders the prompt and
	// starts the turn. Both replace this hook, which is for a test to see what matched until then.
	OnTrigger func(ctx context.Context, m TriggerMatch) error
	Logger    *slog.Logger // slog.Default() if nil
}

// Intake is the Sink that applies spec 8.2's trigger table, the same for every platform: it turns each
// Inbound message into a turn for the session manager, or ignores it, and passes presses on. The adapter
// has decided what is a mention and who is who, so nothing here knows a platform's syntax. It is safe for
// concurrent use, though a platform's Run calls it from one goroutine.
type Intake struct {
	cfg    IntakeConfig
	conn   string // the connection as it appears in keys: namespace/name
	log    *slog.Logger
	seen   recent
	tables tableCache
	// mentionless holds the Bindings that were said to have no mention trigger, so that it is said once.
	mentionless sync.Map
}

var (
	_ Sink      = (*Intake)(nil)
	_ BotFilter = (*Intake)(nil)
)

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
//   - A bot's message (m.Bot set) is never a mention and is never submitted: it is tried against the
//     botMessage triggers of its channel's Binding, in order (botMessage). A match is handed to
//     IntakeConfig.OnTrigger, and a message no trigger matches is ignored and counted.
//
// It returns an error only when Submit failed in a way a retry may cure, so the platform leaves the
// event unacknowledged. An invalid turn is logged and dropped; no retry could mend it.
// A message is remembered once a turn is queued for it, or a notice written for it (see
// IntakeConfig.Notice), so the second event of a mention (a platform may deliver it twice) is skipped; an
// event that queued nothing, or whose notice failed to write, may be tried again.
func (in *Intake) Message(ctx context.Context, m Inbound) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.intake", attribute.String("courier.source", "message"), attribute.String("courier.platform", m.At.Platform))
	disposition := "ignored"
	defer func() {
		if telemetryErr != nil {
			disposition = "error"
		}
		telemetry.Count(ctx, "courier_intake_total", "message", disposition)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("courier.intake.disposition", disposition))
		op.Finish(telemetryErr)
	}()
	seen := [2]string{m.At.Channel, m.At.ID}
	if in.seen.has(seen) {
		disposition = "duplicate"
		return nil
	}
	if m.Bot != nil {
		d, err := in.botMessage(ctx, m)
		if err != nil {
			return err
		}
		if disposition = d; d != "ignored" {
			in.seen.add(seen) // a redelivery is neither matched nor counted again
		}
		return nil
	}
	req := session.Inbound{
		Key:        session.Key{Connection: in.conn, Channel: m.At.Channel, Thread: cmp.Or(m.At.Thread, m.At.ID)},
		MessageID:  MessageID(Ref{Platform: m.At.Platform, Conn: in.conn, Channel: m.At.Channel, ID: m.At.ID}),
		MessageRef: m.At.ID, UserID: m.User, Text: m.Text,
	}
	if m.Mentioned {
		req.Binding, req.Create = in.binding(ctx, m.At.Channel)
	}
	if !req.Create && m.At.Thread == "" {
		return nil
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("courier.binding", req.Binding))
	res, err := in.cfg.Submitter.Submit(ctx, req)
	if res.Disposition != "" {
		disposition = string(res.Disposition)
	}
	switch {
	case errors.Is(err, store.ErrInvalid):
		disposition = "invalid"
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
func (in *Intake) Interaction(ctx context.Context, i Interaction) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.intake", attribute.String("courier.source", "interaction"))
	defer func() {
		telemetry.Count(ctx, "courier_intake_total", "interaction", telemetry.ErrorClass(telemetryErr))
		op.Finish(telemetryErr)
	}()
	if in.cfg.Interactions == nil {
		in.log.WarnContext(ctx, "chat: press dropped; no handler", "channel", i.At.Channel, "message", i.At.ID)
		return nil
	}
	return in.cfg.Interactions(ctx, i)
}

// botMessage tries a bot's message against the botMessage triggers of the Binding that owns its channel, and
// returns the disposition for the intake metric. Nothing here starts a session (CT-2).
//   - A channel with no Binding, or a bot's reply inside a thread, is ignored and not counted: a trigger is about
//     a message that opens its own, and a bot must not continue, or hijack, a thread of people or of courier's
//     own. The adapter has already refused courier's own messages, and every bot no trigger names.
//   - A message that no trigger matches is counted as courier_trigger_unmatched_total of the Binding, so that a
//     changed template shows up as a rising count, and logged at DEBUG with where it was and which bot, and never
//     what it said.
//   - A match is counted, logged and handed to OnTrigger (the stub, see IntakeConfig.OnTrigger).
func (in *Intake) botMessage(ctx context.Context, m Inbound) (disposition string, err error) {
	snap := in.cfg.Snapshot()
	if snap == nil {
		return "ignored", nil
	}
	rb, ok := snap.BindingForChannel(in.cfg.Connection.Namespace, in.cfg.Connection.Name, m.At.Channel)
	if !ok || (m.At.Thread != "" && m.At.Thread != m.At.ID) {
		return "ignored", nil
	}
	name := v1alpha1.NamespacedName(rb.Binding.Namespace, rb.Binding.Name)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("courier.binding", name))
	index, fields, ok := in.tables.table(snap, rb.Binding, in.log).match(m)
	if !ok {
		telemetry.Count(ctx, "courier_trigger_unmatched_total", name)
		in.log.DebugContext(ctx, "chat: bot message matched no trigger", "binding", name, "channel", m.At.Channel, "message", m.At.ID, "bot", m.Bot.BotID)
		return "trigger_unmatched", nil
	}
	match := TriggerMatch{Binding: name, Index: index, Fields: fields, Message: m}
	in.log.InfoContext(ctx, "chat: bot message matched a trigger; no turn is started yet", "binding", name, "trigger", index, "channel", m.At.Channel, "message", m.At.ID, "bot", m.Bot.BotID)
	if in.cfg.OnTrigger != nil {
		if err := in.cfg.OnTrigger(ctx, match); err != nil {
			return "", fmt.Errorf("trigger %d of %s for %s: %w", index, name, MessageID(Ref{Platform: m.At.Platform, Conn: in.conn, Channel: m.At.Channel, ID: m.At.ID}), err)
		}
	}
	telemetry.Count(ctx, "courier_trigger_matched_total", name) // only once the match was taken: a redelivery counts it then
	return "trigger_matched", nil
}

// AcceptsBot implements BotFilter: it says whether a botMessage trigger of the Binding that owns channel
// names botID, by ID and never by display name (D36). A channel with no Binding, and an empty ID, accept none.
func (in *Intake) AcceptsBot(channel, botID string) bool {
	snap := in.cfg.Snapshot()
	if snap == nil || botID == "" {
		return false
	}
	rb, ok := snap.BindingForChannel(in.cfg.Connection.Namespace, in.cfg.Connection.Name, channel)
	if !ok {
		return false
	}
	return slices.ContainsFunc(rb.Binding.Spec.Chat.Triggers, func(t v1alpha1.Trigger) bool {
		return t.Type == v1alpha1.TriggerBotMessage && t.From != nil && t.From.BotID == botID
	})
}

// binding returns the namespace/name of the Binding that owns channel on this connection and takes @mentions: a
// Binding whose triggers are listed and leave out the mention one does not (spec 8.2), and says so, once. An empty
// list is the mention trigger. It decides only whether a mention may create a session: a reply in a thread
// courier owns continues it whatever the triggers say.
func (in *Intake) binding(ctx context.Context, channel string) (name string, ok bool) {
	snap := in.cfg.Snapshot()
	if snap == nil {
		return "", false
	}
	rb, ok := snap.BindingForChannel(in.cfg.Connection.Namespace, in.cfg.Connection.Name, channel)
	if !ok {
		return "", false
	}
	name = v1alpha1.NamespacedName(rb.Binding.Namespace, rb.Binding.Name)
	triggers := rb.Binding.Spec.Chat.Triggers
	if len(triggers) > 0 && !slices.ContainsFunc(triggers, func(t v1alpha1.Trigger) bool { return t.Type == v1alpha1.TriggerMention }) {
		if _, told := in.mentionless.LoadOrStore(name, struct{}{}); !told {
			in.log.InfoContext(ctx, "chat: the triggers of a Binding have no mention trigger, so an @mention in its channel starts no session; replies in a thread courier owns still continue it", "binding", name)
		}
		return "", false
	}
	return name, true
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

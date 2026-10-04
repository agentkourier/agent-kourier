package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TriggerDecision is what a matched bot message does to the thread of its key (spec 8.2). It is the decision
// label of courier_trigger_decisions_total.
type TriggerDecision string

// The decisions. A fire is a message whose extracted fields do not satisfy the trigger's closeWhen; a close is
// one that does.
const (
	// DecisionStarted: a fire, and the key has no thread, or its thread is closed and its cooldown has ended.
	// The message starts a new investigation, anchored to itself, and the Intake records the thread first.
	DecisionStarted TriggerDecision = "started"
	// DecisionRepeat: a fire, and the key has a thread that is open or inside its cooldown. A note in that
	// thread says it fired again, a closed thread is open again, and no turn starts.
	DecisionRepeat TriggerDecision = "repeat"
	// DecisionClosed: a close, and the key has an open thread. A note in it says it closed, the thread is
	// closed, and no turn starts. The cooldown, which runs from the start, goes on.
	DecisionClosed TriggerDecision = "closed"
	// DecisionCloseUnknown: a close, and no open thread of the key to close: it has none, or it is closed
	// already. It is ignored. A redelivery of a close whose first delivery was taken, after a restart made the
	// Intake forget it, is this too.
	DecisionCloseUnknown TriggerDecision = "close_unknown"
	// DecisionDedup: Slack delivered a message again that was taken already. An Intake that remembers it does
	// nothing. One that does not, after a restart, finds in the store that the message started the thread,
	// and asks for the start again (OnStart), since the first ask may have been lost.
	DecisionDedup TriggerDecision = "dedup"
)

// threadKeyField is the extract that names the thread of a message (v1alpha1.ThreadByKey).
const threadKeyField = "key"

// errNoThreadStore is why a keyed trigger's match cannot be decided: the Intake was given no
// IntakeConfig.TriggerThreads.
var errNoThreadStore = errors.New("a trigger threads by key and the Intake has no store for threads")

// decided is the outcome of deciding a match: the decision, which is empty if there is none, and what the logs
// say of it. The key is untrusted, so they say its short hash.
type decided struct {
	decision TriggerDecision
	keyHash  string // the first eight characters of the hash of the key; empty if the match has none
	thread   string // the thread the decision is about, if known
}

// decide applies the thread rules of a match to the store, and carries out what they say:
//
//	fire, no thread for the key, or a closed one past its cooldown   started: record the thread, then OnStart
//	fire, the message is the thread's own (a redelivery)             dedup: OnStart again
//	fire, any other thread of the key                                repeat: note, then reopen it
//	close, the key has an open thread                                closed: note, then close it
//	close, otherwise                                                 close_unknown: nothing
//
// A trigger with no thread rules, or a match whose key is empty, has no thread to apply them to: every fire
// starts a turn and every close is unknown. The cooldown runs from the start, not from the last message, so a
// key starts at most one turn in each window; an open thread takes its key's messages however old it is.
//
// Each step is idempotent, and the note comes before the change it goes with, so an error at any point is
// mended by the redelivery: a note is one row of the outbox whatever its asks, and a redelivered start finds its
// own thread. The decision is returned with the error when it was made already (the start was recorded, the
// OnStart failed).
func (in *Intake) decide(ctx context.Context, m TriggerMatch) (decided, error) {
	key := m.Fields[threadKeyField]
	closing := closes(m.Thread.CloseWhen, m.Fields)
	if m.Thread.By != v1alpha1.ThreadByKey || key == "" {
		if m.Thread.By == v1alpha1.ThreadByKey {
			in.log.WarnContext(ctx, "chat: a trigger that threads by key extracted an empty key, so the message has no thread", "binding", m.Binding, "trigger", m.Index, "channel", m.Message.At.Channel, "message", m.Message.At.ID)
		}
		if closing {
			return decided{decision: DecisionCloseUnknown}, nil
		}
		return decided{decision: DecisionStarted, thread: m.Message.At.ID}, in.start(ctx, m)
	}
	if in.cfg.TriggerThreads == nil {
		return decided{}, errNoThreadStore
	}
	hash := keyHash(in.conn, m.Message.At.Channel, m.Message.Bot.BotID, key)
	out := decided{keyHash: hash[:8]}
	if closing {
		return in.closeThread(ctx, m, hash, out)
	}
	return in.fireThread(ctx, m, hash, out)
}

// fireThread is decide for a fire of a key that has threads.
func (in *Intake) fireThread(ctx context.Context, m TriggerMatch, hash string, out decided) (decided, error) {
	at := m.Message.At.ID
	th, opened, err := in.cfg.TriggerThreads.OpenTriggerThread(ctx, store.TriggerThread{
		Binding: m.Binding, KeyHash: hash, Thread: at, Opened: m.Received, CooldownUntil: m.Received.Add(m.Thread.Cooldown.Duration)})
	if err != nil {
		return out, fmt.Errorf("open the thread of key %s: %w", out.keyHash, err)
	}
	out.thread = th.Thread
	switch {
	case opened:
		out.decision = DecisionStarted
		return out, in.start(ctx, m)
	case th.Thread == at:
		out.decision = DecisionDedup
		return out, in.start(ctx, m)
	}
	if err := in.note(ctx, m, th.Thread, session.NoticeTriggerRepeat); err != nil {
		return out, err
	}
	if err := in.cfg.TriggerThreads.ReopenTriggerThread(ctx, m.Binding, hash, m.Received); err != nil {
		return out, fmt.Errorf("reopen the thread of key %s: %w", out.keyHash, err)
	}
	out.decision = DecisionRepeat
	return out, nil
}

// closeThread is decide for a close of a key that has threads.
func (in *Intake) closeThread(ctx context.Context, m TriggerMatch, hash string, out decided) (decided, error) {
	th, err := in.cfg.TriggerThreads.GetTriggerThread(ctx, m.Binding, hash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		out.decision = DecisionCloseUnknown
		return out, nil
	case err != nil:
		return out, fmt.Errorf("get the thread of key %s: %w", out.keyHash, err)
	}
	out.thread = th.Thread
	if !th.ClosedAt.IsZero() {
		out.decision = DecisionCloseUnknown
		return out, nil
	}
	if err := in.note(ctx, m, th.Thread, session.NoticeTriggerClosed); err != nil {
		return out, err
	}
	if _, err := in.cfg.TriggerThreads.CloseTriggerThread(ctx, m.Binding, hash, m.Received); err != nil {
		return out, fmt.Errorf("close the thread of key %s: %w", out.keyHash, err)
	}
	out.decision = DecisionClosed
	return out, nil
}

// start hands the match on as a turn to start, if there is where to.
func (in *Intake) start(ctx context.Context, m TriggerMatch) error {
	if in.cfg.OnStart == nil {
		return nil
	}
	return in.cfg.OnStart(ctx, m)
}

// note asks for a short note of courier's own in the thread: its kind and the time of the match, and nothing the
// message said. Its ID names the message and the kind, so asking again, as a redelivery does, is the same row of
// the outbox. Without a Notice sink there is no note, and that is logged.
func (in *Intake) note(ctx context.Context, m TriggerMatch, thread string, kind session.NoticeKind) error {
	if in.cfg.Notice == nil {
		in.log.WarnContext(ctx, "chat: a trigger's note was not posted; the Intake has no notice sink", "kind", kind, "channel", m.Message.At.Channel, "thread", thread)
		return nil
	}
	id := in.messageID(m.Message) + "/" + string(kind)
	key := session.Key{Connection: in.conn, Channel: m.Message.At.Channel, Thread: thread}
	n := session.Notice{ID: id, Kind: kind, MessageRef: m.Message.At.ID, Detail: m.Received.UTC().Format("2006-01-02 15:04 UTC") + "."}
	if err := in.cfg.Notice(ctx, key, n); err != nil {
		return fmt.Errorf("notice %s: %w", id, err)
	}
	return nil
}

// closes says whether fields satisfy a closeWhen rule: it has an entry, and every entry equals. A rule with no
// entries closes nothing.
func closes(rule, fields map[string]string) bool {
	if len(rule) == 0 {
		return false
	}
	for name, want := range rule {
		if got, ok := fields[name]; !ok || got != want {
			return false
		}
	}
	return true
}

// keyHash is the hash that identifies a key in the store. The key is untrusted text out of a bot's message, so
// it is hashed, and it is hashed with what scopes it: the connection and channel, since a thread is a message
// of one channel, and the bot, since another bot's key of the same text is not the same alert. Fields are
// separated by a byte no ID holds.
func keyHash(conn, channel, botID, key string) string {
	h := sha256.New()
	for _, part := range []string{conn, channel, botID, key} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

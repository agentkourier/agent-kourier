package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TriggerDecision is what a matched bot message does to the thread of its key (spec 8.2). It is the decision
// label of agentkourier_trigger_decisions_total.
type TriggerDecision string

// The decisions. A fire is a message whose extracted fields do not satisfy the trigger's closeWhen; a close is
// one that does.
const (
	// DecisionStarted: a fire, and the key has no thread, or its thread is closed and its cooldown has ended, or
	// its turn never started and the reservation is stale, or it is open but has been idle past the trigger's
	// maxIdle, which is a resolve that never came. The message starts a new investigation, anchored to
	// itself: the Intake reserves the thread, asks for the turn (OnStart), and, once that is taken, marks the
	// thread started, which is when its cooldown begins.
	DecisionStarted TriggerDecision = "started"
	// DecisionRepeat: a fire, and the key has a thread that is open or inside its cooldown. A note in that
	// thread says it fired again, a closed thread is open again, and no turn starts.
	DecisionRepeat TriggerDecision = "repeat"
	// DecisionClosed: a close, and the key has an open thread whose turn started. A note in it says it
	// closed, the thread is closed, and no turn starts. The cooldown, which runs from the start, goes on.
	DecisionClosed TriggerDecision = "closed"
	// DecisionCloseUnknown: a close, and no open thread of the key to close: it has none, or it is closed
	// already, or its turn never started so there is no investigation to note. It is ignored. A redelivery of
	// a close whose first delivery was taken, after a restart made the Intake forget it, is this too.
	DecisionCloseUnknown TriggerDecision = "close_unknown"
	// DecisionDedup: Slack delivered a message again that was taken already. An Intake that remembers it does
	// nothing. One that does not, after a restart, finds in the store that the message reserved the thread:
	// if its turn started that is all, and if not the start is asked for again (OnStart), since the first ask
	// failed.
	DecisionDedup TriggerDecision = "dedup"
	// DecisionEmptyKey: a fire of a trigger that threads by key whose key came out empty, which names no
	// thread. It is an investigation of its own each time, with no cooldown and no note, as a trigger with no
	// thread rules is, and a warning says so, once for the trigger.
	DecisionEmptyKey TriggerDecision = "empty_key"
	// DecisionLimited: a fire that would have started a turn, and a limit of the trigger stopped it (rate,
	// concurrency or the daily cap: v1alpha1.TriggerLimits). No turn starts and nothing is posted in the alert's
	// thread: the alert reached people through its own receiver, and a digest line says how many were not
	// investigated. The key's reservation is given back and holds nothing, so a later fire of the key starts once
	// there is capacity. Only a start is limited: a repeat, a close and a note never are.
	DecisionLimited TriggerDecision = "limited"
	// DecisionNotStarted: a fire that would have started a turn, and OnStart said it will not (ErrNotStarted),
	// such as a prompt that was refused. The event is acknowledged, the thread is left unmarked with no
	// cooldown, and the key's next fire takes it over once it is stale.
	DecisionNotStarted TriggerDecision = "not_started"
)

// ErrNotStarted is what OnStart returns, itself or wrapped, when the match will not start a turn and asking again
// would not change that, such as a prompt that was refused. The Intake acknowledges the event and does not have
// it delivered again, it leaves the thread reserved but unmarked, so that no cooldown begins and a later fire of
// the key may take the key over, and it counts the decision as not_started. The error's text is not logged: OnStart
// logs its own reason. Any other error of OnStart is transient: it is returned, and the redelivery asks again.
var ErrNotStarted = errors.New("chat: the trigger's turn was not started")

// reservationGrace is how long a thread that reserved its key, and whose turn has not started, is left to start
// before a later message of the key may take the key over. A turn that is starting takes milliseconds, and a
// failed one is asked for again by its redelivery within seconds, so a minute tells the two apart.
const reservationGrace = time.Minute

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
//	fire, no thread for the key, or a closed one past its cooldown,
//	  or a reservation that never started and is stale,
//	  or an open thread idle past thread.maxIdle                     started: reserve, check the limits, OnStart, mark started
//	  ... and a limit of the trigger is reached                      limited: give the reservation back
//	fire, the message is the thread's own (a redelivery)             dedup: OnStart again if it never started
//	fire, any other thread of the key                                repeat: note, then reopen it
//	close, the key has an open thread whose turn started             closed: note, then close it
//	close, otherwise                                                 close_unknown: nothing
//
// A trigger with no thread rules has no thread to apply them to: every fire starts a turn and every close is
// unknown. So has a match whose key is empty, which is counted as such. The cooldown runs from the message that
// started the turn, not from the last message, so a key starts at most one turn in each window. An open thread
// takes its key's messages until it has had none for thread.maxIdle, which counts it abandoned (its resolve was
// lost), so the next fire starts afresh. The cooldown begins when the turn is marked started, so a
// turn that OnStart refused, or never finished asking for, does not hold the key: the next message takes over.
//
// Each step is idempotent, and the note comes before the change it goes with, so an error at any point is
// mended by the redelivery: a note is one row of the outbox whatever its asks, and a redelivered start finds its
// own reservation. The decision is returned with the error when it was made already (the thread was reserved,
// the OnStart failed). Everything here is a local write, since it runs on the platform's acknowledgement path.
func (in *Intake) decide(ctx context.Context, m TriggerMatch) (decided, error) {
	key := m.Fields[threadKeyField]
	closing := closes(m.Thread.CloseWhen, m.Fields)
	if m.Thread.By != v1alpha1.ThreadByKey || key == "" {
		return in.unthreaded(ctx, m, closing)
	}
	if in.cfg.TriggerThreads == nil {
		return decided{}, errNoThreadStore
	}
	hash := keyHash(in.conn, m.Message.At.Channel, m.Message.Bot.ID, key)
	out := decided{keyHash: hash[:8]}
	if closing {
		return in.closeThread(ctx, m, hash, out)
	}
	return in.fireThread(ctx, m, hash, out)
}

// fireThread is decide for a fire of a key that has threads.
func (in *Intake) fireThread(ctx context.Context, m TriggerMatch, hash string, out decided) (decided, error) {
	at := m.Message.At.ID
	if err := in.adoptQueuedTurn(ctx, m, hash); err != nil {
		return out, err
	}
	th, opened, err := in.cfg.TriggerThreads.OpenTriggerThread(ctx,
		store.TriggerThread{Binding: m.Binding, KeyHash: hash, Thread: at, Opened: m.Received},
		store.TriggerStale{Unstarted: m.Received.Add(-reservationGrace), Idle: m.Received.Add(-m.Thread.EffectiveMaxIdle())})
	if err != nil {
		return out, fmt.Errorf("open the thread of key %s: %w", out.keyHash, err)
	}
	out.thread = th.Thread
	switch {
	case opened:
		out.decision = DecisionStarted
		return in.startThread(ctx, m, hash, out)
	case th.Thread == at:
		out.decision = DecisionDedup
		if th.Started() {
			return out, nil
		}
		return in.startThread(ctx, m, hash, out)
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

// adoptQueuedTurn closes the window in which one key gets two turns. OnStart queues a turn and then the thread is
// marked started; if the mark never lands (a failed write, a crash) and the event is not delivered again, the
// reservation stays unmarked, and the next fire to find it stale would take the key over and start a second turn
// beside the first. So, before a fire may take over a reservation of another message that is stale and not started,
// it asks (IntakeConfig.TurnQueued) whether that reservation's turn was taken, and if so marks that thread started,
// as the lost write would have, with the cooldown running from its own message. OpenTriggerThread then finds a
// started open thread, and the fire is a repeat in it. A reservation whose turn was not taken (the start failed or
// was refused) is left for the takeover. Marking is idempotent, and a thread that someone else marked or took
// over in the meantime is not this fire's to mend.
func (in *Intake) adoptQueuedTurn(ctx context.Context, m TriggerMatch, hash string) error {
	if in.cfg.TurnQueued == nil {
		return nil
	}
	th, err := in.cfg.TriggerThreads.GetTriggerThread(ctx, m.Binding, hash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("get the thread of key %s: %w", hash[:8], err)
	}
	if th.Started() || th.Thread == m.Message.At.ID || !th.Opened.Before(m.Received.Add(-reservationGrace)) {
		return nil // not the stale reservation of another message, which is what OpenTriggerThread takes over
	}
	reserved := m.Message // the reserving message sat in the same channel of the same platform
	reserved.At.ID = th.Thread
	queued, err := in.cfg.TurnQueued(ctx, in.messageID(reserved))
	if err != nil {
		return fmt.Errorf("ask whether the turn of thread %s of key %s was queued: %w", th.Thread, hash[:8], err)
	}
	if !queued {
		return nil
	}
	switch err := in.cfg.TriggerThreads.MarkTriggerStarted(ctx, m.Binding, hash, th.Thread, th.Opened.Add(m.Thread.Cooldown.Duration)); {
	case err == nil:
		in.log.WarnContext(ctx, "chat: a thread's turn was queued but its mark was lost; marked it started instead of taking the key over",
			"binding", m.Binding, "keyHash", hash[:8], "thread", th.Thread, "channel", m.Message.At.Channel)
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrConflict):
	default:
		return fmt.Errorf("mark the thread %s of key %s started: %w", th.Thread, hash[:8], err)
	}
	return nil
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
	if !th.ClosedAt.IsZero() || !th.Started() {
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

// unthreaded is decide for a match that has no thread to apply the rules to: its trigger threads by nothing, or
// by a key that came out empty. A close is unknown, and a fire starts a turn of its own, with no cooldown.
func (in *Intake) unthreaded(ctx context.Context, m TriggerMatch, closing bool) (decided, error) {
	out := decided{decision: DecisionStarted, thread: m.Message.At.ID}
	switch {
	case closing:
		return decided{decision: DecisionCloseUnknown}, nil
	case m.Thread.By == v1alpha1.ThreadByKey:
		out.decision = DecisionEmptyKey
		if _, warned := in.emptyKeyWarned.LoadOrStore(fmt.Sprintf("%s#%d", m.Binding, m.Index), true); !warned {
			in.log.WarnContext(ctx, "chat: a trigger that threads by key extracted an empty key, so its message starts a turn of its own; this is logged once for the trigger",
				"binding", m.Binding, "trigger", m.Index, "channel", m.Message.At.Channel, "message", m.Message.At.ID)
		}
	}
	if limit, err := in.admit(ctx, m); err != nil {
		return out, err
	} else if limit != "" {
		in.countLimited(ctx, m, limit)
		out.decision = DecisionLimited
		return out, nil
	}
	err := in.start(ctx, m)
	if errors.Is(err, ErrNotStarted) {
		out.decision, err = DecisionNotStarted, nil
	}
	return out, err
}

// start hands the match on as a turn to start, if there is where to.
func (in *Intake) start(ctx context.Context, m TriggerMatch) error {
	if in.cfg.OnStart == nil {
		return nil
	}
	return in.cfg.OnStart(ctx, m)
}

// startThread starts the turn of the thread this match reserved, if the trigger's limits allow it, and once that is
// taken marks the thread started, which is when the cooldown begins: a turn that could not start leaves none. If
// a limit stops it the reservation is given back (stopThread) and the decision is limited. If OnStart says it will
// not start (ErrNotStarted) the thread stays unmarked and the decision is not_started, with no error; the start
// stays counted against the rate and daily limits, because the refusal still posts a note, so a storm of refusals
// is held to the limits too.
func (in *Intake) startThread(ctx context.Context, m TriggerMatch, hash string, out decided) (decided, error) {
	if limit, err := in.admit(ctx, m); err != nil {
		return out, err
	} else if limit != "" {
		return in.stopThread(ctx, m, hash, out, limit)
	}
	switch err := in.start(ctx, m); {
	case errors.Is(err, ErrNotStarted):
		out.decision = DecisionNotStarted
		return out, nil
	case err != nil:
		return out, err
	}
	until := m.Received.Add(m.Thread.Cooldown.Duration)
	if err := in.cfg.TriggerThreads.MarkTriggerStarted(ctx, m.Binding, hash, m.Message.At.ID, until); err != nil {
		return out, fmt.Errorf("mark the thread of key %s started: %w", hash[:8], err)
	}
	return out, nil
}

// note asks for a short note of Agent Kourier's own in the thread: its kind and the time of the match, and nothing the
// message said. Its ID names the message and the kind, so asking again, as a redelivery does, is the same row of
// the outbox. Without a Notice sink there is no note, and that is logged.
func (in *Intake) note(ctx context.Context, m TriggerMatch, thread string, kind session.NoticeKind) error {
	if in.cfg.Notice == nil {
		in.log.WarnContext(ctx, "chat: a trigger's note was not posted; the Intake has no notice sink", "kind", kind, "channel", m.Message.At.Channel, "thread", thread)
		return nil
	}
	id := m.MessageID + "/" + string(kind)
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
// of one channel, and the sender's ID, since another sender's key of the same text is not the same alert. The sender's kind
// is not in it: the hash is what a thread row of an earlier release is stored under, and a row must still be found
// after the kind was added to the sender. The cost: two triggers of one channel that name the same ID with different kinds
// share their key threads, so an adapter with more than one kind for an ID must revisit this hash. Fields are separated
// by a byte no ID holds.
func keyHash(conn, channel, senderID, key string) string {
	h := sha256.New()
	for _, part := range []string{conn, channel, senderID, key} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

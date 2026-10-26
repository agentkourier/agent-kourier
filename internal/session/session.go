// Package session is Agent Kourier's session manager: it maps a chat conversation to an agent
// session, queues the thread's replies durably, and runs one turn at a time against the
// agent (spec 8.2, 8.3, D21, D28). The durable intake here never talks to the agent.
package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
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
	store.Recovery
}

// Questions finds the question a thread is waiting on. The intake asks it, in a mention-only Binding, whether a reply
// that does not mention Agent Kourier is the answer to a question that takes text replies (D38). store.Interactions
// satisfies it.
type Questions interface {
	// PendingTextQuestion returns the pending interaction of the thread that takes text replies, or
	// store.ErrNotFound if there is none.
	PendingTextQuestion(ctx context.Context, key store.ConversationKey) (store.Interaction, error)
}

// AgentAPI is the part of *agent.Agent the manager uses.
type AgentAPI interface {
	Send(ctx context.Context, msg *a2a.Message, h agent.Handler) error
	Follow(ctx context.Context, id a2a.TaskID, h agent.Handler) error
	GetTask(ctx context.Context, id a2a.TaskID) (*a2a.Task, error)
	CancelTask(ctx context.Context, id a2a.TaskID) (*a2a.Task, error)
	Answer(ctx context.Context, msg *a2a.Message, h agent.Handler) error
	FindTask(ctx context.Context, contextID, messageID string) (*a2a.Task, []a2a.TaskID, error)
}

// Backend is what the manager needs to know about one Binding.
type Backend struct {
	// Name, AgentName and Dialect identify configured backend resources for spans. Dialect is also what says whether
	// the Binding takes text replies to the agent's questions (TakesTextReplies).
	Name, AgentName, Dialect string
	// ThreadReplies is the Binding's chat.threadReplies. Anything but mention, the empty value too,
	// means every person's reply in a thread Agent Kourier owns is a turn (spec 8.2, D38).
	ThreadReplies v1alpha1.ThreadReplies
	// ThreadTTL is how long after its last activity a thread keeps its session (D28).
	ThreadTTL time.Duration
	// Agent runs the Binding's turns. Its credentials resolve on every call.
	Agent AgentAPI
	// Attribute records the acting user on a turn: the dialect's Attribute. Nil means a turn
	// carries no attribution.
	Attribute func(msg *a2a.Message, actor dialect.User)
	// Platform is the chat platform of the Binding's ChatConnection, which the manager puts on the actor it
	// attributes a turn to. The zero value attributes a turn with no platform.
	Platform dialect.Platform
	// Rejected is identity.Source.Rejected. The manager calls it for every call the front door
	// refused the credentials of (ErrCredentialsRejected); it re-reads the token, and writes the
	// only audit row for the call, so the manager does not audit it again. Nil does nothing.
	Rejected func(ctx context.Context, actor audit.Actor, call audit.Call) error
	// Identity names the Binding's service identity for the audit rows the manager writes.
	Identity string
	// Output is the Binding's chat output mode (live or final), which the sink is told of each
	// turn. Empty means live.
	Output v1alpha1.OutputMode
	// Progress is the Binding's chat.progress, which the sink is told of each turn. The API fills it with
	// steps-and-activity when a Binding sets none; empty here, as in a Backend that no Binding made, means steps.
	Progress v1alpha1.ProgressMode
	// Feedback is the Binding's feedback.enabled: the final message of an answer to a person carries the
	// useful and not-useful buttons (spec 8.7). An investigation that nobody asked for always does.
	Feedback bool
	// AcknowledgmentEnabled adds a temporary reaction to each accepted message. AcknowledgmentEmoji is the
	// bare platform reaction name kept with the queued reply so a reload cannot change what is removed.
	AcknowledgmentEnabled bool
	AcknowledgmentEmoji   string
	// AcknowledgmentConfigured distinguishes a Binding that explicitly selected the new lifecycle (including disabled)
	// from older direct session configurations that still use the queued marker.
	AcknowledgmentConfigured bool
}

// Auditor records agent calls: *audit.Writer. A failed write is logged, never fatal (spec 8.6).
type Auditor interface {
	AgentCall(ctx context.Context, a audit.Actor, c audit.Call) error
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
	Key Key
	// Binding is the name of the thread's Binding, namespace/name, which a renderer that hands a pause on
	// needs to find the Binding's policy.
	Binding string
	// MessageID names the message the output is posted as: the turn's, or, for the output of an
	// answer to a pause, the answer's.
	MessageID string
	// UserID is the Slack user who wrote the turn, which the renderer shows a stream to; SystemUser
	// for an alert's prompt, which has none.
	UserID string
	// Output is the Binding's output mode.
	Output v1alpha1.OutputMode
	// Progress is the Binding's chat.progress: how much of the agent's work the renderer shows before the text arrives.
	// The renderer ignores it when Output is final.
	Progress v1alpha1.ProgressMode
	// Recovered says the output continues a message that may exist already: the task was found
	// again after a restart, followed again after a lost stream, or looked up or sent again after
	// a send of unknown outcome. Some of it may be in the
	// thread, so the renderer updates what it posted before, or posts the whole. The output of an
	// answer to a pause is not that: its MessageID is the answer's own, a new message.
	Recovered bool
	// Restarted says the task was found again after a process restart. It is separate from Recovered because a lost-stream
	// follow continues the same output message but must keep the original pause retry window.
	Restarted bool
	// Feedback says which feedback buttons the final message of the output carries (spec 8.7): none for the
	// zero value, those of an investigation for a turn that nobody wrote (an alert's first prompt: its user is
	// SystemUser), and those of an answer for a person's turn on a Binding with feedback.enabled.
	Feedback store.FeedbackKind
	// ThreadReplies is the Binding's chat.threadReplies. In mention the final message of the output tells the thread to mention
	// Agent Kourier to ask a follow-up (D38); anything else, the empty value too, adds nothing.
	ThreadReplies v1alpha1.ThreadReplies
}

// NoticeKind is what a Notice tells the thread.
type NoticeKind string

// The kinds of notice.
const (
	// NoticeQueued asks for the reaction that marks a reply waiting for the active task (spec 8.2).
	NoticeQueued NoticeKind = "queued"
	// NoticeDequeued asks for that reaction to be taken off: the reply's turn was sent, or has ended some other
	// way, so it no longer waits. Its ID is the queued notice's it undoes, so a sink that puts the two in one
	// place finds the first from the second, whichever it sees first. It is raised only for a turn that may
	// carry the reaction.
	NoticeDequeued NoticeKind = "dequeued"
	// NoticeAcknowledged asks for the configured temporary acknowledgment reaction to be added to an accepted message.
	NoticeAcknowledged NoticeKind = "acknowledged"
	// NoticeUnacknowledged asks for the temporary acknowledgment reaction to be removed when processing ends.
	NoticeUnacknowledged NoticeKind = "unacknowledged"
	// NoticeStillBusy says a turn has waited, for a task of the session that Agent Kourier did not record, longer
	// than waitNoticeAfter: it is sent again after each backoff, and never dropped.
	NoticeStillBusy NoticeKind = "still_busy"
	// NoticeNotReady says the same of a turn the agent answered, for that long, that its session cannot take
	// input yet (agent.ErrNotReady): it is starting, or in a lifecycle operation.
	NoticeNotReady NoticeKind = "not_ready"
	// NoticeRetrying says the agent cannot be reached and Agent Kourier is trying again (spec 11). It follows the
	// second failed send in a row: the first is retried silently.
	NoticeRetrying NoticeKind = "retrying"
	// NoticeFailed says the turn failed and will not be sent; Detail says why.
	NoticeFailed NoticeKind = "failed"
	// NoticeSessionGone says the agent no longer has the thread's session: the next turn starts a
	// fresh one, and the agent's memory of the conversation is lost (spec 8.2).
	NoticeSessionGone NoticeKind = "session_gone"
	// NoticeBindingGone says the thread's Binding is no longer configured, so its turns cannot be
	// sent.
	NoticeBindingGone NoticeKind = "binding_gone"
	// NoticeExpired says the thread's session is past its threadTTL, so a reply in it cannot be
	// continued: a new mention starts a conversation (spec 8.2).
	NoticeExpired NoticeKind = "expired"
	// NoticeAnswerRejected says the agent rejected an answer and its task was cancelled (R17).
	NoticeAnswerRejected NoticeKind = "answer_rejected"
	// NoticeAnswerDropped says an answer arrived after its task had ended, and was not sent.
	NoticeAnswerDropped NoticeKind = "answer_dropped"
	// NoticeStopped says a person stopped a task that was not streaming, so there is nothing else
	// to show it (spec 8.2).
	NoticeStopped NoticeKind = "stopped"
	// NoticeWedged says a task ran past maxWorkingAge and was cancelled (R17).
	NoticeWedged NoticeKind = "wedged"
	// NoticeDuplicateRun says the agent ran a turn more than once (spec 8.2).
	NoticeDuplicateRun NoticeKind = "duplicate_run"
	// NoticeTaskFailed says the agent's task ended failed. Its ID is the task's, so the turn that sees
	// the end more than once, or a restart that follows the task again, raises one post.
	NoticeTaskFailed NoticeKind = "task_failed"
	// NoticeTaskRejected says the agent rejected the request, so its task ended with no answer.
	NoticeTaskRejected NoticeKind = "task_rejected"
	// NoticeTaskCanceled says the agent's task ended cancelled, such as when a person stopped it.
	NoticeTaskCanceled NoticeKind = "task_canceled"
	// NoticeNoOutput says the agent's task completed with nothing to show.
	NoticeNoOutput NoticeKind = "no_output"
	// NoticeTriggerRepeat says a botMessage trigger's key fired again, in the thread that holds it, and starts
	// no turn (spec 8.2). Detail is the time, which Agent Kourier made.
	NoticeTriggerRepeat NoticeKind = "trigger_repeat"
	// NoticeTriggerClosed says a botMessage trigger's key closed, such as an alert that resolved, in the thread
	// that holds it, and starts no turn. Detail is the time, which Agent Kourier made.
	NoticeTriggerClosed NoticeKind = "trigger_closed"
	// NoticeTriggerRefused says a botMessage trigger's start was refused, so the alert's thread has no
	// investigation. Detail names which value failed its check (".Fields.alertname failed the prompt check.")
	// and never the value: the name is the Binding's own, and the value is the message's, untrusted.
	NoticeTriggerRefused NoticeKind = "trigger_refused"
)

// Notice is something for the thread that no turn's output says. ID is stable, so a renderer
// that posts through the outbox posts it once.
type Notice struct {
	ID         string
	Kind       NoticeKind
	MessageRef string // the platform's ID of the message the notice is about
	Reaction   string // the bare reaction name for acknowledgment notices
	Detail     string // a short reason, fixed text: never agent output, never a secret
}

// ErrTurnContinues is the cause with which the context a Sink's Turn was given ends when the turn's task goes on without
// it: its stream was lost and the task is followed again, its send's outcome is unknown and the turn is looked up and
// followed or sent again, or the manager is shutting down and recovery follows the task after the restart. A later turn of the same message renders the rest, so a renderer leaves what the turn showed for
// that turn to take over. Any other end of that context ends the turn for good: the send failed, the agent did not take
// the turn, or the turn's events are over.
var ErrTurnContinues = errors.New("session: the turn's task continues under a later turn")

// Sink receives the output of turns; the renderer (WP-32) implements it. Turn returns the
// handler for one turn's events, FinalEvent included, or nil. The handler runs on the goroutine
// that reads the agent's stream, so it must not block: a slow renderer stalls the read, and
// can trip the inactivity watchdog into a needless reconnect. Notice must not block for long.
type Sink interface {
	Turn(ctx context.Context, t TurnRef) agent.Handler
	Notice(ctx context.Context, key Key, n Notice)
}

// NoThreadContext is the ThreadContext of a manager whose mentioned turns carry their own message alone: the platform
// cannot read a thread, or the deployment does not want it to. It adds no text, but the runner still reads where the
// window opens (one Store.PreviousReply per mentioned turn of a mention-only Binding) before it asks the hook, and a store
// that fails there holds the turn back like any other store failure of the runner.
func NoThreadContext(context.Context, Key, string, string, string) string { return "" }

// TextReplyOutcome says what the interaction module did with a queued reply.
type TextReplyOutcome uint8

// The outcomes of deciding a text reply. Errors always leave the reply queued for retry.
const (
	TextReplyRetry     TextReplyOutcome = iota
	TextReplyConsumed                   // decided or discarded; persist consumption before sending an answer
	TextReplyNotAnswer                  // leave an ordinary turn queued until the pause ends
	TextReplyWait                       // the question's prompt is not ready yet
)

// NoTextReply leaves ordinary turns queued while a task is paused. It is only for setups
// where no reply can be pinned to a question; pinned replies require the interaction module.
func NoTextReply(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
	return TextReplyNotAnswer, nil
}

// AnswerWatcher is told when a paused task is waiting again because the answer sent to it was not delivered. The caller of
// Manager.Answer learns only that the runner has the answer; the send happens after, and a send that fails reopens the pause
// without the sink hearing of it again. A watcher is how the caller that sent the answer learns that it did not arrive.
type AnswerWatcher interface {
	// AnswerReopened is called by the runner, on its goroutine, once the pause of taskID is parked again (so that a
	// Cancel or an Answer for it is accepted from the moment the call is made), for each answer that was not delivered:
	// answerID is the ID of the message that was sent. It is not called for an answer the agent took, one that ended the
	// task, or one that was rejected (the task is cancelled then). It must not block for long. told says that the watcher
	// will tell the thread itself, so that the manager does not post its notice that the answer could not be delivered,
	// which is worded for the person who gave it.
	AnswerReopened(ctx context.Context, key Key, taskID a2a.TaskID, answerID string) (told bool)
}

// Config configures a Manager. Store, Bindings, Audit, Users, Sink, Questions, TextReply, ThreadContext and OnTurnEnd are required:
// New refuses a Config without them and names the field, so that a wiring that leaves one out fails at start-up and
// not by doing nothing. Where nothing is a choice, there is a value that says so (NoThreadContext, NoTextReply).
type Config struct {
	// Context supplies tracing and other values. Cancellation belongs to Close.
	Context  context.Context
	Store    Store
	Bindings Bindings
	Users    Users
	Sink     Sink
	Now      func() time.Time
	Logger   *slog.Logger
	Audit    Auditor // a failure the manager must record is never dropped (spec 8.6 step 7)
	// Questions is asked, in a Binding whose threadReplies is mention, whether a thread has a pending question that
	// takes text replies, so that a typed answer needs no mention (D38). A value that finds none ignores every
	// unmentioned reply.
	Questions Questions
	// TextReply durably decides a queued answer through the interaction policy and audit.
	// It runs off the manager mutex and may hand an answer back through Answer. The answer channel
	// holds one item, so that re-entrant handoff never waits for the runner calling this handler.
	// A consumed reply is recorded before that answer is sent.
	TextReply func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error)
	// ThreadContext returns what a mentioned turn of a Binding with chat.threadReplies: mention carries of the
	// thread (spec 8.2, D38): the replies posted between the previous turn's message (after) and this one's
	// (before), both exclusive, already framed as untrusted data, or a fixed line saying they could not be read,
	// or "" when there is nothing to add. The window can be empty or inverted (after at or after before: two
	// mentions enqueued against ts order), and the hook returns "" for it. It never fails, and it may do I/O, so it
	// is called off every lock. The
	// runner calls it once for each turn it sends, in the send of a turn only: never for a task that is followed
	// again, found again or recovered. NoThreadContext adds nothing.
	ThreadContext func(ctx context.Context, key Key, binding, after, before string) string
	// After waits for a duration, and defaults to time.After. A test injects a clock.
	After func(time.Duration) <-chan time.Time
	// RetryBase and RetryMax are the first pause before a resend, which doubles up to RetryMax.
	// They default to 1 s and 60 s (spec 8.2).
	RetryBase, RetryMax time.Duration
	// MaxWorkingAge is how long a WORKING task may go without life, after a stream was lost,
	// before it is presumed wedged and cancelled (R17). Its age counts from the later of the
	// task's status timestamp and the last event Agent Kourier saw of it, so a task that streamed for
	// hours and then dropped is followed. It defaults to an hour, and never limits a live stream.
	MaxWorkingAge time.Duration
	// SendGiveUp is how long an unreachable agent is retried before the turn fails (spec 11).
	// It defaults to 15 minutes.
	SendGiveUp time.Duration
	// OnTurnEnd is called once for each turn that ends, with the Binding's name and how it ended, for a
	// metric. It must not block.
	OnTurnEnd func(binding string, outcome TurnOutcome)
	// AnswerWatcher, if set, is told of each answer that was not delivered (see AnswerWatcher). Nil tells no one, and the
	// manager's notice is the only word of it.
	AnswerWatcher AnswerWatcher
}

// attempt is a head turn that was sent and not yet taken and recorded. unknown says an earlier
// send of it ended with an unknown outcome, which only a lookup that finds the turn, or the turn
// being recorded, settles: a lookup that finds nothing does not, for the send may still land.
type attempt struct {
	id      string
	unknown bool
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
	// attempted holds, per conversation, the head turn that was sent and not yet taken and
	// recorded: a send of it again is a resend, and is looked up first.
	attempted map[Key]attempt
	// begun holds, per turn that has started and not ended, when its start was logged, for the
	// duration of its end line (see begin and finish). A turn that is retried is one entry.
	begun map[string]time.Time
	// fresh holds the turns this process enqueued itself, and has not yet sent and recorded: they
	// cannot have been sent before. Any other undelivered head may have been, by a process that died.
	// A turn leaves it when the agent takes it and it is recorded (accept), when it fails for good
	// (fail), and when it is dropped for a removed Binding. It stays while its row is queued in any
	// other way, such as a held unrecorded turn, which is what lets it be looked up first.
	fresh map[string]struct{}
	// marked holds the turns that may carry the queued reaction, which a turn gives up when it is sent or ends
	// (unqueue). A turn is in it from the notice that asks for the reaction, or, for a turn an earlier process may
	// have marked, from when the runner takes it up. Only those turns raise a NoticeDequeued: the sink is not asked
	// to take a reaction off every turn that never had one.
	marked map[string]struct{}
	closed bool // set by Close: no runner starts after it, so wg.Add cannot race wg.Wait, and no convos entry leaks
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
	case cfg.Audit == nil:
		return nil, errors.New("session: Audit is required")
	case cfg.Users == nil:
		return nil, errors.New("session: Users is required")
	case cfg.Sink == nil:
		return nil, errors.New("session: Sink is required")
	case cfg.Questions == nil:
		return nil, errors.New("session: Questions is required")
	case cfg.TextReply == nil:
		return nil, errors.New("session: TextReply is required (NoTextReply is only for setups without pinned replies)")
	case cfg.ThreadContext == nil:
		return nil, errors.New("session: ThreadContext is required (NoThreadContext adds nothing)")
	case cfg.OnTurnEnd == nil:
		return nil, errors.New("session: OnTurnEnd is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = time.Minute
	}
	if cfg.MaxWorkingAge <= 0 {
		cfg.MaxWorkingAge = time.Hour
	}
	if cfg.SendGiveUp <= 0 {
		cfg.SendGiveUp = 15 * time.Minute
	}
	m := &Manager{cfg: cfg, convos: map[Key]*convo{}, unrecorded: map[Key]string{}, attempted: map[Key]attempt{}, begun: map[string]time.Time{}, fresh: map[string]struct{}{}, marked: map[string]struct{}{}}
	base := cfg.Context
	if base == nil {
		base = context.Background()
	}
	m.ctx, m.stop = context.WithCancel(context.WithoutCancel(base))
	m.wake = m.start
	return m, nil
}

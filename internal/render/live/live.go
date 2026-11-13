// Package live renders one turn's output into a single chat message, which it keeps up to date while
// the agent works (spec 8.3). It speaks the chat-neutral model (package chat) and names no platform: what a
// message looks like, and what a platform can do, is the Platform's. Each turn has one goroutine that holds the
// latest state; the handler it gives out only stores and signals, so it never blocks the stream
// reader. Writes are throttled per message and coalesced: whatever the state is when the window
// opens is what is sent. The first post goes through the outbox, so a crash cannot post it twice
// (spec 8.6); later changes are updates, which are idempotent.
//
// Two paths write the message. For a chat thread, where the platform shows a stream to the person who
// asked, the turn streams: Stream, throttled Append of what is new, Stop with the outbox tag and the
// final message, and an Update after the stop only if what was streamed diverged from the final text (see
// stream.go). On a platform that has progress steps (Caps.Steps) a stream shows each tool call as a step, where in
// the text it was made: begun when the call is, finished when its result is. A step is only ever moved
// forward, so nothing has to be taken back, save a call that a pause waits to have approved, which ends as waiting
// (snapshot). Otherwise, such as in an alert thread, or on a platform that does not
// stream, the turn posts and updates. Its first post shows the latest tool call as a status line, in the past tense once
// its result has arrived (render.Turn says when the line is shown), since the outbox stores no steps; on a platform that has
// progress steps every update after it shows the tool calls as steps above the text, the latest maxListed of them, in place of
// the line, as a stream's do, and the final update keeps them. Neither path ever
// updates a message that is still streaming. Between tool calls the agent is silent and the model is deciding, which
// can take longer than a person waits, so a turn may show that it is working (Turn.Progress, spec 8.3): once a gap with
// nothing new passes Config.ActivityDelay, a Working… card begins in the stream, or in the message that is updated (a
// Working… line in its first post, or where there are no steps). The delay is one timer wake of the run, and it is the only
// write that comes of a clock, with the update that puts a first post's steps in place of its line: every other write
// follows an event. A turn whose context ends before its final event, as when the agent refuses the send or does
// not take an answer, is ended the way a failed task is: its stream is stopped and its status line goes. One whose task
// goes on under a later turn (session.ErrTurnContinues) is left for that turn to take over, or to end as this one would
// have if that turn ends with no final event and nothing of its own. Text is built at the last step, from the raw events
// (package render), and is literal: the platform makes it inert.
package live

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// defaultThrottle is the least time between two writes to one message, and defaultFinalDeadline how long the final text is tried
// for: Config.Throttle and Config.FinalDeadline when they are not set. The docs page of the limits holds them to these.
const (
	defaultThrottle      = 2 * time.Second
	defaultFinalDeadline = 3 * time.Minute
)

// maxSteps is the most steps a stream shows. A turn may call tools without end, and each step is a call to the
// platform and a card in the message: the calls after these are made, and not shown.
const maxSteps = 50

// maxBegins is the most steps one window begins: a platform's rate limit is a few calls a minute, and each step is
// a call to begin it, with another to finish it. A window that finds more waits for the next.
const maxBegins = 2

// maxListed is the most steps a message that is updated shows: the latest, under a line that counts the ones before them, so
// that a long run never crowds the text and the buttons out of the message.
const maxListed = 20

// Sender posts through the outbox: *outbox.Outbox.
type Sender interface {
	Send(ctx context.Context, p outbox.Post) (chat.Ref, error)
	Enqueue(ctx context.Context, p outbox.Post) (string, error)
}

// MentionAuditor records each person a turn's reply notified: *audit.Writer.
type MentionAuditor interface {
	Mention(ctx context.Context, a audit.Actor, m audit.Mention) error
}

// Config configures a Renderer. Sender, Platforms and Store are required: New refuses a Config without them.
type Config struct {
	Sender Sender
	// Audit records the mentions a turn makes. Nil records none.
	Audit MentionAuditor
	// Platforms gives the platform of a turn's connection (Turn.Connection) when the turn starts: it edits a message, runs a
	// stream, and says what the platform can do (Capabilities) and what an error means (Classify). A turn streams only if
	// Capabilities say Streams. A turn whose connection has no platform running (chat.ErrConnectionDown) is rendered by a
	// platform that fails every call with that error, which the turn treats as the platform being down: its writes are
	// tried again every Throttle, and not counted as attempts, and its reply goes through the outbox, whose row waits for the
	// connection. The turn asks for the connection's platform again when a write fails because the connection is down (or the
	// platform rejects its credential), and goes on through the one it gets (Run.reresolve), so a turn finishes through the
	// runtime of a connection that comes up, or is restarted, while it runs.
	Platforms chat.Resolver[chat.Platform]
	// Store is the outbox's: the renderer creates the stream's row itself, with no payload, and records its
	// ts the moment it has one.
	Store store.Outbox
	// Throttle is the least time between two writes to one message (default 2 seconds), and how often a write is tried again
	// while its connection is down.
	Throttle time.Duration
	// Attempts is how many times in a row a write may fail before it is given up (default 5). A
	// rate limit is not an attempt: Slack says when to come back, and the write waits for it. Nor is a write that failed
	// because the turn's connection is down (chat.ErrConnectionDown): it waits for the connection, up to FinalDeadline
	// for the final text.
	Attempts int
	// WriteTimeout bounds each write to Slack (default 30 seconds). A write never watches the
	// turn's context: a request cut short may still have been taken, and a retry would repeat it.
	WriteTimeout time.Duration
	// FinalDeadline is how long the final text is tried for, from the moment the run sees it
	// (default 3 minutes), the wait for a connection that is down included. It outlasts the turn's context, which ends when the
	// send returns.
	FinalDeadline time.Duration
	// ActivityDelay is how long a turn goes with nothing new (no text, and no tool call under way) before it shows that
	// it is working (default 3 seconds). It is the renderer's own setting, not the Binding's. Only the progress modes
	// steps-and-activity and activity use it.
	ActivityDelay time.Duration
	// Now and After are the clock, which a test injects: time.Now and time.After by default.
	Now    func() time.Time
	After  func(time.Duration) <-chan time.Time
	Logger *slog.Logger // slog.Default() if nil
}

// Renderer starts the rendering of turns. It is safe for concurrent use.
type Renderer struct {
	cfg  Config
	runs sync.WaitGroup // the goroutines of the turns started and not over
	// busyOff is set once the platform of a connection has said it cannot show a thread as busy (chat.ErrNoBusy): no turn of
	// that connection asks again, by the connection's name (the same app may lack the permission on one connection and have
	// it on another). Guarded by mu.
	busyOff map[string]*atomic.Bool
	// current is the latest run of each turn that has not ended, by the turn's Source: a run of a turn that one already
	// renders takes the message over from it (see Start).
	mu      sync.Mutex
	current map[string]*Run
	// left is the run of each turn that ended with its task going on and a message still showing it at work, by the turn's
	// Source, until a later run of the turn ends it or takes the message over (see endLeft).
	left map[string]*Run
}

// New returns a Renderer over cfg. It refuses a Config without its Sender, Platforms or Store, naming the field, so that a
// wiring that leaves one out fails at start-up (a Renderer with no Store would never stream, and nothing would say so).
func New(cfg Config) (*Renderer, error) {
	switch {
	case cfg.Sender == nil:
		return nil, errors.New("live: Config.Sender is required")
	case cfg.Platforms == nil:
		return nil, errors.New("live: Config.Platforms is required")
	case cfg.Store == nil:
		return nil, errors.New("live: Config.Store is required")
	}
	if cfg.Throttle <= 0 {
		cfg.Throttle = defaultThrottle
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = 5
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	if cfg.FinalDeadline <= 0 {
		cfg.FinalDeadline = defaultFinalDeadline
	}
	if cfg.ActivityDelay <= 0 {
		cfg.ActivityDelay = 3 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Renderer{cfg: cfg, busyOff: map[string]*atomic.Bool{}, current: map[string]*Run{}, left: map[string]*Run{}}, nil
}

// Wait returns once every turn started so far has ended, or with ctx's error if that comes first, so that
// a shutdown can wait for the final texts that are still being written, which a turn's own context does
// not stop. It is for after the turns have stopped being started: a turn started during Wait may or may
// not be waited for.
func (r *Renderer) Wait(ctx context.Context) error {
	over := make(chan struct{})
	go func() { r.runs.Wait(); close(over) }()
	select {
	case <-over:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("live: wait for the renders: %w", ctx.Err())
	}
}

// Turn is where one turn's output goes.
type Turn struct {
	Source string // the turn's ID, from which the outbox row of its reply is named
	// Connection is the ChatConnection ("namespace/name") whose app renders the turn: its platform is the one every call of the
	// turn goes to, and its name is on the outbox rows the turn makes.
	Connection      string
	Channel, Thread string
	// Mode is live, which updates the message as the turn runs, or final, which posts it once
	// when the turn ends.
	Mode v1alpha1.OutputMode
	// Progress is how much of the agent's work the turn shows before its text arrives (spec 8.3): a card or line per tool
	// call (steps), those and a Working… indicator in a gap (steps-and-activity), or one Working… indicator that counts
	// the steps and names no tool (activity). Empty, and a Mode of final, mean steps: a final turn shows nothing until
	// it ends.
	Progress v1alpha1.ProgressMode
	// Recipient is the person who asked, to whom Slack shows a stream in a channel. Empty, as in an
	// alert thread, takes the chat.update path.
	Recipient string
	// Recovered says the turn continues a message that may exist already, and is updated to the
	// whole text. A chat turn that finds its stream's row with the ts recorded stops that stream
	// (which it has lost the connection to) and updates the message, with its steps (takeOver). A row with no ts is the window
	// of R16: an orphan stream may exist and cannot be found, so the whole text is posted as a new
	// message, under its own row, and the orphan is left to Slack's timeout.
	// UNCONFIRMED(WP-04): whether Slack ends an orphaned stream, and what it answers to a stop of a
	// stream that has ended.
	Recovered bool
	// Footer is what the final message ends with, under its text: the follow-up hint of a mention Binding (D38) and the parts of
	// the feedback buttons (spec 8.7). It is added once, to the message the turn ends with, and to no message before it, and not
	// at all to a turn that ended with nothing to show, or with a question for the person to answer. The renderer does not look
	// into it.
	Footer []chat.Part
	// Binding is the name of the turn's Binding, namespace/name, which the record of a mention names.
	Binding string
	// Mentions are the people the Binding declares, and MaxMentions the most a turn notifies. A turn that ended with text
	// that names one posts a message of its own for each, after the reply (notifyMentions). Empty means none.
	Mentions    []v1alpha1.ChatMention
	MaxMentions int
}

// Run is the rendering of one turn.
type Run struct {
	cfg Config
	// platform is the platform of the turn's connection, resolved when the run started and again when a write fails because the
	// connection is down (reresolve), and caps what it can do and how much it shows, asked then. Only the goroutine uses them.
	platform runPlatform
	caps     chat.Caps
	turn     Turn
	wake     chan struct{} // a slot: the state changed
	done     chan struct{}
	// end ends the run's context, as the end of the turn's does: a later run of the same turn ends it so, with
	// session.ErrTurnContinues (see Start).
	end   context.CancelCauseFunc
	owner *Renderer // the Renderer that started the run, which keeps the run its turn left (endLeft)
	// earlier is the run of the same turn that was still rendering it when this one started, if any (Start).
	earlier *Run
	// eligible says the turn may show steps: it shows output as it goes, on a platform that has them, as a stream or as a
	// message that is updated in a mode that names tools (not activity). listed says it is the second, from the start: its
	// updates show the latest maxListed steps. Both are set at the start and never changed, which is why Handle may read
	// them; streaming, which a takeover clears, is the goroutine's.
	eligible, listed bool
	// progress is the Turn's, reduced to what the run does: steps unless the mode is one that shows activity and the
	// turn shows output as it goes. timed says the run keeps time: it has an activity mode and something to show it
	// with (a stream that has steps, or a message that is updated). Both are set at the start and never changed.
	progress v1alpha1.ProgressMode
	timed    bool
	// busy marks the thread as busy while the turn runs: only a turn on the update path in a mode that shows activity has one.
	busy *busyMark

	mu        sync.Mutex // guards the state below
	texts     map[a2a.ArtifactID]string
	order     []a2a.ArtifactID
	tool      *agent.ToolCallEvent // the latest call made, if any: the update path's status line is about it
	toolState chat.ToolState       // how far the latest call has got: under way, or how it ended
	steps     []stepRec            // the tool calls of a stream, or of a listed turn, in the order they were made
	// dropped is how many steps a listed turn no longer keeps, the oldest, to keep maxListed; gone is the IDs of their calls,
	// so that a call reported again is not shown as a new one.
	dropped int
	gone    map[string]bool
	final   bool
	// paused says the final event was a pause for input, not the end of the task: the reply so far is a question, and no answer to
	// rate.
	paused bool
	// endedBadly says the final event's task failed, was cancelled or was rejected: what it shows is no reply, so it notifies nobody.
	endedBadly bool
	// awaiting is the call IDs of the tool calls that the pause asks a person to approve (FinalEvent.AwaitingApproval):
	// their steps end as waiting, not as how the call went so far.
	awaiting []string

	// Below: only a run that is timed keeps these.
	started  time.Time       // when the run started
	gapAt    time.Time       // when the gap began: the last text, the last tool result with none under way, or the start
	gapSeq   int             // which gap it is: it changes each time a gap begins
	gapOpen  bool            // the gap has not yet shown that the agent is working, and may
	idle     bool            // the message that is updated shows Working… (steps-and-activity)
	texted   bool            // text that shows has arrived
	calls    map[string]bool // the tool calls that have an ID: true once the result has arrived
	anon     bool            // the latest event was a tool call with no ID, which no result can end: it is under way until the next event
	inflight int             // calls with an ID that have no result yet
	finished int             // tool calls whose result has arrived
	failed   int             // and those that report a failure
	updates  int             // working updates (agent.ProgressEvent): the activity mode counts them until a step is counted
	note     string          // the agent's progress line, which stands for Working… until the next event (steps-and-activity)
	begun    bool            // the activity mode has begun to show: its card, or its line

	// Below: the goroutine's alone.
	deadline <-chan time.Time // the final text's own deadline, once the final is seen
	sends    int              // Sends tried
	stale    bool             // the message may hold older content than the last Send was given

	sent      string // what is shown, as want says
	streaming bool   // this turn streams
	rowMade   bool   // the stream's outbox row exists
	recorded  bool   // and holds the stream's ts
	stopped   bool
	adoptStop bool        // the message is a stream of an earlier run, to stop before it is updated
	adopted   bool        // the message is a stream of an earlier run, which this turn took over and updates
	diverged  bool        // the streamed text is not a prefix of the final text
	stream    chat.Stream // the stream this turn started
	streamed  string      // the markdown sent to the stream so far
	stepsSent []chat.Step // the steps sent to the stream, or shown by an update, each as it was last sent
	stepsOff  bool        // the platform refused a step, or a message with steps, for good: the turn goes on without steps
	stepFails int         // steps that failed in a row
	holdsText bool        // the platform refused an update with no text (ErrWouldDropText): no update is made until there is some
	more      bool        // a write left steps to begin, which the next window goes on with
	// cutShort says the turn's context ended before its final event, and the run is ending what the turn showed (see cut).
	cutShort bool
	// continues says the turn's context ended with session.ErrTurnContinues, and the run left what it showed for a later
	// turn; ref is the message as the run left it, which a later run that ends it runs the loop on again (endLeft).
	continues bool
	ref       chat.Ref
}

// Start begins rendering a turn and returns it. Its goroutine ends once the final text is
// written, or given up on, or when ctx ends: a turn that has shown something by then is ended first, as a task
// that failed is (see cut).
//
// A turn that a run of this Renderer still renders, under the same Source, is taken over: the earlier run's context is
// ended with session.ErrTurnContinues, so it leaves what it showed, and the new one writes nothing until the earlier has
// finished, so that the two never write the message at once and the earlier, which knows less, never writes over the
// later. The session does this when it follows a task again under the turn whose send has not yet returned.
func (r *Renderer) Start(ctx context.Context, t Turn) *Run {
	var platform runPlatform
	var caps chat.Caps
	if p, err := r.cfg.Platforms.Platform(t.Connection); err != nil {
		r.cfg.Logger.WarnContext(ctx, "live: the turn's connection is not running; its writes fail until it is, and its reply waits in the outbox", "connection", t.Connection, "channel", t.Channel, "err", err)
		d := down{conn: t.Connection, err: err}
		platform, caps = d, d.Capabilities()
	} else {
		platform, caps = p, p.Capabilities()
	}
	streaming := t.Mode == v1alpha1.OutputLive && t.Recipient != "" && caps.Streams
	u := &Run{cfg: r.cfg, platform: platform, caps: caps, owner: r, turn: t, streaming: streaming, wake: make(chan struct{}, 1), done: make(chan struct{}), texts: map[a2a.ArtifactID]string{}, progress: v1alpha1.ProgressSteps}
	if t.Mode == v1alpha1.OutputLive && (t.Progress == v1alpha1.ProgressStepsAndActivity || t.Progress == v1alpha1.ProgressActivity) {
		u.progress = t.Progress
	}
	u.eligible = caps.Steps && t.Mode == v1alpha1.OutputLive && (streaming || u.progress != v1alpha1.ProgressActivity)
	if u.listed = u.eligible && !streaming; u.listed {
		u.gone = map[string]bool{}
	}
	if caps.Busy && !streaming && u.progress != v1alpha1.ProgressSteps {
		u.busy = &busyMark{m: platform, off: r.busyOffOf(t.Connection), at: chat.Target{Conn: t.Connection, Channel: t.Channel, Thread: t.Thread}, run: u}
	}
	if u.timed = u.progress != v1alpha1.ProgressSteps && (!streaming || caps.Steps); u.timed {
		u.started = r.cfg.Now()
		u.gapAt, u.gapOpen, u.gapSeq, u.calls = u.started, true, 1, map[string]bool{}
	}
	ctx, u.end = context.WithCancelCause(ctx)
	r.mu.Lock()
	prev := r.current[t.Source]
	r.current[t.Source] = u
	r.mu.Unlock()
	u.earlier = prev
	if prev != nil {
		prev.end(session.ErrTurnContinues)
	}
	r.runs.Add(1)
	go func() {
		defer r.runs.Done()
		defer close(u.done) // once the run is forgotten, or left for a later run, so that the later one finds it
		defer u.end(nil)
		defer r.forget(t.Source, u)
		if prev != nil {
			select { // a run that the earlier one would hold up for its final deadline need not wait once its own turn is over
			case <-prev.done:
			case <-ctx.Done():
			}
		}
		u.run(ctx)
		u.earlier = nil // a run left for a later one holds no chain of the runs before it
	}()
	return u
}

// busyOffOf is the flag of the connection conn that its platform cannot show a thread as busy.
func (r *Renderer) busyOffOf(conn string) *atomic.Bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	off, ok := r.busyOff[conn]
	if !ok {
		off = new(atomic.Bool)
		r.busyOff[conn] = off
	}
	return off
}

// forget drops u as the current run of its turn, unless a later run has taken its place. A run that left a message showing
// its turn at work, for the turn its task goes on under, is kept as the turn's left run; a run whose turn is over has ended
// the left one, or taken its message over, and drops it. One that continues with nothing shown keeps the left run as it is.
func (r *Renderer) forget(source string, u *Run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current[source] == u {
		delete(r.current, source)
	}
	switch {
	case !u.continues:
		delete(r.left, source)
	case u.ref.ID != "" || u.streamOpen():
		r.left[source] = u
	}
}

// takeLeft returns the left run of the turn source, if there is one, and drops it.
func (r *Renderer) takeLeft(source string) *Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.left[source]
	delete(r.left, source)
	return u
}

// Done is closed when the goroutine has ended.
func (u *Run) Done() <-chan struct{} { return u.done }

// Handle takes an event of the turn. It is the agent.Handler: it stores the event and signals,
// and never blocks, so it may be called from the stream's reader. After the end it does nothing.
func (u *Run) Handle(ev agent.Event) {
	select {
	case <-u.done:
		return
	default:
	}
	var now time.Time
	if u.timed { // only a run that keeps time reads the clock, which a test may hold
		now = u.cfg.Now()
	}
	var line string // a working update's progress line, redacted before the lock: only steps-and-activity shows one
	if p, ok := ev.(agent.ProgressEvent); ok && u.timed && u.progress == v1alpha1.ProgressStepsAndActivity {
		line = render.ProgressLine(p.Text)
	}
	u.mu.Lock()
	u.note = "" // the progress line stands until the next event, whatever it is
	switch e := ev.(type) {
	case agent.TextEvent:
		if _, seen := u.texts[e.ArtifactID]; !seen {
			u.order = append(u.order, e.ArtifactID)
		}
		u.texts[e.ArtifactID] = e.Text
		if u.timed && (u.texted || strings.TrimSpace(chat.Clean(e.Text, false)) != "") { // text that shows, once there is some
			u.texted, u.idle, u.anon = true, false, false
			u.endWorking(now, render.EndedByText)
			u.reopenGap(now)
		}
	case agent.ToolCallEvent:
		u.noteCall(e)
		u.beginStep(e)
		if u.timed {
			u.callMade(e, now)
		}
	case agent.ToolResultEvent:
		if u.tool != nil && u.tool.CallID == e.CallID && u.toolState == chat.ToolCalling {
			u.toolState = chat.ToolRan
			if render.Failed(e.Response) {
				u.toolState = chat.ToolFailed
			}
		}
		u.endStep(e)
		if u.timed {
			u.resultIn(e, now)
		}
	case agent.ProgressEvent:
		if u.timed {
			u.progressIn(line, now)
		}
	case agent.FinalEvent:
		u.final = true
		u.paused = e.Task != nil && (e.Task.Status.State == a2a.TaskStateInputRequired || e.Task.Status.State == a2a.TaskStateAuthRequired)
		u.endedBadly = endingOf(e.Task) == render.EndedFailed
		if u.paused {
			u.awaiting = e.AwaitingApproval
		}
		if u.timed {
			u.gapOpen = false
			u.endWorking(now, endingOf(e.Task))
		}
	}
	u.mu.Unlock()
	select {
	case u.wake <- struct{}{}:
	default: // a signal is waiting already
	}
}

// endingOf is how a task's end closes a card that stands for time: by the state the task was left in. A final with no task is
// a reply that was one message, which completed.
func endingOf(task *a2a.Task) render.Ending {
	if task == nil {
		return render.EndedCompleted
	}
	switch task.Status.State {
	case a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired:
		return render.EndedPaused
	case a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
		return render.EndedFailed
	}
	return render.EndedCompleted
}

// run writes the state each time its window opens, until the final text is out.
func (u *Run) run(ctx context.Context) {
	defer func() { u.busy.clear(ctx) }() // on every way out: the turn is over, and the thread is not busy
	u.busy.set(ctx)
	var finalOp *telemetry.Operation
	finalCtx := ctx
	writes := 0
	finalOutcome := "cancelled"
	defer func() {
		if finalOp == nil && u.isFinal() {
			finalCtx, finalOp = telemetry.Start(context.WithoutCancel(ctx), "agentkourier.render.finalize", attribute.String("agentkourier.render.mode", string(u.turn.Mode)), attribute.String("agentkourier.turn.id", u.turn.Source))
		}
		if u.cutShort && (finalOutcome == "delivered" || finalOutcome == "empty_output") { // no final came: what was written ends the turn, and is no output delivered
			finalOutcome = "cancelled"
		}
		if finalOutcome == "delivered" {
			u.notifyMentions(ctx)
		}
		if finalOp != nil {
			trace.SpanFromContext(finalCtx).SetAttributes(attribute.Int("agentkourier.render.attempts", writes))
			telemetry.Count(ctx, "agentkourier_render_final_total", finalOutcome)
			var failure error
			switch finalOutcome {
			case "deadline_exceeded", "permanent_failure", "retry_exhausted":
				failure = errors.New(finalOutcome)
			}
			finalOp.FinishOutcome(failure, finalOutcome)
		}
	}()
	ref := u.ref // the message, once there is one: the one an earlier pass left, when the run is ending it (endLeft)
	defer func() { u.ref = ref }()
	var next time.Time
	fails := 0
	_, startedDown := u.platform.(down)
	downLogged := startedDown // the connection's being down has been logged (Start does, for a turn that began so), for the spell that goes on
	shown := false            // the final text is in the thread: what is left to write only adds its steps
	// giveUp ends the run on a final write that will not go through. Once the final text is shown it is the steps that were
	// not added, and the turn was delivered: the alert on failed finals is not for them.
	giveUp := func(outcome, msg string, args ...any) {
		args = append([]any{"channel", u.turn.Channel}, args...)
		if shown {
			finalOutcome = "delivered"
			u.cfg.Logger.WarnContext(ctx, "live: the steps could not be added to the reply; what was posted stands", args...)
			return
		}
		finalOutcome = outcome
		u.cfg.Logger.ErrorContext(ctx, msg, args...)
	}
	var gap <-chan time.Time // the delay timer of the open gap, if the turn keeps time
	gapSeq := 0              // and the gap it is for
	for {
		gap, gapSeq = u.arm(gap, gapSeq)
		select {
		case <-ctx.Done():
			if !u.isFinal() && !u.cut(ctx, ref) { // a turn that never ended owes nothing, but what it showed
				return
			}
		case <-u.wake:
		case <-gap: // nothing new for the delay: the turn shows that it is working
			gap = nil
			u.openIdle(gapSeq, u.cfg.Now())
		}
		for {
			if u.isFinal() && finalOp == nil {
				finalCtx, finalOp = telemetry.Start(context.WithoutCancel(ctx), "agentkourier.render.finalize", attribute.String("agentkourier.render.mode", string(u.turn.Mode)), attribute.String("agentkourier.turn.id", u.turn.Source))
			}
			if d := next.Sub(u.cfg.Now()); d > 0 && !u.sleep(ctx, d) {
				if !u.isFinal() && u.cut(ctx, ref) {
					continue
				}
				if u.isFinal() {
					giveUp("deadline_exceeded", "live: the final text was not written before its deadline")
				}
				return
			}
			v := u.snapshot() // after the wait, so that what is sent is the latest
			if u.holdsText && v.body == "" {
				if !v.final {
					break
				}
				finalOutcome = "empty_output" // nothing of this turn's was written
				return
			}
			if !v.ok && v.final { // the turn ended with nothing to show
				// A stream that was started stands as a message too, though ref is the update path's alone: it was started for a step
				// that the platform then refused, and is empty. It is stopped with the text of a turn with no output, as the update
				// path's message is, or it is left open for good.
				if ref.ID == "" && !u.streamOpen() && !u.adopt(ctx, &ref) {
					finalOutcome = "empty_output"
					return
				}
				// But a message stands: do not leave its tool line up. Nothing was shown, so there is nothing to rate (Turn.Footer).
				v.m, v.ok, v.rated = render.Nothing(), true, false
				if u.streaming { // a stream's text is its body. A message's body stays empty: the placeholder is no text of the turn's
					v.body = render.NoOutput
				}
			}
			if !v.ok || (u.turn.Mode == v1alpha1.OutputFinal && !v.final) {
				break
			}
			if u.want(v) == u.sent {
				if v.final {
					finalOutcome = "delivered"
					return
				}
				break
			}
			writes++
			writeCtx := ctx
			if finalOp != nil {
				writeCtx = finalCtx
			}
			err := u.write(writeCtx, &ref, v)
			if errors.Is(err, errMessageStands) { // the next pass sees holdsText, and waits for text or ends the turn
				continue
			}
			next = u.cfg.Now().Add(u.cfg.Throttle)
			if errors.Is(err, errNothingToShow) { // a turn that moved to the update path, and ended with nothing to post
				finalOutcome = "empty_output"
				return
			}
			if err != nil {
				verdict := u.platform.Classify(chat.OpPost, err)
				down := errors.Is(err, chat.ErrConnectionDown)
				switch {
				case errors.Is(err, store.ErrInvalid), errors.Is(err, chat.ErrInvalid), errors.Is(err, outbox.ErrAbandoned): // retrying cannot mend them
					giveUp("permanent_failure", "live: the message cannot be sent; giving up", "err", err)
					return
				case verdict.RetryAfter > 0:
					u.cfg.Logger.WarnContext(ctx, "live: the platform rate-limited the message; waiting as it asked", "channel", u.turn.Channel, "retry_after", verdict.RetryAfter)
					next = u.cfg.Now().Add(max(u.cfg.Throttle, verdict.RetryAfter))
					continue
				}
				// The platform the run holds is not the connection's now: none ran the connection when the turn started, or the runtime
				// it holds was retired, or its credential was rejected after a restart halted it, which no report can end for the
				// runtime. The connection may run on another platform by the next try.
				waiting := down
				if down || verdict.Class == chat.FatalAuth {
					// A rejected credential whose connection is down is the gap of a restart: the old runtime is out of the table
					// and the new one is not in it yet, so the try is a wait for the connection, not an attempt. Were the new one never
					// to come up (its Secret cannot be used), each try is one real call with the revoked token, every Throttle: bounded by
					// the final deadline and the turn's end, as any wait is.
					if lookup := u.reresolve(ctx, &ref); verdict.Class == chat.FatalAuth && errors.Is(lookup, chat.ErrConnectionDown) {
						waiting = true
					}
				}
				if waiting {
					// Waiting for the connection is no attempt, as a rate limit is none: a restart of a connection takes longer than
					// Attempts tries. It is bounded by the final's deadline (sleep) and by the turn's end.
					if !downLogged {
						u.cfg.Logger.WarnContext(ctx, "live: the turn's connection is not running; its writes are tried again until it is", "connection", u.turn.Connection, "channel", u.turn.Channel, "err", err)
						downLogged = true
					}
					continue
				}
				downLogged = false
				u.cfg.Logger.WarnContext(ctx, "live: writing the message failed; it will be tried again", "channel", u.turn.Channel, "err", err)
				if fails++; fails >= u.cfg.Attempts {
					if v.final {
						giveUp("retry_exhausted", "live: giving up on a message")
						return
					}
					u.cfg.Logger.ErrorContext(ctx, "live: giving up on a message", "channel", u.turn.Channel)
					fails = 0
					break
				}
				continue
			}
			fails, downLogged = 0, false
			if u.stale {
				u.stale, u.sent = false, "" // the post may be the old message: write once more
				continue
			}
			shown = shown || v.final
			if u.more { // steps are left to begin, or to show in place of a post's line: the next window, which the throttle spaces, goes on with them
				u.more = false
				if !v.final { // the post may have taken the mark off, and the next write is a window away; a final's is the last
					u.busy.set(ctx)
				}
				continue
			}
			if v.final {
				finalOutcome = "delivered"
				return
			}
			u.busy.set(ctx) // a post in the thread may have taken the mark off; this rides the write just made and adds none
			break
		}
	}
}

// cut ends a turn whose context ended before its final event, and says whether the run has a write to make for it. The send
// failed, or the agent did not take the turn (the session tells the thread so with a notice of its own): no event will come.
// A turn whose task goes on under a later turn (the cause is session.ErrTurnContinues: a lost stream that is followed again,
// a send of unknown outcome that is tried again, a shutdown that recovery follows, or a later run of the turn in this process) leaves what it showed for that turn, which
// takes the message over, and writes nothing. A turn that has shown nothing owes nothing. One that has shown something is made
// final, as a task that failed is: an unclaimed Working… card and the activity card end as Stopped, a step with no result is
// ended, and the update path's status line goes, so that nothing is left saying that the agent is at work. A stream is
// stopped, and a message is updated, by the writes of a final, with no footer: nothing ended that could be rated. A message
// that already shows what the final would makes no write. It is the goroutine's.
func (u *Run) cut(ctx context.Context, ref chat.Ref) bool {
	var now time.Time
	if u.timed {
		now = u.cfg.Now()
	}
	u.continues = errors.Is(context.Cause(ctx), session.ErrTurnContinues)
	u.mu.Lock()
	switch {
	case u.final: // the final came after all: the run writes it
		u.mu.Unlock()
		return true
	case u.continues:
		u.mu.Unlock()
		return false
	case ref.ID == "" && !u.streamOpen():
		u.mu.Unlock()
		u.endLeft(ctx)
		return false
	}
	u.final, u.paused, u.gapOpen = true, false, false
	u.endWorking(now, render.EndedFailed)
	u.mu.Unlock()
	u.cutShort = true
	v := u.snapshot()
	return !v.ok || u.want(v) != u.sent // with nothing left to show, the message is replaced by "(no output)" (run)
}

// endLeft ends what an earlier run of a recovered turn left when its task went on, for a turn that ends with no final event
// and nothing of its own: the follow or the resend was refused, or the turn ended without driving the task, and the session
// tells the thread why.
// The earlier run is over, and its state is as it left it: its loop runs again, here, on this run's context, which has ended
// for good, so it is cut as it would have been had its task not gone on, its text kept and its stream stopped. A turn that is
// not recovered has no earlier run, and neither has one whose earlier run left nothing, or was in another process. With no
// earlier run here, the message the turn's row records may still be a stream that another process left: a turn that streams
// stops it, which keeps its text (stopEarlier). It is the goroutine's.
func (u *Run) endLeft(ctx context.Context) {
	if !u.turn.Recovered {
		return
	}
	if u.earlier != nil { // it may be the one to leave the message, once it has ended
		<-u.earlier.done
	}
	if left := u.owner.takeLeft(u.turn.Source); left != nil {
		<-left.done
		left.run(ctx)
		return
	}
	if u.turn.Recipient != "" { // only a turn shown to a person streams
		u.stopEarlier(ctx)
	}
}

// stopEarlier stops the stream that the turn's outbox row records, if it still runs, for a recovered turn that ends with nothing
// of its own and no earlier run of this Renderer to end what was shown: another process started it and left it for the task that
// went on. Its state is not known here, so the stop carries nothing: the stream keeps its text, and the platform shows a card that
// was under way as ended (Slack shows it as an error). A message that has stopped already answers ErrNotStreaming. A turn with no
// Recipient never streams, and its message, such as an alert thread's, is left as it is (endLeft), since an update would have to
// replace what it says. It is tried once: a stream that cannot be stopped now is left to the platform. It is the goroutine's.
func (u *Run) stopEarlier(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.WriteTimeout)
	defer cancel()
	id := outbox.ID(u.turn.Source, store.OutboxReply)
	row, err := u.cfg.Store.GetOutbox(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return
	case err != nil:
		u.cfg.Logger.WarnContext(ctx, "live: the earlier message of a recovered turn could not be read; it is left as it is", "channel", u.turn.Channel, "err", err)
		return
	case row.MessageRef == "":
		return
	}
	err = u.platform.StreamAt(u.platform.RefAt(u.turn.Channel, u.turn.Thread, row.MessageRef)).Stop(ctx, "", chat.Message{}, id)
	if err != nil && !errors.Is(err, chat.ErrNotStreaming) {
		u.cfg.Logger.WarnContext(ctx, "live: the stream an earlier process left could not be stopped; it is left to the platform", "channel", u.turn.Channel, "err", err)
	}
}

// adopt finds the message that an earlier run of a recovered turn left, for a turn that ends with nothing to show of its own:
// that run's stream, left open when its task went on, or its message, with its Working… or tool line. It makes the message
// this run's to end, stopped if it streams and updated, as takeOver does, and says whether there is one. A turn that is not
// recovered has no earlier run. It is the goroutine's.
func (u *Run) adopt(ctx context.Context, ref *chat.Ref) bool {
	if !u.turn.Recovered {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.WriteTimeout)
	defer cancel()
	row, err := u.cfg.Store.GetOutbox(ctx, outbox.ID(u.turn.Source, store.OutboxReply))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false
	case err != nil:
		u.cfg.Logger.WarnContext(ctx, "live: the earlier message of a recovered turn could not be read; it is left as it is", "channel", u.turn.Channel, "err", err)
		return false
	case row.MessageRef == "":
		return false
	}
	u.streaming = false
	*ref, u.adoptStop = u.platform.RefAt(u.turn.Channel, u.turn.Thread, row.MessageRef), true
	return true
}

// streamOpen says a stream this turn started has not been stopped. It is the goroutine's.
func (u *Run) streamOpen() bool { return u.streaming && u.stream != nil && !u.stopped }

// isFinal says whether the turn's final event has arrived.
func (u *Run) isFinal() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.final
}

// sleep waits d, and says whether the run goes on. Only the waits watch the turn's context, and
// only until the final event is in: from then on the run keeps writing, for the FinalDeadline,
// because the task is over and nothing else will say what it said.
func (u *Run) sleep(ctx context.Context, d time.Duration) bool {
	timer := u.cfg.After(d)
	if !u.isFinal() {
		select {
		case <-timer:
			return true
		case <-ctx.Done():
			if !u.isFinal() {
				return false
			}
		}
	}
	if u.deadline == nil {
		u.deadline = u.cfg.After(u.cfg.FinalDeadline)
	}
	select {
	case <-timer:
		return true
	case <-u.deadline:
		return false
	}
}

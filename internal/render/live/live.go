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
// stream, the turn posts and updates, and shows the latest tool call as a status line, in the past tense once its result
// has arrived (render.Turn says when the line is shown). Neither path ever
// updates a message that is still streaming. Between tool calls the agent is silent and the model is deciding, which
// can take longer than a person waits, so a turn may show that it is working (Turn.Progress, spec 8.3): once a gap with
// nothing new passes Config.ActivityDelay, a Working… card begins in the stream, or a Working… line in the message
// that is updated. The delay is one timer wake of the run, and it is the only write that comes of a clock: every other
// write follows an event. A turn whose context ends before its final event, as when the agent refuses the send or does
// not take an answer, is ended the way a failed task is: its stream is stopped and its status line goes. One whose task
// goes on under a later turn (session.ErrTurnContinues) is left for that turn to take over, or to end as this one would
// have if that turn ends with no final event and nothing of its own. Text is built at the last step, from the raw events
// (package render), and is literal: the platform makes it inert.
package live

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// maxSteps is the most steps a stream shows. A turn may call tools without end, and each step is a call to the
// platform and a card in the message: the calls after these are made, and not shown.
const maxSteps = 50

// maxBegins is the most steps one window begins: a platform's rate limit is a few calls a minute, and each step is
// a call to begin it, with another to finish it. A window that finds more waits for the next.
const maxBegins = 2

// Sender posts through the outbox: *outbox.Outbox.
type Sender interface {
	Send(ctx context.Context, p outbox.Post) (chat.Ref, error)
}

// Config configures a Renderer. Sender, Platform and Store are required: New refuses a Config without them.
type Config struct {
	Sender Sender
	// Platform edits a message, runs a stream, and says what the platform can do (Capabilities) and what an
	// error means (Classify). A turn streams only if Capabilities say Streams.
	Platform chat.Platform
	// Store is the outbox's: the renderer creates the stream's row itself, with no payload, and records its
	// ts the moment it has one.
	Store store.Outbox
	// Throttle is the least time between two writes to one message (default 2 seconds).
	Throttle time.Duration
	// Attempts is how many times in a row a write may fail before it is given up (default 5). A
	// rate limit is not an attempt: Slack says when to come back, and the write waits for it.
	Attempts int
	// WriteTimeout bounds each write to Slack (default 30 seconds). A write never watches the
	// turn's context: a request cut short may still have been taken, and a retry would repeat it.
	WriteTimeout time.Duration
	// FinalDeadline is how long the final text is tried for, from the moment the run sees it
	// (default 3 minutes). It outlasts the turn's context, which ends when the send returns.
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
	caps chat.Caps      // what the platform can do and how much it shows, asked once
	runs sync.WaitGroup // the goroutines of the turns started and not over
	// busyOff is set once the platform has said it cannot show a thread as busy (chat.ErrNoBusy): no turn asks again.
	busyOff atomic.Bool
	// current is the latest run of each turn that has not ended, by the turn's Source: a run of a turn that one already
	// renders takes the message over from it (see Start).
	mu      sync.Mutex
	current map[string]*Run
	// left is the run of each turn that ended with its task going on and a message still showing it at work, by the turn's
	// Source, until a later run of the turn ends it or takes the message over (see endLeft).
	left map[string]*Run
}

// New returns a Renderer over cfg. It refuses a Config without its Sender, Platform or Store, naming the field, so that a
// wiring that leaves one out fails at start-up (a Renderer with no Store would never stream, and nothing would say so).
func New(cfg Config) (*Renderer, error) {
	switch {
	case cfg.Sender == nil:
		return nil, errors.New("live: Config.Sender is required")
	case cfg.Platform == nil:
		return nil, errors.New("live: Config.Platform is required")
	case cfg.Store == nil:
		return nil, errors.New("live: Config.Store is required")
	}
	if cfg.Throttle <= 0 {
		cfg.Throttle = 2 * time.Second
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = 5
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	if cfg.FinalDeadline <= 0 {
		cfg.FinalDeadline = 3 * time.Minute
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
	return &Renderer{cfg: cfg, caps: cfg.Platform.Capabilities(), current: map[string]*Run{}, left: map[string]*Run{}}, nil
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
	Source          string // the turn's ID, from which the outbox row of its reply is named
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
}

// Run is the rendering of one turn.
type Run struct {
	cfg  Config
	caps chat.Caps
	turn Turn
	wake chan struct{} // a slot: the state changed
	done chan struct{}
	// end ends the run's context, as the end of the turn's does: a later run of the same turn ends it so, with
	// session.ErrTurnContinues (see Start).
	end   context.CancelCauseFunc
	owner *Renderer // the Renderer that started the run, which keeps the run its turn left (endLeft)
	// earlier is the run of the same turn that was still rendering it when this one started, if any (Start).
	earlier *Run
	// eligible says the turn may show steps: it began as a stream, on a platform that has them. It is set at the
	// start and never changed, which is why Handle may read it; streaming, which a takeover clears, is the goroutine's.
	eligible bool
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
	steps     []stepRec            // the tool calls of a stream, in the order they were made
	final     bool
	// paused says the final event was a pause for input, not the end of the task: the reply so far is a question, and no answer to
	// rate.
	paused bool
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
	updates  int             // working updates (agent.ProgressEvent): the activity mode counts them while no tool event has come
	tooled   bool            // a tool call or result has come
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
	stepsSent []chat.Step // the steps sent to the stream, each as it was last sent
	stepsOff  bool        // the platform refused a step, or a message with steps, for good: the turn goes on without steps
	stepFails int         // steps that failed in a row
	more      bool        // a write left steps to begin, which the next window goes on with
	// cutShort says the turn's context ended before its final event, and the run is ending what the turn showed (see cut).
	cutShort bool
	// continues says the turn's context ended with session.ErrTurnContinues, and the run left what it showed for a later
	// turn; ref is the message as the run left it, which a later run that ends it runs the loop on again (endLeft).
	continues bool
	ref       chat.Ref
}

// stepRec is a tool call of a stream, which is a step: the call, how it ended, and where in the text it was made.
type stepRec struct {
	call  agent.ToolCallEvent
	state chat.StepState
	after int // how many text artifacts there were when the call was made: the step follows their text
	kind  stepKind
	// ending is how a card that stands for time ended; NotEnded while it is under way. Only a card under way can be taken over.
	ending render.Ending
	// An activity card is ended with what it counted by then, and when.
	counts  render.Counts
	endedAt time.Time
}

// stepKind is what a step stands for. The zero value is a tool call.
type stepKind int

const (
	// callStep is a tool call: the call, and how it ended.
	callStep stepKind = iota
	// workingStep is a Working… card for a gap. The next tool call takes it over and it becomes that call's step; text, or the
	// end of the turn, ends it under a title of its own (render.WorkingEnded), which a tool call after the text changes.
	workingStep
	// activityStep is the one card of the activity mode, which shows counts and no call; it ends at the first text or the end
	// of the turn, with what it counted then.
	activityStep
)

// Start begins rendering a turn and returns it. Its goroutine ends once the final text is
// written, or given up on, or when ctx ends: a turn that has shown something by then is ended first, as a task
// that failed is (see cut).
//
// A turn that a run of this Renderer still renders, under the same Source, is taken over: the earlier run's context is
// ended with session.ErrTurnContinues, so it leaves what it showed, and the new one writes nothing until the earlier has
// finished, so that the two never write the message at once and the earlier, which knows less, never writes over the
// later. The session does this when it follows a task again under the turn whose send has not yet returned.
func (r *Renderer) Start(ctx context.Context, t Turn) *Run {
	streaming := t.Mode == v1alpha1.OutputLive && t.Recipient != "" && r.caps.Streams
	u := &Run{cfg: r.cfg, caps: r.caps, owner: r, turn: t, streaming: streaming, eligible: streaming && r.caps.Steps, wake: make(chan struct{}, 1), done: make(chan struct{}), texts: map[a2a.ArtifactID]string{}, progress: v1alpha1.ProgressSteps}
	if t.Mode == v1alpha1.OutputLive && (t.Progress == v1alpha1.ProgressStepsAndActivity || t.Progress == v1alpha1.ProgressActivity) {
		u.progress = t.Progress
	}
	if r.caps.Busy && !streaming && u.progress != v1alpha1.ProgressSteps {
		u.busy = &busyMark{m: r.cfg.Platform, off: &r.busyOff, at: chat.Target{Channel: t.Channel, Thread: t.Thread}, run: u}
	}
	if u.timed = u.progress != v1alpha1.ProgressSteps && (!streaming || r.caps.Steps); u.timed {
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
			u.progressIn(e, now)
		}
	case agent.FinalEvent:
		u.final = true
		u.paused = e.Task != nil && (e.Task.Status.State == a2a.TaskStateInputRequired || e.Task.Status.State == a2a.TaskStateAuthRequired)
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

// noteCall makes e the latest call, under way: the update path's status line is about it. A call that is reported
// again after its result keeps the outcome it has, so that the line does not go back to a call under way that nothing
// would end. The caller holds u.mu.
func (u *Run) noteCall(e agent.ToolCallEvent) {
	if u.tool != nil && e.CallID != "" && u.tool.CallID == e.CallID {
		u.tool = &e
		return
	}
	u.tool, u.toolState = &e, chat.ToolCalling
}

// beginStep records a tool call as a step of the stream, once: a call that is reported again is the same step, and
// a call with no ID, which no result can name, is a step of its own. A call that comes while the last step is a
// Working… card that nothing has claimed takes that card over, so a gap and the call that ended it are one card; it
// costs no step beyond the card. The activity mode shows no step for a call. The caller holds u.mu.
func (u *Run) beginStep(e agent.ToolCallEvent) {
	if !u.eligible || u.progress == v1alpha1.ProgressActivity || (e.CallID != "" && u.stepIndex(e.CallID) >= 0) {
		return
	}
	if n := len(u.steps); n > 0 && u.steps[n-1].open() { // a Working… card that is still unclaimed: the call is that card
		u.steps[n-1].call, u.steps[n-1].kind = e, callStep
		return
	}
	if len(u.steps) >= maxSteps {
		return
	}
	u.steps = append(u.steps, stepRec{call: e, after: len(u.order)})
}

// endStep finishes the step of a tool call that has its result: done, or failed if the response says so (render.Failed).
// A result of a call that is no step, or of one that has its result already, changes nothing. The caller holds u.mu.
func (u *Run) endStep(e agent.ToolResultEvent) {
	if i := u.stepIndex(e.CallID); i >= 0 && u.steps[i].state == chat.StepRunning {
		u.steps[i].state = chat.StepDone
		if render.Failed(e.Response) {
			u.steps[i].state = chat.StepFailed
		}
	}
}

// callMade notes a tool call for the activity indicators: a call with an ID is under way until its result, and a call under
// way is no gap. A call with no ID can never have a result, so it is under way until the next event and is counted as it
// arrives. A call that is reported again, with an ID the run has, changes nothing. In the activity mode the first tool event
// begins it. The caller holds u.mu.
func (u *Run) callMade(e agent.ToolCallEvent, now time.Time) {
	if e.CallID == "" {
		u.anon = true
		u.finished++
	} else {
		if _, known := u.calls[e.CallID]; known {
			return
		}
		u.calls[e.CallID] = false
		u.inflight++
		u.anon = false
	}
	u.idle, u.tooled = false, true // the tool's own line takes the place of Working…
	u.followText()
	u.beginActivity()
	u.reopenGap(now)
}

// resultIn counts the result of a tool call the run knows, once, and ends the call's time as a call under way: a new gap
// begins if no other call is. The caller holds u.mu.
func (u *Run) resultIn(e agent.ToolResultEvent, now time.Time) {
	u.anon, u.tooled = false, true
	if done, known := u.calls[e.CallID]; known && !done {
		u.calls[e.CallID] = true
		u.inflight--
		u.finished++
		if render.Failed(e.Response) {
			u.failed++
		}
	}
	u.beginActivity()
	u.reopenGap(now)
}

// progressIn takes a working update (spec D45 (1)), which is activity whether or not anything in it is understood. The
// activity mode counts it, and begins its indicator at once, as a tool event does; it shows none of the update's text, since
// that mode names nothing the agent does. In steps-and-activity an update with a line of the agent's ends the call with no ID
// that was under way until the next event, and, where Working… may show (no call under way), the line stands for Working…
// until the next event: it titles the open Working… card, or the card the delay begins, and on a message that is updated it
// is the line at once, which the next write can take back. It begins no card itself: a card stays in the stream once sent,
// and kagent 0.9.x repeats its answer in an update just before the text that would end such a card. An update with no line
// shows nothing there, and the gap runs on: were it to begin the gap again, an agent that sends empty updates more often
// than the delay would never show Working…. The caller holds u.mu.
func (u *Run) progressIn(e agent.ProgressEvent, now time.Time) {
	if u.progress == v1alpha1.ProgressActivity {
		u.updates++
		u.anon = false
		u.beginActivity()
		u.reopenGap(now)
		return
	}
	line := render.ProgressLine(e.Text)
	if line == "" || u.final {
		return
	}
	u.anon = false
	if u.inflight > 0 {
		return
	}
	u.note = line
	if !u.eligible {
		u.showWorking()
	}
}

// reopenGap begins a gap at now, in which the run waits the delay for something new before it shows that it is working. No
// gap runs while a call is under way (its own card, or line, is the sign), once the turn is final, or, in the activity
// mode, once the indicator has begun or the first text has come: the indicator is for the time before the text, it
// begins once, and it is rewritten by events and never by the clock. The caller holds u.mu.
func (u *Run) reopenGap(now time.Time) {
	over := u.progress == v1alpha1.ProgressActivity && (u.texted || u.begun)
	u.gapOpen = !u.final && u.inflight == 0 && !u.anon && !over
	if u.gapOpen {
		u.gapAt = now
		u.gapSeq++
	}
}

// beginActivity begins the activity mode's indicator, once, if no text has come: the one card of a stream, or the line of a
// message that is updated. The caller holds u.mu.
func (u *Run) beginActivity() {
	if u.progress != v1alpha1.ProgressActivity || u.texted || u.begun {
		return
	}
	u.begun = true
	if u.eligible {
		u.steps = append(u.steps, stepRec{kind: activityStep, after: len(u.order)})
	}
}

// openIdle is the delay timer firing for the gap seq: if that gap is still the gap and has run the delay, the run shows
// that it is working, which is a Working… card unless the platform has no cards, and the line of a message that is
// updated. It is the one thing a clock writes; the gap is then used, and a new one begins with the next event. A gap that
// events have ended since the timer was set shows nothing. now is read by the caller.
func (u *Run) openIdle(seq int, now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.gapOpen || u.gapSeq != seq || now.Before(u.gapAt.Add(u.cfg.ActivityDelay)) {
		return
	}
	u.gapOpen = false
	if u.progress == v1alpha1.ProgressActivity {
		u.beginActivity()
		return
	}
	u.showWorking()
}

// showWorking shows that the agent is working, in steps-and-activity: a Working… card, unless one is open already or the
// platform has no cards, and the line of a message that is updated. The gap is used. The caller holds u.mu.
func (u *Run) showWorking() {
	u.gapOpen, u.idle = false, true
	if n := len(u.steps); u.eligible && len(u.steps) < maxSteps && (n == 0 || !u.steps[n-1].open()) {
		u.steps = append(u.steps, stepRec{kind: workingStep, after: len(u.order)})
	}
}

// endWorking ends what stands for time, as e says, when text comes or the turn ends: a Working… card that no call claimed,
// and the activity card, with the counts and the time it stands at. A card ends once. Only text that came while a Working…
// card was open says that it wrote the answer, until a tool call follows that text (followText); a final that ends it says how
// the task ended. The caller holds u.mu.
func (u *Run) endWorking(now time.Time, e render.Ending) {
	n := len(u.steps)
	if n == 0 || u.steps[n-1].ending != render.NotEnded {
		return
	}
	switch r := &u.steps[n-1]; r.kind {
	case workingStep:
		r.ending = e
	case activityStep:
		r.ending, r.counts, r.endedAt = e, u.counts(), now
	}
}

// counts is what the activity mode shows it counted: the tool calls that have their result and the failures among them, and
// the working updates while no tool event has come. The caller holds u.mu.
func (u *Run) counts() render.Counts {
	c := render.Counts{Steps: u.finished, Failed: u.failed}
	if !u.tooled {
		c.Updates = u.updates
	}
	return c
}

// followText makes the text that ended a Working… card an update, now that a tool call follows it: the card said that it
// wrote the answer while nothing but text came after (render.WroteTitle), and the answer is what comes after this call. The
// caller holds u.mu.
func (u *Run) followText() {
	for i := range u.steps {
		if r := &u.steps[i]; r.kind == workingStep && r.ending == render.EndedByText {
			r.ending = render.EndedByUpdate
		}
	}
}

// arm returns the timer for the open gap, the one it was given if that is still the gap's, and nil if no gap is open. seq
// is the gap the timer is for. It is the run's goroutine's alone.
func (u *Run) arm(timer <-chan time.Time, seq int) (<-chan time.Time, int) {
	u.mu.Lock()
	open, at, cur := u.gapOpen, u.gapAt, u.gapSeq
	u.mu.Unlock()
	switch {
	case !open:
		return nil, seq
	case timer != nil && cur == seq:
		return timer, seq
	}
	return u.cfg.After(max(at.Add(u.cfg.ActivityDelay).Sub(u.cfg.Now()), 0)), cur
}

// open says the step is a Working… card that nothing has claimed or ended.
func (r stepRec) open() bool { return r.kind == workingStep && r.ending == render.NotEnded }

// stepIndex is the place of the step of the call callID, or -1. The caller holds u.mu.
func (u *Run) stepIndex(callID string) int {
	if callID == "" {
		return -1
	}
	return slices.IndexFunc(u.steps, func(s stepRec) bool { return s.call.CallID == callID })
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
		if u.cutShort && finalOutcome == "delivered" { // no final came: what was written ends the turn, and is no output delivered
			finalOutcome = "cancelled"
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
					finalOutcome = "deadline_exceeded"
					u.cfg.Logger.ErrorContext(ctx, "live: the final text was not written before its deadline", "channel", u.turn.Channel)
				}
				return
			}
			v := u.snapshot()     // after the wait, so that what is sent is the latest
			if !v.ok && v.final { // the turn ended with nothing to show
				// A stream that was started stands as a message too, though ref is the update path's alone: it was started for a step
				// that the platform then refused, and is empty. It is stopped with the text of a turn with no output, as the update
				// path's message is, or it is left open for good.
				if ref.ID == "" && !u.streamOpen() && !u.adopt(ctx, &ref) {
					finalOutcome = "empty_output"
					return
				}
				// But a message stands: do not leave its tool line up. Nothing was shown, so there is nothing to rate (Turn.Footer).
				v.m, v.body, v.ok, v.rated = render.Nothing(), render.NoOutput, true, false
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
			next = u.cfg.Now().Add(u.cfg.Throttle)
			if errors.Is(err, errNothingToShow) { // a turn that moved to the update path, and ended with nothing to post
				finalOutcome = "empty_output"
				return
			}
			if err != nil {
				switch verdict := u.cfg.Platform.Classify(chat.OpPost, err); {
				case errors.Is(err, store.ErrInvalid), errors.Is(err, chat.ErrInvalid), errors.Is(err, outbox.ErrAbandoned): // retrying cannot mend them
					finalOutcome = "permanent_failure"
					u.cfg.Logger.ErrorContext(ctx, "live: the message cannot be sent; giving up", "channel", u.turn.Channel, "err", err)
					return
				case verdict.RetryAfter > 0:
					u.cfg.Logger.WarnContext(ctx, "live: the platform rate-limited the message; waiting as it asked", "channel", u.turn.Channel, "retry_after", verdict.RetryAfter)
					next = u.cfg.Now().Add(max(u.cfg.Throttle, verdict.RetryAfter))
					continue
				}
				u.cfg.Logger.WarnContext(ctx, "live: writing the message failed; it will be tried again", "channel", u.turn.Channel, "err", err)
				if fails++; fails >= u.cfg.Attempts {
					u.cfg.Logger.ErrorContext(ctx, "live: giving up on a message", "channel", u.turn.Channel)
					if v.final {
						finalOutcome = "retry_exhausted"
						return
					}
					fails = 0
					break
				}
				continue
			}
			fails = 0
			if u.stale {
				u.stale, u.sent = false, "" // the post may be the old message: write once more
				continue
			}
			if u.more { // steps are left to begin: the next window, which the throttle spaces, goes on with them
				u.more = false
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
	err = u.cfg.Platform.StreamAt(u.cfg.Platform.RefAt(u.turn.Channel, u.turn.Thread, row.MessageRef)).Stop(ctx, "", chat.Message{}, id)
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
	*ref, u.adoptStop = u.cfg.Platform.RefAt(u.turn.Channel, u.turn.Thread, row.MessageRef), true
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

// want is the key of what the state would show: the message's content, or for a stream its text, its steps and
// whether it has ended.
func (u *Run) want(v view) string {
	if u.streaming {
		return v.body + "\x00" + stepsKey(v.steps) + map[bool]string{false: "", true: "\x00final"}[v.final]
	}
	key, _ := json.Marshal(u.message(v)) // a message of parts of known kinds always marshals
	return string(key)
}

// message is the message an update writes for v: v's own, after the steps of v while the turn shows them. A stream's
// steps are the stream's, and a message that replaces the stream must carry them, for a platform may drop the stream's
// cards when the message is updated, as Slack does (probed 2026-10-21): the update of a stream that diverged, and every
// update of a stream that this turn took over (carries). No post carries them: what the outbox stores is v.m.
func (u *Run) message(v view) chat.Message {
	if len(v.steps) == 0 || u.stepsOff {
		return v.m
	}
	m := v.m
	m.Parts = append([]chat.Part{chat.Steps{Steps: v.steps}}, v.m.Parts...)
	return m
}

// update replaces the message at ref with the message of v. The steps never cost the reply its text: a message that
// carries them and that the platform refuses as content it cannot show is sent again at once without them, with a
// warning, and the turn shows no steps from then on. Today a refusal of that kind is never retried, so without this the
// steps would take the answer down with them. A final whose steps were all it showed says "(no output)" without them.
func (u *Run) update(ctx context.Context, ref chat.Ref, v view) error {
	carried := len(v.steps) > 0 && !u.stepsOff
	err := u.cfg.Platform.Update(ctx, ref, chat.Say(u.message(v)))
	if err == nil || !carried || !u.cannotShow(err) {
		return err
	}
	u.cfg.Logger.WarnContext(ctx, "live: the platform refused the message with its steps; it is sent again without them", "channel", u.turn.Channel, "err", err)
	u.stepsOff = true
	m := v.m
	if v.final && m.Fallback == "" && len(m.Parts) == 0 {
		m = render.Nothing()
	}
	return u.cfg.Platform.Update(ctx, ref, chat.Say(m))
}

// cannotShow says whether err is the platform's refusal of a message's content: one it found malformed, too large, or of a
// form it does not take.
func (u *Run) cannotShow(err error) bool {
	if errors.Is(err, chat.ErrInvalid) || errors.Is(err, chat.ErrTooLarge) {
		return true
	}
	v := u.cfg.Platform.Classify(chat.OpPost, err)
	return v.Class == chat.Permanent && v.Reason == chat.ReasonInvalidContent
}

// stepsKey is the steps by ID, state and title: what a stream shows of them.
func stepsKey(steps []chat.Step) string {
	var b strings.Builder
	for _, s := range steps {
		fmt.Fprintf(&b, "%s:%d:%s;", s.ID, s.State, s.Title)
	}
	return b.String()
}

// write posts the message the first time, through the outbox, and updates it after, or, on the
// stream path, takes the next step of the stream.
func (u *Run) write(ctx context.Context, ref *chat.Ref, v view) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.render.write", attribute.Bool("agentkourier.render.final", v.final), attribute.Bool("agentkourier.render.streaming", u.streaming))
	defer func() {
		spanErr := telemetryErr
		if errors.Is(spanErr, errNothingToShow) { // a turn with nothing to post is a normal end, not a failed write
			spanErr = nil
		}
		op.Finish(spanErr)
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.WriteTimeout) // see WriteTimeout
	defer cancel()
	if u.streaming {
		return u.writeStream(ctx, ref, v)
	}
	if ref.ID != "" {
		if u.adoptStop { // an earlier run's stream: end it, or the platform refuses the update
			err := u.cfg.Platform.StreamAt(*ref).Stop(ctx, "", chat.Message{}, outbox.ID(u.turn.Source, store.OutboxReply))
			if err != nil && !errors.Is(err, chat.ErrNotStreaming) {
				return err
			}
			u.adoptStop = false
		}
		err := u.update(ctx, *ref, v)
		if err == nil {
			u.sent = u.want(v)
		}
		return err
	}
	u.sends++
	earlier := u.sends == 1 && u.rowMadeBefore(ctx)
	got, err := u.cfg.Sender.Send(ctx, outbox.Post{Source: u.turn.Source, Role: store.OutboxReply, Channel: u.turn.Channel, Thread: u.turn.Thread, Out: chat.Say(v.m)})
	if err == nil {
		*ref = got
		// A Send that follows a failed one, or that finds the turn's row made already (by an earlier attempt, here or in an
		// earlier process), returns the message that is there, or posts the payload stored first, not necessarily this content.
		u.stale = u.sends > 1 || earlier
		u.sent = u.want(v)
	}
	return err
}

// rowMadeBefore says whether the outbox row this turn posts under is there before its first post: an earlier attempt at the turn
// made it, and may have posted a message that the post then returns. A read that fails says so too: at worst one more write.
func (u *Run) rowMadeBefore(ctx context.Context) bool {
	_, err := u.cfg.Store.GetOutbox(ctx, outbox.ID(u.turn.Source, store.OutboxReply))
	return !errors.Is(err, store.ErrNotFound)
}

// view is what the state shows at one moment.
type view struct {
	m    chat.Message // the message of the update path: the text so far, and the latest tool call's status line as render.Turn shows it
	body string       // the text a stream carries
	// steps are the steps a stream shows, in the order of the calls, each in the state to show; and those of a turn that took
	// over a stream, which its updates carry (message).
	steps []chat.Step
	pos   []int // where in body each step follows: the text before that place was written before the call
	final bool
	rated bool // the final is the task's end, not a pause: the footer belongs on it
	ok    bool // there is something to show yet
}

// snapshot builds the state: the text so far as the body and, on the update path, the status line of the latest tool
// call, in the tense of how far it has got (render.Turn says when it is shown), and on the stream path the steps. A
// state with a status line is writable with no text, so a call that returned replaces the line that said it was under way.
// Once the turn is final the line is gone, and a step with no result is ended: the turn will not hear of it again. A turn
// that took over a stream has the steps too, which its updates carry, and no status line, since its cards say the same:
// steps make it writable with no text, the final included: a pause, or an end, with no text shows the cards and nothing
// under them, as a stream that stops with steps and no text does. Only a turn with no text and no step is "(no output)".
//
// At a pause for a tool approval the step of each call it waits on is shown as waiting for the approval, done, whatever the
// call had reported (render.AwaitingApproval): kagent reports such a call with a "requires confirmation" error, and a step
// that is not done when the stream stops reads as failed. Such a step may go from failed to done, the one outcome that is
// changed once shown; the answer's message shows the call again, with how it ended.
func (u *Run) snapshot() view {
	carries := u.carries()
	u.mu.Lock()
	events := make([]agent.TextEvent, 0, len(u.order))
	for _, id := range u.order {
		events = append(events, agent.TextEvent{ArtifactID: id, Text: u.texts[id]})
	}
	tool, toolState, final, paused, awaiting := u.tool, u.toolState, u.final, u.paused, u.awaiting
	idle, begun, counts, note := u.idle, u.begun && !u.texted, u.counts(), u.note
	var recs []stepRec
	if u.stepsOn() || carries {
		recs = slices.Clone(u.steps)
	}
	u.mu.Unlock()
	var now time.Time
	if u.timed {
		now = u.cfg.Now()
	}
	v := view{final: final, rated: final && !paused && !u.cutShort}
	var shown *agent.ToolCallEvent
	var status string                       // the update path's indicator that the agent is working
	if !final && !u.streaming && !carries { // on the stream path a tool call is a step, and has no status line
		shown = tool
		switch {
		case u.progress == v1alpha1.ProgressActivity: // names no tool
			shown = nil
			if begun {
				status = render.ActivityLine(now.Sub(u.started), counts)
			}
		case idle: // Working…, or the agent's progress line, takes the place of the line of a call that has returned
			shown, status = nil, cmp.Or(note, render.WorkingTitle)
		}
	}
	v.body = render.Body(events, u.caps.Limit(chat.WhereStream))
	v.m, v.ok = render.Turn(events, shown, toolState, u.caps)
	if status != "" {
		v.m, v.ok = render.WithStatus(v.m, status), true
	}
	if v.rated && v.body != "" && len(u.turn.Footer) > 0 { // a turn that ended with no text has no reply to rate, as a stream's stop says (stopFooter)
		v.m.Parts = append(slices.Clone(v.m.Parts), u.turn.Footer...)
	}
	for i, r := range recs {
		id := fmt.Sprintf("s%d", i+1)
		if _, sent := u.sentOf(id); final && r.kind != callStep && v.body == "" && !sent {
			continue // a card for time that was never shown is not content of a turn with no text: no stream is made for it
		}
		state := r.state
		if final && state == chat.StepRunning {
			state = chat.StepEnded
		}
		before := render.Body(events[:min(r.after, len(events))], u.caps.Limit(chat.WhereStream))
		pos := len(v.body)
		if strings.HasPrefix(v.body, before) {
			pos = len(before)
		}
		if r.kind == callStep && r.call.CallID != "" && slices.Contains(awaiting, r.call.CallID) {
			v.steps = append(v.steps, render.AwaitingApproval(id, r.call))
		} else {
			v.steps = append(v.steps, u.stepOf(id, r, state, now, counts, note))
		}
		v.pos = append(v.pos, pos)
	}
	v.ok = v.ok || len(v.steps) > 0
	return v
}

// stepOf is the step a record shows in the state given: the call it is, or the card that stands for time. A call with no
// result at the final is ended with them (state); a card that stands for time has its own ending. A Working… card under way
// is titled with the agent's progress line, if there is one: only the last card can be under way.
func (u *Run) stepOf(id string, r stepRec, state chat.StepState, now time.Time, counts render.Counts, note string) chat.Step {
	switch r.kind {
	case workingStep:
		st := render.WorkingEnded(id, r.ending)
		if r.ending == render.NotEnded && note != "" {
			st.Title = note
		}
		return st
	case activityStep:
		if r.ending == render.NotEnded {
			return render.ActivityStep(id, counts, now.Sub(u.started), r.ending)
		}
		return render.ActivityStep(id, r.counts, r.endedAt.Sub(u.started), r.ending)
	}
	return render.Step(id, r.call, state)
}

// stepsOn says whether the stream shows steps: the turn is eligible for them (a stream, on a platform that has them),
// it is still the stream it began as, and the platform has not refused one.
func (u *Run) stepsOn() bool { return u.eligible && u.streaming && !u.stepsOff }

// carries says whether the turn's updates carry its steps: it is eligible for them, it took over the stream of an earlier
// run, which is the message it updates (a platform such as Slack drops a stream's cards from a message it updates), and the
// platform has not refused a message with them.
func (u *Run) carries() bool { return u.eligible && u.adopted && !u.streaming && !u.stepsOff }

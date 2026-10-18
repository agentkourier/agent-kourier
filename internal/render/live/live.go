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
// forward, so nothing has to be taken back. Otherwise, such as in an alert thread, or on a platform that does not
// stream, the turn posts and updates, and shows the latest tool call as a status line, in the past tense once its result
// has arrived (render.Turn says when the line is shown). Neither path ever
// updates a message that is still streaming. Text is built at the last step, from the raw events (package
// render), and is literal: the platform makes it inert.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Renderer{cfg: cfg, caps: cfg.Platform.Capabilities()}, nil
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
	// Recipient is the person who asked, to whom Slack shows a stream in a channel. Empty, as in an
	// alert thread, takes the chat.update path.
	Recipient string
	// Recovered says the turn continues a message that may exist already, and is updated to the
	// whole text. A chat turn that finds its stream's row with the ts recorded stops that stream
	// (which it has lost the connection to) and updates the message. A row with no ts is the window
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
	// eligible says the turn may show steps: it began as a stream, on a platform that has them. It is set at the
	// start and never changed, which is why Handle may read it; streaming, which a takeover clears, is the goroutine's.
	eligible bool

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
	diverged  bool        // the streamed text is not a prefix of the final text
	stream    chat.Stream // the stream this turn started
	streamed  string      // the markdown sent to the stream so far
	stepsSent []chat.Step // the steps sent to the stream, each in the state it was last sent
	stepsOff  bool        // the platform refused a step for good: the turn goes on without steps
	stepFails int         // steps that failed in a row
	more      bool        // a write left steps to begin, which the next window goes on with
}

// stepRec is a tool call of a stream, which is a step: the call, how it ended, and where in the text it was made.
type stepRec struct {
	call  agent.ToolCallEvent
	state chat.StepState
	after int // how many text artifacts there were when the call was made: the step follows their text
}

// Start begins rendering a turn and returns it. Its goroutine ends once the final text is
// written, or given up on, or when ctx ends.
func (r *Renderer) Start(ctx context.Context, t Turn) *Run {
	streaming := t.Mode == v1alpha1.OutputLive && t.Recipient != "" && r.caps.Streams
	u := &Run{cfg: r.cfg, caps: r.caps, turn: t, streaming: streaming, eligible: streaming && r.caps.Steps, wake: make(chan struct{}, 1), done: make(chan struct{}), texts: map[a2a.ArtifactID]string{}}
	r.runs.Add(1)
	go func() { defer r.runs.Done(); u.run(ctx) }()
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
	u.mu.Lock()
	switch e := ev.(type) {
	case agent.TextEvent:
		if _, seen := u.texts[e.ArtifactID]; !seen {
			u.order = append(u.order, e.ArtifactID)
		}
		u.texts[e.ArtifactID] = e.Text
	case agent.ToolCallEvent:
		u.noteCall(e)
		u.beginStep(e)
	case agent.ToolResultEvent:
		if u.tool != nil && u.tool.CallID == e.CallID && u.toolState == chat.ToolCalling {
			u.toolState = chat.ToolRan
			if render.Failed(e.Response) {
				u.toolState = chat.ToolFailed
			}
		}
		u.endStep(e)
	case agent.FinalEvent:
		u.final = true
		u.paused = e.Task != nil && (e.Task.Status.State == a2a.TaskStateInputRequired || e.Task.Status.State == a2a.TaskStateAuthRequired)
	}
	u.mu.Unlock()
	select {
	case u.wake <- struct{}{}:
	default: // a signal is waiting already
	}
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
// a call with no ID, which no result can name, is a step of its own. The caller holds u.mu.
func (u *Run) beginStep(e agent.ToolCallEvent) {
	if !u.eligible || len(u.steps) >= maxSteps || (e.CallID != "" && u.stepIndex(e.CallID) >= 0) {
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

// stepIndex is the place of the step of the call callID, or -1. The caller holds u.mu.
func (u *Run) stepIndex(callID string) int {
	if callID == "" {
		return -1
	}
	return slices.IndexFunc(u.steps, func(s stepRec) bool { return s.call.CallID == callID })
}

// run writes the state each time its window opens, until the final text is out.
func (u *Run) run(ctx context.Context) {
	defer close(u.done)
	var finalOp *telemetry.Operation
	finalCtx := ctx
	writes := 0
	finalOutcome := "cancelled"
	defer func() {
		if finalOp == nil && u.isFinal() {
			finalCtx, finalOp = telemetry.Start(context.WithoutCancel(ctx), "agentkourier.render.finalize", attribute.String("agentkourier.render.mode", string(u.turn.Mode)), attribute.String("agentkourier.turn.id", u.turn.Source))
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
	var ref chat.Ref // the message, once there is one
	var next time.Time
	fails := 0
	for {
		select {
		case <-ctx.Done():
			if !u.isFinal() { // a turn that never ended owes nothing
				return
			}
		case <-u.wake:
		}
		for {
			if u.isFinal() && finalOp == nil {
				finalCtx, finalOp = telemetry.Start(context.WithoutCancel(ctx), "agentkourier.render.finalize", attribute.String("agentkourier.render.mode", string(u.turn.Mode)), attribute.String("agentkourier.turn.id", u.turn.Source))
			}
			if d := next.Sub(u.cfg.Now()); d > 0 && !u.sleep(ctx, d) {
				if u.isFinal() {
					finalOutcome = "deadline_exceeded"
					u.cfg.Logger.ErrorContext(ctx, "live: the final text was not written before its deadline", "channel", u.turn.Channel)
				}
				return
			}
			v := u.snapshot()     // after the wait, so that what is sent is the latest
			if !v.ok && v.final { // the turn ended with nothing to show
				if ref.ID == "" {
					finalOutcome = "empty_output"
					return
				}
				v.m, v.body, v.ok = render.Nothing(), render.NoOutput, true // but a message stands: do not leave its tool line up
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
			break
		}
	}
}

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
	key, _ := json.Marshal(v.m) // a message of parts of known kinds always marshals
	return string(key)
}

// stepsKey is the steps by ID and state: what a stream shows of them.
func stepsKey(steps []chat.Step) string {
	var b strings.Builder
	for _, s := range steps {
		fmt.Fprintf(&b, "%s:%d;", s.ID, s.State)
	}
	return b.String()
}

// write posts the message the first time, through the outbox, and updates it after, or, on the
// stream path, takes the next step of the stream.
func (u *Run) write(ctx context.Context, ref *chat.Ref, v view) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.render.write", attribute.Bool("agentkourier.render.final", v.final), attribute.Bool("agentkourier.render.streaming", u.streaming))
	defer func() { op.Finish(telemetryErr) }()
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
		err := u.cfg.Platform.Update(ctx, *ref, chat.Say(v.m))
		if err == nil {
			u.sent = u.want(v)
		}
		return err
	}
	u.sends++
	got, err := u.cfg.Sender.Send(ctx, outbox.Post{Source: u.turn.Source, Role: store.OutboxReply, Channel: u.turn.Channel, Thread: u.turn.Thread, Out: chat.Say(v.m)})
	if err == nil {
		*ref = got
		// A Send that follows a failed one, or a recovered turn's, returns the message that is
		// there already, or posts the payload stored first, not necessarily this content.
		u.stale = u.sends > 1 || u.turn.Recovered
		u.sent = u.want(v)
	}
	return err
}

// view is what the state shows at one moment.
type view struct {
	m     chat.Message // the message of the update path: the text so far, and the latest tool call's status line as render.Turn shows it
	body  string       // the text a stream carries
	steps []chat.Step  // the steps a stream shows, in the order of the calls, each in the state to show
	pos   []int        // where in body each step follows: the text before that place was written before the call
	final bool
	rated bool // the final is the task's end, not a pause: the footer belongs on it
	ok    bool // there is something to show yet
}

// snapshot builds the state: the text so far as the body and, on the update path, the status line of the latest tool
// call, in the tense of how far it has got (render.Turn says when it is shown), and on the stream path the steps. A
// state with a status line is writable with no text, so a call that returned replaces the line that said it was under way.
// Once the turn is final the line is gone, and a step with no result is ended: the turn will not hear of it again.
func (u *Run) snapshot() view {
	u.mu.Lock()
	events := make([]agent.TextEvent, 0, len(u.order))
	for _, id := range u.order {
		events = append(events, agent.TextEvent{ArtifactID: id, Text: u.texts[id]})
	}
	tool, toolState, final, paused := u.tool, u.toolState, u.final, u.paused
	var recs []stepRec
	if u.stepsOn() {
		recs = slices.Clone(u.steps)
	}
	u.mu.Unlock()
	v := view{final: final, rated: final && !paused}
	var shown *agent.ToolCallEvent
	if tool != nil && !final && !u.streaming { // on the stream path a tool call is a step, and has no status line
		shown = tool
	}
	v.body = render.Body(events, u.caps.Limit(chat.WhereStream))
	v.m, v.ok = render.Turn(events, shown, toolState, u.caps)
	if v.rated && len(u.turn.Footer) > 0 { // a turn that ended with no text is replaced below by "(no output)", footer and all
		v.m.Parts = append(slices.Clone(v.m.Parts), u.turn.Footer...)
	}
	for i, r := range recs {
		state := r.state
		if final && state == chat.StepRunning {
			state = chat.StepEnded
		}
		before := render.Body(events[:min(r.after, len(events))], u.caps.Limit(chat.WhereStream))
		pos := len(v.body)
		if strings.HasPrefix(v.body, before) {
			pos = len(before)
		}
		v.steps = append(v.steps, render.Step(fmt.Sprintf("s%d", i+1), r.call, state))
		v.pos = append(v.pos, pos)
	}
	v.ok = v.ok || len(v.steps) > 0
	return v
}

// stepsOn says whether the stream shows steps: the turn is eligible for them (a stream, on a platform that has them),
// it is still the stream it began as, and the platform has not refused one.
func (u *Run) stepsOn() bool { return u.eligible && u.streaming && !u.stepsOff }

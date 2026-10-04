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
// stream.go). Otherwise, such as in an alert thread, or on a platform that does not stream, it posts and
// updates. Neither ever updates a message that is still streaming. Text is built at the last step, from
// the raw events (package render), and is literal: the platform makes it inert.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// Sender posts through the outbox: *outbox.Outbox.
type Sender interface {
	Send(ctx context.Context, p outbox.Post) (chat.Ref, error)
}

// Config configures a Renderer. Sender and Platform are required. Store is for the stream path; a Renderer
// with no Store never streams.
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

// New returns a Renderer over cfg.
func New(cfg Config) *Renderer {
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
	return &Renderer{cfg: cfg, caps: cfg.Platform.Capabilities()}
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
}

// Run is the rendering of one turn.
type Run struct {
	cfg  Config
	caps chat.Caps
	turn Turn
	wake chan struct{} // a slot: the state changed
	done chan struct{}

	mu    sync.Mutex // guards the state below
	texts map[a2a.ArtifactID]string
	order []a2a.ArtifactID
	tool  *agent.ToolCallEvent // the call being made, if any
	final bool

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
}

// Start begins rendering a turn and returns it. Its goroutine ends once the final text is
// written, or given up on, or when ctx ends.
func (r *Renderer) Start(ctx context.Context, t Turn) *Run {
	u := &Run{cfg: r.cfg, caps: r.caps, turn: t, streaming: t.Mode == v1alpha1.OutputLive && t.Recipient != "" && r.caps.Streams && r.cfg.Store != nil, wake: make(chan struct{}, 1), done: make(chan struct{}), texts: map[a2a.ArtifactID]string{}}
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
		u.tool = &e
	case agent.ToolResultEvent:
		if u.tool != nil && u.tool.CallID == e.CallID {
			u.tool = nil
		}
	case agent.FinalEvent:
		u.final = true
	}
	u.mu.Unlock()
	select {
	case u.wake <- struct{}{}:
	default: // a signal is waiting already
	}
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
			finalCtx, finalOp = telemetry.Start(context.WithoutCancel(ctx), "courier.render.finalize", attribute.String("courier.render.mode", string(u.turn.Mode)), attribute.String("courier.turn.id", u.turn.Source))
		}
		if finalOp != nil {
			trace.SpanFromContext(finalCtx).SetAttributes(attribute.Int("courier.render.attempts", writes))
			telemetry.Count(ctx, "courier_render_final_total", finalOutcome)
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
				finalCtx, finalOp = telemetry.Start(context.WithoutCancel(ctx), "courier.render.finalize", attribute.String("courier.render.mode", string(u.turn.Mode)), attribute.String("courier.turn.id", u.turn.Source))
			}
			if d := next.Sub(u.cfg.Now()); d > 0 && !u.sleep(ctx, d) {
				if u.isFinal() {
					finalOutcome = "deadline_exceeded"
					u.cfg.Logger.ErrorContext(ctx, "live: the final text was not written before its deadline", "channel", u.turn.Channel)
				}
				return
			}
			m, body, final, ok := u.snapshot() // after the wait, so that what is sent is the latest
			if !ok && final {                  // the turn ended with nothing to show
				if ref.ID == "" {
					finalOutcome = "empty_output"
					return
				}
				m, body, ok = render.Nothing(), render.NoOutput, true // but a message stands: do not leave its tool line up
			}
			if !ok || (u.turn.Mode == v1alpha1.OutputFinal && !final) {
				break
			}
			if u.want(m, body, final) == u.sent {
				if final {
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
			err := u.write(writeCtx, &ref, m, body, final)
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
					if final {
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
			if final {
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

// want is the key of what the state would show: the message's content, or for a stream its text
// and whether it has ended.
func (u *Run) want(m chat.Message, body string, final bool) string {
	if u.streaming {
		return body + map[bool]string{false: "", true: "\x00final"}[final]
	}
	key, _ := json.Marshal(m) // a message of parts of known kinds always marshals
	return string(key)
}

// write posts the message the first time, through the outbox, and updates it after, or, on the
// stream path, takes the next step of the stream.
func (u *Run) write(ctx context.Context, ref *chat.Ref, m chat.Message, body string, final bool) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.render.write", attribute.Bool("courier.render.final", final), attribute.Bool("courier.render.streaming", u.streaming))
	defer func() { op.Finish(telemetryErr) }()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.WriteTimeout) // see WriteTimeout
	defer cancel()
	if u.streaming {
		return u.writeStream(ctx, ref, m, body, final)
	}
	if ref.ID != "" {
		if u.adoptStop { // an earlier run's stream: end it, or the platform refuses the update
			err := u.cfg.Platform.StreamAt(*ref).Stop(ctx, "", chat.Message{}, outbox.ID(u.turn.Source, store.OutboxReply))
			if err != nil && !errors.Is(err, chat.ErrNotStreaming) {
				return err
			}
			u.adoptStop = false
		}
		err := u.cfg.Platform.Update(ctx, *ref, chat.Say(m))
		if err == nil {
			u.sent = u.want(m, body, final)
		}
		return err
	}
	u.sends++
	got, err := u.cfg.Sender.Send(ctx, outbox.Post{Source: u.turn.Source, Role: store.OutboxReply, Channel: u.turn.Channel, Thread: u.turn.Thread, Out: chat.Say(m)})
	if err == nil {
		*ref = got
		// A Send that follows a failed one, or a recovered turn's, returns the message that is
		// there already, or posts the payload stored first, not necessarily this content.
		u.stale = u.sends > 1 || u.turn.Recovered
		u.sent = u.want(m, body, final)
	}
	return err
}

// snapshot builds the state as a message: the text so far as its body and, while a tool call is under way, its
// status line. Once the turn is final the line is gone. body is the text a stream carries. ok is false if there
// is nothing to show yet.
func (u *Run) snapshot() (m chat.Message, body string, final, ok bool) {
	u.mu.Lock()
	events := make([]agent.TextEvent, 0, len(u.order))
	for _, id := range u.order {
		events = append(events, agent.TextEvent{ArtifactID: id, Text: u.texts[id]})
	}
	tool, final := u.tool, u.final
	u.mu.Unlock()
	var shown *agent.ToolCallEvent
	if tool != nil && !final && !u.streaming { // no tool lines in a stream (v1): they cannot be taken back
		shown = tool
	}
	body = render.Body(events, u.caps.Limit(chat.WhereStream))
	m, ok = render.Turn(events, shown, u.caps)
	return m, body, final, ok
}

package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// errSessionNotReady is what a late-bound session answers until the session manager exists. A pause
// cannot reach the intake or decider before then: the tap has no turns and text decisions wait for
// interaction startup. It guards the shared handle rather than a production startup path.
var errSessionNotReady = errors.New("broker: the session manager is not built yet")

// lateSession is the session the interactions use, bound to the manager once that exists (D2): the manager
// is built with the tap as its sink, the tap hands pauses to the intake, and the intake answers through the
// manager, so one of them has to be made before the one it points at.
type lateSession struct {
	m atomic.Pointer[session.Manager]
}

var _ interact.Session = (*lateSession)(nil)

// bind sets the manager the session delegates to.
func (l *lateSession) bind(m *session.Manager) { l.m.Store(m) }

func (l *lateSession) Answer(ctx context.Context, key store.ConversationKey, msg *a2a.Message) error {
	m := l.m.Load()
	if m == nil {
		return errSessionNotReady
	}
	return m.Answer(ctx, key, msg)
}

func (l *lateSession) Cancel(ctx context.Context, key store.ConversationKey, id a2a.TaskID, reason string) error {
	m := l.m.Load()
	if m == nil {
		return errSessionNotReady
	}
	return m.Cancel(ctx, key, id, reason)
}

// interactionDeps is what the interactions are built from: the parts of Run they share. Every part but trace is required
// (check names the one that is missing).
type interactionDeps struct {
	st       store.Store
	ob       *outbox.Outbox
	platform builtPlatform // the chat platform: prompts, edits, who may answer, and what a person is told go through it
	auditor  *audit.Writer
	bindings *Bindings
	metrics  *Metrics
	log      *slog.Logger
	trace    func(step string) // a test's: told as each step of startup is done. Optional.
}

// check refuses a deps with a part left out, and names it: the interactions are built from these in two steps (build,
// then start), and a part that is nil would fail in the first call that uses it, or not at all.
func (d interactionDeps) check() error {
	switch {
	case d.st == nil:
		return errors.New("interactions: st is required")
	case d.ob == nil:
		return errors.New("interactions: ob is required")
	case d.platform.Platform == nil:
		return errors.New("interactions: platform is required")
	case d.auditor == nil:
		return errors.New("interactions: auditor is required")
	case d.bindings == nil:
		return errors.New("interactions: bindings is required")
	case d.metrics == nil:
		return errors.New("interactions: metrics is required")
	case d.log == nil:
		return errors.New("interactions: log is required")
	}
	return nil
}

func (d interactionDeps) traced(step string) {
	if d.trace != nil {
		d.trace(step)
	}
}

// recovered runs a recovery and, if it succeeded, tells the trace it is done: the step is told by the
// call itself, so the trace cannot say a recovery happened where the call does not.
func (d interactionDeps) recovered(step string, fn func() error) error {
	if err := fn(); err != nil {
		return err
	}
	d.traced(step)
	return nil
}

// interactionRuntime is constructed before the session manager. Both pause intake and answer
// decisions share its one late session handle; no handler is installed after construction.
type interactionRuntime struct {
	tap      *interact.Tap
	late     *lateSession
	replies  *interact.TextReplies
	pipeline *interact.Pipeline
	votes    *feedback.Handler
	replacer *interact.Replacer
	worker   *interact.Worker
	ready    chan struct{}
}

// build constructs the interaction graph without starting loops or accepting text decisions.
func (d interactionDeps) build(inner session.Sink) (*interactionRuntime, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	late := &lateSession{}
	intake, err := interact.New(interact.Config{
		Store: d.st, Lookup: d.bindings, Platform: d.platform, Outbox: d.ob, Session: late, Audit: d.auditor,
		OnApprovalOutcome: d.metrics.ApprovalOutcome, Logger: d.log,
	})
	if err != nil {
		return nil, fmt.Errorf("interactions: the intake: %w", err)
	}
	replacer, err := interact.NewReplacer(interact.ReplacerConfig{Store: d.st, Updater: d.platform, Logger: d.log})
	if err != nil {
		return nil, fmt.Errorf("interactions: the replacer: %w", err)
	}
	decider, err := interact.NewDecider(interact.DecideConfig{
		Store: d.st, Auditor: d.auditor, Ephemerals: d.platform, Session: late, Sessions: d.st, Outbox: d.ob, Replacer: replacer,
		OnDecision: d.metrics.InteractionDecision, Logger: d.log,
	})
	if err != nil {
		return nil, fmt.Errorf("interactions: the decider: %w", err)
	}
	var worker *interact.Worker // Notify needs the worker and the worker the pipeline: the closure breaks the cycle (D2)
	pipeline, err := interact.NewPipeline(interact.PressConfig{
		Store: d.st, Sessions: d.st, Lookup: d.bindings, Authorizer: d.platform, Ephemerals: d.platform, Decider: decider, Auditor: d.auditor,
		Notify: func(p store.Press) { worker.Submit(p) }, Logger: d.log,
	})
	if err != nil {
		return nil, fmt.Errorf("interactions: the press pipeline: %w", err)
	}
	votes, err := feedback.New(feedback.Config{
		Store: d.st, Sessions: d.st, Authorizer: d.platform, Ephemerals: d.platform, Auditor: d.auditor,
		Notify: func(p store.Press) { worker.Submit(p) }, Logger: d.log,
	})
	if err != nil {
		return nil, fmt.Errorf("interactions: the feedback handler: %w", err)
	}
	if worker, err = interact.NewWorker(interact.WorkerConfig{Processor: pressRouter{interactions: pipeline, feedback: votes}, Logger: d.log}); err != nil {
		return nil, fmt.Errorf("interactions: the worker: %w", err)
	}
	tap, err := interact.NewTap(interact.TapConfig{Sink: inner, Handler: intake, Lookup: d.bindings, Logger: d.log})
	if err != nil {
		return nil, fmt.Errorf("interactions: the tap: %w", err)
	}

	ready := make(chan struct{})
	replies, err := interact.NewTextReplies(pipeline, intake, ready)
	if err != nil {
		return nil, fmt.Errorf("interactions: text replies: %w", err)
	}
	return &interactionRuntime{tap: tap, late: late, replies: replies, pipeline: pipeline, votes: votes, replacer: replacer, worker: worker, ready: ready}, nil
}

// start recovers presses and starts the interaction loops after session recovery. Only then
// may the already-constructed text handler decide replies. Stops retain their existing order.
func (d interactionDeps) start(ctx context.Context, mgr *session.Manager, r *interactionRuntime, stops *stopList) (func(context.Context, chat.Interaction) error, error) {
	tap, pipeline, votes, replacer, worker := r.tap, r.pipeline, r.votes, r.replacer, r.worker
	sweeper, err := interact.NewSweeper(interact.SweepConfig{
		Store: d.st, Sessions: d.st, Session: mgr, Updater: d.platform, Outbox: d.ob, Auditor: d.auditor,
		Presses: d.st, OnTimeout: d.metrics.InteractionTimeout, Logger: d.log,
	})
	if err != nil {
		return nil, fmt.Errorf("interactions: the sweeper: %w", err)
	}
	retention, err := interact.NewRetention(interact.RetentionConfig{Store: d.st, OnCleared: func(n int) { d.metrics.RetentionRemoved(RemovedClearedInteractions, n) }, Logger: d.log})
	if err != nil {
		return nil, fmt.Errorf("interactions: the retention loop: %w", err)
	}
	backstop, err := interact.NewBackstop(interact.BackstopConfig{Store: d.st, Sessions: d.st, Outbox: d.ob, Logger: d.log})
	if err != nil {
		return nil, fmt.Errorf("interactions: the backstop: %w", err)
	}

	if err := d.recovered("recover-presses", func() error { return worker.Recover(ctx, d.st) }); err != nil {
		return nil, fmt.Errorf("recover presses: %w", err)
	}

	// Stops run last added first: the tap, which hands the intake no more pauses, then the worker, the
	// sweeper and the rest.
	stops.add("retention", startLoop(ctx, retention.Run))
	stops.add("backstop", startLoop(ctx, backstop.Run))
	stops.add("replacer", startLoop(ctx, replacer.Run))
	stops.add("sweeper", startLoop(ctx, sweeper.Run))
	stops.add("worker", startLoop(ctx, worker.Run))
	stops.add("tap", func(c context.Context) {
		if err := tap.Close(c); err != nil {
			d.log.Warn("closing the pause tap", "error", err)
		}
	})
	close(r.ready) // every text handler exists before recovery, but decisions wait for these loops
	d.traced("start-interactions")
	return interactionsHandler(pipeline, votes, d.log), nil
}

// startLoop runs run in a goroutine of its own until its stop is called, and returns that stop: it ends
// the loop's context, and waits for the goroutine, or for ctx to end.
func startLoop(parent context.Context, run func(context.Context) error) func(context.Context) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() { defer close(done); _ = run(ctx) }()
	return func(c context.Context) { cancel(); waitFor(c, done) }
}

// presser takes the press of a button: *interact.Pipeline for a prompt's, *feedback.Handler for a feedback button's.
type presser interface {
	Press(ctx context.Context, i chat.Interaction) error
}

// interactionsHandler is what the chat intake calls for each press of a button, which the adapter has decoded
// into the neutral model. A prompt's button goes to the interactions' pipeline; one that is not goes to the feedback
// buttons' handler, and one that is neither's is someone else's, and is dropped without a word. A press refused because too
// many wait is told to the log, and acknowledged, since Slack redelivering it would only add to them. Any other error
// leaves the press unacknowledged, for Slack to deliver again.
func interactionsHandler(p, votes presser, log *slog.Logger) func(context.Context, chat.Interaction) error {
	return func(ctx context.Context, i chat.Interaction) error {
		err := p.Press(ctx, i)
		if errors.Is(err, interact.ErrNotInteraction) {
			err = votes.Press(ctx, i)
		}
		switch {
		case errors.Is(err, interact.ErrNotInteraction), errors.Is(err, feedback.ErrNotFeedback):
			return nil
		case errors.Is(err, store.ErrLimit):
			log.WarnContext(ctx, "interact: a press was refused, because too many presses are waiting", "channel", i.At.Channel, "err", err)
			return nil
		}
		return err
	}
}

// pressRouter is the press worker's processor. Both kinds of button record their presses in one table, and the worker takes
// them as they come: a feedback press, by its lane, goes to the feedback handler, and any other to the interactions'
// pipeline.
type pressRouter struct{ interactions, feedback interact.PressProcessor }

func (r pressRouter) pick(p store.Press) interact.PressProcessor {
	if feedback.IsPress(p) {
		return r.feedback
	}
	return r.interactions
}

// Process implements interact.PressProcessor.
func (r pressRouter) Process(ctx context.Context, p store.Press) (interact.Result, error) {
	return r.pick(p).Process(ctx, p)
}

// GiveUp implements interact.PressProcessor.
func (r pressRouter) GiveUp(ctx context.Context, p store.Press) error {
	return r.pick(p).GiveUp(ctx, p)
}

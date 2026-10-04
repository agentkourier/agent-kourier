package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// errSessionNotReady is what a late-bound session answers until the session manager exists. A pause
// cannot reach the interactions before then, and the tap tries again, so it is a safety and not a path.
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

// interactionDeps is what the interactions are built from: the parts of Run they share.
type interactionDeps struct {
	st       *sqlite.Store
	ob       *outbox.Outbox
	platform builtPlatform // the chat platform: prompts, edits, who may answer, and what a person is told go through it
	auditor  *audit.Writer
	bindings *Bindings
	metrics  *Metrics
	log      *slog.Logger
	trace    func(step string) // a test's: told as each step of startup is done
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

// newTap builds the intake of pauses, with a session that is bound later, and the tap that hands the
// renderer's turns' pauses to it. The manager is built over the tap, and the session bound to the manager.
func (d interactionDeps) newTap(inner session.Sink) (*interact.Tap, *lateSession, error) {
	late := &lateSession{}
	intake, err := interact.New(interact.Config{
		Store: d.st, Lookup: d.bindings, Platform: d.platform, Outbox: d.ob, Session: late, Audit: d.auditor,
		OnApprovalOutcome: d.metrics.ApprovalOutcome, Logger: d.log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("interactions: the intake: %w", err)
	}
	tap, err := interact.NewTap(interact.TapConfig{Sink: inner, Handler: intake, Lookup: d.bindings, Logger: d.log})
	if err != nil {
		return nil, nil, fmt.Errorf("interactions: the tap: %w", err)
	}
	return tap, late, nil
}

// start builds the parts that take a person's answer, finds again what an earlier run left, starts the
// loops and registers every stop with stops. It returns the handler of the buttons that the Slack intake
// takes. mgr has recovered its sessions: the sweeper starts after that, and the presses are recovered
// after it, before the Slack intake opens, so that a press listed just before it finishes is not announced
// after.
func (d interactionDeps) start(ctx context.Context, mgr *session.Manager, tap *interact.Tap, stops *stopList) (func(context.Context, chat.Interaction) error, error) {
	replacer, err := interact.NewReplacer(interact.ReplacerConfig{Store: d.st, Updater: d.platform, Logger: d.log})
	if err != nil {
		return nil, fmt.Errorf("interactions: the replacer: %w", err)
	}
	decider, err := interact.NewDecider(interact.DecideConfig{
		Store: d.st, Auditor: d.auditor, Ephemerals: d.platform, Session: mgr, Sessions: d.st, Outbox: d.ob, Replacer: replacer,
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
	mgr.SetTextReplyHandler(func(ctx context.Context, key session.Key, reply store.Reply, task a2a.Task) (bool, error) {
		if reply.InteractionID == "" {
			sess, err := d.st.GetSession(ctx, key, 100*365*24*time.Hour, time.Now())
			if err != nil {
				return false, err
			}
			if err := tap.PauseNow(ctx, interact.Pause{Key: key, Binding: sess.Binding, Task: task}); err != nil {
				return false, err
			}
		}
		return pipeline.TextReply(ctx, key, reply, task)
	})
	if worker, err = interact.NewWorker(interact.WorkerConfig{Processor: pipeline, Logger: d.log}); err != nil {
		return nil, fmt.Errorf("interactions: the worker: %w", err)
	}
	sweeper, err := interact.NewSweeper(interact.SweepConfig{
		Store: d.st, Sessions: d.st, Session: mgr, Updater: d.platform, Outbox: d.ob, Auditor: d.auditor,
		Presses: d.st, OnTimeout: d.metrics.InteractionTimeout, Logger: d.log,
	})
	if err != nil {
		return nil, fmt.Errorf("interactions: the sweeper: %w", err)
	}
	retention, err := interact.NewRetention(interact.RetentionConfig{Store: d.st, Logger: d.log})
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
	d.traced("start-interactions")
	return interactionsHandler(pipeline, d.log), nil
}

// startLoop runs run in a goroutine of its own until its stop is called, and returns that stop: it ends
// the loop's context, and waits for the goroutine, or for ctx to end.
func startLoop(parent context.Context, run func(context.Context) error) func(context.Context) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() { defer close(done); _ = run(ctx) }()
	return func(c context.Context) { cancel(); waitFor(c, done) }
}

// presser takes the press of a button: *interact.Pipeline.
type presser interface {
	Press(ctx context.Context, i chat.Interaction) error
}

// interactionsHandler is what the chat intake calls for each press of a button, which the adapter has decoded
// into the neutral model. A button that is not an
// interaction's is someone else's, and is dropped without a word; a press refused because too many wait is
// told to the log, and acknowledged, since Slack redelivering it would only add to them. Any other error
// leaves the press unacknowledged, for Slack to deliver again.
func interactionsHandler(p presser, log *slog.Logger) func(context.Context, chat.Interaction) error {
	return func(ctx context.Context, i chat.Interaction) error {
		err := p.Press(ctx, i)
		switch {
		case errors.Is(err, interact.ErrNotInteraction):
			return nil
		case errors.Is(err, store.ErrLimit):
			log.WarnContext(ctx, "interact: a press was refused, because too many presses are waiting", "channel", i.At.Channel, "err", err)
			return nil
		}
		return err
	}
}

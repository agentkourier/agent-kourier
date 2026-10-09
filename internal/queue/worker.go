// Package queue turns the store's durable event queue into handled events.
//
// A Worker claims one event at a time, hands it to a Handler, and records the
// outcome. Delivery is at-least-once (spec 8.1, 11): a worker that crashes
// between claim and completion leaves the event in processing, and it is
// redelivered once its lease expires.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// Handler processes one claimed event.
//
// A handler's job ends once the event is durably turned into session state and
// the A2A send is accepted; streaming belongs to the session manager. Handlers
// must therefore be short, because leases are never renewed: Config.Lease must
// exceed a handler's worst case, or the event is redelivered while the first
// handler still runs. Handlers must be idempotent for the same reason. A
// non-nil error schedules a retry with backoff, and a panic counts as an error.
// Error text and panic values are logged and persisted to the event's
// last_error, so they must not include payload content or credentials (spec
// 10; payloads are untrusted).
type Handler interface {
	Handle(ctx context.Context, e store.Event) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, e store.Event) error

// Handle calls f.
func (f HandlerFunc) Handle(ctx context.Context, e store.Event) error { return f(ctx, e) }

// Config configures a Worker. Queue, Handler, Lease, and MaxAttempts are
// required.
type Config struct {
	// Queue is the store's event queue.
	Queue store.Queue
	// Handler processes claimed events.
	Handler Handler
	// Lease is how long a claim holds an event before it becomes claimable again.
	Lease time.Duration
	// MaxAttempts is how many times an event is claimed before it is dead-lettered.
	MaxAttempts int
	// Backoff returns the delay before retrying after the given failed attempt
	// (1 for the first). Nil means ExponentialBackoff(1s, 5m).
	Backoff func(attempt int) time.Duration
	// PollInterval is how long Run waits after finding the queue empty. Zero
	// means one second.
	PollInterval time.Duration
	// DrainTimeout is how long Run lets an in-flight handler finish after its
	// context is cancelled, before cancelling the handler's own context. Zero
	// means no grace.
	DrainTimeout time.Duration
	// Now is the worker's clock. Nil means time.Now.
	Now func() time.Time
	// Logger receives worker logs. Nil means slog.Default().
	Logger *slog.Logger
}

// defaultPollInterval is used when Config.PollInterval is unset.
const defaultPollInterval = time.Second

// Worker applies the queue's processing policy: retry with backoff, and
// dead-letter after MaxAttempts.
type Worker struct{ cfg Config }

// New validates cfg, fills in defaults, and returns a Worker.
func New(cfg Config) (*Worker, error) {
	switch {
	case cfg.Queue == nil:
		return nil, errors.New("queue: Queue is required")
	case cfg.Handler == nil:
		return nil, errors.New("queue: Handler is required")
	case cfg.Lease <= 0:
		return nil, fmt.Errorf("queue: Lease must be positive, got %v", cfg.Lease)
	case cfg.MaxAttempts < 1:
		return nil, fmt.Errorf("queue: MaxAttempts must be at least 1, got %d", cfg.MaxAttempts)
	case cfg.DrainTimeout < 0:
		return nil, fmt.Errorf("queue: DrainTimeout must not be negative, got %v", cfg.DrainTimeout)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Backoff == nil {
		cfg.Backoff = ExponentialBackoff(defaultBackoffBase, defaultBackoffCeiling)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{cfg: cfg}, nil
}

// RunOnce claims and processes at most one event. It reports whether an event
// was claimed. A handler failure or a lost lease is not an error: the outcome
// is recorded (or, for a lost lease, left to the new holder) and RunOnce
// returns nil. Only store failures are returned.
//
// If ctx is cancelled while the handler runs, the handler gets DrainTimeout,
// then its context is cancelled; a failure after that is abandoned to lease
// expiry. Outcomes are recorded even after ctx is cancelled.
func (w *Worker) RunOnce(ctx context.Context) (handled bool, err error) {
	e, err := w.cfg.Queue.ClaimEvent(ctx, w.cfg.Now(), w.cfg.Lease)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("queue: %w", err)
	}
	return true, w.settle(ctx, e)
}

// Run claims and processes events until ctx is cancelled, waiting PollInterval
// whenever the queue is empty or the store fails. Cancelling ctx stops claiming;
// a handler already running gets DrainTimeout to finish and is then cancelled
// and abandoned to lease expiry. Run returns nil once that is done, and no
// goroutine it started outlives it.
func (w *Worker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		handled, err := w.RunOnce(ctx)
		// A claim that fails because ctx was cancelled is expected (handled is
		// false); a failed outcome write (handled is true) must never vanish.
		if err != nil && (handled || ctx.Err() == nil) {
			w.cfg.Logger.Error("queue worker", "error", err)
		}
		if handled && err == nil {
			continue // more events may be waiting
		}
		timer := time.NewTimer(w.cfg.PollInterval)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
	return nil
}

// settle runs the handler on a claimed event and records the outcome, passing
// the claim's attempt as the fencing token. A claim past MaxAttempts (after
// crashes) is dead-lettered without running the handler. Outcomes are written
// with a context that outlives ctx's cancellation, so a handler that finishes
// during the drain is still recorded.
func (w *Worker) settle(ctx context.Context, e store.Event) (telemetryErr error) {
	ctx = telemetry.Restore(ctx, e.TraceParent, e.TraceState)
	ctx, op := telemetry.Start(ctx, "agentkourier.queue.process", attribute.String("agentkourier.event.id", e.ID), attribute.Int("agentkourier.attempt", e.Attempts))
	defer func() { op.Finish(telemetryErr) }()
	log := w.cfg.Logger.With("event", e.ID, "attempt", e.Attempts)
	q, maxAttempts := w.cfg.Queue, w.cfg.MaxAttempts
	wctx := context.WithoutCancel(ctx)
	var err error
	if e.Attempts > maxAttempts {
		log.Error("dead-lettering event claimed past its attempt limit", "limit", maxAttempts)
		msg := fmt.Sprintf("attempt %d exceeds limit %d", e.Attempts, maxAttempts)
		err = q.FailEvent(wctx, e.ID, e.Attempts, w.cfg.Now(), maxAttempts, msg)
	} else if abandoned, herr := w.runHandler(ctx, e, log); abandoned {
		log.Warn("handler abandoned at shutdown; its lease will expire", "error", herr)
		return nil
	} else if herr == nil {
		err = q.CompleteEvent(wctx, e.ID, e.Attempts)
	} else {
		retryAt := w.cfg.Now().Add(w.cfg.Backoff(e.Attempts))
		if e.Attempts == maxAttempts { // no retry will happen, so no retry_at
			log.Warn("handler failed", "error", herr)
			log.Error("dead-lettering event after its final attempt", "limit", maxAttempts)
		} else {
			log.Warn("handler failed", "error", herr, "retry_at", retryAt)
		}
		err = q.FailEvent(wctx, e.ID, e.Attempts, retryAt, maxAttempts, herr.Error())
	}
	if errors.Is(err, store.ErrConflict) {
		log.Warn("lease lost; leaving the event to its new holder", "error", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("record outcome of event %q: %w", e.ID, err)
	}
	return nil
}

// runHandler runs the handler with a context detached from ctx, so cancelling
// ctx does not interrupt it at once. One watcher goroutine starts the drain
// timer when ctx is cancelled and cancels the handler's context when it fires;
// runHandler waits for the watcher before returning. abandoned reports that
// the handler failed after that cancellation, so its outcome is not recorded.
func (w *Worker) runHandler(ctx context.Context, e store.Event, log *slog.Logger) (abandoned bool, err error) {
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	finished, watcherDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}
		timer := time.NewTimer(w.cfg.DrainTimeout)
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			cancel()
		}
	}()
	err = w.handle(hctx, e, log)
	cancelled := hctx.Err() != nil // before the handler returned, not after finished closes
	close(finished)
	<-watcherDone
	return err != nil && cancelled, err
}

// handle calls the handler, converting a panic into an error so that a buggy
// handler dead-letters its event instead of taking down the process.
func (w *Worker) handle(ctx context.Context, e store.Event, log *slog.Logger) (err error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.queue.handle")
	defer func() { op.Finish(err) }()
	defer func() {
		if r := recover(); r != nil {
			log.Error("handler panicked", "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return w.cfg.Handler.Handle(ctx, e)
}

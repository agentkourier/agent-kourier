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

// Config configures a Worker. Queue, Handler, Lease, MaxAttempts, and Backoff
// are required.
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
	// (1 for the first).
	Backoff func(attempt int) time.Duration
	// Now is the worker's clock. Nil means time.Now.
	Now func() time.Time
	// Logger receives worker logs. Nil means slog.Default().
	Logger *slog.Logger
}

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
	case cfg.Backoff == nil:
		return nil, errors.New("queue: Backoff is required")
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

// settle runs the handler on a claimed event and records the outcome, passing
// the claim's attempt as the fencing token. A claim past MaxAttempts (after
// crashes) is dead-lettered without running the handler.
func (w *Worker) settle(ctx context.Context, e store.Event) error {
	log := w.cfg.Logger.With("event", e.ID, "attempt", e.Attempts)
	q, maxAttempts := w.cfg.Queue, w.cfg.MaxAttempts
	var err error
	if e.Attempts > maxAttempts {
		log.Error("dead-lettering event claimed past its attempt limit", "limit", maxAttempts)
		msg := fmt.Sprintf("attempt %d exceeds limit %d", e.Attempts, maxAttempts)
		err = q.FailEvent(ctx, e.ID, e.Attempts, w.cfg.Now(), maxAttempts, msg)
	} else if herr := w.handle(ctx, e, log); herr == nil {
		err = q.CompleteEvent(ctx, e.ID, e.Attempts)
	} else {
		retryAt := w.cfg.Now().Add(w.cfg.Backoff(e.Attempts))
		log.Warn("handler failed", "error", herr, "retry_at", retryAt)
		if e.Attempts == maxAttempts {
			log.Error("dead-lettering event after its final attempt", "limit", maxAttempts)
		}
		err = q.FailEvent(ctx, e.ID, e.Attempts, retryAt, maxAttempts, herr.Error())
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

// handle calls the handler, converting a panic into an error so that a buggy
// handler dead-letters its event instead of taking down the process.
func (w *Worker) handle(ctx context.Context, e store.Event, log *slog.Logger) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("handler panicked", "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return w.cfg.Handler.Handle(ctx, e)
}

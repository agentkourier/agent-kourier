package interact

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// PauseHandler takes up a pause: *Intake.
type PauseHandler interface {
	Paused(ctx context.Context, p Pause) error
}

// TapConfig configures a Tap. Sink and Handler are required.
type TapConfig struct {
	// Sink is the sink the tap wraps: the renderer.
	Sink    session.Sink
	Handler PauseHandler
	Logger  *slog.Logger // slog.Default() if nil
}

// Tap is a session.Sink that passes everything to the sink it wraps and watches the turns' final events
// for a task paused on input, which it hands to the intake (spec 8.4 step 1). The handler that a turn
// is given runs on the agent's stream goroutine and must not block, so the hand-off is a goroutine of its
// own and the call returns at once. Every path of Paused is idempotent, so a try that failed part way is
// finished by the next (WP-35h2b makes it, with a backoff). A pause that comes again, as 28h's recovery
// re-emits a task it finds paused after a restart, is handed off again, since that is how an answer
// recorded and not sent is sent. It is safe for concurrent use; Close stops it.
type Tap struct {
	cfg TapConfig
	ctx context.Context
	end context.CancelFunc

	mu     sync.Mutex
	flying map[string]struct{} // hand-offs in progress, by pause
	closed bool
	wg     sync.WaitGroup
}

var _ session.Sink = (*Tap)(nil)

// NewTap returns a Tap over cfg.
func NewTap(cfg TapConfig) (*Tap, error) {
	if cfg.Sink == nil || cfg.Handler == nil {
		return nil, errors.New("interact: Sink and Handler are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ctx, end := context.WithCancel(context.Background())
	return &Tap{cfg: cfg, ctx: ctx, end: end, flying: map[string]struct{}{}}, nil
}

// Turn gives the turn to the wrapped sink and returns a handler that gives it every event, and hands off
// a task that ends the turn paused for input.
func (t *Tap) Turn(ctx context.Context, ref session.TurnRef) agent.Handler {
	inner := t.cfg.Sink.Turn(ctx, ref)
	return func(e agent.Event) {
		if inner != nil {
			inner(e)
		}
		if f, ok := e.(agent.FinalEvent); ok && f.Task != nil && f.Task.Status.State == a2a.TaskStateInputRequired {
			t.handOff(ref, *f.Task)
		}
	}
}

// Notice passes the notice to the wrapped sink.
func (t *Tap) Notice(ctx context.Context, key session.Key, n session.Notice) {
	t.cfg.Sink.Notice(ctx, key, n)
}

// Close ends the hand-offs, cuts the ones that are in the intake short through their context, and returns once every one is over, or with ctx's error if that comes
// first. A pause that was not taken up is found again by the next start. It starts nothing after Close.
func (t *Tap) Close(ctx context.Context) error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.end()
	over := make(chan struct{})
	go func() { t.wg.Wait(); close(over) }()
	select {
	case <-over:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("interact: wait for the pause hand-offs: %w", ctx.Err())
	}
}

// handOff starts the hand-off of a pause, unless it is over or the same pause is in progress. A pause is
// the task's and the message that asked: the same task paused again is another pause.
func (t *Tap) handOff(ref session.TurnRef, task a2a.Task) {
	id := string(task.ID) + "|"
	if m := task.Status.Message; m != nil {
		id += m.ID
	}
	if ts := task.Status.Timestamp; ts != nil {
		id += "|" + ts.UTC().Format(time.RFC3339Nano)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, busy := t.flying[id]; busy || t.closed {
		return
	}
	t.flying[id] = struct{}{}
	t.wg.Add(1)
	go t.run(id, Pause{Key: ref.Key, Binding: ref.Binding, Task: task})
}

func (t *Tap) run(id string, p Pause) {
	defer t.wg.Done()
	defer func() { t.mu.Lock(); delete(t.flying, id); t.mu.Unlock() }()
	if err := t.cfg.Handler.Paused(t.ctx, p); err != nil && t.ctx.Err() == nil { // a cut-off is found again by the next start
		t.cfg.Logger.Warn("interact: a pause could not be taken up", "task", p.Task.ID, "binding", p.Binding, "err", err)
	}
}

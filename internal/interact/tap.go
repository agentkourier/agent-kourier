package interact

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// PauseHandler takes up a pause: *Intake.
type PauseHandler interface {
	Paused(ctx context.Context, p Pause) error
}

// TapConfig configures a Tap. Sink, Handler and Lookup are required.
type TapConfig struct {
	// Sink is the sink the tap wraps: the renderer.
	Sink    session.Sink
	Handler PauseHandler
	// Lookup gives a Binding's interaction timeout, from which a pause's deadline comes.
	Lookup Lookup
	// Backoff is the wait before a pause that could not be taken up is tried again (default 1 second),
	// doubling with each try up to MaxBackoff (default 30 seconds).
	Backoff, MaxBackoff time.Duration
	Now                 func() time.Time // time.Now if nil
	Logger              *slog.Logger     // slog.Default() if nil
}

// Tap is a session.Sink that passes everything to the sink it wraps and watches the turns' final events
// for a task paused on input, which it hands to the intake (spec 8.4 step 1). The handler that a turn
// is given runs on the agent's stream goroutine and must not block, so the hand-off is a goroutine of its
// own and the call returns at once. The goroutine tries Intake.Paused again after a backoff for as long
// as the pause can still be answered, which is until its own deadline: every path of Paused is idempotent,
// so a try that failed part way is finished by the next. A pause that comes again, as 28h's recovery
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
	if cfg.Sink == nil || cfg.Handler == nil || cfg.Lookup == nil {
		return nil, errors.New("interact: Sink, Handler and Lookup are required")
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = time.Second
	}
	if cfg.MaxBackoff < cfg.Backoff {
		cfg.MaxBackoff = max(30*time.Second, cfg.Backoff)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
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
			t.handOff(ctx, ref, *f.Task)
		}
	}
}

// Notice passes the notice to the wrapped sink.
func (t *Tap) Notice(ctx context.Context, key session.Key, n session.Notice) {
	t.cfg.Sink.Notice(ctx, key, n)
}

// Close ends the hand-offs, cuts the one that is in the intake short through its context and stops the
// ones that are waiting to try again, and returns once every one is over, or with ctx's error if that comes
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
func (t *Tap) handOff(ctx context.Context, ref session.TurnRef, task a2a.Task) {
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
	go t.run(ctx, id, Pause{Key: ref.Key, Binding: ref.Binding, Task: task})
}

// deadline is when the pause can no longer be answered: as the interaction's own, the task's time plus
// the Binding's timeout. A Binding that cannot be read still gets the hand-off, which then says why it fails,
// and the default timeout. It reads the Binding, so run calls it, and the stream goroutine that handed the
// pause off, which holds t.mu, does not.
func (t *Tap) deadline(binding string, task a2a.Task) time.Time {
	timeout := v1alpha1.DefaultInteractionTimeout
	if b, err := t.cfg.Lookup.Binding(binding); err != nil || b.Timeout <= 0 {
		t.cfg.Logger.Warn("interact: the Binding of a pause cannot be read; its hand-off is bounded by the default timeout", "binding", binding, "err", err)
	} else {
		timeout = b.Timeout
	}
	return pausedAt(task, t.cfg.Now()).Add(timeout)
}

func (t *Tap) run(caller context.Context, id string, p Pause) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(caller))
	defer cancel()
	defer context.AfterFunc(t.ctx, cancel)() //nolint:contextcheck // join the tap lifetime to detached turn values

	defer t.wg.Done()
	defer func() { t.mu.Lock(); delete(t.flying, id); t.mu.Unlock() }()
	deadline := t.deadline(p.Binding, p.Task)
	for tries := 1; ; tries++ {
		attemptCtx, op := telemetry.Start(ctx, "courier.interaction.pause")
		err := t.cfg.Handler.Paused(attemptCtx, p)
		op.Finish(err)
		switch {
		case err == nil || t.ctx.Err() != nil: // done, or cut off by Close: the next start finds the pause again
			return
		case errors.Is(err, ErrDisallowed): // no retry changes what the Binding allows
			t.cfg.Logger.Error("interact: a pause cannot be rendered, and will not be tried again", "task", p.Task.ID, "binding", p.Binding, "err", err)
			return
		case !t.cfg.Now().Before(deadline):
			t.cfg.Logger.Error("interact: giving up on a pause that was never taken up", "task", p.Task.ID, "binding", p.Binding, "tries", tries, "err", err)
			return
		}
		t.cfg.Logger.Warn("interact: a pause could not be taken up; it will be tried again", "task", p.Task.ID, "binding", p.Binding, "tries", tries, "err", err)
		wait := time.NewTimer(t.delay(tries))
		select {
		case <-t.ctx.Done():
			wait.Stop()
			return
		case <-wait.C:
		}
	}
}

// delay is the wait after a pause's tries-th failure.
func (t *Tap) delay(tries int) time.Duration {
	d := t.cfg.Backoff
	for i := 1; i < tries && d < t.cfg.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, t.cfg.MaxBackoff)
}

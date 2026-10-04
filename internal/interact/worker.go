package interact

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// PressProcessor does the work on one press: *Pipeline.
type PressProcessor interface {
	Process(ctx context.Context, press store.Press) (Result, error)
	// GiveUp ends a press that could not be processed, for good: the person is told, and the press is
	// deleted. An error leaves the press recorded.
	GiveUp(ctx context.Context, press store.Press) error
}

const defaultGiveUpAfter = time.Hour

// WorkerConfig configures a Worker. Everything but Processor is optional.
type WorkerConfig struct {
	Processor PressProcessor
	// Workers is how many interactions are worked on at once (default 4).
	Workers int
	// Backoff is the wait before a press that could not be processed is tried again (default 5 seconds), and
	// doubles with each try up to MaxBackoff (default 30 seconds). It is seconds and not an instant because
	// a retry that comes too soon finds what it found: a task the session has not parked again yet.
	Backoff, MaxBackoff time.Duration
	// GiveUpAfter is how old a press may be, by courier's clock from the time it was recorded, before one
	// that still cannot be processed is given up on (default 1 hour), so that a press that nothing ends
	// does not hold its interaction open for the timeout sweep. A press always gets one try first.
	GiveUpAfter time.Duration
	Now         func() time.Time // time.Now if nil
	Logger      *slog.Logger     // slog.Default() if nil
}

// Worker processes recorded presses (spec 8.4 step 3): a fixed pool of workers, with the presses of one
// interaction taken one at a time and in the order they were recorded, and those of different
// interactions in parallel. A press that cannot be processed yet waits out a backoff on a timer, holding no worker,
// and holds only its own interaction's later presses. The press store, not the Worker, is the record: a
// press the Worker has not finished is still stored, and the next start finds it.
type Worker struct {
	cfg  WorkerConfig
	wake chan struct{} // a signal that ready is not empty; one pending is enough

	mu     sync.Mutex
	lanes  map[string]*lane // by interaction: the lanes that are queued, running or waiting out a backoff
	ready  []*lane          // lanes whose first press is to be tried now, oldest first
	seen   map[int64]bool   // Seqs held in a lane, so that a press announced twice runs once
	closed bool
}

// lane is the presses of one interaction, oldest first; the first is the one being tried.
type lane struct {
	id    string
	jobs  []*job
	timer *time.Timer // set while the first press waits out a backoff
}

type job struct {
	press store.Press
	tries int
}

// NewWorker returns a Worker over cfg.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.Processor == nil {
		return nil, errors.New("interact: a Processor is required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 5 * time.Second
	}
	if cfg.MaxBackoff < cfg.Backoff {
		cfg.MaxBackoff = max(30*time.Second, cfg.Backoff)
	}
	if cfg.GiveUpAfter <= 0 {
		cfg.GiveUpAfter = defaultGiveUpAfter
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{cfg: cfg, wake: make(chan struct{}, 1), lanes: map[string]*lane{}, seen: map[int64]bool{}}, nil
}

// Submit hands the Worker a press that is recorded, and returns at once: it is Pipeline's Notify. A press
// that is already held is ignored, and so is any after Run has ended: it is recorded, and the next start
// finds it.
func (w *Worker) Submit(p store.Press) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.seen[p.Seq] {
		return
	}
	w.seen[p.Seq] = true
	if l, ok := w.lanes[p.InteractionID]; ok {
		l.jobs = append(l.jobs, &job{press: p})
		return
	}
	l := &lane{id: p.InteractionID, jobs: []*job{{press: p}}}
	w.lanes[l.id] = l
	w.ready = append(w.ready, l)
	w.signal()
}

// PressSource lists the presses that are recorded and not finished, oldest first: store.Presses.
type PressSource interface {
	ListPresses(ctx context.Context) ([]store.Press, error)
}

// Recover hands the Worker every press the store still holds, oldest first, which is how a press recorded
// before a crash, or one the Worker did not finish, is found again (spec 8.4 step 3). A press the Worker
// already holds is ignored, so Recover may run before or after Run starts. It should run before the
// intake is open: a press listed just before it is finished, and announced after, would be processed again.
func (w *Worker) Recover(ctx context.Context, src PressSource) error {
	presses, err := src.ListPresses(ctx)
	if err != nil {
		return fmt.Errorf("interact: list the presses waiting: %w", err)
	}
	for _, p := range presses {
		w.Submit(p)
	}
	return nil
}

// Run processes presses until ctx ends, then returns its error once every worker has stopped. It is for one
// call. Presses not finished by then stay recorded.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for range w.cfg.Workers {
		wg.Go(func() { w.loop(ctx) })
	}
	<-ctx.Done()
	w.mu.Lock()
	w.closed = true
	for _, l := range w.lanes {
		if l.timer != nil {
			l.timer.Stop()
		}
	}
	w.mu.Unlock()
	wg.Wait()
	return ctx.Err()
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		l := w.take()
		if l == nil {
			select {
			case <-ctx.Done():
			case <-w.wake:
			}
			continue
		}
		w.work(ctx, l)
	}
}

// take removes the lane that has waited longest to be tried, if any.
func (w *Worker) take() *lane {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.ready) == 0 {
		return nil
	}
	l := w.ready[0]
	w.ready = w.ready[1:]
	if len(w.ready) > 0 {
		w.signal() // another worker may be waiting for these
	}
	return l
}

// signal wakes a worker. The caller holds w.mu.
func (w *Worker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// work tries the first press of l once, and then finishes it, gives up on it or sets it to wait.
func (w *Worker) work(ctx context.Context, l *lane) {
	w.mu.Lock()
	j := l.jobs[0]
	w.mu.Unlock()
	ctx = telemetry.Restore(ctx, j.press.TraceParent, j.press.TraceState)
	ctx, op := telemetry.Start(ctx, "courier.interaction.process")
	defer func() { op.Finish(nil) }()
	res, err := w.cfg.Processor.Process(ctx, j.press)
	op.Finish(err)
	if ctx.Err() != nil { // cut off by shutdown: the press is not at fault, and is still recorded
		return
	}
	if res == Done {
		w.next(l)
		return
	}
	j.tries++
	if age := w.cfg.Now().Sub(j.press.PressedAt); age >= w.cfg.GiveUpAfter {
		w.cfg.Logger.ErrorContext(ctx, "interact: giving up on a press that could not be processed", "interaction", l.id, "tries", j.tries, "age", age, "err", err)
		if gerr := w.cfg.Processor.GiveUp(ctx, j.press); gerr == nil {
			w.next(l)
			return
		} else if ctx.Err() == nil {
			w.cfg.Logger.WarnContext(ctx, "interact: a press could not be given up on; it will be tried again", "interaction", l.id, "err", gerr)
		}
	} else if err != nil {
		w.cfg.Logger.WarnContext(ctx, "interact: a press could not be processed; it will be tried again", "interaction", l.id, "tries", j.tries, "err", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		l.timer = time.AfterFunc(w.delay(j.tries), func() { w.requeue(l) })
	}
}

// next drops the finished first press of l, and goes on to the next one, if there is one.
func (w *Worker) next(l *lane) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.seen, l.jobs[0].press.Seq)
	if l.jobs = l.jobs[1:]; len(l.jobs) == 0 {
		delete(w.lanes, l.id)
		return
	}
	w.ready = append(w.ready, l)
	w.signal()
}

// requeue is a lane's backoff ending.
func (w *Worker) requeue(l *lane) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.ready = append(w.ready, l)
		w.signal()
	}
}

// delay is the wait after a press's tries-th failure.
func (w *Worker) delay(tries int) time.Duration {
	d := w.cfg.Backoff
	for i := 1; i < tries && d < w.cfg.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, w.cfg.MaxBackoff)
}

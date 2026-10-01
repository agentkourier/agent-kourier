package interact

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// PressProcessor does the work on one press: *Pipeline.
type PressProcessor interface {
	Process(ctx context.Context, press store.Press) (Result, error)
}

// WorkerConfig configures a Worker. Everything but Processor is optional.
type WorkerConfig struct {
	Processor PressProcessor
	// Workers is how many interactions are worked on at once (default 4).
	Workers int
	Logger  *slog.Logger // slog.Default() if nil
}

// Worker processes recorded presses (spec 8.4 step 3): a fixed pool of workers, with the presses of one
// interaction taken one at a time and in the order they were recorded, and those of different
// interactions in parallel. A press that cannot be processed yet is not tried again until the
// next start (WP-33d1d2 paces its retry). The press store, not the Worker, is the record: a
// press the Worker has not finished is still stored, and the next start finds it.
type Worker struct {
	cfg  WorkerConfig
	wake chan struct{} // a signal that ready is not empty; one pending is enough

	mu     sync.Mutex
	lanes  map[string]*lane // by interaction: the lanes that are queued or running
	ready  []*lane          // lanes whose first press is to be tried now, oldest first
	seen   map[int64]bool   // Seqs held in a lane, so that a press announced twice runs once
	closed bool
}

// lane is the presses of one interaction, oldest first; the first is the one being tried.
type lane struct {
	id   string
	jobs []*job
}

type job struct {
	press store.Press
}

// NewWorker returns a Worker over cfg.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.Processor == nil {
		return nil, errors.New("interact: a Processor is required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
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

// work tries the first press of l once, and goes on to the next.
func (w *Worker) work(ctx context.Context, l *lane) {
	w.mu.Lock()
	j := l.jobs[0]
	w.mu.Unlock()
	res, err := w.cfg.Processor.Process(ctx, j.press)
	if ctx.Err() != nil { // cut off by shutdown: the press is not at fault, and is still recorded
		return
	}
	if res == Done {
		w.next(l)
		return
	}
	if err != nil {
		w.cfg.Logger.WarnContext(ctx, "interact: a press could not be processed; it stays recorded", "interaction", l.id, "err", err)
	}
	w.next(l) // the press is left recorded, and the lane goes on
}

// next drops the first press of l, which is finished or left recorded, and goes on to the next one, if there is one.
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

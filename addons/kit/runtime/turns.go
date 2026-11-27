package runtime

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// turns is the backend as the request handler runs it. Each Execute is one turn, which it traces as a child of the span
// in the request's context (the caller's) and counts by how it ended. a2a-go runs a turn on a context that no request's
// end cancels, so turns runs it on one that the add-on's stop cancels too (life), and keeps it until a2a-go's Cleanup,
// after the turn's last write: the stop waits for it before the lease is released and the store closed.
type turns struct {
	backend  Backend
	tel      *telemetry.Telemetry
	classify TurnClassifier // nil: every failed turn is telemetry.TurnFailed
	life     context.Context

	mu      sync.Mutex
	running map[*a2asrv.ExecutorContext]func() // the turns Execute started that a2a-go has not cleaned up
	byTask  map[a2a.TaskID]*outcome            // the outcome of each running turn, for Cancel to mark
	wg      sync.WaitGroup
}

var (
	_ a2asrv.AgentExecutor         = (*turns)(nil)
	_ a2asrv.AgentExecutionCleaner = (*turns)(nil)
)

func newTurns(life context.Context, backend Backend, tel *telemetry.Telemetry) *turns {
	t := &turns{backend: backend, tel: tel, life: life, running: map[*a2asrv.ExecutorContext]func(){}, byTask: map[a2a.TaskID]*outcome{}}
	t.classify, _ = backend.(TurnClassifier)
	return t
}

// Execute runs one turn of the backend. Once the add-on's stop has begun (a send that waited on its message ID's claim,
// or a connection still open after the drain), the turn ends at once on errStopping, which leaves its task as it was:
// the backend never runs it, and the stop does not wait for it.
func (t *turns) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	t.mu.Lock()
	if t.life.Err() != nil {
		t.mu.Unlock()
		return func(yield func(a2a.Event, error) bool) { yield(nil, errStopping) }
	}
	if _, held := t.running[ec]; !held {
		var release func()
		if slot, _ := ctx.Value(reservationKey{}).(*reservation); slot != nil {
			if !slot.taken.CompareAndSwap(false, true) {
				t.mu.Unlock()
				return func(yield func(a2a.Event, error) bool) { yield(nil, errStopping) }
			}
			release = slot.release
		}
		t.running[ec] = release
		t.wg.Add(1)
	}
	t.mu.Unlock()
	return func(yield func(a2a.Event, error) bool) {
		ctx, turn := t.tel.StartTurn(ctx, string(ec.TaskID), ec.ContextID)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(t.life, cancel)() //nolint:contextcheck // the add-on's stop ends the turn too, whatever its request's context
		o := t.start(ec.TaskID)
		defer t.end(ctx, ec.TaskID, o, turn)
		for ev, err := range t.backend.Execute(ctx, ec) {
			if !o.pass(yield, ev, err) {
				return
			}
		}
		// a2a-go ends an execution only on an event that settles or pauses its task, or on an error: a backend that
		// returns nothing once the stop has cut its turn would hold the stop for TurnStopTimeout.
		if t.life.Err() != nil && !o.errored && !o.state.Terminal() && o.state != a2a.TaskStateInputRequired {
			yield(nil, errStopping)
		}
	}
}

// Cancel marks the task's running turn canceled, so that it counts as canceled however it then ends, and has the
// backend cancel it.
func (t *turns) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	t.mu.Lock()
	if o := t.byTask[ec.TaskID]; o != nil {
		o.canceled.Store(true)
	}
	t.mu.Unlock()
	return t.backend.Cancel(ctx, ec)
}

// Cleanup runs after a2a-go has processed an execution's or a cancel's last event: the backend's own cleanup, if it has
// one, then the end of the turn's hold on the stop.
func (t *turns) Cleanup(ctx context.Context, ec *a2asrv.ExecutorContext, result a2a.SendMessageResult, err error) {
	if c, ok := t.backend.(a2asrv.AgentExecutionCleaner); ok {
		c.Cleanup(ctx, ec, result, err)
	}
	t.mu.Lock()
	release, held := t.running[ec]
	delete(t.running, ec)
	t.mu.Unlock()
	if held {
		if release != nil {
			release()
		}
		t.wg.Done()
	}
}

// wait waits up to timeout for every turn to be cleaned up, and reports whether they were. It runs once life has ended,
// and takes mu first: an Execute that found life live made its Add under mu before then, and every later one starts no
// turn, so each Add happens before the Wait.
func (t *turns) wait(timeout time.Duration) bool {
	t.mu.Lock()
	t.mu.Unlock() //nolint:staticcheck // an empty critical section: it orders every Add before the Wait
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (t *turns) start(task a2a.TaskID) *outcome {
	o := &outcome{}
	t.mu.Lock()
	t.byTask[task] = o
	t.mu.Unlock()
	return o
}

// end forgets the turn and ends its span: counted under its outcome, or, when the add-on's stop cut it short before it
// completed or paused, cut (telemetry.Turn.Cut): the leader that fails its task counts it.
func (t *turns) end(ctx context.Context, task a2a.TaskID, o *outcome, turn *telemetry.Turn) {
	t.mu.Lock()
	if t.byTask[task] == o {
		delete(t.byTask, task)
	}
	t.mu.Unlock()
	got := o.of(ctx, t.classify)
	if t.life.Err() != nil && got != telemetry.TurnCompleted && got != telemetry.TurnInputRequired {
		turn.Cut()
		return
	}
	turn.End(got)
}

// outcome follows what a turn yields, to say how it ended. Its fields are the turn's goroutine's, but for canceled,
// which Cancel sets.
type outcome struct {
	state    a2a.TaskState
	status   a2a.TaskStatus // the last status the turn yielded
	errored  bool           // the turn ended on an error
	stopped  bool           // the consumer stopped taking events
	canceled atomic.Bool
}

// pass yields ev and err, noting the state and error it passes and whether the consumer stopped.
func (o *outcome) pass(yield func(a2a.Event, error) bool, ev a2a.Event, err error) bool {
	if err != nil {
		o.errored = true
	}
	switch v := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		o.state, o.status = v.Status.State, v.Status
	case *a2a.Task:
		o.state, o.status = v.Status.State, v.Status
	}
	if !yield(ev, err) {
		o.stopped = true
		return false
	}
	return true
}

// of is the turn's outcome once it has returned. A cancel wins over whatever the turn yielded after it; one that ended
// on an error failed; one that stopped before it settled or paused was canceled. A failed turn is the classifier's to
// name.
func (o *outcome) of(ctx context.Context, classify TurnClassifier) telemetry.TurnOutcome {
	switch {
	case o.canceled.Load(), o.state == a2a.TaskStateCanceled:
		return telemetry.TurnCanceled
	case o.state == a2a.TaskStateCompleted && !o.errored:
		return telemetry.TurnCompleted
	case o.state == a2a.TaskStateInputRequired && !o.errored:
		return telemetry.TurnInputRequired
	case o.state == a2a.TaskStateFailed:
		if classify != nil {
			if named := classify.FailedOutcome(o.status); named != "" {
				return named
			}
		}
		return telemetry.TurnFailed
	case o.errored:
		return telemetry.TurnFailed
	case o.stopped || ctx.Err() != nil:
		return telemetry.TurnCanceled
	}
	return telemetry.TurnFailed
}

type reservationKey struct{}

type reservation struct {
	taken   atomic.Bool
	release func()
}

func (r *reservation) releaseUnclaimed() {
	if r.taken.CompareAndSwap(false, true) {
		r.release()
	}
}

// errStopping is a2a-go's write of a task refused, or a turn not started, once the add-on has begun to stop its turns:
// the task stays as it was, and the leader that takes over next fails it as interrupted, as it would had the process
// ended.
var errStopping = errors.New("the add-on is stopping: the task is left for the next leader to settle")

// guardedTasks is the task store a2a-go writes through, which refuses its writes once stop has run.
type guardedTasks struct {
	taskstore.Store
	stopped atomic.Bool
}

func (g *guardedTasks) stop() { g.stopped.Store(true) }

// Create implements taskstore.Store.
func (g *guardedTasks) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if g.stopped.Load() {
		return taskstore.TaskVersionMissing, errStopping
	}
	return g.Store.Create(ctx, task)
}

// Update implements taskstore.Store.
func (g *guardedTasks) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if g.stopped.Load() {
		return taskstore.TaskVersionMissing, errStopping
	}
	return g.Store.Update(ctx, req)
}

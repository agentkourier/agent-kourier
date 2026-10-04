package broker

import (
	"context"
	"log/slog"
	"time"
)

// stepName is one stage of shutdown, in the order they run: what feeds a stage stops before it.
type stepName int

const (
	stepIntake       stepName = iota // the Slack run and its intake stop, so nothing new reaches the interactions
	stepInteractions                 // what the interactions started stops: the press worker, the sweeper, the replacer, retention and the pause tap, and the trigger-thread sweep
	stepManager                      // the session manager stops its runners
	stepDrain                        // the outbox stops, then posts what is pending one last time, and Slack's idle connections close
	stepPool                         // the agent clients close
	stepResolver                     // the Reloader stops, then the Secret informers
	stepStore                        // the store closes, after everything that writes to it
	stepHealth                       // the health server goes last, so /healthz answers to the end
)

func (n stepName) String() string {
	return [...]string{"intake", "interactions", "manager", "drain", "pool", "resolver", "store", "health"}[n]
}

// Defaults of a shutdown: the total stays inside the Deployment's 40 s terminationGracePeriodSeconds
// (and Kubernetes' default of 30 s), and a step is never left less than the floor, so a late step such
// as the store's still gets its turn.
const (
	shutdownTotal = 25 * time.Second
	shutdownFloor = time.Second
)

// defaultBounds are how long each step may take (D7). The first seven add up to the total, and the
// total caps them all.
func defaultBounds() map[stepName]time.Duration {
	return map[stepName]time.Duration{
		stepIntake: 4 * time.Second, stepInteractions: 3 * time.Second, stepManager: 7 * time.Second, stepDrain: 5 * time.Second,
		stepPool: 2 * time.Second, stepResolver: 2 * time.Second, stepStore: 2 * time.Second, stepHealth: 2 * time.Second,
	}
}

// stopList collects what the interactions start and must stop (the press worker, the sweeper, the
// replacer, the retention loop and the pause tap add theirs as WP-35h2, 35h8 and 35h9 build them), to
// be run at stepInteractions.
type stopList struct {
	stops []namedStop
	trace func(name string) // a test's: told each stop's name as it begins
}

type namedStop struct {
	name string
	fn   func(context.Context)
}

// add registers fn, which must return when its context ends, under name.
func (s *stopList) add(name string, fn func(context.Context)) {
	s.stops = append(s.stops, namedStop{name, fn})
}

// run calls every stop, the one added last first, as parts stop in the reverse order of their start.
// A stop that waits on its context holds up the ones after it only until the step is cut.
func (s *stopList) run(ctx context.Context) {
	for i := len(s.stops) - 1; i >= 0; i-- {
		if s.trace != nil {
			s.trace(s.stops[i].name)
		}
		s.stops[i].fn(ctx)
	}
}

// shutdown runs the steps Run started, in the order of stepName whatever order they were added in,
// each with its own bound and all within one total budget. A step that overruns is cut: its context
// ends, one line says so, and the next step runs. It owns the final order (WP-35f2c1).
type shutdown struct {
	log    *slog.Logger
	steps  map[stepName]func(context.Context)
	total  time.Duration
	trace  func(step string) // a test's: called as each step begins, before it runs
	floor  time.Duration
	bounds map[stepName]time.Duration
}

func newShutdown(log *slog.Logger) *shutdown {
	return &shutdown{log: log, steps: map[stepName]func(context.Context){}, total: shutdownTotal, floor: shutdownFloor, bounds: defaultBounds()}
}

// waitFor waits for done to be closed, or for ctx to end, whichever is first.
func waitFor(ctx context.Context, done <-chan struct{}) {
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// add registers what to do at the step n. fn must return when its context ends.
func (s *shutdown) add(n stepName, fn func(context.Context)) { s.steps[n] = fn }

// run performs the registered steps once, and forgets them. The steps' contexts do not inherit ctx's
// end, which is what started the shutdown.
func (s *shutdown) run(ctx context.Context) {
	deadline := time.Now().Add(s.total)
	base := context.WithoutCancel(ctx)
	for n := stepIntake; n <= stepHealth; n++ {
		fn, ok := s.steps[n]
		if !ok {
			continue
		}
		delete(s.steps, n)
		if s.trace != nil {
			s.trace(n.String())
		}
		bound := min(s.bounds[n], max(time.Until(deadline), s.floor))
		stepCtx, cancel := context.WithCancel(base)
		done := make(chan struct{})
		go func() { defer close(done); fn(stepCtx) }()
		timer := time.NewTimer(bound)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C: // not waited for after this: a step stuck past its context must not hold up the ones after it
			s.log.Warn("shutdown step cut", "step", n.String(), "bound", bound)
		}
		cancel()
	}
}

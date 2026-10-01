package broker

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// recorder notes the order in which steps ran.
type recorder struct {
	mu    sync.Mutex
	order []string
}

func (r *recorder) step(name string) func(context.Context) {
	return func(context.Context) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.order = append(r.order, name)
	}
}

func (r *recorder) got() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.order, ",")
}

// newTestShutdown is a shutdown that logs to out, with a budget a test can use.
func newTestShutdown(out *logBuffer, total, floor time.Duration) *shutdown {
	s := newShutdown(slog.New(slog.NewTextHandler(out, nil)))
	s.total, s.floor = total, floor
	return s
}

// runWithin runs the shutdown and fails the test if it has not finished within limit. A shutdown that
// does not cut a stuck step never finishes, and the test must say so, not wait for the binary's timeout.
func runWithin(t *testing.T, s *shutdown, limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); s.run(t.Context()) }()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("the shutdown did not finish within %s: a stuck step was not cut", limit)
	}
}

// The order is the contract: the intake stops first, so nothing new reaches the interactions, which
// stop before the manager they feed, and the store closes last, after everything that writes to it.
// Registration order does not matter.
func TestShutdownRunsTheStepsInTheAgreedOrderWhateverTheRegistrationOrder(t *testing.T) {
	var r recorder
	s := newTestShutdown(&logBuffer{}, 10*time.Second, time.Second)
	for _, n := range []stepName{stepStore, stepHealth, stepResolver, stepInteractions, stepIntake, stepPool, stepDrain, stepManager} { // scrambled
		s.add(n, r.step(n.String()))
	}

	s.run(t.Context())

	if want := "intake,interactions,manager,drain,pool,resolver,store,health"; r.got() != want {
		t.Errorf("shutdown ran %s, want %s", r.got(), want)
	}
}

// The bounds are agreed (D7), and the seven that make up the total add up to it, so a step that is cut
// has never been given time that the steps after it need.
func TestShutdownDefaultBoundsAreTheAgreedOnesAndFitTheTotal(t *testing.T) {
	want := map[stepName]time.Duration{
		stepIntake: 4 * time.Second, stepInteractions: 3 * time.Second, stepManager: 7 * time.Second, stepDrain: 5 * time.Second,
		stepPool: 2 * time.Second, stepResolver: 2 * time.Second, stepStore: 2 * time.Second, stepHealth: 2 * time.Second,
	}
	got := defaultBounds()
	var sum time.Duration
	for n, d := range want {
		if got[n] != d {
			t.Errorf("the bound of %s is %s, want %s", n, got[n], d)
		}
		if n != stepHealth { // the health server stops last, outside the budget, so that /healthz answers to the end
			sum += got[n]
		}
	}
	if len(got) != len(want) || sum != shutdownTotal {
		t.Errorf("%d bounds adding up to %s (without health), want %d adding up to %s", len(got), sum, len(want), shutdownTotal)
	}
}

// The stops of the interactions' parts run, last started first, each with the step's own context, and
// one that is slow does not keep the others from running once the step's context has ended.
func TestStopListRunsEveryStopLastAddedFirst(t *testing.T) {
	var r, told recorder
	s := stopList{trace: func(name string) { told.step(name)(context.Background()) }} // told each stop as it begins
	s.add("first", r.step("first"))
	s.add("second", func(ctx context.Context) { <-ctx.Done(); r.step("second")(ctx) }) // holds until the step is cut
	s.add("third", r.step("third"))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.run(ctx) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stops did not finish once their context ended")
	}

	if want := "third,second,first"; r.got() != want || told.got() != want {
		t.Errorf("the stops ran as %s and were told as %s, want %s for both", r.got(), told.got(), want)
	}
}

func TestShutdownRunsOnlyWhatWasStartedAndOnlyOnce(t *testing.T) {
	var r recorder
	s := newTestShutdown(&logBuffer{}, 10*time.Second, time.Second)
	s.add(stepStore, r.step("store"))
	s.add(stepHealth, r.step("health"))

	s.run(t.Context())
	s.run(t.Context())

	if r.got() != "store,health" {
		t.Errorf("shutdown ran %s, want each registered step once", r.got())
	}
}

// A step that does not finish in its bound is cut, its context ends, and the next steps still run.
// Operators read the log during a slow rollout, so the line names the step and the bound and holds nothing else.
func TestShutdownCutsAStepThatOverrunsItsBoundAndSaysSo(t *testing.T) {
	before := goleak.IgnoreCurrent()
	var r recorder
	out := &logBuffer{}
	s := newTestShutdown(out, 10*time.Second, time.Second)
	s.bounds[stepManager] = 50 * time.Millisecond
	s.add(stepManager, func(ctx context.Context) { <-ctx.Done() }) // hangs until it is cut
	s.add(stepStore, r.step("store"))

	begin := time.Now()
	runWithin(t, s, 2*time.Second)

	if took := time.Since(begin); took > 2*time.Second {
		t.Errorf("shutdown took %s with a 50 ms bound on the stuck step", took)
	}
	if r.got() != "store" {
		t.Errorf("a step after the cut one ran as %q, want store", r.got())
	}
	line := out.String()
	if strings.Count(line, "shutdown step cut") != 1 || !strings.Contains(line, "step=manager") || !strings.Contains(line, "bound=50ms") {
		t.Errorf("the log %q should hold one 'shutdown step cut' line naming step=manager and bound=50ms", line)
	}
	goleak.VerifyNone(t, before)
}

// The total budget caps the whole shutdown, but every step still gets the floor, so the store is
// closed even when the steps before it used the budget up.
func TestShutdownTotalBudgetCapsItAndLaterStepsKeepTheirFloor(t *testing.T) {
	var r recorder
	out := &logBuffer{}
	s := newTestShutdown(out, 150*time.Millisecond, 40*time.Millisecond)
	for _, n := range []stepName{stepIntake, stepManager, stepDrain} {
		s.bounds[n] = 800 * time.Millisecond // the total, not these bounds, must stop them
		s.add(n, func(ctx context.Context) { <-ctx.Done() })
	}
	s.add(stepStore, r.step("store"))

	begin := time.Now()
	runWithin(t, s, 2*time.Second)

	if took := time.Since(begin); took > 700*time.Millisecond { // 150 ms, then 40 ms for each later step; the bounds are 800 ms
		t.Errorf("shutdown took %s with a 150 ms total", took)
	}
	if r.got() != "store" {
		t.Errorf("the last step did not run (%q): a spent budget must still leave it its floor", r.got())
	}
	if n := strings.Count(out.String(), "shutdown step cut"); n != 3 {
		t.Errorf("%d steps were cut, want the 3 that hung: %s", n, out.String())
	}
}

// A shutdown starts when the signal has already ended the run's context, so it must not inherit that cancellation.
func TestShutdownStepsAreNotGivenAnEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var got error
	s := newTestShutdown(&logBuffer{}, 10*time.Second, time.Second)
	s.add(stepManager, func(c context.Context) { got = c.Err() })

	s.run(ctx)

	if got != nil {
		t.Errorf("a step's context was already done: %v", got)
	}
}

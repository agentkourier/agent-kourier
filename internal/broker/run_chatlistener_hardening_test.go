package broker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// A request that hangs at the shutdown is cut once the drain's bound has passed, unanswered, and the intakes, which stop after the
// listener in the same step, still get their share of it: the connection's run is halted no sooner than chatDrainTimeout into the intake
// step, before the interactions' step begins, and the intake step is not cut.
//
// The order is read where it is decided, not from a goroutine woken by the halt: the trace's hook for the interactions' step runs in the
// shutdown's goroutine before that step does, and the run's halt is synchronous, so the run's context has ended by then or the halt came
// late. The halt's time is bounded on each side by a time that cannot come before it: from above by the interactions' step's start (once
// the order holds), from below by a context.AfterFunc stamp, which runs only after the context has ended. A slow wake-up only moves that
// stamp later, away from the lower bound it is held to, so it cannot fail the test. The times keep their monotonic readings.
func TestRunCutsAHungRequestAndLeavesTheIntakesTheirShare(t *testing.T) {
	hang, entered := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(hang) }) // after Run's own cleanup has waited for it
	var once sync.Once
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		once.Do(func() { close(entered) })
		<-hang // ignores its context: a handler that is stuck
	})
	var runCtx atomic.Pointer[context.Context]
	var intakeAt, interactionsAt time.Time // written by the hook in Run's goroutine, read once r.wait has seen Run return
	var haltedByInteractions bool
	trace := &stepTrace{hook: func(step string) {
		switch step {
		case "intake":
			intakeAt = time.Now()
		case "interactions":
			interactionsAt = time.Now()
			haltedByInteractions = (*runCtx.Load()).Err() != nil
		}
	}}
	logs := &logBuffer{}
	slk := &ctxSlack{got: make(chan context.Context, 1)}
	c := startConfig(t)
	c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	c.newSlack = func(slack.Config) slackAPI { return slk }
	c.shutdownTrace = trace.record
	addr, _ := chatListen(t, &c, h)
	started := leaderUp(t, &c)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	ctx := <-slk.got
	runCtx.Store(&ctx)
	halted := make(chan time.Time, 1)
	context.AfterFunc(ctx, func() { halted <- time.Now() })
	started()

	answered := make(chan int, 1)
	go func() { answered <- chatPost(t, http.MethodPost, addr, ownRoute) }()
	<-entered
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after its context ended, want nil", err)
	}

	if code := <-answered; code != -1 {
		t.Errorf("the hung request was answered %d, want it cut unanswered, for the platform to send again", code)
	}
	if interactionsAt.IsZero() {
		t.Fatalf("the interactions' step never began: the shutdown ran %s", trace.got())
	}
	if !haltedByInteractions {
		t.Fatal("the connection's run was still live as the interactions' step began: the intakes lost their share")
	}
	var haltedAt time.Time
	select {
	case haltedAt = <-halted:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection's run was halted, yet the context.AfterFunc on it never ran")
	}
	// The drain's bound starts inside the intake step and its timer never fires early, so a halt that waits for the drain comes
	// chatDrainTimeout or more into the step, and the stamp, which never comes before the halt, too: no slack is needed below it.
	if into := haltedAt.Sub(intakeAt); into < chatDrainTimeout {
		t.Errorf("the connection's run was halted %s into the intake step, before the drain's %s had passed", into, chatDrainTimeout)
	}
	if took := interactionsAt.Sub(intakeAt); took > chatDrainTimeout+time.Second {
		t.Errorf("the intake step, in which the run was halted, took %s, want about the drain's %s", took, chatDrainTimeout)
	}
	if strings.Contains(logs.String(), "step=intake") {
		t.Errorf("the intake step was cut:\n%s", logs.String())
	}
}

// A leader that loses its lease drops its routes before it exits: at the exit, the own connection's route, which a moment before reached
// its handler, answers 503.
func TestRunALeaderThatLosesItsLeaseDropsItsRoutesBeforeItExits(t *testing.T) {
	t.Parallel()
	client, fence := cluster(), &fakeFence{}
	var unreachable atomic.Bool
	client.PrependReactor("*", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if unreachable.Load() {
			return true, nil, errors.New("the API server is unreachable")
		}
		return false, nil, nil
	})
	ca, a := electionConfig(t, client, fence, "pod-a_1")
	addr, _ := chatListen(t, &ca, &heard{})
	var atExit atomic.Int32
	ca.exit = func(code int) {
		atExit.Store(int32(chatPost(t, http.MethodPost, addr, ownRoute)))
		a.exits <- code
	}
	a.running = start(t, ca)
	testutil.Eventually(t, "the leader's own route never reached its handler", func() bool {
		return chatPost(t, http.MethodPost, addr, ownRoute) == http.StatusOK
	}, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))

	unreachable.Store(true)

	select {
	case code := <-a.exits:
		if code != ExitLeadershipLost {
			t.Errorf("exit code = %d; want %d", code, ExitLeadershipLost)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the process did not exit after the lease could not be renewed")
	}
	if got := atExit.Load(); got != http.StatusServiceUnavailable {
		t.Errorf("at the exit the own route answered %d, want 503", got)
	}
}

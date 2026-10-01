package broker

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// heldSlack is a Slack whose Run, once its context ends, does not return until the test lets it.
type heldSlack struct {
	idleSlack
	started sync.WaitGroup
	release chan struct{}
}

func (s *heldSlack) Run(ctx context.Context, _ slack.Sink) error {
	s.started.Done()
	<-ctx.Done()
	<-s.release
	return ctx.Err()
}

// Run does not return while the Slack run is still stopping: the shutdown waits for the goroutine, so
// nothing it was feeding is closed under it, and nothing outlives Run.
func TestRunDoesNotReturnWhileTheSlackRunIsStillStopping(t *testing.T) {
	held := &heldSlack{release: make(chan struct{})}
	held.started.Add(1)
	var releaseOnce sync.Once
	c := startConfig(t)
	c.newSlack = func(slack.Config) slackAPI { return held }
	r := start(t, c)
	t.Cleanup(func() { releaseOnce.Do(func() { close(held.release) }) }) // after start's own, so it runs first
	r.until("/readyz", http.StatusOK)
	held.started.Wait()

	r.cancel()

	select {
	case err := <-r.done:
		t.Fatalf("Run returned (%v) while the Slack run was still held", err)
	case <-time.After(300 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(held.release) })
	if err := r.wait(); err != nil {
		t.Errorf("Run = %v after the Slack run was released, want nil", err)
	}
}

// failedSlack is a Slack whose Run ends at once with the error a rejected token gives.
type failedSlack struct{ idleSlack }

func (failedSlack) Run(context.Context, slack.Sink) error { return slackErr("invalid_auth") }

// A rejected token is a failure however a signal races it: if the signal arrives as the broker shuts
// down for that failure, the exit is still the failure's, and not the clean stop of a signal.
func TestRunKeepsSlackAuthWhenASignalFollowsIt(t *testing.T) {
	var run *running
	assigned := make(chan struct{})
	trace := &stepTrace{hook: func(step string) {
		if step == "intake" { // the shutdown for the failure has begun: now the signal arrives
			<-assigned
			run.cancel()
		}
	}}
	c := startConfig(t)
	c.newSlack = func(slack.Config) slackAPI { return failedSlack{} }
	c.shutdownTrace = trace.record
	run = start(t, c)
	close(assigned)

	if err := run.wait(); !errors.Is(err, ErrSlackAuth) {
		t.Errorf("Run = %v, want ErrSlackAuth: a signal after the failure must not turn it into a clean stop", err)
	}
}

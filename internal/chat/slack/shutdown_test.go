package slack_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// heldRun runs a client whose sink holds the first message it is given until open is called, with queued more messages of the
// same channel waiting on the lane behind it. It returns once they are queued. calls counts the sink's calls.
func heldRun(t *testing.T, queued int) (fake *fakeslack.Server, sk *sink, cancel context.CancelFunc, wait func() error, open func(), calls *atomic.Int32, ids []string) {
	t.Helper()
	fake = newFake(t)
	sk = &sink{}
	held, release := make(chan struct{}), make(chan struct{})
	open = sync.OnceFunc(func() { close(release) })
	t.Cleanup(open) // a failed check must not leave the client stuck
	calls = new(atomic.Int32)
	var first sync.Once
	sk.fn = func(slack.Message) error {
		calls.Add(1)
		first.Do(func() { close(held); <-release })
		return nil
	}
	cancel, wait = run(t, slack.New(clientConfig(fake)), sk)
	waitConnected(t, fake)
	for i := 0; i <= queued; i++ {
		ids = append(ids, send(t, fake, onChannel("C1", fmt.Sprintf("%d.1", i+1), "x")))
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the sink was not called")
	}
	time.Sleep(200 * time.Millisecond) // the reader has taken the rest and queued them behind the held one
	return fake, sk, cancel, wait, open, calls, ids
}

// Once Run's context has ended, no event that waits on a lane is given to the sink: each is left unacknowledged, as a shed
// one is, so that Slack sends it again to the next Run. Without this a channel whose Binding is costly to match would pay for
// every event it had queued after the broker had decided to stop.
func TestEventsQueuedOnALaneAreNotGivenToTheSinkAfterRunEnds(t *testing.T) {
	fake, _, cancel, wait, open, calls, ids := heldRun(t, 5)
	cancel()
	open()
	_ = wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("the sink was called %d times; want once, for the event it held when Run was cancelled", got)
	}
	for _, id := range ids[1:] { // the envelopes the fake sent after the held one
		if _, acked := fake.Ack(id); acked {
			t.Errorf("envelope %s, which never reached the sink, was acknowledged", id)
		}
	}
}

// Run does not return while the sink holds an event: the next Run, after a reconnect, must not overlap the lanes of this
// one, which would handle one channel's events at the same time, out of order.
func TestRunWaitsForTheEventTheSinkHolds(t *testing.T) {
	_, _, cancel, wait, open, _, _ := heldRun(t, 0)
	cancel()
	returned := make(chan struct{})
	go func() { _ = wait(); close(returned) }()
	select {
	case <-returned:
		t.Fatal("Run returned while the sink held an event")
	case <-time.After(300 * time.Millisecond):
	}
	open()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the sink did")
	}
}

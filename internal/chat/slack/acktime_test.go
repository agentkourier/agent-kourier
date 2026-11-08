package slack_test

import (
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

// The acknowledgment time of an event counts from when the adapter took it off the connection, so the time it waited on
// its channel's lane, which Slack's clock also runs through, is part of it.
func TestTheAcknowledgmentTimeIncludesTheWaitOnTheLane(t *testing.T) {
	const hold = 200 * time.Millisecond
	// The first event's hold starts only once the second is on the lane behind it, so the second waits out all of it, however
	// late the reader takes the second off the connection.
	queued := make(chan struct{})
	release := sync.OnceFunc(func() { close(queued) })
	var first sync.Once
	sk := &sink{fn: func(slack.Message) error {
		first.Do(func() { <-queued })
		time.Sleep(hold)
		return nil
	}}
	fake := newFake(t)
	h := telemetrytest.New(t)
	client := slack.New(clientConfig(fake))
	cancel, wait := runIn(h.Ctx, t, client, sk)
	t.Cleanup(release) // runs before Run is ended, so that a failed check does not leave the sink stuck
	waitConnected(t, fake)
	ids := []string{send(t, fake, onChannel("C1", "1.1", "first")), send(t, fake, onChannel("C1", "2.1", "second"))}
	for deadline := time.Now().Add(5 * time.Second); client.PendingOn("C1") != 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("C1's lane holds %d events, want 2", client.PendingOn("C1"))
		}
	}
	release()
	waitAck(t, fake, ids...)
	// The time is observed after the acknowledgment is written, so the fake can see an ack before its time is counted. Run
	// returns only when every event it took has been handled, its observation included.
	cancel()
	_ = wait()

	families, err := h.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "agentkourier_slack_ack_duration_seconds" {
			continue
		}
		for _, m := range f.Metric {
			if labels := m.GetLabel(); len(labels) != 1 || labels[0].GetValue() != "acknowledged" {
				continue // the fake Slack's hello is withheld at once
			}
			// The first took one hold, and the second the first's and its own: three holds. Were the wait on the lane left
			// out, they would take two.
			count, sum := m.Histogram.GetSampleCount(), m.Histogram.GetSampleSum()
			if count != 2 || sum < 2.5*hold.Seconds() {
				t.Fatalf("%d acknowledgments took %.3fs in all; want 2 that took at least %.3fs", count, sum, 2.5*hold.Seconds())
			}
			return
		}
	}
	t.Fatalf("no acknowledgment time was observed: %s", h.Exported(t))
}

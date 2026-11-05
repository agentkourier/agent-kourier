package slack_test

import (
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// The acknowledgment time of an event counts from when the adapter took it off the connection, so the time it waited on
// its channel's lane, which Slack's clock also runs through, is part of it.
func TestTheAcknowledgmentTimeIncludesTheWaitOnTheLane(t *testing.T) {
	const hold = 200 * time.Millisecond
	sk := &sink{}
	fake, h, _ := startObserved(t, sk)
	sk.fn = func(slack.Message) error { time.Sleep(hold); return nil }
	waitAck(t, fake, send(t, fake, onChannel("C1", "1.1", "first")), send(t, fake, onChannel("C1", "2.1", "second")))
	families, err := h.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "agentkourier_slack_ack_duration_seconds" {
			continue
		}
		var count uint64
		var sum float64
		for _, m := range f.Metric {
			count += m.Histogram.GetSampleCount()
			sum += m.Histogram.GetSampleSum()
		}
		// The first took one hold, and the second the hold it waited out and its own: three holds, less what the clocks lose.
		// (The fake Slack's other envelopes are acknowledged at once, and add to the count and not the sum.)
		if count < 2 || sum < 2.5*hold.Seconds() {
			t.Fatalf("%d acknowledgments took %.3fs in all; want at least 2 that took %.3fs", count, sum, 2.5*hold.Seconds())
		}
		return
	}
	t.Fatal("no acknowledgment time was observed")
}

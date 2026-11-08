package slack_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// Two connections of one broker share its observer and registry: the series of each carry its own connection, and the adapters do not
// add to each other's.
const (
	connA = "agent-kourier-system/slack-main"
	connB = "team-a/slack"
)

// connected runs a Client for the connection conn against a fake Slack of its own, under the observer of h, and returns the fake, and the
// func that ends Run and waits for it. The end of the test does that too.
func connected(t *testing.T, h *telemetrytest.Harness, conn string, sk slack.Sink) (fake *fakeslack.Server, stop func()) {
	t.Helper()
	fake = newFake(t)
	cfg := clientConfig(fake)
	cfg.Connection = conn
	cancel, wait := runIn(h.Ctx, t, slack.New(cfg), sk)
	waitConnected(t, fake)
	return fake, func() { cancel(); _ = wait() }
}

// Every call of the Web API is counted under the connection of the client that made it.
func TestChatCallsAreCountedByConnection(t *testing.T) {
	h := telemetrytest.New(t)
	for conn, calls := range map[string]int{connA: 1, connB: 3} {
		c := refusingClientFor(t, "some_error", conn)
		for i := range calls {
			if _, err := c.Post(h.Ctx, "C1", "", slack.Content{Text: "text"}, fmt.Sprint("ob-", i)); err == nil {
				t.Fatal("Post succeeded")
			}
		}
	}
	for conn, want := range map[string]float64{connA: 1, connB: 3} {
		labels := map[string]string{"operation": "Post", "outcome": "api_error", "connection": conn}
		if got := h.Counter(t, "agentkourier_chat_api_calls_total", labels); got != want {
			t.Errorf("agentkourier_chat_api_calls_total%v = %v; want %v: %s", labels, got, want, h.Exported(t))
		}
	}
}

// An event that the lanes would not take is counted under the connection whose adapter shed it.
func TestShedEventsAreCountedByConnection(t *testing.T) {
	h := telemetrytest.New(t)
	for conn, extra := range map[string]int{connA: 2, connB: 3} {
		held, release := make(chan struct{}), make(chan struct{})
		open := sync.OnceFunc(func() { close(release) })
		var first sync.Once
		sk := &sink{fn: func(slack.Message) error {
			first.Do(func() { close(held); <-release })
			return nil
		}}
		fake, _ := connected(t, h, conn, sk)
		t.Cleanup(open) // after connected's, so that it runs before the wait for Run: a failed check would otherwise leave the sink held
		for i := range slack.MaxChannelBacklog {
			send(t, fake, onChannel("C1", fmt.Sprintf("%d.1", i+1), "kept"))
		}
		<-held
		for i := range extra {
			send(t, fake, onChannel("C1", fmt.Sprintf("%d.2", 1000+i), "shed"))
		}
		labels := map[string]string{"reason": "channel_backlog", "connection": conn}
		for deadline := time.Now().Add(5 * time.Second); h.Counter(t, "agentkourier_slack_events_shed_total", labels) < float64(extra); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the events shed by %s were not counted: %s", conn, h.Exported(t))
			}
		}
		open()
	}
	for conn, want := range map[string]float64{connA: 2, connB: 3} {
		labels := map[string]string{"reason": "channel_backlog", "connection": conn}
		if got := h.Counter(t, "agentkourier_slack_events_shed_total", labels); got != want {
			t.Errorf("agentkourier_slack_events_shed_total%v = %v; want %v", labels, got, want)
		}
	}
}

// The acknowledgment time of an event is observed under the connection of the adapter that took the event.
func TestAckTimeIsObservedByConnection(t *testing.T) {
	h := telemetrytest.New(t)
	var stops []func()
	for conn, events := range map[string]int{connA: 1, connB: 2} {
		fake, stop := connected(t, h, conn, &sink{})
		stops = append(stops, stop)
		var ids []string
		for i := range events {
			ids = append(ids, send(t, fake, onChannel("C1", fmt.Sprintf("%d.1", i+1), "hello")))
		}
		waitAck(t, fake, ids...)
	}
	// The time is observed after the acknowledgment is written, and Run returns only when every event it took has been handled.
	for _, stop := range stops {
		stop()
	}
	families, err := h.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, f := range families {
		if f.GetName() != "agentkourier_slack_ack_duration_seconds" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["outcome"] == "acknowledged" {
				got[labels["connection"]] += m.Histogram.GetSampleCount()
			}
		}
	}
	if got[connA] != 1 || got[connB] != 2 || len(got) != 2 {
		t.Errorf("acknowledgments observed by connection = %v; want 1 for %s, 2 for %s, and no other", got, connA, connB)
	}
}

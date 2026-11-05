package slack_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// The WARN line for shed events is paced, so the events shed after a line are counted and said when Run ends: a count that
// is still pending then is logged once, and is never lost with the connection.
func TestTheShedEventsNotYetLoggedAreLoggedWhenRunEnds(t *testing.T) {
	sk := &sink{}
	fake, h, logs, cancel, wait := startObservedRun(t, sk)
	held, release := make(chan struct{}), make(chan struct{})
	open := sync.OnceFunc(func() { close(release) })
	t.Cleanup(open)
	var first sync.Once
	sk.fn = func(slack.Message) error {
		first.Do(func() { close(held); <-release })
		return nil
	}
	for i := 0; i < slack.MaxChannelBacklog; i++ {
		send(t, fake, onChannel("C1", fmt.Sprintf("%d.1", i+1), "kept"))
	}
	<-held
	const extra = 3
	for i := 0; i < extra; i++ {
		send(t, fake, onChannel("C1", fmt.Sprintf("%d.2", 1000+i), "private-text"))
	}
	for deadline := time.Now().Add(5 * time.Second); h.Counter(t, "agentkourier_slack_events_shed_total", map[string]string{"reason": "channel_backlog"}) < extra; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the shed events were not counted: %s", h.Exported(t))
		}
	}
	if strings.Contains(logs.String(), "alsoShedSinceLastLine=2") {
		t.Fatal("the count of the events shed after the first line was logged before Run ended")
	}
	cancel()
	open()
	_ = wait()
	if !strings.Contains(logs.String(), "alsoShedSinceLastLine=2") {
		t.Errorf("the two events shed after the first line were not counted in the log when Run ended:\n%s", logs.String())
	}
}

// A button press waits on its channel's lane like a message does: it is not given to the sink while the channel's earlier
// event is held, and it is acknowledged when the sink has taken it.
func TestAPressWaitsOnItsChannelsLane(t *testing.T) {
	fake, sk := setup(t)
	ts := prompt(t, fake) // a message with a button, in C1
	held, release := make(chan struct{}), make(chan struct{})
	open := sync.OnceFunc(func() { close(release) })
	t.Cleanup(open)
	sk.fn = func(slack.Message) error { close(held); <-release; return nil }
	send(t, fake, onChannel("C1", "1.1", "holds the lane"))
	<-held
	press, err := fake.Click("U1", ts, "submit", "int-1")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if n := len(sk.interactions()); n != 0 {
		t.Fatalf("%d presses reached the sink while its channel's lane was held", n)
	}
	if _, acked := fake.Ack(press); acked {
		t.Fatal("the press was acknowledged while its channel's lane was held")
	}
	open()
	waitAck(t, fake, press)
	if n := len(sk.interactions()); n != 1 {
		t.Fatalf("%d presses reached the sink, want 1", n)
	}
}

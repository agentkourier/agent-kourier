package slack_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// startObservedRun is start with a client that logs to a buffer and a context that carries an observer, for a test that reads
// the metrics and the log of the adapter. cancel ends Run, and wait returns its error; the end of the test does both.
func startObservedRun(t *testing.T, sk slack.Sink) (fake *fakeslack.Server, h *telemetrytest.Harness, logs *syncBuffer, cancel context.CancelFunc, wait func() error) {
	t.Helper()
	fake = newFake(t)
	h = telemetrytest.New(t)
	logs = &syncBuffer{}
	cfg := slack.Config{BotToken: secret.New(botToken), AppToken: secret.New(appToken), APIURL: fake.URL() + "/", Connection: intakeConn,
		Logger: slog.New(slog.NewTextHandler(logs, nil))}
	cancel, wait = runIn(h.Ctx, t, slack.New(cfg), sk)
	waitConnected(t, fake)
	return fake, h, logs, cancel, wait
}

// startObserved is startObservedRun for a test that does not end Run itself.
func startObserved(t *testing.T, sk slack.Sink) (*fakeslack.Server, *telemetrytest.Harness, *syncBuffer) {
	t.Helper()
	fake, h, logs, _, _ := startObservedRun(t, sk)
	return fake, h, logs
}

// onChannel returns a message event in channel with a ts of its own.
func onChannel(channel, ts, text string) map[string]any {
	return message(map[string]any{"channel": channel, "ts": ts, "event_ts": ts, "text": text})
}

// A sink that is slow for one channel does not hold up another's events or their acknowledgments: that is the point of
// handling a channel's events on a lane of its own. The slow channel's own event is acknowledged when its sink returns, and
// not before.
func TestASlowSinkDoesNotDelayAnotherChannelsAcknowledgment(t *testing.T) {
	sk := &sink{}
	fake := start(t, sk)
	held, release := make(chan struct{}), make(chan struct{})
	open := sync.OnceFunc(func() { close(release) })
	t.Cleanup(open) // a failed check must not leave the client stuck
	sk.fn = func(m slack.Message) error {
		if m.Channel == "C1" {
			close(held)
			<-release
		}
		return nil
	}
	slow := send(t, fake, onChannel("C1", "1.1", "slow"))
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the sink was not called")
	}
	waitAck(t, fake, send(t, fake, onChannel("C2", "2.1", "fast")), send(t, fake, onChannel("C3", "3.1", "fast")))
	if _, acked := fake.Ack(slow); acked {
		t.Fatal("the slow channel's event was acknowledged while its sink held it")
	}
	open()
	waitAck(t, fake, slow)
}

// The events of a channel are handled one at a time and in the order they arrived, whatever each takes, and the events of
// other channels go on beside them.
func TestTheEventsOfAChannelAreHandledInOrderAndOneAtATime(t *testing.T) {
	const perChannel = 8
	sk := &sink{}
	fake := start(t, sk)
	var mu sync.Mutex
	done := map[string][]string{} // the texts, by channel, in the order the sink finished them
	var inside = map[string]int{}
	var overlapped atomic.Bool
	sk.fn = func(m slack.Message) error {
		mu.Lock()
		inside[m.Channel]++
		if inside[m.Channel] > 1 {
			overlapped.Store(true)
		}
		mu.Unlock()
		time.Sleep(time.Duration(perChannel-len(m.Text)) * 5 * time.Millisecond) // the earlier the longer: concurrent handling would reverse them
		mu.Lock()
		inside[m.Channel]--
		done[m.Channel] = append(done[m.Channel], m.Text)
		mu.Unlock()
		return nil
	}
	var ids []string
	want := map[string][]string{}
	for i := 0; i < perChannel; i++ {
		for _, ch := range []string{"C1", "C2", "C3"} {
			text := strings.Repeat("x", i+1)
			want[ch] = append(want[ch], text)
			ids = append(ids, send(t, fake, onChannel(ch, fmt.Sprintf("%d.%d", i+1, len(ch)), text)))
		}
	}
	waitAck(t, fake, ids...)
	mu.Lock()
	defer mu.Unlock()
	for ch, w := range want {
		if fmt.Sprint(done[ch]) != fmt.Sprint(w) {
			t.Errorf("channel %s was handled in the order %v, want %v", ch, done[ch], w)
		}
	}
	if overlapped.Load() {
		t.Error("two events of one channel were handled at once")
	}
}

// An event that arrives while its channel's lane holds as many as it may is not queued: it is left unacknowledged, so Slack
// sends it again, and the adapter counts it and logs it at WARN, naming where it was and never what it said. The events the
// lane did hold are handled and acknowledged as before.
func TestAnEventPastTheChannelBacklogIsLeftUnacknowledged(t *testing.T) {
	sk := &sink{}
	fake, h, logs := startObserved(t, sk)
	held, release := make(chan struct{}), make(chan struct{})
	open := sync.OnceFunc(func() { close(release) })
	t.Cleanup(open)
	var first sync.Once
	sk.fn = func(m slack.Message) error {
		if m.Channel == "C1" {
			first.Do(func() { close(held) })
			<-release
		}
		return nil
	}
	var taken []string
	for i := 0; i < slack.MaxChannelBacklog; i++ {
		taken = append(taken, send(t, fake, onChannel("C1", fmt.Sprintf("%d.1", i+1), "kept")))
	}
	<-held
	const extra = 3
	var shed []string
	for i := 0; i < extra; i++ {
		shed = append(shed, send(t, fake, onChannel("C1", fmt.Sprintf("%d.2", 1000+i), "private-text")))
	}
	// The other channels still work.
	waitAck(t, fake, send(t, fake, onChannel("C2", "5.1", "other")))
	for deadline := time.Now().Add(5 * time.Second); h.Counter(t, "agentkourier_slack_events_shed_total", map[string]string{"reason": "channel_backlog", "connection": intakeConn}) < extra; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the shed events were not counted: %s", h.Exported(t))
		}
	}
	open()
	waitAck(t, fake, taken...)
	for _, id := range shed {
		if _, acked := fake.Ack(id); acked {
			t.Errorf("a shed event %s was acknowledged", id)
		}
	}
	if got := h.Counter(t, "agentkourier_slack_events_shed_total", map[string]string{"reason": "channel_backlog", "connection": intakeConn}); got != extra {
		t.Errorf("shed events counted = %v, want %d", got, extra)
	}
	log := logs.String()
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "channel_backlog") || !strings.Contains(log, "channel=C1") {
		t.Errorf("the log does not say, at WARN, that a C1 event was shed for channel_backlog:\n%s", log)
	}
	if strings.Contains(log, "private-text") {
		t.Error("the log has the text of a shed message")
	}
}

// A sink that fails still leaves its event unacknowledged, on a lane as before, and the lane goes on to the next.
func TestAFailedEventOnALaneIsLeftUnacknowledgedAndTheNextIsHandled(t *testing.T) {
	sk := &sink{}
	fake := start(t, sk)
	sk.fn = func(m slack.Message) error {
		if m.Text == "fails" {
			return errors.New("store unavailable")
		}
		return nil
	}
	failed := send(t, fake, onChannel("C1", "1.1", "fails"))
	waitAck(t, fake, send(t, fake, onChannel("C1", "2.1", "fine")))
	if _, acked := fake.Ack(failed); acked {
		t.Fatal("an event the sink failed on was acknowledged")
	}
}

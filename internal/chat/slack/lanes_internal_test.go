package slack

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLanesPage holds the page docs/reference/limits.md to the bounds of the events the adapter holds.
func TestLanesPage(t *testing.T) {
	limitsdoc.Count(t, "channelBacklog", maxChannelBacklog)
	limitsdoc.Count(t, "pendingEvents", maxPendingEvents)
	limitsdoc.Duration(t, "shedLogEvery", shedLogEvery)
}

// The events of one channel run one at a time, in the order they were submitted, though each takes a different time.
func TestALaneRunsItsEventsInOrder(t *testing.T) {
	l := newLanes(100, 100)
	var mu sync.Mutex
	var got []int
	var running, overlap int
	for i := range 20 {
		if shed := l.submit("C1", func() {
			mu.Lock()
			running++
			if running > 1 {
				overlap++
			}
			mu.Unlock()
			time.Sleep(time.Duration(20-i) * time.Millisecond) // the earlier the longer: a parallel run would reverse them
			mu.Lock()
			got = append(got, i)
			running--
			mu.Unlock()
		}); shed != "" {
			t.Fatalf("event %d was shed: %s", i, shed)
		}
	}
	l.wait()
	want := make([]int, 20)
	for i := range want {
		want[i] = i
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
	if overlap > 0 {
		t.Fatalf("two events of one channel ran at once")
	}
}

// An event that holds its channel's lane does not hold another channel's.
func TestALaneDoesNotHoldAnotherChannel(t *testing.T) {
	l := newLanes(100, 100)
	held, release := make(chan struct{}), make(chan struct{})
	l.submit("C1", func() { close(held); <-release })
	<-held
	other := make(chan struct{})
	l.submit("C2", func() { close(other) })
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("an event of C2 was held behind C1's")
	}
	close(release)
	l.wait()
}

// The events a lane holds are bounded per channel and in all. An event past a bound is shed with the reason, nothing is
// queued for it, and the room comes back as the held events finish.
func TestLanesAreBounded(t *testing.T) {
	l := newLanes(2, 3)
	release := make(chan struct{})
	hold := func(channel string) string { return l.submit(channel, func() { <-release }) }
	for _, ch := range []string{"C1", "C1", "C2"} {
		if shed := hold(ch); shed != "" {
			t.Fatalf("an event of %s was shed: %s", ch, shed)
		}
	}
	// The bound of the channel is told first, as it names the channel that is behind.
	if shed := hold("C1"); shed != shedChannelBacklog {
		t.Fatalf("a third event held for C1 was shed as %q, want %q", shed, shedChannelBacklog)
	}
	if shed := hold("C3"); shed != shedAdapterBacklog {
		t.Fatalf("a fourth event held in all was shed as %q, want %q", shed, shedAdapterBacklog)
	}
	close(release)
	l.wait()
	// Once they finish the lanes are gone and nothing is counted as held.
	if n, p := len(l.byChannel), l.pending; n != 0 || p != 0 {
		t.Fatalf("after wait %d lanes and %d events remain", n, p)
	}
}

// The room that finished events leave is given back: a channel that was full takes another event.
func TestLaneRoomIsGivenBack(t *testing.T) {
	l := newLanes(1, 10)
	release := make(chan struct{})
	l.submit("C1", func() { <-release })
	if shed := l.submit("C1", func() {}); shed != shedChannelBacklog {
		t.Fatalf("shed = %q, want %q", shed, shedChannelBacklog)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if shed := l.submit("C1", func() {}); shed == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the channel never took an event again")
		}
		time.Sleep(time.Millisecond)
	}
	l.wait()
}

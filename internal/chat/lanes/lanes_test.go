package lanes

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the bounds of the lanes. The Slack adapter's rows and the Teams
// receiver's name the same bounds: both run on New's lanes.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "channelBacklog", PerChannel)
	limitsdoc.Count(t, "pendingEvents", Total)
	limitsdoc.Count(t, "teamsChannelBacklog", PerChannel)
	limitsdoc.Count(t, "teamsPending", Total)
}

// New's lanes are bounded by PerChannel and Total, the values the page holds.
func TestNewHasTheBoundsOfThePage(t *testing.T) {
	l := New()
	if l.maxLane != PerChannel || l.maxAll != Total {
		t.Fatalf("New bounds a lane at %d and all at %d, want %d and %d", l.maxLane, l.maxAll, PerChannel, Total)
	}
}

// The items of one channel run one at a time, in the order they were submitted, though each takes a different time.
func TestALaneRunsItsItemsInOrder(t *testing.T) {
	l := newLanes(100, 100)
	var mu sync.Mutex
	var got []int
	var running, overlap int
	for i := range 20 {
		if shed := l.Submit("C1", func() {
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
			t.Fatalf("item %d was shed: %s", i, shed)
		}
	}
	l.Wait()
	want := make([]int, 20)
	for i := range want {
		want[i] = i
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
	if overlap > 0 {
		t.Fatalf("two items of one channel ran at once")
	}
}

// An item that holds its channel's lane does not hold another channel's.
func TestALaneDoesNotHoldAnotherChannel(t *testing.T) {
	l := newLanes(100, 100)
	held, release := make(chan struct{}), make(chan struct{})
	l.Submit("C1", func() { close(held); <-release })
	<-held
	other := make(chan struct{})
	l.Submit("C2", func() { close(other) })
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("an item of C2 was held behind C1's")
	}
	close(release)
	l.Wait()
}

// The items the lanes hold are bounded per channel and in all. An item past a bound is shed with the reason and nothing is
// queued for it. The bound of the channel is told first, as it names the channel that is behind; a channel that holds none
// always takes its first, so the lanes hold more than the total when many channels have one; and a channel that holds one is
// held to the total. Once the held items finish the lanes are gone and nothing is counted as held.
func TestLanesShedPastTheirBounds(t *testing.T) {
	type step struct{ channel, want string }
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"the channel's bound before the total", []step{
			{"C1", ""}, {"C1", ""}, {"C2", ""},
			{"C1", ShedChannelBacklog},
			{"C3", ""},
			{"C2", ShedAdapterBacklog},
		}},
		{"a channel full, then the total", []step{
			{"a", ""}, {"a", ""}, {"a", ShedChannelBacklog},
			{"b", ""}, {"b", ShedAdapterBacklog},
			{"c", ""}, {"c", ShedAdapterBacklog},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLanes(2, 3)
			release := make(chan struct{})
			block := func() { <-release }
			for i, s := range tc.steps {
				if got := l.Submit(s.channel, block); got != s.want {
					close(release)
					l.Wait()
					t.Fatalf("step %d, submit to %s: shed %q, want %q", i, s.channel, got, s.want)
				}
			}
			close(release)
			l.Wait()
			if n, held := len(l.byChannel), l.Held(); n != 0 || held != 0 {
				t.Fatalf("after Wait %d lanes and %d items remain", n, held)
			}
		})
	}
}

// However many channels are hot, every other channel can still have its first item taken: a hostile Binding owns one channel,
// and several of them must not fill the lanes so that a healthy channel's items are all shed. With the total full a channel
// takes one item at a time, so a second item for it is shed.
func TestEveryChannelHasASlot(t *testing.T) {
	l := newLanes(PerChannel, 4)
	release := make(chan struct{})
	for i := range 4 { // four hot channels fill the total
		if shed := l.Submit(string(rune('A'+i)), func() { <-release }); shed != "" {
			t.Fatalf("hot channel %d: %q", i, shed)
		}
	}
	if shed := l.Submit("A", func() {}); shed != ShedAdapterBacklog {
		t.Fatalf("a hot channel's next item was shed as %q, want %q", shed, ShedAdapterBacklog)
	}
	healthy := make(chan struct{})
	if shed := l.Submit("healthy", func() { close(healthy) }); shed != "" {
		t.Fatalf("a healthy channel's first item was shed as %q with the lanes full", shed)
	}
	select {
	case <-healthy:
	case <-time.After(5 * time.Second):
		t.Fatal("the healthy channel's item did not run")
	}
	close(release)
	l.Wait()
}

// The room that finished items leave is given back: a channel that was full takes another item.
func TestLaneRoomIsGivenBack(t *testing.T) {
	l := newLanes(1, 10)
	release := make(chan struct{})
	l.Submit("C1", func() { <-release })
	if shed := l.Submit("C1", func() {}); shed != ShedChannelBacklog {
		t.Fatalf("shed = %q, want %q", shed, ShedChannelBacklog)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if shed := l.Submit("C1", func() {}); shed == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the channel never took an item again")
		}
		time.Sleep(time.Millisecond)
	}
	l.Wait()
}

// Held counts the items of all lanes, the ones running included, and falls to zero once they ran.
func TestHeldCountsTheItemsHeld(t *testing.T) {
	l := newLanes(PerChannel, Total)
	running, release := make(chan struct{}), make(chan struct{})
	l.Submit("C1", func() { close(running); <-release })
	<-running
	l.Submit("C1", func() {})
	l.Submit("C1", func() {})
	held := make(chan struct{})
	l.Submit("C2", func() { <-held })
	if n := l.Held(); n != 4 {
		t.Errorf("Held = %d, want 4", n)
	}
	close(release)
	close(held)
	l.Wait()
	if n := l.Held(); n != 0 {
		t.Fatalf("after Wait Held = %d, want 0", n)
	}
}

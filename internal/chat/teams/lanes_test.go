package teams

import (
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// TestLanesKeepAChannelsOrderAndRunChannelsApart: a channel's functions run in order, one at a time, and a channel whose function is
// held does not hold up another channel.
func TestLanesKeepAChannelsOrderAndRunChannelsApart(t *testing.T) {
	l := newLanes(channelBacklog, pendingActivities)
	hold := make(chan struct{})
	var mu sync.Mutex
	var order []int
	l.submit("slow", func() { <-hold })
	for i := range 5 {
		l.submit("quick", func() {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, i)
		})
	}
	testutil.Eventually(t, "the quick channel runs while the slow one is held", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 5
	})
	for i, got := range order {
		if got != i {
			t.Fatalf("order %v, want 0 to 4", order)
		}
	}
	close(hold)
	l.wait()
	if got := l.held(); got != 0 {
		t.Fatalf("held %d after wait", got)
	}
}

// TestLanesShedPastTheirBounds: a channel's lane takes channelBacklog activities, and once all lanes hold pendingActivities a channel
// with one held is refused another, while a channel with none still takes its first.
func TestLanesShedPastTheirBounds(t *testing.T) {
	l := newLanes(2, 3)
	hold := make(chan struct{})
	defer func() { close(hold); l.wait() }()
	block := func() { <-hold }
	for _, step := range []struct {
		channel, want string
	}{
		{"a", ""}, {"a", ""}, {"a", shedChannelBacklog},
		{"b", ""}, {"b", shedAdapterBacklog},
		{"c", ""}, {"c", shedAdapterBacklog},
	} {
		if got := l.submit(step.channel, block); got != step.want {
			t.Fatalf("submit to %s: %q, want %q", step.channel, got, step.want)
		}
	}
}

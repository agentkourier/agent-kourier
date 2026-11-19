// Package lanes holds what a chat adapter has taken from its platform while the broker works through it. Each item is run on a
// lane of its channel, one at a time and in order, and the lanes of different channels run at the same time, so that a sink that
// is slow for one channel (a Binding whose triggers are costly to match) holds up that channel's items, and not the adapter's
// intake or the items of a channel whose sink is quick. It is no isolation of the CPU: lanes that are busy compete for it.
//
// An item past either bound is not queued, and Submit says why (shed). What a shed means to the platform is the adapter's: the
// Slack adapter leaves the event unacknowledged, so that Slack sends it again, and the Teams receiver answers 503 and releases the
// activity's dedup key. It is a leaf: it imports nothing of the module.
package lanes

import "sync"

// The bounds of the lanes that New returns.
const (
	// PerChannel is the items a channel's lane holds: the one being run and those that wait behind it.
	PerChannel = 16
	// Total is the items all lanes hold, past which a channel that already has one held is refused another. A channel with none
	// is never refused its first, however busy the others are, but once the total is full a channel takes one at a time: a second
	// item while its first is held, such as a press during a message or the next alert of a burst, is shed. The lanes hold at most
	// this many items and one for each other channel with an item waiting.
	Total = 256
)

// The reasons Submit sheds an item, which the adapters count as the reason label of their shed metric.
const (
	ShedChannelBacklog = "channel_backlog" // its channel's lane holds as many items as it may
	ShedAdapterBacklog = "adapter_backlog" // the lanes hold as many items as they may in all, and its channel has one held
)

// Lanes runs functions one channel at a time, each channel's in the order submitted. A lane exists while it holds an item: its
// goroutine starts with the first and ends with the last, so an idle channel costs nothing. It is safe for concurrent use.
type Lanes struct {
	maxLane, maxAll int
	wg              sync.WaitGroup

	mu        sync.Mutex
	byChannel map[string]*lane // guarded by mu
	pending   int              // the items held in all lanes, those being run included; guarded by mu
}

// lane is the items of one channel that are not yet done.
type lane struct {
	queue []func() // waiting, oldest first
	held  int      // waiting, plus the one running
}

// New returns lanes that hold at most PerChannel items for a channel and Total in all.
func New() *Lanes { return newLanes(PerChannel, Total) }

// newLanes returns lanes that hold at most perChannel items for a channel and total in all.
func newLanes(perChannel, total int) *Lanes {
	return &Lanes{maxLane: perChannel, maxAll: total, byChannel: map[string]*lane{}}
}

// Submit queues fn on the lane of channel, to run after the earlier items of that channel. It returns the reason, a Shed*
// constant, if the channel's lane holds its bound, or the lanes as a whole hold theirs and the channel already has an item held,
// and then runs nothing; otherwise "". The first item of a channel that holds none is always taken, so the lanes hold more than
// the total when many channels each have one. It never blocks on fn.
func (l *Lanes) Submit(channel string, fn func()) (shed string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ln, running := l.byChannel[channel]
	switch {
	case running && ln.held >= l.maxLane:
		return ShedChannelBacklog
	case running && l.pending >= l.maxAll:
		// A channel that holds no item is never refused its first: however many channels are hot, a healthy one has a slot.
		return ShedAdapterBacklog
	}
	if !running {
		ln = &lane{}
		l.byChannel[channel] = ln
		l.wg.Add(1)
		go l.drain(channel, ln)
	}
	ln.queue = append(ln.queue, fn)
	ln.held++
	l.pending++
	return ""
}

// drain runs the items of a lane in order, and ends, taking the lane away, when it has none.
func (l *Lanes) drain(channel string, ln *lane) {
	defer l.wg.Done()
	for {
		l.mu.Lock()
		if len(ln.queue) == 0 {
			delete(l.byChannel, channel)
			l.mu.Unlock()
			return
		}
		fn := ln.queue[0]
		ln.queue[0] = nil
		ln.queue = ln.queue[1:]
		l.mu.Unlock()

		fn()

		l.mu.Lock()
		ln.held--
		l.pending--
		l.mu.Unlock()
	}
}

// Wait returns when every item submitted has run. It is for the end of a run: Submit must not be called while it waits.
func (l *Lanes) Wait() { l.wg.Wait() }

// Held is the items the lanes hold now, those being run included.
func (l *Lanes) Held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pending
}

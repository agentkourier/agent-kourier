package slack

import "sync"

// What the adapter holds of the events it has read while the sink works through them. Each event is handled on a lane of its
// channel, one at a time and in order, and the lanes of different channels run at the same time, so that a sink that is slow
// for one channel (a Binding whose triggers are costly to match) delays that channel's events and no other's. An event past
// either bound is not queued: the adapter leaves it unacknowledged, so Slack sends it again, and says so (shedReason).
const (
	// maxChannelBacklog is the events a channel's lane holds: the one being handled and those that wait behind it.
	maxChannelBacklog = 64
	// maxPendingEvents is the events all lanes hold, which is also the most goroutines the lanes run.
	maxPendingEvents = 256
)

// The reasons an event is shed.
const (
	shedChannelBacklog = "channel_backlog" // its channel's lane holds maxChannelBacklog events
	shedAdapterBacklog = "adapter_backlog" // the lanes hold maxPendingEvents events in all
)

// lanes runs functions one channel at a time, each channel's in the order submitted. A lane exists while it holds an event:
// its goroutine starts with the first and ends with the last, so an idle channel costs nothing.
type lanes struct {
	maxLane, maxAll int
	wg              sync.WaitGroup

	mu        sync.Mutex
	byChannel map[string]*lane // guarded by mu
	pending   int              // the events held in all lanes, those being run included; guarded by mu
}

// lane is the events of one channel that are not yet done.
type lane struct {
	queue []func() // waiting, oldest first
	held  int      // waiting, plus the one running
}

// newLanes returns lanes that hold at most perChannel events for a channel and total in all.
func newLanes(perChannel, total int) *lanes {
	return &lanes{maxLane: perChannel, maxAll: total, byChannel: map[string]*lane{}}
}

// submit queues fn on the lane of channel, to run after the earlier events of that channel. It returns the reason, a shed*
// constant, if the lane or the lanes as a whole hold their bound, and then runs nothing; otherwise "". It never blocks on fn.
func (l *lanes) submit(channel string, fn func()) (shed string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ln, running := l.byChannel[channel]
	switch {
	case running && ln.held >= l.maxLane:
		return shedChannelBacklog
	case l.pending >= l.maxAll:
		return shedAdapterBacklog
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

// drain runs the events of a lane in order, and ends, taking the lane away, when it has none.
func (l *lanes) drain(channel string, ln *lane) {
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

// wait returns when every event submitted has run. It is for the end of a run: submit must not be called while it waits.
func (l *lanes) wait() { l.wg.Wait() }

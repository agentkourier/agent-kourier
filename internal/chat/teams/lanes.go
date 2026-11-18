package teams

import "sync"

// What the receiver holds of the activities it has answered while the sink and the store work through them. Each is handled on a lane
// of its channel, one at a time and in order, and the lanes of different channels run at the same time, so that a channel whose sink
// is slow holds up its own activities and not another channel's. An activity past either bound is not queued: it is answered 503, and
// its dedup key released. Teams sends a message answered 503 at once again once, about 0.6 seconds later (measured on a test tenant on
// 2026-11-18, for a message only), which is taken if the lane has room by then, and lost if not.
//
// These lanes are a copy of the Slack adapter's (internal/chat/slack/lanes.go), which differ only in what a shed means there (Slack
// sends the event again as often as it is refused). A shared package replaces both before the Teams adapter is wired into the broker.
const (
	// channelBacklog is the activities a channel's lane holds: the one being handled and those that wait behind it.
	channelBacklog = 16
	// pendingActivities is the activities all lanes hold, past which a channel that already has one held is refused another. A
	// channel with none is never refused its first, however busy the others are, so the lanes hold at most this many and one for each
	// other channel with an activity waiting.
	pendingActivities = 256
)

// The reasons an activity is shed.
const (
	shedChannelBacklog = "channel_backlog" // its channel's lane holds channelBacklog activities
	shedAdapterBacklog = "adapter_backlog" // the lanes hold pendingActivities activities in all
)

// lanes runs functions one channel at a time, each channel's in the order submitted. A lane exists while it holds an activity: its
// goroutine starts with the first and ends with the last, so an idle channel costs nothing.
type lanes struct {
	maxLane, maxAll int
	wg              sync.WaitGroup

	mu        sync.Mutex
	byChannel map[string]*lane // guarded by mu
	pending   int              // the activities held in all lanes, those being run included; guarded by mu
}

// lane is the activities of one channel that are not yet done.
type lane struct {
	queue []func() // waiting, oldest first
	held  int      // waiting, plus the one running
}

// newLanes returns lanes that hold at most perChannel activities for a channel and total in all.
func newLanes(perChannel, total int) *lanes {
	return &lanes{maxLane: perChannel, maxAll: total, byChannel: map[string]*lane{}}
}

// submit queues fn on the lane of channel, to run after the earlier activities of that channel. It returns the reason, a shed*
// constant, if the channel's lane holds its bound, or the lanes as a whole hold theirs and the channel already has one held, and then
// runs nothing; otherwise "". It never blocks on fn.
func (l *lanes) submit(channel string, fn func()) (shed string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ln, running := l.byChannel[channel]
	switch {
	case running && ln.held >= l.maxLane:
		return shedChannelBacklog
	case running && l.pending >= l.maxAll:
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

// drain runs the activities of a lane in order, and ends, taking the lane away, when it has none.
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

// wait returns when every activity submitted has run. It is for the end of a Serve: submit must not be called while it waits.
func (l *lanes) wait() { l.wg.Wait() }

// held is the activities the lanes hold now, those being run included.
func (l *lanes) held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pending
}

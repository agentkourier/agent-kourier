package teams

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/internal/backoff"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// This file reads the bound channels' new messages from Microsoft Graph by polling, the alert route of a Teams connection with no
// notificationURL (in either mode): Teams sends a bot no other bot's post, an alert's card that Workflows posted among them, while Graph
// gives every post. The leader alone polls, while the connection runs. It also holds the catch-up read that both routes share: at the
// start of a run, after a gap of the sockets (socket mode), and after Graph's lifecycle notification missed (push.go). What it has read
// is held in memory only: a run begins anew, with a catch-up read of the look-back.
//
// Measured on a test tenant in November 2026: GET /teams/{group}/channels/{channel}/messages/delta under the resource-specific permission
// alone, its first read slow (1.3 s) and paged by @odata.nextLink to an @odata.deltaLink, and each read of the deltaLink after about 110
// to 190 ms; a new post in the channel found by the next read of the deltaLink (read every 5 s), a reply to an existing thread NOT found
// by it; 22 reads in 100 seconds with no 429; GET …/messages?$top=20&$expand=replies answered with the roots and their replies.
// Microsoft's documentation, not measured: that a deltaLink Graph no longer takes is answered 410, or 400 with a code that asks for a
// sync anew; the replies of a root, GET …/messages/{root}/replies, newest first (its example), $top at most 50; the order of the
// channel's messages in a read of them (taken here as newest first, as the replies'); and Graph's throttle of a channel's messages, which
// a summary read in November 2026 put at 1 request a second for a channel and 20 for an app in a tenant (not seen on Microsoft's page).

// The bounds of a poll. Each bound channel is read every pollInterval, the first reads of the channels that a round finds new spread over
// one interval, through the deltaLink of its last read. The connection's reads go one at a time, pollCallGap apart at the least, so a
// connection reads at most 4 a second, well inside the 20 a second for an app in a tenant. A delta read follows at most pollMaxPages pages.
// The replies of the pollRootCap newest roots that the run read, among those posted within pollRootWindow, are read each poll, each its
// newest graphPageTop (one page). A message is new to a delta read when it was made at most pollSkew before the last read began, which
// covers a clock that runs ahead of Graph's and a post that a read sees late. A channel whose poll fails is read again after pollInterval,
// twice as long after each further failure, up to pollFailMax.
const (
	pollInterval   = 10 * time.Second
	pollCallGap    = 250 * time.Millisecond
	pollMaxPages   = 500
	pollRootCap    = 3
	pollRootWindow = time.Hour
	pollSkew       = 30 * time.Second
	pollFailMax    = 5 * time.Minute
)

// pollTick is how often the poller looks for a channel that is due.
const pollTick = time.Second

// The bounds of a catch-up read: one read of the channel's catchUpTop newest roots with their replies, of which the messages made since
// the catch-up's start, at most pollLookback ago, are handed on. pollLookback is the dedup's window (dedupWindow): a message that the run
// handed on already is remembered still, and not handed on twice; what was posted before it is not read.
const (
	pollLookback = dedupWindow
	catchUpTop   = 20
)

// The outcomes of a channel's poll, the label of agentkourier_teams_graph_polls_total: a closed set.
const (
	polledStarted   = "started"   // the run's first read of the channel: its deltaLink fetched, nothing handed on from it
	polledRead      = "polled"    // a read of the deltaLink, its new messages handed on
	polledReset     = "reset"     // Graph refused the deltaLink: read anew from now, nothing handed on from that read
	polledThrottled = "throttled" // Graph answered 429: every read of the connection waits for its Retry-After
	polledFailed    = "failed"    // a read failed in a way that may pass (a 5xx, a timeout, a full lane)
	polledRefused   = "refused"   // Graph refused the read for good (401, 403, 404, 400): the channel is failing
)

// The triggers of a catch-up read, a label of agentkourier_teams_graph_catchups_total.
const (
	catchUpStart  = "start"      // the run's first poll of the channel, or a read anew after Graph refused its deltaLink
	catchUpGap    = "socket_gap" // socket mode: a gap of the sockets ended
	catchUpMissed = "missed"     // a notificationURL: Graph's lifecycle notification missed
)

// The outcomes of a catch-up read, the other label of agentkourier_teams_graph_catchups_total.
const (
	catchUpRead       = "read"        // read, and its new messages handed on
	catchUpFailed     = "failed"      // the read failed, or its channel's lane was full
	catchUpNotRunning = "not_running" // the connection took nothing then
)

// The outcomes of a message that a poll or a catch-up read found, besides handOn's: one made before the read's bound (history, or an edit
// or another change of an older post, which delta gives again), and one that its channel's lane had no room for.
const (
	readOld  = "old"
	readShed = "shed"
)

// channelPolling is the state of a bound channel that is polled, a label of agentkourier_teams_graph_channels besides the subscriber's.
const channelPolling = "polling"

// polled is what the poller holds of one bound channel, in memory.
type polled struct {
	link     string    // the deltaLink of the last read, "" before the first; never logged, since it carries Graph's state token
	prev     time.Time // when the last read whose messages were handed on began
	next     time.Time // when it is read next
	roots    map[string]*root
	state    string // channelPolling, channelWaitingGroup or channelFailing
	warned   bool   // its failing or waiting was told at WARN
	failures int
	// catchUp, if not zero, is the start of a catch-up read that the channel's next poll makes, for the trigger catchUpWhy.
	catchUp    time.Time
	catchUpWhy string
}

// root is a thread whose replies a poll reads: when its root was made, and the newest reply that a read saw (its ID), "" before any.
type root struct {
	created time.Time
	after   string
}

// poller polls the bound channels of a connection with no notificationURL. run polls them on its own goroutine; gap is told of a gap of
// the sockets from theirs.
type poller struct {
	c                                                      *Client
	interval, callGap, rootWindow, skew, failMax, lookback time.Duration
	tick                                                   time.Duration
	rootCap, maxPages                                      int
	now                                                    func() time.Time
	sleep                                                  func(context.Context, time.Duration) bool // waitFor; a test's moves its clock
	log                                                    *slog.Logger
	wake                                                   chan struct{} // a round is wanted now; holds one
	rounds                                                 atomic.Uint64 // the rounds done, which a test waits on

	mu      sync.Mutex
	gapFrom time.Time // the earliest start of the gaps not yet acted on; guarded by mu

	// What run's goroutine alone reads and writes.
	chans    map[string]*polled
	lastCall time.Time // when the last read began
	paused   time.Time // no read before this: a 429's Retry-After, for the whole connection
}

func newPoller(c *Client) *poller {
	return &poller{c: c, interval: pollInterval, callGap: pollCallGap, rootWindow: pollRootWindow, skew: pollSkew, failMax: pollFailMax,
		lookback: pollLookback, tick: pollTick, rootCap: pollRootCap, maxPages: pollMaxPages, now: c.graph.now, sleep: waitFor, log: c.log,
		wake: make(chan struct{}, 1), chans: map[string]*polled{}}
}

// gap is told of a gap of the sockets that ended (Config.OnSocketGap): every channel is read at once, with a catch-up read of what was
// posted since the gap began, at most the look-back ago. The gap that begins a run is the run's start, whose catch-up each channel's
// first poll makes already.
func (p *poller) gap(g Gap) {
	if g.Initial {
		return
	}
	p.mu.Lock()
	if p.gapFrom.IsZero() || g.Start.Before(p.gapFrom) {
		p.gapFrom = g.Start
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// run polls the channels that lister names until ctx ends: a round every tick, and at once when a gap asks. It starts from nothing
// held. With no lister nothing is bound, and nothing is read.
func (p *poller) run(ctx context.Context, lister chat.ChannelLister) {
	clear(p.chans)
	p.lastCall, p.paused = time.Time{}, time.Time{}
	t := time.NewTicker(p.tick)
	defer t.Stop()
	defer p.setGauges(ctx, 0, 0, 0)
	for {
		p.round(ctx, lister)
		p.rounds.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.wake:
		}
	}
}

// round reads each bound channel that is due, in the order of their IDs, while the connection takes messages. A channel new to the run
// is due within one interval of now, the new ones spread over it, and its first poll makes a catch-up read of the look-back; one that no
// Binding names any longer is forgotten, with what was read of it.
func (p *poller) round(ctx context.Context, lister chat.ChannelLister) {
	if ctx.Err() != nil {
		return
	}
	var bound []string
	if lister != nil {
		bound = lister.BoundChannels()
	}
	now := p.now()
	want := map[string]bool{}
	var added []string
	for _, ch := range bound {
		want[ch] = true
		if _, ok := p.chans[ch]; !ok {
			added = append(added, ch)
		}
	}
	for ch := range p.chans {
		if !want[ch] {
			delete(p.chans, ch)
		}
	}
	for i, ch := range added {
		p.chans[ch] = &polled{roots: map[string]*root{}, state: channelPolling,
			next:    now.Add(p.interval * time.Duration(i) / time.Duration(len(added))),
			catchUp: now.Add(-p.lookback), catchUpWhy: catchUpStart}
	}
	p.mu.Lock()
	gapFrom := p.gapFrom
	p.gapFrom = time.Time{}
	p.mu.Unlock()
	if !gapFrom.IsZero() {
		from := later(gapFrom, now.Add(-p.lookback))
		for _, st := range p.chans {
			st.next = now
			if st.catchUp.IsZero() || from.Before(st.catchUp) {
				st.catchUp, st.catchUpWhy = from, catchUpGap
			}
		}
	}
	if s := p.c.recv.live.Load(); s != nil {
		for _, ch := range slices.Sorted(maps.Keys(p.chans)) {
			if ctx.Err() != nil {
				break
			}
			if st := p.chans[ch]; !p.now().Before(st.next) {
				p.poll(ctx, s, ch, st)
			}
		}
	}
	polling, waiting, failing := 0, 0, 0
	for _, st := range p.chans {
		switch st.state {
		case channelPolling:
			polling++
		case channelWaitingGroup:
			waiting++
		case channelFailing:
			failing++
		}
	}
	p.setGauges(ctx, polling, waiting, failing)
}

// setGauges sets agentkourier_teams_graph_channels for a polled connection.
func (p *poller) setGauges(ctx context.Context, polling, waiting, failing int) {
	telemetry.SetGauge(ctx, "agentkourier_teams_graph_channels", float64(polling), channelPolling, p.c.conn)
	telemetry.SetGauge(ctx, "agentkourier_teams_graph_channels", float64(waiting), channelWaitingGroup, p.c.conn)
	telemetry.SetGauge(ctx, "agentkourier_teams_graph_channels", float64(failing), channelFailing, p.c.conn)
}

// poll reads channel ch once: its delta, the replies of its recent roots, and the catch-up read it is due, each handed on on its lane. A
// channel whose team's Microsoft Entra group is not known waits, told once at WARN with the fix.
func (p *poller) poll(ctx context.Context, s *serving, ch string, st *polled) {
	start := p.now()
	group, err := p.c.teamGroup(ctx, ch)
	switch {
	case errors.Is(err, errTeamNotKnown):
		if st.state != channelWaitingGroup {
			st.state, st.warned = channelWaitingGroup, true
			p.log.WarnContext(ctx, "teams: a bound channel's messages cannot be polled yet: the Microsoft Entra group of its team is not "+
				"known. Install the app in the channel's team and mention the bot in the channel, so that an activity of the channel names "+
				"its team; the channel is polled at the next round after", "channel", ch)
		}
		st.next = start.Add(p.interval)
		return
	case err != nil:
		p.failed(ctx, ch, st, err)
		return
	}
	outcome, err := p.readDelta(ctx, s, group, ch, st, start)
	if err == nil {
		err = p.readReplies(ctx, s, group, ch, st)
	}
	if err == nil && !st.catchUp.IsZero() {
		err = p.catchUp(ctx, s, group, ch, st)
		o := catchUpRead
		if err != nil {
			o = catchUpFailed
		}
		p.c.countCatchUp(ctx, st.catchUpWhy, o)
		if err == nil {
			st.catchUp, st.catchUpWhy = time.Time{}, ""
		}
	}
	if err != nil {
		p.failed(ctx, ch, st, err)
		return
	}
	p.count(ctx, outcome)
	if st.state != channelPolling && st.warned {
		p.log.InfoContext(ctx, "teams: a bound channel's messages are polled again", "channel", ch)
	}
	st.state, st.warned, st.failures = channelPolling, false, 0
	st.next = st.next.Add(p.interval)
	if st.next.Before(start) {
		st.next = start.Add(p.interval)
	}
}

// failed records that the poll of ch failed with err: a 429 has paused the connection already (call); one that Graph refused for good
// (Classify: Permanent; a 403 is the app's missing permission, which err names) marks the channel failing, told once at WARN; another
// may pass, told at DEBUG. Each is read again after a backoff.
func (p *poller) failed(ctx context.Context, ch string, st *polled, err error) {
	if ctx.Err() != nil {
		return
	}
	v := p.c.Classify(chat.OpOther, err)
	outcome := polledFailed
	switch {
	case v.RetryAfter > 0:
		outcome = polledThrottled
	case v.Class == chat.Permanent:
		outcome = polledRefused
	}
	p.count(ctx, outcome)
	st.failures++
	st.next = p.now().Add(backoff.Doubling(p.interval, p.failMax, st.failures))
	switch {
	case outcome == polledRefused && st.state != channelFailing:
		st.state, st.warned = channelFailing, true
		p.log.WarnContext(ctx, "teams: Microsoft Graph refused a poll of a bound channel's messages; it is tried again, less often",
			"channel", ch, "error", err.Error())
	case outcome == polledRefused:
		p.log.DebugContext(ctx, "teams: Microsoft Graph refused a poll of a bound channel's messages again", "channel", ch, "error", err.Error())
	default:
		p.log.DebugContext(ctx, "teams: a poll of a bound channel's messages failed; it is tried again", "channel", ch, "outcome", outcome,
			"error", err.Error())
	}
}

// count counts one in agentkourier_teams_graph_polls_total.
func (p *poller) count(ctx context.Context, outcome string) {
	telemetry.Count(ctx, "agentkourier_teams_graph_polls_total", outcome, p.c.conn)
}

// deltaPage is a page of a read of a channel's delta.
type deltaPage struct {
	Value     []graphMessage `json:"value"`
	NextLink  string         `json:"@odata.nextLink"`
	DeltaLink string         `json:"@odata.deltaLink"`
}

// errNoDeltaLink is a delta read whose pages end with no deltaLink, or go past the pages it may read.
var errNoDeltaLink = errors.New("teams: Microsoft Graph's delta of the channel's messages named no deltaLink")

// isResync says whether err is Graph's refusal of a deltaLink it no longer takes: a 410, or a 400 whose code asks for a sync anew
// (Microsoft's documentation of delta queries, not measured for a channel's messages), or a link that is not of the Graph endpoint.
func isResync(err error) bool {
	if errors.Is(err, errNotGraph) {
		return true
	}
	var ge *GraphError
	if !errors.As(err, &ge) {
		return false
	}
	code := strings.ToLower(ge.Code)
	return ge.Status == http.StatusGone || (ge.Status == http.StatusBadRequest &&
		(strings.Contains(code, "resync") || strings.Contains(code, "syncstate") || strings.Contains(code, "deltatoken")))
}

// readDelta reads the delta of ch from its deltaLink, hands on, on the channel's lane, each message made at most skew before the last
// read began, and then keeps the new deltaLink, so that a read whose messages could not be put on the lane is made again. A new root is
// kept for its replies. The run's first read of a channel (no deltaLink yet), and a read anew after Graph refused the deltaLink (logged at
// WARN), hand on nothing: what was posted before is history, of which the catch-up read hands on the look-back.
func (p *poller) readDelta(ctx context.Context, s *serving, group, ch string, st *polled, start time.Time) (string, error) {
	first := p.c.graph.resource("teams", group, "channels", ch, "messages", "delta")
	if st.link == "" {
		link, _, err := p.delta(ctx, first)
		if err != nil {
			return "", err
		}
		st.link, st.prev = link, start
		return polledStarted, nil
	}
	link, msgs, err := p.delta(ctx, st.link)
	if isResync(err) {
		p.log.WarnContext(ctx, "teams: Microsoft Graph no longer takes the channel's delta link; it is read anew from now, and a catch-up "+
			"read hands on what was posted since the last poll, within the look-back", "channel", ch, "lookback", p.lookback.String())
		if link, _, err = p.delta(ctx, first); err != nil {
			return "", err
		}
		from := later(st.prev.Add(-p.skew), start.Add(-p.lookback))
		if st.catchUp.IsZero() || from.Before(st.catchUp) {
			st.catchUp, st.catchUpWhy = from, catchUpStart
		}
		st.link, st.prev = link, start
		return polledReset, nil
	}
	if err != nil {
		return "", err
	}
	floor := st.prev.Add(-p.skew)
	var fresh []found
	for _, m := range msgs {
		old := m.CreatedDateTime.Before(floor)
		if m.ReplyToID == "" && !old {
			p.track(st, m.ID, m.CreatedDateTime.Time, start)
		}
		fresh = append(fresh, found{root: cmp.Or(m.ReplyToID, m.ID), m: m, old: old})
	}
	if err := p.c.handOnAll(ctx, s, ch, fresh); err != nil {
		return "", err
	}
	st.link, st.prev = link, start
	return polledRead, nil
}

// delta reads link and each page it names to the deltaLink, at most maxPages, and returns the deltaLink and the messages read.
func (p *poller) delta(ctx context.Context, link string) (string, []graphMessage, error) {
	var msgs []graphMessage
	seen := map[string]bool{link: true}
	for range p.maxPages {
		var page deltaPage
		if err := p.call(ctx, "PollDelta", link, &page); err != nil {
			return "", nil, err
		}
		msgs = append(msgs, page.Value...)
		switch {
		case page.DeltaLink != "":
			return page.DeltaLink, msgs, nil
		case page.NextLink == "":
			return "", nil, errNoDeltaLink
		case seen[page.NextLink]:
			return "", nil, errPageCycle
		}
		seen[page.NextLink] = true
		link = page.NextLink
	}
	return "", nil, fmt.Errorf("%w within %d pages", errNoDeltaLink, p.maxPages)
}

// track keeps the root id, made at created, for its replies, if it was made within the window of now; the newest rootCap are kept.
func (p *poller) track(st *polled, id string, created, now time.Time) {
	if !isMessageID(id) || created.Before(now.Add(-p.rootWindow)) {
		return
	}
	if _, ok := st.roots[id]; !ok {
		st.roots[id] = &root{created: created}
	}
	p.prune(st, now)
}

// prune forgets the roots made before the window of now, and those past the newest rootCap.
func (p *poller) prune(st *polled, now time.Time) {
	for i, id := range newestFirst(st.roots) {
		if i >= p.rootCap || st.roots[id].created.Before(now.Add(-p.rootWindow)) {
			delete(st.roots, id)
		}
	}
}

// newestFirst is the IDs of roots, the newest first.
func newestFirst(roots map[string]*root) []string {
	return slices.SortedFunc(maps.Keys(roots), func(a, b string) int { return compareIDs(b, a) })
}

// readReplies reads the replies of each root that ch keeps, newest root first, one page each, and hands on those newer than the newest
// reply that a read of the root saw before; a root that Graph no longer has (404) is forgotten.
func (p *poller) readReplies(ctx context.Context, s *serving, group, ch string, st *polled) error {
	p.prune(st, p.now())
	for _, id := range newestFirst(st.roots) {
		rt := st.roots[id]
		var page graphPage
		link := p.c.graph.resource("teams", group, "channels", ch, "messages", id, "replies") + fmt.Sprintf("?$top=%d", graphPageTop)
		err := p.call(ctx, "PollReplies", link, &page)
		if isNotFound(err) {
			delete(st.roots, id)
			continue
		}
		if err != nil {
			return err
		}
		var fresh []found
		after := rt.after
		for _, m := range page.Value {
			if !isMessageID(m.ID) || (rt.after != "" && compareIDs(m.ID, rt.after) <= 0) {
				continue
			}
			if after == "" || compareIDs(m.ID, after) > 0 {
				after = m.ID
			}
			fresh = append(fresh, found{root: id, m: m})
		}
		if err := p.c.handOnAll(ctx, s, ch, fresh); err != nil {
			return err
		}
		rt.after = after
	}
	return nil
}

// catchUp makes the catch-up read that ch is due, hands on what it read that was made since the catch-up's start, and keeps the roots it
// read that are within the window for their replies, each from the newest reply that the read gave.
func (p *poller) catchUp(ctx context.Context, s *serving, group, ch string, st *polled) error {
	roots, err := p.c.readRecent(ctx, p.call, group, ch)
	if err != nil {
		return err
	}
	if err := p.c.handOnAll(ctx, s, ch, recentSince(roots, st.catchUp)); err != nil {
		return err
	}
	now := p.now()
	for _, r := range roots {
		if !isMessageID(r.ID) || r.CreatedDateTime.Before(now.Add(-p.rootWindow)) {
			continue
		}
		after := ""
		for _, m := range r.Replies {
			if isMessageID(m.ID) && (after == "" || compareIDs(m.ID, after) > 0) {
				after = m.ID
			}
		}
		if rt, ok := st.roots[r.ID]; ok {
			if after != "" && (rt.after == "" || compareIDs(after, rt.after) > 0) {
				rt.after = after
			}
			continue
		}
		st.roots[r.ID] = &root{created: r.CreatedDateTime.Time, after: after}
	}
	p.prune(st, now)
	return nil
}

// call is one read of the poller, counted as operation: it waits until the connection's last read is callGap behind and no 429's pause
// holds; a 429 pauses every read of the connection for its Retry-After (Graph throttles an app in a tenant), and is counted in
// agentkourier_teams_graph_throttled_total.
func (p *poller) call(ctx context.Context, operation, link string, out any) error {
	at := later(p.lastCall.Add(p.callGap), p.paused)
	if wait := at.Sub(p.now()); wait > 0 && !p.sleep(ctx, wait) {
		return fmt.Errorf("teams: %w", ctx.Err())
	}
	p.lastCall = p.now()
	err := p.c.graphRead(ctx, operation, link, out)
	if v := p.c.Classify(chat.OpOther, err); err != nil && v.RetryAfter > 0 {
		p.paused = p.now().Add(v.RetryAfter)
		telemetry.Count(ctx, "agentkourier_teams_graph_throttled_total", p.c.conn)
		p.log.WarnContext(ctx, "teams: Microsoft Graph throttled the connection's reads; none is made until its Retry-After has passed",
			"retryAfter", v.RetryAfter.String())
	}
	return err
}

// graphRead is one GET of link into out, cut at fetchTimeout and counted as operation in agentkourier_chat_api_calls_total.
func (c *Client) graphRead(ctx context.Context, operation, link string, out any) (err error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat."+operation)
	defer func() { c.finish(ctx, op, operation, err) }()
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	return c.graph.get(ctx, link, out)
}

// readRecent is the catch-up read of ch: its catchUpTop newest roots with their replies (GET …/messages?$top&$expand=replies), one page.
func (c *Client) readRecent(ctx context.Context, get func(context.Context, string, string, any) error, group, ch string) ([]graphMessage, error) {
	var page graphPage
	link := c.graph.resource("teams", group, "channels", ch, "messages") + fmt.Sprintf("?$top=%d&$expand=replies", catchUpTop)
	if err := get(ctx, "CatchUp", link, &page); err != nil {
		return nil, err
	}
	return page.Value, nil
}

// recentSince is what a catch-up read gave, each root and each of its replies with its root, marked old when it was made before since.
func recentSince(roots []graphMessage, since time.Time) []found {
	var out []found
	for _, r := range roots {
		out = append(out, found{root: r.ID, m: r, old: r.CreatedDateTime.Before(since)})
		for _, m := range r.Replies {
			out = append(out, found{root: r.ID, m: m, old: m.CreatedDateTime.Before(since)})
		}
	}
	return out
}

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// found is a message that a poll or a catch-up read gave: its thread's root, and whether it was made before the read's bound.
type found struct {
	root string
	m    graphMessage
	old  bool
}

// handOnAll puts what a read gave on ch's lane as one item, in the order of the messages' IDs, and returns once it is on the lane, which
// hands each on after (handOnFound). It is an error when the lane has no room or the connection takes nothing now, and the read is then
// made again.
func (c *Client) handOnAll(ctx context.Context, s *serving, ch string, items []found) error {
	if len(items) == 0 {
		return nil
	}
	items = slices.Clone(items)
	slices.SortStableFunc(items, func(a, b found) int { return compareIDs(a.m.ID, b.m.ID) })
	shed, open := s.submit(ch, func() { c.handOnFound(s, ch, items) }) //nolint:contextcheck // the lane runs on Serve's context
	switch {
	case !open:
		return errors.New("teams: the connection takes nothing now")
	case shed != "":
		for range items {
			c.countRead(c.recv.telemetryContext(ctx), readShed)
		}
		return fmt.Errorf("teams: the channel's lane has no room for what a read of Graph gave (%s)", shed)
	}
	return nil
}

// handOnFound hands on, on ch's lane, each of items that is new and not a duplicate (handOn), and counts each in
// agentkourier_teams_graph_read_messages_total.
func (c *Client) handOnFound(s *serving, ch string, items []found) {
	tctx := c.recv.telemetryContext(s.ctx)
	for _, it := range items {
		if s.ctx.Err() != nil {
			return
		}
		switch {
		case it.old:
			c.countRead(tctx, readOld)
		case c.recv.seen.has(messageKey(ch, it.m.ID)):
			c.countRead(tctx, notifiedDuplicate)
		default:
			c.countRead(tctx, c.handOn(s, ch, it.root, it.m))
		}
	}
}

// countRead counts one in agentkourier_teams_graph_read_messages_total.
func (c *Client) countRead(ctx context.Context, outcome string) {
	telemetry.Count(ctx, "agentkourier_teams_graph_read_messages_total", outcome, c.conn)
}

// readMissed is the lane's part of missed: it reads the channel of sub back and hands on what was posted since since.
func (p *push) readMissed(s *serving, sub subscription, since time.Time) {
	c := p.c
	tctx := c.recv.telemetryContext(s.ctx)
	roots, err := c.readRecent(s.ctx, c.graphRead, sub.group, sub.channel)
	if err != nil {
		if s.ctx.Err() == nil {
			c.log.WarnContext(s.ctx, "teams: the catch-up read of a channel whose notifications Graph missed failed; its posts are not read",
				"channel", sub.channel, "error", err.Error())
		}
		c.countCatchUp(tctx, catchUpMissed, catchUpFailed)
		return
	}
	items := recentSince(roots, since)
	slices.SortStableFunc(items, func(a, b found) int { return compareIDs(a.m.ID, b.m.ID) })
	c.handOnFound(s, sub.channel, items)
	c.countCatchUp(tctx, catchUpMissed, catchUpRead)
}

// countCatchUp counts one in agentkourier_teams_graph_catchups_total.
func (c *Client) countCatchUp(ctx context.Context, trigger, outcome string) {
	telemetry.Count(ctx, "agentkourier_teams_graph_catchups_total", trigger, outcome, c.conn)
}

// missed is the catch-up of a connection with a notificationURL after Graph's lifecycle notification missed for the channel of sub: one
// read of the channel's catchUpTop newest roots and their replies, made on the channel's lane, of which what was posted within the
// look-back is handed on through the shared dedup. It does not block.
func (p *push) missed(ctx context.Context, sub subscription) {
	c := p.c
	tctx := c.recv.telemetryContext(ctx)
	s := c.recv.live.Load()
	if s == nil {
		c.countCatchUp(tctx, catchUpMissed, catchUpNotRunning)
		return
	}
	since := c.recv.now().Add(-pollLookback)
	shed, open := s.submit(sub.channel, func() { p.readMissed(s, sub, since) }) //nolint:contextcheck // the lane runs on Serve's context, after the notification is answered
	if !open || shed != "" {
		c.log.WarnContext(ctx, "teams: the catch-up read of a channel whose notifications Graph missed was not made: the connection could "+
			"not take it now", "channel", sub.channel)
		c.countCatchUp(tctx, catchUpMissed, catchUpFailed)
	}
}

package teams

import (
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
// start of a run, while a channel has no deltaLink yet, after a gap of the sockets (socket mode), and after Graph's lifecycle
// notification missed (push.go). What it has read is held in memory only: a run begins anew, with a catch-up read of the look-back.
//
// Measured on a test tenant in November 2026: GET /teams/{group}/channels/{channel}/messages/delta under the resource-specific permission
// alone, its first read slow (1.3 s) and paged by @odata.nextLink to an @odata.deltaLink, and each read of the deltaLink after about 110
// to 190 ms; a new post in the channel found by the next read of the deltaLink (read every 5 s), a reply to an existing thread NOT found
// by it; 22 reads in 100 seconds with no 429; GET …/messages?$top=20&$expand=replies answered with the roots and their replies.
// Microsoft's documentation, not measured: that a deltaLink Graph no longer takes is answered 410, or 400 with a code that asks for a
// sync anew; the replies of a root, GET …/messages/{root}/replies, newest first (its example), $top at most 50; the order of the
// channel's messages in a read of them (taken here as newest first, as the replies'); and Graph's throttle of a channel's messages, which
// a summary read in November 2026 put at 1 request a second for a channel and 20 for an app in a tenant (not seen on Microsoft's page).
//
// The first read of a channel's delta pages through its whole history to the deltaLink, which on a long-lived channel is many pages:
// a sync reads at most pollSyncPages of them at each poll and none of their messages is kept. Two options of Graph's delta queries
// could make it one read, and are left for the live check because neither is verified for a channel's messages: $deltatoken=latest
// (a deltaLink from now, with no history) and $filter=lastModifiedDateTime gt {time} (the history since a time). Neither is used.

// The bounds of a poll. Each bound channel is read every pollInterval, the first reads of the channels that a round finds new spread over
// one interval, through the deltaLink of its last read. The connection's reads go one at a time, pollCallGap apart at the least, so a
// connection reads at most 4 a second, well inside the 20 a second for an app in a tenant. A channel with no deltaLink yet (a sync: the
// run's first read of it, or a read anew after Graph refused its deltaLink) reads at most pollSyncPages pages of its delta at each poll,
// at most pollMaxPages in all, past which it is failing and read by its catch-up read alone; a read of the deltaLink follows at most
// pollMaxPages pages too. The replies of the pollRootCap newest roots that the run read, among those posted within pollRootWindow, are
// read each poll, each its newest graphPageTop (one page), with those of the roots read at the poll before (so at most twice pollRootCap
// reads). A message is new to a delta read when it was made at most pollSkew before the last read began, and a reply is new to a root
// when no read gave it and it was made at most pollSkew before the newest reply that a read gave: that covers a clock that runs ahead of
// Graph's and a post that a read sees late. A channel whose poll fails is read again after pollInterval, twice as long after each further
// failure, up to pollFailMax.
const (
	pollInterval   = 10 * time.Second
	pollCallGap    = 250 * time.Millisecond
	pollSyncPages  = 5
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
	polledStarted   = "started"   // a sync reached the channel's deltaLink: nothing handed on from its pages
	polledSyncing   = "syncing"   // a sync read its pages for the poll, with no deltaLink yet; a catch-up read covered the channel
	polledTooLong   = "too_long"  // the channel's sync passed pollMaxPages: it is failing, and read by its catch-up read alone
	polledRead      = "polled"    // a read of the deltaLink, its new messages handed on
	polledReset     = "reset"     // Graph refused the deltaLink: a sync anew from now, nothing handed on from it
	polledThrottled = "throttled" // Graph answered 429: every read of the connection waits for its Retry-After
	polledFailed    = "failed"    // a read failed in a way that may pass (a 5xx, a timeout, a full lane)
	polledRefused   = "refused"   // Graph refused the read for good (401, 403, 404, 400): the channel is failing
)

// The triggers of a catch-up read, a label of agentkourier_teams_graph_catchups_total.
const (
	catchUpStart   = "start"      // the run's first poll of the channel, or a read anew after Graph refused its deltaLink
	catchUpSyncing = "syncing"    // a later poll of a channel with no deltaLink: it syncs still, or its sync passed pollMaxPages
	catchUpGap     = "socket_gap" // socket mode: a gap of the sockets ended
	catchUpMissed  = "missed"     // a notificationURL: Graph's lifecycle notification missed
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
	link    string    // the deltaLink of the last read, "" while the channel syncs; never logged, since it carries Graph's state token
	sync    string    // the page at which the channel's sync goes on, "" for its first; never logged
	pages   int       // the pages that the channel's sync has read, over its polls
	capped  bool      // its sync passed maxPages: the channel is failing, read by its catch-up read alone
	prev    time.Time // when the poll began whose delta read was last handed on, or whose sync reached the deltaLink
	covered time.Time // when the poll began whose catch-up read last succeeded
	next    time.Time // when it is read next
	roots   map[string]*root
	state   string // channelPolling, channelWaitingGroup or channelFailing
	warned  bool   // its failing or waiting was told at WARN
	// failures counts the polls that failed since the last that did not.
	failures int
	// catchUp, if not zero, is the start of a catch-up read that the channel's next poll makes, for the trigger catchUpWhy.
	catchUp    time.Time
	catchUpWhy string
}

// root is a thread whose replies a poll reads: when its root was made, when the newest reply that a read gave was made (zero before
// any), and the replies read that were made at most the skew before that one, by ID: a reply in the set, or made before the skew, is
// not handed on again.
type root struct {
	created time.Time
	newest  time.Time
	read    map[string]time.Time
}

func newRoot(created time.Time) *root { return &root{created: created, read: map[string]time.Time{}} }

// saw records that a read gave the reply id, made at made, and forgets the replies made more than skew before the newest.
func (rt *root) saw(id string, made time.Time, skew time.Duration) {
	if made.After(rt.newest) {
		rt.newest = made
	}
	rt.read[id] = made
	floor := rt.newest.Add(-skew)
	for k, t := range rt.read {
		if t.Before(floor) {
			delete(rt.read, k)
		}
	}
}

// poller polls the bound channels of a connection with no notificationURL. run polls them on its own goroutine; gap is told of a gap of
// the sockets from theirs.
type poller struct {
	c                                                      *Client
	interval, callGap, rootWindow, skew, failMax, lookback time.Duration
	tick                                                   time.Duration
	rootCap, syncPages, maxPages                           int
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
		lookback: pollLookback, tick: pollTick, rootCap: pollRootCap, syncPages: pollSyncPages, maxPages: pollMaxPages, now: c.graph.now,
		sleep: waitFor, log: c.log, wake: make(chan struct{}, 1), chans: map[string]*polled{}}
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
// held, a gap told after the last run ended included. With no lister nothing is bound, and nothing is read.
func (p *poller) run(ctx context.Context, lister chat.ChannelLister) {
	clear(p.chans)
	p.lastCall, p.paused = time.Time{}, time.Time{}
	p.mu.Lock()
	p.gapFrom = time.Time{}
	p.mu.Unlock()
	select {
	case <-p.wake:
	default:
	}
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
	if !gapFrom.IsZero() { // catchUpDue bounds the read by the look-back
		for _, st := range p.chans {
			st.next = now
			if st.catchUp.IsZero() || gapFrom.Before(st.catchUp) {
				st.catchUp, st.catchUpWhy = gapFrom, catchUpGap
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

// poll reads channel ch once: its delta (or the next pages of its sync), the replies of its recent roots, and the catch-up read it is
// due, each handed on on its lane. A channel with no deltaLink makes a catch-up read at each poll, so that a channel that syncs still, or
// whose sync passed its pages, hands on its new posts. A read that fails stops the poll only when Graph throttled it or refused it for
// good; another failure leaves the rest of the poll to be made, and the poll is counted failed. A channel whose team's Microsoft Entra
// group is not known waits, told once at WARN with the fix.
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
	before := slices.Collect(maps.Keys(st.roots))
	outcome := ""
	if st.link != "" {
		outcome, err = p.readDelta(ctx, s, ch, st, start)
	}
	syncing := st.link == ""
	if err == nil && syncing && !st.capped {
		outcome, err = p.syncDelta(ctx, group, ch, st, start, outcome)
	}
	if st.capped && outcome == "" {
		outcome = polledTooLong
	}
	if !p.halts(ctx, err) {
		rerr := p.readReplies(ctx, s, group, ch, st, before)
		err = firstError(err, rerr)
		if !p.halts(ctx, rerr) && (syncing || !st.catchUp.IsZero()) {
			err = firstError(err, p.catchUpDue(ctx, s, group, ch, st, start))
		}
	}
	if err != nil {
		p.failed(ctx, ch, st, err)
		return
	}
	p.count(ctx, outcome)
	if !st.capped {
		if st.state != channelPolling && st.warned {
			p.log.InfoContext(ctx, "teams: a bound channel's messages are polled again", "channel", ch)
		}
		st.state, st.warned = channelPolling, false
	}
	st.failures = 0
	st.next = st.next.Add(p.interval)
	if st.next.Before(start) {
		st.next = start.Add(p.interval)
	}
}

// halts says whether a read that failed with err stops the rest of the poll: the poll's context ended, Graph throttled the connection
// (the next read would wait out its Retry-After), or Graph refused the read for good.
func (p *poller) halts(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	v := p.c.Classify(chat.OpOther, err)
	return ctx.Err() != nil || v.RetryAfter > 0 || v.Class == chat.Permanent
}

// firstError is a, or b if a is nil.
func firstError(a, b error) error {
	if a != nil {
		return a
	}
	return b
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

// deltaPage is a page of a read of a channel's deltaLink.
type deltaPage struct {
	Value     []graphMessage `json:"value"`
	NextLink  string         `json:"@odata.nextLink"`
	DeltaLink string         `json:"@odata.deltaLink"`
}

// syncPage is a page of a sync: its links alone, for its messages are history, and none is kept.
type syncPage struct {
	NextLink  string `json:"@odata.nextLink"`
	DeltaLink string `json:"@odata.deltaLink"`
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

// syncDelta reads, for a channel with no deltaLink, at most syncPages pages of its delta toward the deltaLink, from the page at which the
// last poll left it, keeping none of their messages: they are history, of which the catch-up read hands on the look-back. Reaching the
// deltaLink, the channel is polled from it (polledStarted, unless outcome names the poll already). Past maxPages in all, the channel is
// failing, told once at WARN, and is read by its catch-up read alone until the run ends or no Binding names it.
func (p *poller) syncDelta(ctx context.Context, group, ch string, st *polled, start time.Time, outcome string) (string, error) {
	or := func(o string) string {
		if outcome != "" {
			return outcome
		}
		return o
	}
	link := st.sync
	if link == "" {
		link = p.c.graph.resource("teams", group, "channels", ch, "messages", "delta")
	}
	for range p.syncPages {
		if st.pages >= p.maxPages {
			st.capped, st.sync = true, ""
			st.state, st.warned = channelFailing, true
			p.log.WarnContext(ctx, "teams: the delta of a bound channel's messages named no deltaLink within the pages the broker reads; "+
				"the channel's new posts are read by its catch-up read alone (its newest threads, each interval) until the connection "+
				"restarts", "channel", ch, "pages", p.maxPages)
			return or(polledTooLong), nil
		}
		st.sync = link
		var page syncPage
		if err := p.call(ctx, "PollDelta", link, &page); err != nil {
			return "", err
		}
		st.pages++
		switch {
		case page.DeltaLink != "":
			st.link, st.prev, st.sync, st.pages = page.DeltaLink, start, "", 0
			return or(polledStarted), nil
		case page.NextLink == "":
			st.sync = "" // made again from its first page; the pages read count still
			return "", errNoDeltaLink
		case page.NextLink == link:
			st.sync = ""
			return "", errPageCycle
		}
		link = page.NextLink
	}
	st.sync = link
	return or(polledSyncing), nil
}

// readDelta reads the delta of ch from its deltaLink, hands on, on the channel's lane, each message made at most skew before the last
// read began, and then keeps the new deltaLink, so that a read whose messages could not be put on the lane is made again. A new root is
// kept for its replies. A deltaLink that Graph no longer takes (logged at WARN) leaves the channel to sync anew, from this poll, with a
// catch-up read of what was posted since the last read, within the look-back.
func (p *poller) readDelta(ctx context.Context, s *serving, ch string, st *polled, start time.Time) (string, error) {
	link, msgs, err := p.delta(ctx, st.link)
	if isResync(err) {
		p.log.WarnContext(ctx, "teams: Microsoft Graph no longer takes the channel's delta link; it is read anew from now, and a catch-up "+
			"read hands on what was posted since the last poll, within the look-back", "channel", ch, "lookback", p.lookback.String())
		from := st.prev.Add(-p.skew) // catchUpDue bounds the read by the look-back
		if st.catchUp.IsZero() || from.Before(st.catchUp) {
			st.catchUp, st.catchUpWhy = from, catchUpStart
		}
		st.link, st.sync, st.pages = "", "", 0
		return polledReset, nil
	}
	if err != nil {
		return "", err
	}
	floor := st.prev.Add(-p.skew)
	var fresh []found
	for _, m := range msgs {
		if !p.c.readable(ctx, m) {
			continue
		}
		old := p.c.madeBefore(ctx, ch, m, floor)
		if !old && threadable(m) {
			p.track(st, m.ID, m.CreatedDateTime.Time, start)
		}
		root := m.ReplyToID
		if root == "" {
			root = m.ID
		}
		fresh = append(fresh, found{root: root, m: m, old: old})
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

// threadable says whether m is a root whose replies a poll may read: a post, not a system event and not deleted, with a message ID. A
// root with no text is one (a person's image, say), since its replies may mention the bot.
func threadable(m graphMessage) bool {
	return m.ReplyToID == "" && m.MessageType == "message" && m.DeletedDateTime == nil && isMessageID(m.ID)
}

// readable says whether m names itself and its root by message IDs, as a notified message's resource must (push.go); one that does not
// is dropped, counted as ignored.
func (c *Client) readable(ctx context.Context, m graphMessage) bool {
	if isMessageID(m.ID) && (m.ReplyToID == "" || isMessageID(m.ReplyToID)) {
		return true
	}
	c.countRead(c.recv.telemetryContext(ctx), notifiedIgnored)
	return false
}

// madeBefore says whether m was made before t. A message whose createdDateTime Graph gave in no form that the broker reads (the zero
// time) was, and is logged at DEBUG.
func (c *Client) madeBefore(ctx context.Context, ch string, m graphMessage, t time.Time) bool {
	if !m.CreatedDateTime.Before(t) {
		return false
	}
	if m.CreatedDateTime.IsZero() {
		c.log.DebugContext(ctx, "teams: a message that Microsoft Graph gave has no createdDateTime that the broker reads; it is taken as "+
			"old and not handed on", "channel", ch, "message", m.ID)
	}
	return true
}

// track keeps the root id, made at created, for its replies, if it was made within the window of now. readReplies prunes the roots
// after it has read them, so that a root that a new one pushes out of the newest rootCap has its replies read once more.
func (p *poller) track(st *polled, id string, created, now time.Time) {
	if created.Before(now.Add(-p.rootWindow)) {
		return
	}
	if _, ok := st.roots[id]; !ok {
		st.roots[id] = newRoot(created)
	}
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

// goneRoot says whether Graph refused a read of a root's replies for that root alone (400, 403 or 404): the root is forgotten.
func goneRoot(err error) bool {
	var ge *GraphError
	return errors.As(err, &ge) &&
		(ge.Status == http.StatusNotFound || ge.Status == http.StatusForbidden || ge.Status == http.StatusBadRequest)
}

// readReplies reads, newest root first, one page each, the replies of the newest rootCap roots that ch keeps within the window and of
// those it kept before the poll's delta read (before), and hands on those that no read of the root gave and that were made at most skew
// before the newest that one did. A root that Graph refuses to read (goneRoot) is forgotten. It prunes the roots after.
func (p *poller) readReplies(ctx context.Context, s *serving, group, ch string, st *polled, before []string) error {
	now := p.now()
	defer p.prune(st, now)
	want := map[string]bool{}
	for _, id := range before {
		want[id] = true
	}
	var ids []string
	for _, id := range newestFirst(st.roots) {
		if st.roots[id].created.Before(now.Add(-p.rootWindow)) {
			continue
		}
		if len(ids) < p.rootCap || want[id] {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		rt := st.roots[id]
		var page graphPage
		link := p.c.graph.resource("teams", group, "channels", ch, "messages", id, "replies") + fmt.Sprintf("?$top=%d", graphPageTop)
		err := p.call(ctx, "PollReplies", link, &page)
		if goneRoot(err) {
			p.log.DebugContext(ctx, "teams: Microsoft Graph refused a read of a thread's replies; the thread's replies are not read again",
				"channel", ch, "thread", id, "error", err.Error())
			delete(st.roots, id)
			continue
		}
		if err != nil {
			return err
		}
		floor := rt.newest.Add(-p.skew)
		var fresh []found
		for _, m := range page.Value {
			if !p.c.readable(ctx, m) {
				continue
			}
			if _, ok := rt.read[m.ID]; ok {
				continue
			}
			if !rt.newest.IsZero() && p.c.madeBefore(ctx, ch, m, floor) {
				continue
			}
			fresh = append(fresh, found{root: id, m: m})
		}
		if err := p.c.handOnAll(ctx, s, ch, fresh); err != nil {
			return err
		}
		for _, f := range fresh {
			rt.saw(f.m.ID, f.m.CreatedDateTime.Time, p.skew)
		}
	}
	return nil
}

// catchUpDue makes the catch-up read that ch is due, from its start, or, with none, from the last catch-up read that succeeded less the
// skew (a channel with no deltaLink), and never from before the look-back: a read that failed and is made again later hands on nothing
// that the dedup has forgotten. It is counted for its trigger.
func (p *poller) catchUpDue(ctx context.Context, s *serving, group, ch string, st *polled, start time.Time) error {
	since, why := st.catchUp, st.catchUpWhy
	if since.IsZero() {
		since, why = st.covered.Add(-p.skew), catchUpSyncing
	}
	since = later(since, start.Add(-p.lookback))
	err := p.catchUp(ctx, s, group, ch, st, since)
	outcome := catchUpRead
	if err != nil {
		outcome = catchUpFailed
	}
	p.c.countCatchUp(ctx, why, outcome)
	if err == nil {
		st.catchUp, st.catchUpWhy, st.covered = time.Time{}, "", start
	}
	return err
}

// catchUp makes a catch-up read of ch, hands on what it read that was made since since, and keeps the roots it read that are within the
// window for their replies, each with the replies that the read gave as read.
func (p *poller) catchUp(ctx context.Context, s *serving, group, ch string, st *polled, since time.Time) error {
	roots, err := p.c.readRecent(ctx, p.call, group, ch)
	if err != nil {
		return err
	}
	if err := p.c.handOnAll(ctx, s, ch, p.c.recentSince(ctx, ch, roots, since)); err != nil {
		return err
	}
	now := p.now()
	for _, r := range roots {
		if !threadable(r) || r.CreatedDateTime.Before(now.Add(-p.rootWindow)) {
			continue
		}
		rt, ok := st.roots[r.ID]
		if !ok {
			rt = newRoot(r.CreatedDateTime.Time)
			st.roots[r.ID] = rt
		}
		for _, m := range r.Replies {
			if isMessageID(m.ID) {
				rt.saw(m.ID, m.CreatedDateTime.Time, p.skew)
			}
		}
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

// recentSince is what a catch-up read of ch gave, each root and each of its replies with its root, marked old when it was made before
// since; a message that does not name itself or its root by message IDs (readable) is dropped.
func (c *Client) recentSince(ctx context.Context, ch string, roots []graphMessage, since time.Time) []found {
	var out []found
	for _, r := range roots {
		if !c.readable(ctx, r) || r.ReplyToID != "" {
			continue
		}
		out = append(out, found{root: r.ID, m: r, old: c.madeBefore(ctx, ch, r, since)})
		for _, m := range r.Replies {
			if c.readable(ctx, m) {
				out = append(out, found{root: r.ID, m: m, old: c.madeBefore(ctx, ch, m, since)})
			}
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
	items := c.recentSince(s.ctx, sub.channel, roots, since)
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

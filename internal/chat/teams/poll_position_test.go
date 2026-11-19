package teams

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/lanes"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// memPositions is a PollPositions in memory: what it keeps, the writes it took, and the errors a test makes it give.
type memPositions struct {
	mu                          sync.Mutex
	kept                        map[[2]string]PollPosition
	saves                       int
	forgot                      []string
	loadErr, saveErr, forgetErr error
}

func (m *memPositions) Load(_ context.Context, conn, channel string) (PollPosition, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return PollPosition{}, false, m.loadErr
	}
	p, ok := m.kept[[2]string{conn, channel}]
	return p, ok, nil
}

func (m *memPositions) Save(_ context.Context, conn string, p PollPosition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if m.saveErr != nil {
		return m.saveErr
	}
	if m.kept == nil {
		m.kept = map[[2]string]PollPosition{}
	}
	m.kept[[2]string{conn, p.Channel}] = p
	return nil
}

func (m *memPositions) Forget(_ context.Context, conn, channel string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.forgetErr != nil {
		return m.forgetErr
	}
	m.forgot = append(m.forgot, channel)
	delete(m.kept, [2]string{conn, channel})
	return nil
}

// get is what m keeps for the test channel.
func (m *memPositions) get() (PollPosition, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.kept[[2]string{testConn, testChannel}]
	return p, ok
}

func (m *memPositions) put(p PollPosition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kept[[2]string{testConn, p.Channel}] = p
}

func (m *memPositions) saved() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saves
}

func (m *memPositions) set(edit func(m *memPositions)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	edit(m)
}

// errStoreDown is a store that does not answer.
var errStoreDown = errors.New("the store is down")

// errDeposed is the store's refusal of a write of a broker that no longer leads, as the broker gives it.
var errDeposed = fmt.Errorf("%w: store: deposed", ErrNotLeader)

// newPositionTest is a pollTest whose Client keeps its positions in a memPositions, with the test channel bound, read to its deltaLink,
// and that position stored by the poll after.
func newPositionTest(t *testing.T) (*pollTest, *memPositions) {
	t.Helper()
	pos := &memPositions{}
	pt := newPollTestWith(t, func(c *Config) { c.Positions = pos })
	pt.bound(t, testChannel)
	pt.round(t)
	pt.next(t)
	if _, ok := pos.get(); !ok {
		t.Fatal("the channel's position was not stored by the poll after its deltaLink")
	}
	return pt, pos
}

// restart ends the poller's run and starts it anew, as a restart of the broker or a change of leader does: what the run held in memory
// goes, and the store stays.
func (pt *pollTest) restart(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	pt.p.run(ctx, pt.sink)
}

// freshSyncs is how many of calls begin a sync of the test channel's delta: a read of …/messages/delta with no query.
func freshSyncs(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.HasSuffix(c, "/channels/"+testChannel+"/messages/delta?") {
			n++
		}
	}
	return n
}

// A run resumes a channel's delta from its stored position: it reads the stored deltaLink, not the channel anew, and hands on a post
// made while no run polled, older than the catch-up read's look-back; a change of a post made before the stored poll is not handed on.
func TestPollResumesTheDeltaFromTheStoredPosition(t *testing.T) {
	pt, pos := newPositionTest(t)
	stored, _ := pos.get()
	if stored.DeltaLink != pt.p.chans[testChannel].link || stored.Channel != testChannel {
		t.Fatalf("stored %+v; want the channel's deltaLink", stored)
	}
	pt.restart(t)
	pt.clk.advance(30 * time.Minute)
	const during, older = "1794971800200", "1794971800201"
	pt.f.putMessage(testChannel, made(person(during, "", testUser, "<p>posted while no broker polled</p>"), pt.ago(20*time.Minute)))
	pt.f.putMessage(testChannel, made(person(older, "", testUser, "<p>edited, made before the stored poll</p>"), pt.ago(50*time.Minute)))
	pt.calls()
	pt.round(t)
	calls := pt.calls()
	_, query, _ := strings.Cut(stored.DeltaLink, "?")
	if freshSyncs(calls) != 0 || !slices.ContainsFunc(calls, func(c string) bool { return strings.HasSuffix(c, "/messages/delta?"+query) }) {
		t.Errorf("the resumed run read %v; want the stored deltaLink, and no sync anew", calls)
	}
	if got := pt.sink.callsOf(during); got != 1 {
		t.Errorf("the post made while no broker polled was handed on %d times; want once, by the resumed delta", got)
	}
	if got := pt.sink.callsOf(older); got != 0 {
		t.Errorf("a change of a post made before the stored poll was handed on %d times; want never", got)
	}
	if got := pt.polls(t, polledStarted); got != 1 {
		t.Errorf("polls{started} = %v; want 1: the second run did not sync", got)
	}
}

// The position of a delta read is stored only once the lane has handed on what the read gave: while the lane holds it the store keeps the
// position before, so a run that starts then reads it again rather than losing it; once it is handed on, the next due write stores it.
func TestPollStoresAPositionOnlyOnceItsMessagesAreHandedOn(t *testing.T) {
	pt, pos := newPositionTest(t)
	before, _ := pos.get()
	saves := pos.saved()
	pt.sink.block, pt.sink.blockChannel = make(chan struct{}), testChannel
	const root = "1794971800210"
	pt.f.putMessage(testChannel, made(person(root, "", testUser, "<p>new root</p>"), pt.clk.now().Add(time.Second)))
	pt.clk.advance(pt.p.saveEvery)
	pt.p.round(pt.obs.ctx, pt.sink) // the read gives the root, which the lane holds
	read := pt.p.chans[testChannel].link
	if read == before.DeltaLink {
		t.Fatal("the read of the new root gave no new deltaLink")
	}
	for range 2 { // the second's write would store what a read with nothing new recorded meanwhile
		pt.clk.advance(pt.p.interval)
		pt.p.round(pt.obs.ctx, pt.sink)
	}
	if got, _ := pos.get(); got != before || pos.saved() != saves {
		t.Errorf("stored %+v after %d writes while the lane held the root; want %+v, unwritten", got, pos.saved()-saves, before)
	}
	close(pt.sink.block)
	s := pt.c.recv.live.Load()
	testutil.Eventually(t, "the lanes idle", func() bool { return s.lanes.Held() == 0 })
	pt.next(t)
	if got, _ := pos.get(); got.DeltaLink != read {
		t.Errorf("stored %q once the root was handed on; want the read's deltaLink", got.DeltaLink)
	}
}

// A position is written at most once each saveEvery while its deltaLink changes, and, while the deltaLink stays the same, only once the
// stored time is saveIdle old: a poll that changes nothing writes nothing.
func TestPollStoresThePositionAtABoundedRate(t *testing.T) {
	pt, pos := newPositionTest(t)
	first := pos.saved()
	for i := range 5 {
		pt.f.putMessage(testChannel, made(person(fmt.Sprintf("17949718002%02d", i+20), "", testUser, "<p>root</p>"), pt.clk.now().Add(time.Second)))
		pt.next(t)
	}
	if got := pos.saved(); got != first {
		t.Errorf("writes within %v of the last = %d; want none", pt.p.saveEvery, got-first)
	}
	pt.next(t)
	if got := pos.saved(); got != first+1 {
		t.Fatalf("writes once %v had passed = %d; want 1", pt.p.saveEvery, got-first)
	}
	stored, _ := pos.get()
	if stored.DeltaLink != pt.p.chans[testChannel].link {
		t.Errorf("stored %q; want the newest deltaLink handed on, %q", stored.DeltaLink, pt.p.chans[testChannel].link)
	}
	for pt.clk.now().Add(pt.p.interval).Before(stored.PolledAt.Add(pt.p.saveIdle)) {
		pt.next(t)
	}
	if got := pos.saved(); got != first+1 {
		t.Errorf("writes of an unchanged deltaLink within %v = %d; want none", pt.p.saveIdle, got-first-1)
	}
	pt.next(t)
	again, _ := pos.get()
	if got := pos.saved(); got != first+2 || again.DeltaLink != stored.DeltaLink || !again.PolledAt.After(stored.PolledAt) {
		t.Errorf("after %v: %d writes, stored %+v; want one write of the same deltaLink at a later time than %v", pt.p.saveIdle,
			got-first-1, again, stored.PolledAt)
	}
}

// A stored position more than resumeMax old is not resumed, told at WARN: the channel is read anew, a post made while no broker polled
// and before the look-back is not handed on, one within the look-back is, and the new position replaces the stored one.
func TestPollReadsAnewFromAStoredPositionPastTheResumeBound(t *testing.T) {
	pt, pos := newPositionTest(t)
	stale, _ := pos.get()
	pt.restart(t)
	pt.clk.advance(pt.p.resumeMax + time.Minute)
	const old, recent = "1794971800230", "1794971800231"
	pt.f.putMessage(testChannel, made(person(old, "", testUser, "<p>while away, before the look-back</p>"), pt.ago(30*time.Minute)))
	pt.f.putMessage(testChannel, made(person(recent, "", testUser, "<p>within the look-back</p>"), pt.ago(2*time.Minute)))
	pt.calls()
	pt.round(t)
	if got := freshSyncs(pt.calls()); got != 1 {
		t.Errorf("syncs anew of the channel = %d; want 1", got)
	}
	for id, want := range map[string]int{old: 0, recent: 1} {
		if got := pt.sink.callsOf(id); got != want {
			t.Errorf("the sink was given %s %d times; want %d", id, got, want)
		}
	}
	if n := strings.Count(pt.logs.String(), "stored read position is not resumed"); n != 1 {
		t.Errorf("WARN lines of the stale position = %d; want 1", n)
	}
	pt.next(t)
	if got, _ := pos.get(); got.DeltaLink == stale.DeltaLink || !got.PolledAt.After(stale.PolledAt) {
		t.Errorf("stored %+v; want the new deltaLink in place of %+v", got, stale)
	}
}

// A stored deltaLink that Graph no longer takes is a reset, told at WARN as the stored one: the channel is read anew from now, the post
// made since the stored poll is handed on by the catch-up read, and the new deltaLink is stored in its place.
func TestPollReadsAnewWhenGraphRefusesTheStoredDeltaLink(t *testing.T) {
	pt, pos := newPositionTest(t)
	refused, _ := pos.get()
	pt.restart(t)
	pt.clk.advance(time.Minute)
	const since = "1794971800240"
	pt.f.putMessage(testChannel, made(person(since, "", testUser, "<p>since the stored poll</p>"), pt.ago(30*time.Second)))
	pt.f.setGraph(func(g *fakeGraph) { g.deltaGone = true })
	pt.round(t)
	pt.f.setGraph(func(g *fakeGraph) { g.deltaGone = false })
	if got := pt.polls(t, polledReset); got != 1 {
		t.Errorf("polls{reset} = %v; want 1", got)
	}
	if n := strings.Count(pt.logs.String(), "no longer takes the channel's delta link that the store kept"); n != 1 {
		t.Errorf("WARN lines of the refused stored deltaLink = %d; want 1", n)
	}
	if got := pt.sink.callsOf(since); got != 1 {
		t.Errorf("the post made since the stored poll was handed on %d times; want once", got)
	}
	pt.next(t)
	if got, _ := pos.get(); got.DeltaLink == refused.DeltaLink || got.DeltaLink != pt.p.chans[testChannel].link {
		t.Errorf("stored %q; want the new deltaLink %q in place of the refused one", got.DeltaLink, pt.p.chans[testChannel].link)
	}
}

// A stored time ahead of the poller's clock by more than the skew (another replica's clock ran ahead) is not resumed, told at WARN: the
// posts made since that replica's last poll would be taken as old. The channel is read anew, with the look-back. A time ahead by less is
// resumed, as the skew covers it.
func TestPollReadsAnewFromAStoredTimeAheadOfTheClock(t *testing.T) {
	for name, tc := range map[string]struct {
		ahead   time.Duration
		resumed bool
	}{
		"past the skew":   {ahead: 5 * time.Minute},
		"within the skew": {ahead: 10 * time.Second, resumed: true},
	} {
		t.Run(name, func(t *testing.T) {
			pt, pos := newPositionTest(t)
			ahead, _ := pos.get()
			ahead.PolledAt = pt.clk.now().Add(tc.ahead)
			pos.put(ahead)
			pt.restart(t)
			pt.calls()
			pt.round(t)
			if syncs := freshSyncs(pt.calls()); (syncs == 0) != tc.resumed {
				t.Errorf("syncs anew = %d; want the position resumed %v", syncs, tc.resumed)
			}
			if n := strings.Count(pt.logs.String(), "stored read position is not resumed"); (n == 0) != tc.resumed {
				t.Errorf("WARN lines of the position not resumed = %d; want it resumed %v", n, tc.resumed)
			}
		})
	}
}

// A channel that no Binding names any longer has its stored position deleted: bound again, it is read anew, not resumed.
func TestPollForgetsTheStoredPositionOfAnUnboundChannel(t *testing.T) {
	pt, pos := newPositionTest(t)
	pt.sink.bind()
	pt.next(t)
	if p, ok := pos.get(); ok {
		t.Errorf("stored %+v after the unbind; want none", p)
	}
	pos.mu.Lock()
	forgot := slices.Clone(pos.forgot)
	pos.mu.Unlock()
	if !slices.Equal(forgot, []string{testChannel}) {
		t.Errorf("forgot %v; want the unbound channel", forgot)
	}
	pt.sink.bind(testChannel)
	pt.calls()
	pt.next(t)
	if got := freshSyncs(pt.calls()); got != 1 {
		t.Errorf("syncs anew of the channel bound again = %d; want 1", got)
	}
}

// A stored position that cannot be read fails the channel's poll, which reads nothing of Graph's delta and is made again after the
// backoff: the run then resumes from the position, and does not read the channel anew.
func TestPollRetriesAStoredPositionThatCannotBeRead(t *testing.T) {
	pt, pos := newPositionTest(t)
	pt.restart(t)
	pos.set(func(m *memPositions) { m.loadErr = errStoreDown })
	pt.calls()
	pt.round(t)
	if got := deltaReads(pt.calls(), testChannel); got != 0 {
		t.Errorf("delta reads while the position cannot be read = %d; want none", got)
	}
	if got := pt.polls(t, polledFailed); got != 1 {
		t.Errorf("polls{failed} = %v; want 1", got)
	}
	pos.set(func(m *memPositions) { m.loadErr = nil })
	pt.clk.advance(time.Minute)
	pt.round(t)
	calls := pt.calls()
	if freshSyncs(calls) != 0 || deltaReads(calls, testChannel) != 1 {
		t.Errorf("the poll after the backoff read %v; want the stored deltaLink, and no sync anew", calls)
	}
}

// A write of the position that fails is told once at WARN and tried again saveEvery later, while the channel is polled on; one that
// succeeds after is told at INFO.
func TestPollWarnsOnceWhenAPositionCannotBeStored(t *testing.T) {
	pt, pos := newPositionTest(t)
	pos.set(func(m *memPositions) { m.saveErr = errStoreDown })
	tried := pos.saved()
	const root = "1794971800260"
	pt.f.putMessage(testChannel, made(person(root, "", testUser, "<p>root</p>"), pt.clk.now().Add(time.Second)))
	pt.clk.advance(pt.p.saveEvery)
	pt.round(t)
	pt.next(t) // the write of the root's read fails
	pt.next(t) // not tried again yet
	pt.clk.advance(pt.p.saveEvery)
	pt.round(t) // tried again, and fails again
	if got := pos.saved() - tried; got != 2 {
		t.Errorf("writes tried = %d; want 2, saveEvery apart", got)
	}
	if n := strings.Count(pt.logs.String(), "read position could not be stored;"); n != 1 {
		t.Errorf("WARN lines of the failing write = %d; want 1", n)
	}
	if got := pt.sink.callsOf(root); got != 1 {
		t.Errorf("the root was handed on %d times while the store failed; want once", got)
	}
	pos.set(func(m *memPositions) { m.saveErr = nil })
	pt.clk.advance(pt.p.saveEvery)
	pt.round(t)
	if got, _ := pos.get(); got.DeltaLink != pt.p.chans[testChannel].link {
		t.Errorf("stored %q once the store answered; want %q", got.DeltaLink, pt.p.chans[testChannel].link)
	}
	if n := strings.Count(pt.logs.String(), "read position is stored again"); n != 1 {
		t.Errorf("INFO lines of the write that succeeded = %d; want 1", n)
	}
}

// A read whose lane had no room leaves the positions recorded after it to the reads that follow: once its post is handed on by the read
// made again, the positions go on being stored, an unchanged deltaLink's at saveIdle included.
func TestPollStoresPositionsAfterAReadWhoseLaneHadNoRoom(t *testing.T) {
	pt, pos := newPositionTest(t)
	pt.sink.block, pt.sink.blockChannel = make(chan struct{}), testChannel
	for i := range lanes.PerChannel {
		id := fmt.Sprintf("%s%06d", flushPrefix, i+1)
		if ans := pt.c.recv.takePayload(t.Context(), inChannel(t, "message_plain", testChannel, id)); ans.status != http.StatusOK {
			t.Fatalf("filling the lane: %d", ans.status)
		}
	}
	const root = "1794971800270"
	pt.f.putMessage(testChannel, made(person(root, "", testUser, "<p>while the lane is full</p>"), pt.clk.now().Add(time.Second)))
	pt.clk.advance(pt.p.interval)
	pt.p.round(pt.obs.ctx, pt.sink)
	if got := pt.read(t, readShed); got != 1 {
		t.Fatalf("read_messages{shed} = %v; want 1", got)
	}
	close(pt.sink.block)
	s := pt.c.recv.live.Load()
	testutil.Eventually(t, "the lanes idle", func() bool { return s.lanes.Held() == 0 })
	pt.clk.advance(time.Hour) // past the backoff
	pt.round(t)
	pt.clk.advance(pt.p.saveEvery)
	pt.round(t)
	stored, _ := pos.get()
	if got := pt.sink.callsOf(root); got != 1 || stored.DeltaLink != pt.p.chans[testChannel].link {
		t.Fatalf("the root handed on %d times, stored %q; want once, and the read made again stored", got, stored.DeltaLink)
	}
	pt.clk.advance(pt.p.saveIdle)
	pt.round(t)
	if got, _ := pos.get(); !got.PolledAt.After(stored.PolledAt) {
		t.Errorf("stored at %v after %v of an unchanged deltaLink; want a later time than %v", got.PolledAt, pt.p.saveIdle, stored.PolledAt)
	}
}

// A write of the position that the store refuses because this broker no longer leads, a save or the delete of an unbound channel's, stops
// the poller's run, told at WARN, and it reads nothing more.
func TestPollStopsWhenTheStoreSaysThisBrokerNoLongerLeads(t *testing.T) {
	for name, refuse := range map[string]func(pt *pollTest, pos *memPositions){
		"a save": func(pt *pollTest, pos *memPositions) {
			pos.set(func(m *memPositions) { m.saveErr = errDeposed })
			pt.clk.advance(pt.p.interval) // the next poll writes the run's deltaLink
		},
		"a delete": func(pt *pollTest, pos *memPositions) {
			pos.set(func(m *memPositions) { m.forgetErr = errDeposed })
			pt.sink.bind()
		},
	} {
		t.Run(name, func(t *testing.T) {
			pos := &memPositions{}
			pt := newPollTestWith(t, func(c *Config) { c.Positions = pos })
			pt.bound(t, testChannel)
			pt.p.tick = time.Millisecond
			done := make(chan struct{})
			go func() { defer close(done); pt.p.run(t.Context(), pt.sink) }()
			testutil.Eventually(t, "the run's first round", func() bool { return pt.p.rounds.Load() > 0 })
			refuse(pt, pos)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the poller ran on after the store refused its write")
			}
			if n := strings.Count(pt.logs.String(), "the polls of the connection's channels stop"); n != 1 {
				t.Errorf("WARN lines of the stop = %d; want 1", n)
			}
		})
	}
}

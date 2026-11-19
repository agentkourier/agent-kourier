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
	unbound                     [][]string // the bound lists of each ForgetUnbound that the store took
	unboundCalls                int        // the ForgetUnbound calls, those that failed included
	loadErr, saveErr, forgetErr error
	unboundErr                  error
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

func (m *memPositions) ForgetUnbound(_ context.Context, conn string, bound []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unboundCalls++
	if m.unboundErr != nil {
		return m.unboundErr
	}
	m.unbound = append(m.unbound, slices.Clone(bound))
	for k := range m.kept {
		if k[0] == conn && !slices.Contains(bound, k[1]) {
			delete(m.kept, k)
		}
	}
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

// unboundOf is what ForgetUnbound of pos was given, call by call, and how many calls it took, failed ones included.
func unboundOf(pos *memPositions) ([][]string, int) {
	pos.mu.Lock()
	defer pos.mu.Unlock()
	return slices.Clone(pos.unbound), pos.unboundCalls
}

// A run's first round deletes the stored positions of the connection's channels that no Binding names, once: a Binding that went while
// no run saw it go leaves no position that a Binding naming the channel again would resume. A bound channel keeps its position, and
// another connection's are not touched.
func TestPollDeletesAtARunsStartThePositionsOfChannelsNoBindingNames(t *testing.T) {
	pt, pos := newPositionTest(t)
	stored, _ := pos.get()
	const gone = "19:gone@thread.tacv2"
	pos.set(func(m *memPositions) {
		m.kept[[2]string{testConn, gone}] = PollPosition{Channel: gone, DeltaLink: stored.DeltaLink + "-gone", PolledAt: stored.PolledAt}
		m.kept[[2]string{"ns/other", gone}] = PollPosition{Channel: gone, DeltaLink: stored.DeltaLink + "-other", PolledAt: stored.PolledAt}
	})
	_, before := unboundOf(pos)
	pt.restart(t)
	pt.clk.advance(time.Minute)
	pt.round(t)
	pt.next(t)
	if got, calls := unboundOf(pos); calls != before+1 || !slices.Equal(got[len(got)-1], []string{testChannel}) {
		t.Errorf("ForgetUnbound took %v in %d calls; want the bound channel, once in the run", got, calls)
	}
	pos.mu.Lock()
	_, goneKept := pos.kept[[2]string{testConn, gone}]
	_, otherKept := pos.kept[[2]string{"ns/other", gone}]
	pos.mu.Unlock()
	if goneKept || !otherKept {
		t.Errorf("after the run's start: the unbound channel's position kept = %v, another connection's = %v; want false, true", goneKept,
			otherKept)
	}
	if got, ok := pos.get(); !ok || got.DeltaLink != stored.DeltaLink {
		t.Errorf("the bound channel's position = %+v, %v; want %q kept", got, ok, stored.DeltaLink)
	}
}

// A channel whose Binding was removed while no run polled, and that a Binding names again after the next run started, is read anew: the
// run's start deleted its position, so what was posted while it was not bound is not handed on.
func TestPollReadsAnewAChannelUnboundWhileNoRunPolled(t *testing.T) {
	pt, pos := newPositionTest(t)
	pt.sink.bind()
	pt.restart(t)
	pt.clk.advance(30 * time.Minute)
	const during = "1794971800300"
	pt.f.putMessage(testChannel, made(person(during, "", testUser, "<p>posted while the channel was not bound</p>"), pt.ago(20*time.Minute)))
	pt.round(t)
	if p, ok := pos.get(); ok {
		t.Fatalf("stored %+v after the start of a run that the channel is not bound in; want none", p)
	}
	pt.sink.bind(testChannel)
	pt.calls()
	pt.next(t)
	if got := freshSyncs(pt.calls()); got != 1 {
		t.Errorf("syncs anew of the channel bound again = %d; want 1", got)
	}
	if got := pt.sink.callsOf(during); got != 0 {
		t.Errorf("the post made while the channel was not bound was handed on %d times; want never", got)
	}
}

// A delete of an unbound channel's position that fails leaves the channel stale: a Binding that names it again in the run reads it anew,
// not from the position the store still keeps, and the polls go on while the delete fails. The delete of the unbound channels' positions
// is made again saveEvery later, and each saveEvery while it fails, told once at WARN until it succeeds (told at INFO).
func TestPollReadsAnewAChannelWhosePositionCouldNotBeDeleted(t *testing.T) {
	pt, pos := newPositionTest(t)
	pos.set(func(m *memPositions) { m.forgetErr, m.unboundErr = errStoreDown, errStoreDown })
	_, before := unboundOf(pos)
	pt.sink.bind()
	pt.next(t)
	if _, ok := pos.get(); !ok {
		t.Fatal("the failing store deleted the position")
	}
	if _, calls := unboundOf(pos); calls != before {
		t.Errorf("ForgetUnbound calls in the round of the failed delete = %d; want %d: it is made again saveEvery later", calls, before)
	}
	const during = "1794971800310"
	pt.f.putMessage(testChannel, made(person(during, "", testUser, "<p>posted while the channel was not bound</p>"), pt.ago(time.Second)))
	pt.sink.bind(testChannel)
	pt.calls()
	pt.next(t)
	if got := freshSyncs(pt.calls()); got != 1 {
		t.Errorf("syncs anew of the channel bound again = %d; want 1, not a resume from the position left in the store", got)
	}
	pt.next(t)
	if _, calls := unboundOf(pos); calls != before {
		t.Errorf("ForgetUnbound calls within saveEvery of the failed delete = %d; want %d", calls, before)
	}
	pt.clk.advance(pt.p.saveEvery)
	pt.round(t)
	if _, calls := unboundOf(pos); calls != before+1 {
		t.Errorf("ForgetUnbound calls saveEvery after the failed delete = %d; want %d", calls, before+1)
	}
	pt.next(t)
	if _, calls := unboundOf(pos); calls != before+1 {
		t.Errorf("ForgetUnbound calls within saveEvery of its failure = %d; want %d", calls, before+1)
	}
	pos.set(func(m *memPositions) { m.forgetErr, m.unboundErr = nil, nil })
	pt.clk.advance(pt.p.saveEvery)
	pt.round(t)
	if got, calls := unboundOf(pos); calls != before+2 || !slices.Equal(got[len(got)-1], []string{testChannel}) {
		t.Errorf("ForgetUnbound took %v, %d calls; want a second try, with the bound channel", got, calls)
	}
	pt.next(t)
	if got, _ := pos.get(); got.DeltaLink != pt.p.chans[testChannel].link {
		t.Errorf("stored %q; want the deltaLink of the channel read anew", got.DeltaLink)
	}
	logs := pt.logs.String()
	for line, want := range map[string]int{
		"the stored read position of a channel that no Binding names any longer could not be deleted": 1,
		"the channels that no Binding names could not be deleted;":                                    1,
		"the channels that no Binding names are deleted":                                              1,
	} {
		if n := strings.Count(logs, line); n != want {
			t.Errorf("lines %q = %d; want %d", line, n, want)
		}
	}
}

// A run whose delete of the unbound channels' positions fails polls its channels all the same, the stored position resumed.
func TestPollPollsOnWhenThePositionsOfUnboundChannelsCannotBeDeleted(t *testing.T) {
	pt, pos := newPositionTest(t)
	pos.set(func(m *memPositions) { m.unboundErr = errStoreDown })
	pt.restart(t)
	pt.clk.advance(time.Minute)
	pt.calls()
	pt.round(t)
	if got := freshSyncs(pt.calls()); got != 0 || pt.polls(t, polledRead) < 1 {
		t.Errorf("syncs anew = %d, polls{polled} = %v; want 0 and the stored position read", got, pt.polls(t, polledRead))
	}
}

// A read put on the lane before Graph refused the channel's deltaLink is not recorded once the lane hands it on: its link may be the
// refused one. The lane's count of pending reads comes back to nothing, so a read after the re-sync that gives nothing is recorded, and
// stored.
func TestPollDoesNotStoreAReadPutOnTheLaneBeforeGraphRefusedItsLink(t *testing.T) {
	pt, pos := newPositionTest(t)
	before, _ := pos.get()
	pt.sink.block, pt.sink.blockChannel = make(chan struct{}), testChannel
	pt.f.putMessage(testChannel, made(person("1794971800320", "", testUser, "<p>new root</p>"), pt.clk.now().Add(time.Second)))
	pt.clk.advance(pt.p.saveEvery)
	pt.p.round(pt.obs.ctx, pt.sink) // the read gives the root, which the lane holds
	refused := pt.p.chans[testChannel].link
	// A post before the re-sync moves the fake's delta on, so that the re-sync's deltaLink is not the refused one's text.
	pt.f.putMessage(testChannel, made(person("1794971800321", "", testUser, "<p>before the re-sync</p>"), pt.clk.now().Add(time.Second)))
	pt.f.setGraph(func(g *fakeGraph) { g.deltaGone = true })
	pt.clk.advance(pt.p.interval)
	pt.p.round(pt.obs.ctx, pt.sink) // Graph refuses the read's deltaLink: the channel syncs anew while the root is on the lane
	pt.f.setGraph(func(g *fakeGraph) { g.deltaGone = false })
	if got := pt.polls(t, polledReset); got != 1 {
		t.Fatalf("polls{reset} = %v; want 1", got)
	}
	close(pt.sink.block)
	s := pt.c.recv.live.Load()
	testutil.Eventually(t, "the lanes idle", func() bool { return s.lanes.Held() == 0 })
	pt.clk.advance(pt.p.saveEvery)
	pt.round(t) // a write is due at the start of the poll, and nothing was recorded since the reset; then a read that gives nothing
	if got, _ := pos.get(); got != before {
		t.Errorf("stored %+v once the lane handed on the read made before the reset; want %+v kept", got, before)
	}
	want := pt.p.chans[testChannel].link
	if want == refused {
		t.Fatal("the re-sync gave the refused deltaLink's text")
	}
	pt.clk.advance(pt.p.saveEvery)
	pt.round(t)
	if got, _ := pos.get(); got.DeltaLink != want {
		t.Errorf("stored %q after the re-sync; want the read's %q, recorded once nothing was pending", got.DeltaLink, want)
	}
}

// A link recorded as handed on before Graph refused it is not written while the channel syncs anew over several polls.
func TestPollDoesNotStoreARefusedLinkWhileTheChannelSyncsAnew(t *testing.T) {
	pt, pos := newPositionTest(t)
	before, _ := pos.get()
	pt.f.putMessage(testChannel, made(person("1794971800330", "", testUser, "<p>new root</p>"), pt.clk.now().Add(time.Second)))
	pt.next(t) // the read is handed on and recorded, and not yet due to be written
	if pt.p.chans[testChannel].link == before.DeltaLink {
		t.Fatal("the read of the new root gave no new deltaLink")
	}
	if got, _ := pos.get(); got != before {
		t.Fatalf("stored %+v within saveEvery of the last write; want %+v", got, before)
	}
	pt.f.setGraph(func(g *fakeGraph) { g.deltaGone, g.deltaPages[testChannel] = true, pt.p.syncPages })
	pt.next(t) // Graph refuses it: the re-sync reads its first pages
	pt.f.setGraph(func(g *fakeGraph) { g.deltaGone = false })
	if pt.p.chans[testChannel].link != "" {
		t.Fatal("the re-sync reached its deltaLink in one poll; want it to take two")
	}
	pt.clk.advance(pt.p.saveEvery)
	pt.round(t) // a write is due at the start of the poll
	if got, _ := pos.get(); got != before {
		t.Errorf("stored %+v while the channel synced anew; want %+v kept", got, before)
	}
}

// A read whose items the stop of Serve cut is not recorded as handed on, nor a read after it in the run: what it gave was not all handed
// on.
func TestPollDoesNotRecordAReadThatTheStopOfServeCut(t *testing.T) {
	pt, pos := newPositionTest(t)
	before, _ := pos.get()
	st := pt.p.chans[testChannel]
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s := &serving{ctx: ctx, sink: pt.sink, lanes: lanes.New()}
	const id = "1794971800340"
	cut := position{link: before.DeltaLink + "-cut", polled: pt.clk.now()}
	if err := pt.p.handOnDelta(pt.obs.ctx, s, testChannel, st, []found{{root: id, m: graphMessage{ID: id}}}, cut); err != nil {
		t.Fatal(err)
	}
	s.lanes.Wait()
	pt.p.handedOn(st, position{link: before.DeltaLink + "-later", polled: pt.clk.now()}) // a later read that gave nothing
	const later = "1794971800341"                                                        // and one that gave a message, handed on by another Serve
	live := pt.c.recv.live.Load()
	if err := pt.p.handOnDelta(pt.obs.ctx, live, testChannel, st, []found{{root: later, m: graphMessage{ID: later}}},
		position{link: before.DeltaLink + "-after", polled: pt.clk.now()}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the lanes idle", func() bool { return live.lanes.Held() == 0 })
	pt.clk.advance(pt.p.saveEvery)
	if !pt.p.save(pt.obs.ctx, testChannel, st, pt.clk.now()) {
		t.Fatal("save stopped the poller")
	}
	if got, _ := pos.get(); got != before {
		t.Errorf("stored %+v after a read that the stop of Serve cut; want %+v kept", got, before)
	}
	if got := pt.sink.callsOf(id); got != 0 {
		t.Errorf("the cut read's message was handed on %d times; want never", got)
	}
}

// A run whose first round's delete of the unbound channels' positions the store refuses because this broker no longer leads stops.
func TestPollStopsWhenTheStoreRefusesTheDeleteOfTheUnboundPositions(t *testing.T) {
	pos := &memPositions{unboundErr: errDeposed}
	pt := newPollTestWith(t, func(c *Config) { c.Positions = pos })
	pt.bound(t, testChannel)
	pt.p.tick = time.Millisecond
	done := make(chan struct{})
	go func() { defer close(done); pt.p.run(t.Context(), pt.sink) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the poller ran on after the store refused its delete")
	}
	if n := strings.Count(pt.logs.String(), "the polls of the connection's channels stop"); n != 1 {
		t.Errorf("WARN lines of the stop = %d; want 1", n)
	}
	if got := freshSyncs(pt.calls()); got != 0 {
		t.Errorf("syncs of the stopped run = %d; want none", got)
	}
}

// A channel that a Binding names again while the run's delete of the unbound channels' positions waits to be made again is read anew:
// its stored position may be one from before it was unbound, which that delete was to remove. A channel bound at the run's first round
// resumes as ever.
func TestPollReadsAnewAChannelBoundWhileTheDeleteOfUnboundPositionsWaits(t *testing.T) {
	pt, pos := newPositionTest(t)
	pos.set(func(m *memPositions) { m.unboundErr = errStoreDown })
	pt.sink.bind() // the Binding goes while no run polls
	pt.restart(t)
	pt.clk.advance(30 * time.Minute)
	const during = "1794971800350"
	pt.f.putMessage(testChannel, made(person(during, "", testUser, "<p>posted while the channel was not bound</p>"), pt.ago(20*time.Minute)))
	pt.round(t) // the run's first round: the delete fails, and the position stays
	if _, ok := pos.get(); !ok {
		t.Fatal("the failing store deleted the position")
	}
	pt.sink.bind(testChannel)
	pt.calls()
	pt.next(t)
	pt.next(t)
	if got := freshSyncs(pt.calls()); got != 1 {
		t.Errorf("syncs anew of the channel bound again = %d; want 1, not a resume from the position the delete was to remove", got)
	}
	if got := pt.sink.callsOf(during); got != 0 {
		t.Errorf("the post made while the channel was not bound was handed on %d times; want never", got)
	}
}

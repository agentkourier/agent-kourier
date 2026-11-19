package teams

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/lanes"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// pollTest is a Client with no notificationURL on the fake, its poller on a fake clock whose waits move the clock and are recorded, and a
// Serve that takes what the poller hands on. A test drives the rounds itself.
type pollTest struct {
	*clientTest
	p      *poller
	sink   *bindingSink
	obs    observed
	logs   *syncBuffer
	clk    *fakeClock
	mu     sync.Mutex
	sleeps []time.Duration
}

func newPollTest(t *testing.T) *pollTest {
	t.Helper()
	f := newFakeConnector(t)
	key := newSigningKey(t, "k", "msteams")
	bf := newFakeBotFramework(t, key)
	convs := NewMemoryConversations()
	pt := &pollTest{sink: &bindingSink{}, obs: newObserved(t), logs: &syncBuffer{}, clk: &fakeClock{t: time.Now().UTC().Truncate(time.Second)}}
	c, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret),
		TokenURL: f.srv.URL + "/token", MetadataURL: bf.metadataURL(), GraphURL: f.graphURL(), HTTPClient: f.srv.Client(),
		Conversations: convs, Logger: slog.New(slog.NewTextHandler(pt.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	if c.poll == nil || c.push != nil {
		t.Fatal("a Client with no notificationURL has no poller")
	}
	c.recv.sinkRetryBase = time.Millisecond
	pt.clientTest, pt.p = &clientTest{c: c, f: f, convs: convs, key: key}, c.poll
	pt.p.now = pt.clk.now
	pt.p.sleep = func(_ context.Context, d time.Duration) bool {
		pt.mu.Lock()
		pt.sleeps = append(pt.sleeps, d)
		pt.mu.Unlock()
		pt.clk.advance(d)
		return true
	}
	ctx, cancel := context.WithCancel(pt.obs.ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = c.recv.serve(ctx, pt.sink, false) }()
	t.Cleanup(func() { cancel(); <-done })
	testutil.Eventually(t, "the Serve", func() bool { return c.recv.live.Load() != nil })
	return pt
}

// round runs one round of the poller and waits until its lanes have handed on what it put on them.
func (pt *pollTest) round(t *testing.T) {
	t.Helper()
	pt.p.round(pt.obs.ctx, pt.sink)
	s := pt.c.recv.live.Load()
	testutil.Eventually(t, "the lanes idle", func() bool { return s.lanes.Held() == 0 })
}

// next moves the clock one interval on, and runs a round.
func (pt *pollTest) next(t *testing.T) {
	t.Helper()
	pt.clk.advance(pt.p.interval)
	pt.round(t)
}

// bound binds and records channels, in the test team.
func (pt *pollTest) bound(t *testing.T, channels ...string) {
	t.Helper()
	for _, ch := range channels {
		pt.record(t, ch, testChannel)
	}
	pt.sink.bind(channels...)
}

// ago is the fake clock's now less d.
func (pt *pollTest) ago(d time.Duration) time.Time { return pt.clk.now().Add(-d) }

func (pt *pollTest) polls(t *testing.T, outcome string) float64 {
	t.Helper()
	return pt.obs.series(t, "agentkourier_teams_graph_polls_total", outcome)
}

func (pt *pollTest) read(t *testing.T, outcome string) float64 {
	t.Helper()
	return pt.obs.series(t, "agentkourier_teams_graph_read_messages_total", outcome)
}

// calls is the paths of the Graph reads taken since the last, with each query.
func (pt *pollTest) calls() []string {
	var out []string
	for _, r := range pt.f.graphTaken() {
		out = append(out, r.path+"?"+r.query)
	}
	return out
}

// The run's first poll of a channel hands on what was posted within the look-back, roots and replies, and nothing older; the delta read
// of the start hands on nothing of its own.
func TestPollStartHandsOnTheLookbackAlone(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.f.putMessage(testChannel, made(person("1794971800001", "", testUser, "<p>an hour ago</p>"), pt.ago(time.Hour)))
	pt.f.putMessage(testChannel, made(person("1794971800002", "1794971800001", testUser, "<p>old reply</p>"), pt.ago(time.Hour)))
	pt.f.putMessage(testChannel, made(person("1794971800003", "1794971800001", testUser, "<p>recent reply</p>"), pt.ago(time.Minute)))
	pt.f.putMessage(testChannel, made(person("1794971800004", "", testUser, "<p>recent root</p>"), pt.ago(2*time.Minute)))
	pt.round(t)
	for id, want := range map[string]bool{"1794971800001": false, "1794971800002": false, "1794971800003": true, "1794971800004": true} {
		if got := pt.sink.callsOf(id); (got == 1) != want || got > 1 {
			t.Errorf("the sink was given %s %d times; want it %v", id, got, want)
		}
	}
	if got := pt.sink.got(); len(got) == 2 && (got[0].At.Thread != "1794971800001" || got[1].At.ID != "1794971800004") {
		t.Errorf("the sink was given %+v; want the reply in its root's thread, then the recent root", got)
	}
	if got := pt.polls(t, polledStarted); got != 1 {
		t.Errorf("polls{started} = %v; want 1", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_catchups_total", catchUpStart, catchUpRead); got != 1 {
		t.Errorf("catchups{start,read} = %v; want 1", got)
	}
	if got := pt.read(t, readOld); got != 2 {
		t.Errorf("read_messages{old} = %v; want 2", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelPolling); got != 1 {
		t.Errorf("channels{polling} = %v; want 1", got)
	}
}

// A new root is handed on once, by the poll after it was posted, and a reply to it once, by the reads of its replies; a poll with nothing
// new hands on nothing, and reads each recent root's replies again.
func TestPollNewRootAndItsReplyAreHandedOnOnce(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	pt.calls()
	pt.f.putMessage(testChannel, made(person("1794971800010", "", testUser, "<p>new root</p>"), pt.clk.now().Add(time.Second)))
	pt.next(t)
	if got := pt.sink.callsOf("1794971800010"); got != 1 {
		t.Fatalf("the sink was given the new root %d times; want once", got)
	}
	pt.f.putMessage(testChannel, made(person("1794971800011", "1794971800010", testUser, "<p>a reply</p>"), pt.clk.now().Add(time.Second)))
	pt.next(t)
	pt.next(t)
	if got := pt.sink.callsOf("1794971800011"); got != 1 {
		t.Errorf("the sink was given the reply %d times; want once", got)
	}
	if got := pt.sink.callsOf("1794971800010"); got != 1 {
		t.Errorf("the sink was given the root %d times; want once", got)
	}
	replies := 0
	for _, c := range pt.calls() {
		if strings.Contains(c, "/messages/1794971800010/replies?") {
			replies++
		}
	}
	if replies != 3 {
		t.Errorf("reads of the root's replies = %d; want one each poll since it was found", replies)
	}
	if got := pt.polls(t, polledRead); got != 3 {
		t.Errorf("polls{polled} = %v; want 3", got)
	}
}

// The replies of a root posted before the window, or past the newest pollRootCap, are not read: such a reply is not handed on by a poll.
func TestPollReadsTheRepliesOfRecentRootsAlone(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	// A root within the look-back but whose thread is kept only while it is within the window: moved past it, its replies go unread.
	pt.f.putMessage(testChannel, made(person("1794971800020", "", testUser, "<p>early</p>"), pt.ago(5*time.Minute)))
	pt.round(t)
	if _, ok := pt.p.chans[testChannel].roots["1794971800020"]; !ok {
		t.Fatal("the catch-up read did not keep the recent root for its replies")
	}
	pt.clk.advance(pt.p.rootWindow)
	pt.f.putMessage(testChannel, made(person("1794971800021", "1794971800020", testUser, "<p>late reply</p>"), pt.clk.now()))
	pt.round(t)
	if got := pt.sink.callsOf("1794971800021"); got != 0 {
		t.Errorf("a reply to a root past the window was handed on %d times; want never", got)
	}
	ids := []string{"1794971800030", "1794971800031", "1794971800032", "1794971800033"}
	for _, id := range ids {
		pt.f.putMessage(testChannel, made(person(id, "", testUser, "<p>root</p>"), pt.clk.now().Add(time.Second)))
	}
	pt.round(t)
	pt.f.putMessage(testChannel, made(person("1794971800034", ids[0], testUser, "<p>reply to the oldest of four</p>"), pt.clk.now().Add(time.Second)))
	pt.f.putMessage(testChannel, made(person("1794971800035", ids[3], testUser, "<p>reply to the newest</p>"), pt.clk.now().Add(time.Second)))
	pt.next(t)
	for id, want := range map[string]int{"1794971800021": 0, "1794971800034": 0, "1794971800035": 1} {
		if got := pt.sink.callsOf(id); got != want {
			t.Errorf("the sink was given %s %d times; want %d", id, got, want)
		}
	}
	if n := len(pt.p.chans[testChannel].roots); n != pt.p.rootCap {
		t.Errorf("roots kept = %d; want %d", n, pt.p.rootCap)
	}
}

// What the poller read of a channel goes when no Binding names it: bound again, it begins anew, with a first read and the look-back.
func TestPollForgetsAnUnboundChannel(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	pt.sink.bind()
	pt.next(t)
	if len(pt.p.chans) != 0 {
		t.Fatalf("channels held after the unbind = %v; want none", pt.p.chans)
	}
	pt.calls()
	pt.next(t)
	if calls := pt.calls(); len(calls) != 0 {
		t.Errorf("Graph reads with nothing bound = %v; want none", calls)
	}
	pt.sink.bind(testChannel)
	pt.next(t)
	if got := pt.polls(t, polledStarted); got != 2 {
		t.Errorf("polls{started} = %v; want 2: bound again, the channel is read anew", got)
	}
}

// A bound channel whose team's Microsoft Entra group is not known is not read: it waits, told once at WARN with the fix, and counted.
func TestPollChannelWaitsForItsTeamsGroup(t *testing.T) {
	pt := newPollTest(t)
	pt.sink.bind(testChannel)
	pt.round(t)
	pt.next(t)
	if calls := pt.calls(); len(calls) != 0 {
		t.Errorf("Graph reads of a channel whose group is not known = %v; want none", calls)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelWaitingGroup); got != 1 {
		t.Errorf("channels{waiting_group} = %v; want 1", got)
	}
	if n := strings.Count(pt.logs.String(), "cannot be polled yet"); n != 1 {
		t.Errorf("WARN lines of the waiting channel = %d; want 1", n)
	}
	pt.record(t, testChannel, testChannel)
	pt.next(t)
	if got := pt.polls(t, polledStarted); got != 1 {
		t.Errorf("polls{started} after the group is known = %v; want 1", got)
	}
}

// A 429 pauses every read of the connection for its Retry-After: the next channel's read waits for it.
func TestPollThrottlingPausesTheWholeConnection(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel, testGraphChannel)
	pt.p.interval = 0 // both due in the first round
	pt.f.setGraph(func(g *fakeGraph) {
		g.fail = func(r *http.Request) (int, string, bool) {
			if strings.Contains(r.URL.Path, testChannel) && strings.HasSuffix(r.URL.Path, "/delta") {
				return http.StatusTooManyRequests, `{"error":{"code":"TooManyRequests"}}`, true // the fake's Retry-After is 4
			}
			return 0, "", false
		}
	})
	pt.round(t)
	pt.mu.Lock()
	sleeps := append([]time.Duration(nil), pt.sleeps...)
	pt.mu.Unlock()
	paused := false
	for _, d := range sleeps {
		if d >= 4*time.Second-pt.p.callGap && d <= 4*time.Second {
			paused = true
		}
	}
	if !paused {
		t.Errorf("waits = %v; want the other channel's read to wait out the Retry-After of 4s", sleeps)
	}
	if got := pt.polls(t, polledThrottled); got != 1 {
		t.Errorf("polls{throttled} = %v; want 1", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_throttled_total"); got != 1 {
		t.Errorf("throttled_total = %v; want 1", got)
	}
	if got := pt.polls(t, polledStarted); got != 1 {
		t.Errorf("polls{started} = %v; want 1: the other channel is read after the pause", got)
	}
	n := 0
	for _, c := range pt.calls() {
		if strings.Contains(c, testChannel) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("reads of the throttled channel in the round = %d; want 1: a 429 ends its poll", n)
	}
}

// A 403 marks the channel failing, told once at WARN with the fix of the resource-specific permission; the other channels are polled.
func TestPollForbiddenMarksTheChannelFailing(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel, testGraphChannel)
	pt.p.interval = 0
	pt.f.setGraph(func(g *fakeGraph) {
		g.fail = func(r *http.Request) (int, string, bool) {
			if strings.Contains(r.URL.Path, testChannel) {
				return http.StatusForbidden, `{"error":{"code":"Forbidden"}}`, true
			}
			return 0, "", false
		}
	})
	pt.round(t)
	pt.clk.advance(time.Hour)
	pt.round(t)
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelFailing); got != 1 {
		t.Errorf("channels{failing} = %v; want 1", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelPolling); got != 1 {
		t.Errorf("channels{polling} = %v; want 1: the other channel", got)
	}
	if got := pt.polls(t, polledRefused); got != 2 {
		t.Errorf("polls{refused} = %v; want 2", got)
	}
	logs := pt.logs.String()
	if n := strings.Count(logs, "level=WARN msg=\"teams: Microsoft Graph refused a poll"); n != 1 || !strings.Contains(logs, "ChannelMessage.Read.Group") {
		t.Errorf("WARN lines of the refusal = %d, naming the permission %v; want one that names it", n, strings.Contains(logs, "ChannelMessage.Read.Group"))
	}
}

// A deltaLink that Graph no longer takes is read anew from now, logged at WARN and counted; nothing older than the last poll's bound is
// handed on, and what was posted since the last poll is read back by a catch-up read.
func TestPollExpiredDeltaLinkResetsFromNow(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	pt.f.putMessage(testChannel, made(person("1794971800040", "", testUser, "<p>during the reset</p>"), pt.clk.now().Add(time.Second)))
	pt.f.setGraph(func(g *fakeGraph) { g.deltaGone = true })
	pt.next(t)
	if got := pt.polls(t, polledReset); got != 1 {
		t.Errorf("polls{reset} = %v; want 1", got)
	}
	if !strings.Contains(pt.logs.String(), "no longer takes the channel's delta link") {
		t.Error("the reset was not logged")
	}
	pt.f.setGraph(func(g *fakeGraph) { g.deltaGone = false })
	pt.next(t)
	if got := pt.sink.callsOf("1794971800040"); got != 1 {
		t.Errorf("the post made since the last poll was handed on %d times; want once, by the catch-up read", got)
	}
}

// Graph refusing the deltaLink at once after the start is a reset too, and a post older than the look-back is not handed on by it.
func TestPollResetHandsOnNothingOld(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	pt.clk.advance(time.Hour)
	pt.f.putMessage(testChannel, made(person("1794971800041", "", testUser, "<p>old</p>"), pt.ago(30*time.Minute)))
	pt.f.setGraph(func(g *fakeGraph) { g.deltaGone = true })
	pt.round(t)
	pt.f.setGraph(func(g *fakeGraph) { g.deltaGone = false })
	pt.next(t)
	if got := pt.sink.callsOf("1794971800041"); got != 0 {
		t.Errorf("a post older than the look-back was handed on %d times after a reset; want never", got)
	}
}

// A gap of the sockets that ends reads every channel at once, whenever it is due, with a catch-up read back to the gap's start: a reply
// in a thread whose replies the poll does not read is handed on.
func TestPollSocketGapReadsEveryChannelAtOnce(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.f.putMessage(testChannel, made(person("1794971800050", "", testUser, "<p>older thread</p>"), pt.ago(50*time.Minute)))
	pt.round(t)
	pt.clk.advance(time.Second) // the channel is not due for another interval
	gapStart := pt.clk.now()
	pt.f.putMessage(testChannel, made(person("1794971800051", "1794971800050", testUser, "<p>mention in the gap</p>"), gapStart.Add(time.Second)))
	pt.clk.advance(2 * time.Second)
	pt.c.poll.gap(Gap{Start: gapStart, End: pt.clk.now()})
	pt.round(t)
	if got := pt.sink.callsOf("1794971800051"); got != 1 {
		t.Errorf("the reply posted in the gap was handed on %d times; want once", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_catchups_total", catchUpGap, catchUpRead); got != 1 {
		t.Errorf("catchups{socket_gap,read} = %v; want 1", got)
	}
	pt.c.poll.gap(Gap{Start: pt.clk.now(), End: pt.clk.now(), Initial: true})
	select {
	case <-pt.p.wake:
		// the gap above left its wake, which round does not drain
	default:
	}
	pt.c.poll.gap(Gap{Start: pt.clk.now(), End: pt.clk.now(), Initial: true})
	select {
	case <-pt.p.wake:
		t.Error("the run's initial gap asked for a round; the start's catch-up covers it")
	default:
	}
}

// Socket mode wires the gaps of its sockets to the poller, and still tells Config.OnSocketGap.
func TestPollSocketModeWiresTheGap(t *testing.T) {
	var told []Gap
	c, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret), Socket: true,
		OnSocketGap: func(g Gap) { told = append(told, g) }})
	if err != nil {
		t.Fatal(err)
	}
	c.sockets.onGap(Gap{Start: time.Now().Add(-time.Minute), End: time.Now()})
	c.poll.mu.Lock()
	from := c.poll.gapFrom
	c.poll.mu.Unlock()
	if len(told) != 1 || from.IsZero() {
		t.Errorf("OnSocketGap told %d, poller's gap %v; want both told", len(told), from)
	}
}

// A post found by a poll and as an activity is handed on once, whichever comes first.
func TestPollAndAnActivityTakeAPostOnce(t *testing.T) {
	for _, activityFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("activity first %v", activityFirst), func(t *testing.T) {
			pt := newPollTest(t)
			pt.bound(t, testChannel)
			pt.round(t)
			const id = "1794971800060"
			activity := func() answer {
				ans := pt.c.recv.takePayload(t.Context(), inChannel(t, "message_mention", testChannel, id))
				if ans.status != http.StatusOK {
					t.Fatalf("the activity was answered %d", ans.status)
				}
				return ans
			}
			pt.f.putMessage(testChannel, made(person(id, "", testUser, `<p><at id="0">Agent Kourier</at> hello</p>`, mentionOfBot()),
				pt.clk.now().Add(time.Second)))
			if activityFirst {
				activity()
				testutil.Eventually(t, "the sink has it", func() bool { return pt.sink.has(id) })
				pt.next(t)
				if got := pt.read(t, notifiedDuplicate); got != 1 {
					t.Errorf("read_messages{duplicate} = %v; want 1", got)
				}
			} else {
				pt.next(t)
				if ans := activity(); ans.outcome != requestDuplicate {
					t.Errorf("the activity after the poll was %s; want %s", ans.outcome, requestDuplicate)
				}
			}
			s := pt.c.recv.live.Load()
			testutil.Eventually(t, "the lanes idle", func() bool { return s.lanes.Held() == 0 })
			if got := pt.sink.callsOf(id); got != 1 {
				t.Errorf("the sink was given the post %d times; want once", got)
			}
		})
	}
}

// The bot's own post is dropped; an alert's card that Workflows posted is a bot's message whose sender is the app, if a trigger of the
// channel names it, and is dropped otherwise.
func TestPollHandsOnAlertsAndDropsTheBotsOwnPosts(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	at := pt.clk.now().Add(time.Second)
	pt.f.putMessage(testChannel, made(app("1794971800070", "", testAppID, "<p>Looking into it.</p>"), at))
	pt.f.putMessage(testChannel, made(app("1794971800071", "", testWorkflows, `<attachment id="d1f0"></attachment>`, alertCard), at))
	pt.next(t)
	if got := pt.read(t, notifiedOwn); got != 1 {
		t.Errorf("read_messages{own} = %v; want 1", got)
	}
	if got := pt.read(t, notifiedNotNamed); got != 1 {
		t.Errorf("read_messages{not_named} = %v; want 1: no trigger names the app", got)
	}
	pt.sink.takeBot(testChannel, testWorkflows)
	pt.f.putMessage(testChannel, made(app("1794971800072", "", testWorkflows, `<attachment id="d1f1"></attachment>`, alertCard), pt.clk.now().Add(time.Second)))
	pt.next(t)
	want := chat.Inbound{At: chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1794971800072"},
		Bot: &chat.BotMessage{ID: testWorkflows, Kind: v1alpha1.SenderKindBot, Body: "[FIRING:1] KubePodCrashLooping\nseverity: critical\nnamespace: payments"}}
	var got *chat.Inbound
	for _, m := range pt.sink.got() {
		if m.At.ID == want.At.ID {
			got = &m
		}
	}
	if got == nil || !sameInbound(*got, want) {
		t.Errorf("the sink was given %+v; want the alert as Workflows' message %+v", got, want)
	}
	if pt.sink.has("1794971800070") {
		t.Error("the bot's own post was handed on")
	}
}

// A connection with a notificationURL does not poll, and one whose run never starts (a replica that does not lead) reads nothing.
func TestPollOnlyWithoutANotificationURLAndOnlyInARun(t *testing.T) {
	pt := newPushTest(t, false)
	if pt.c.poll != nil {
		t.Fatal("a Client with a notificationURL has a poller")
	}
	pt.subscribed(t, testChannel)
	pt.settle(t)
	for _, r := range pt.f.graphTaken() {
		if strings.HasSuffix(r.path, "/delta") || strings.HasSuffix(r.path, "/replies") {
			t.Errorf("a connection with a notificationURL read %s", r.path)
		}
	}

	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	time.Sleep(20 * time.Millisecond)
	if got := ct.f.graphTaken(); len(got) != 0 {
		t.Errorf("a Client that is not run read Graph %d times; want never", len(got))
	}
}

// Run polls the bound channels while it runs, and stops when it returns.
func TestPollRunsWithTheRun(t *testing.T) {
	ct := newClientTest(t)
	ct.c.poll.tick = 5 * time.Millisecond
	ct.record(t, testChannel, testChannel)
	sink := &bindingSink{}
	sink.bind(testChannel)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- ct.c.Run(ctx, sink) }()
	testutil.Eventually(t, "a delta read", func() bool {
		for _, r := range ct.f.graphTaken() {
			if strings.HasSuffix(r.path, "/delta") {
				return true
			}
		}
		return false
	})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v; want context.Canceled", err)
	}
	ct.f.graphTaken()
	time.Sleep(20 * time.Millisecond)
	if got := ct.f.graphTaken(); len(got) != 0 {
		t.Errorf("Graph reads after Run returned = %d; want none", len(got))
	}
}

// Graph's lifecycle notification missed makes one catch-up read of the channel, of which what was posted within the look-back is handed
// on, through the dedup that the notifications share: a post that a notification handed on already is not handed on again.
func TestGraphMissedReadsTheChannelBackOnce(t *testing.T) {
	pt := newPushTest(t, false)
	pt.subscribed(t, testChannel)
	sub := pt.subOf(testChannel)
	now := time.Now()
	pt.f.putMessage(testChannel, made(person("1794971800080", "", testUser, "<p>missed</p>"), now.Add(-time.Minute)))
	pt.f.putMessage(testChannel, made(person("1794971800081", "", testUser, "<p>too old</p>"), now.Add(-time.Hour)))
	pt.f.putMessage(testChannel, made(person("1794971800082", "", testUser, "<p>notified</p>"), now.Add(-30*time.Second)))
	if status, _ := pt.notify(t, pt.change(sub, testChannel, "created", "", "1794971800082")); status != http.StatusAccepted {
		t.Fatal("the notification was not taken")
	}
	testutil.Eventually(t, "the notified post", func() bool { return pt.sink.has("1794971800082") })
	pt.graphTaken()
	if status, _ := pt.notify(t, pt.lifecycle(sub, "missed")); status != http.StatusAccepted {
		t.Fatal("the lifecycle notification was not taken")
	}
	testutil.Eventually(t, "the catch-up read", func() bool {
		return pt.obs.series(t, "agentkourier_teams_graph_catchups_total", catchUpMissed, catchUpRead) == 1
	})
	for id, want := range map[string]int{"1794971800080": 1, "1794971800081": 0, "1794971800082": 1} {
		if got := pt.sink.callsOf(id); got != want {
			t.Errorf("the sink was given %s %d times; want %d", id, got, want)
		}
	}
	reads := 0
	for _, r := range pt.f.graphTaken() {
		if strings.HasSuffix(r.path, "/messages") {
			reads++
			if !strings.Contains(r.query, "$expand=replies") || !strings.Contains(r.query, fmt.Sprintf("$top=%d", catchUpTop)) {
				t.Errorf("the catch-up read's query = %q; want $top=%d and $expand=replies", r.query, catchUpTop)
			}
		}
	}
	if reads != 1 {
		t.Errorf("catch-up reads = %d; want 1", reads)
	}
	if got := <-pt.missed; got != testChannel {
		t.Errorf("OnNotificationsMissed was told %q; want the channel", got)
	}
}

// An edit, or another change, of a post older than the last poll's bound, which delta gives again, is not handed on.
func TestPollDropsAChangeOfAnOlderPost(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	pt.f.putMessage(testChannel, made(person("1794971800090", "", testUser, "<p>edited long after</p>"), pt.ago(20*time.Minute)))
	pt.next(t)
	if got := pt.sink.callsOf("1794971800090"); got != 0 {
		t.Errorf("a change of an older post was handed on %d times; want never", got)
	}
	if got := pt.read(t, readOld); got != 1 {
		t.Errorf("read_messages{old} = %v; want 1", got)
	}
}

// The deltaLink is kept only once what its read gave is on the channel's lane: a read whose lane had no room is made again from the
// same deltaLink, and its post is handed on then.
func TestPollKeepsTheDeltaLinkAfterTheHandOff(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	link := pt.p.chans[testChannel].link
	pt.sink.block, pt.sink.blockChannel = make(chan struct{}), testChannel
	for i := range lanes.PerChannel {
		id := fmt.Sprintf("%s%06d", flushPrefix, i+1)
		if ans := pt.c.recv.takePayload(t.Context(), inChannel(t, "message_plain", testChannel, id)); ans.status != http.StatusOK {
			t.Fatalf("filling the lane: %d", ans.status)
		}
	}
	pt.f.putMessage(testChannel, made(person("1794971800091", "", testUser, "<p>while the lane is full</p>"), pt.clk.now().Add(time.Second)))
	pt.clk.advance(pt.p.interval)
	pt.p.round(pt.obs.ctx, pt.sink)
	if got := pt.p.chans[testChannel].link; got != link {
		t.Error("the deltaLink moved on although what its read gave was not put on the lane")
	}
	if got := pt.read(t, readShed); got != 1 {
		t.Errorf("read_messages{shed} = %v; want 1", got)
	}
	close(pt.sink.block)
	s := pt.c.recv.live.Load()
	testutil.Eventually(t, "the lanes idle", func() bool { return s.lanes.Held() == 0 })
	pt.clk.advance(time.Hour) // past the backoff
	pt.round(t)
	if got := pt.sink.callsOf("1794971800091"); got != 1 {
		t.Errorf("the post read while the lane was full was handed on %d times; want once, by the read made again", got)
	}
}

// deltaReads is how many of calls read the delta of channel ch.
func deltaReads(calls []string, ch string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, "/channels/"+ch+"/messages/delta?") {
			n++
		}
	}
	return n
}

// A channel whose delta names no deltaLink within the pages the broker reads is failing, told once at WARN, and its delta is not read
// again; its catch-up read goes on each interval, so a new post in it is still handed on.
func TestPollPageCapMarksTheChannelFailingAndReadsItsNewPosts(t *testing.T) {
	pt := newPollTest(t)
	pt.p.maxPages = 3
	pt.f.setGraph(func(g *fakeGraph) { g.deltaPages[testChannel] = 1000 })
	pt.bound(t, testChannel)
	pt.f.putMessage(testChannel, made(person("1794971800120", "", testUser, "<p>recent</p>"), pt.ago(time.Minute)))
	pt.round(t)
	if got := deltaReads(pt.calls(), testChannel); got != pt.p.maxPages {
		t.Errorf("delta reads = %d; want %d, the cap", got, pt.p.maxPages)
	}
	if got := pt.polls(t, polledTooLong); got != 1 {
		t.Errorf("polls{too_long} = %v; want 1", got)
	}
	if got := pt.sink.callsOf("1794971800120"); got != 1 {
		t.Errorf("the recent post was handed on %d times; want once, by the start's catch-up read", got)
	}
	pt.f.putMessage(testChannel, made(person("1794971800121", "", testUser, "<p>new</p>"), pt.clk.now().Add(time.Second)))
	pt.next(t)
	pt.next(t)
	if got := pt.sink.callsOf("1794971800121"); got != 1 {
		t.Errorf("a new post of the failing channel was handed on %d times; want once, by its catch-up read", got)
	}
	if got := deltaReads(pt.calls(), testChannel); got != 0 {
		t.Errorf("delta reads past the cap = %d; want none", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelFailing); got != 1 {
		t.Errorf("channels{failing} = %v; want 1", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelPolling); got != 0 {
		t.Errorf("channels{polling} = %v; want 0", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_catchups_total", catchUpSyncing, catchUpRead); got != 2 {
		t.Errorf("catchups{syncing,read} = %v; want 2", got)
	}
	if n := strings.Count(pt.logs.String(), "level=WARN msg=\"teams: the delta of a bound channel's messages named no deltaLink"); n != 1 {
		t.Errorf("WARN lines of the cap = %d; want 1", n)
	}
	if got := pt.polls(t, polledTooLong); got != 3 {
		t.Errorf("polls{too_long} = %v; want 3, one each poll", got)
	}
}

// A long history is synced over several polls, at most pollSyncPages pages each, so the other channels are read in the same rounds, and
// a post made while it syncs is handed on by the catch-up read that each of its polls makes.
func TestPollSyncSpreadsItsPagesOverPolls(t *testing.T) {
	pt := newPollTest(t)
	pt.f.setGraph(func(g *fakeGraph) { g.deltaPages[testChannel] = 2 * pt.p.syncPages }) // 12 pages: 5, 5 and 2
	pt.bound(t, testChannel, testGraphChannel)
	pt.round(t)
	if got := deltaReads(pt.calls(), testChannel); got != pt.p.syncPages {
		t.Errorf("delta reads of the first poll = %d; want %d", got, pt.p.syncPages)
	}
	pt.f.putMessage(testChannel, made(person("1794971800130", "", testUser, "<p>while it syncs</p>"), pt.clk.now().Add(time.Second)))
	pt.next(t)
	calls := pt.calls()
	if got := deltaReads(calls, testChannel); got != pt.p.syncPages {
		t.Errorf("delta reads of the second poll = %d; want %d", got, pt.p.syncPages)
	}
	if got := deltaReads(calls, testGraphChannel); got == 0 {
		t.Error("the other channel was not read in the round of the sync")
	}
	if got := pt.sink.callsOf("1794971800130"); got != 1 {
		t.Errorf("the post made while the channel syncs was handed on %d times; want once", got)
	}
	pt.next(t)
	if got := deltaReads(pt.calls(), testChannel); got != 2 {
		t.Errorf("delta reads of the third poll = %d; want 2", got)
	}
	if pt.p.chans[testChannel].link == "" {
		t.Fatal("the sync did not reach the deltaLink")
	}
	if got := pt.polls(t, polledSyncing); got != 2 {
		t.Errorf("polls{syncing} = %v; want 2", got)
	}
	if got := pt.polls(t, polledStarted); got != 2 {
		t.Errorf("polls{started} = %v; want 2, one each channel", got)
	}
	pt.f.putMessage(testChannel, made(person("1794971800131", "", testUser, "<p>after the sync</p>"), pt.clk.now().Add(time.Second)))
	pt.next(t)
	if got := pt.sink.callsOf("1794971800131"); got != 1 {
		t.Errorf("a post after the sync was handed on %d times; want once, by the delta", got)
	}
	if got := pt.sink.callsOf("1794971800130"); got != 1 {
		t.Errorf("the post made while the channel synced was handed on %d times in all; want once", got)
	}
}

// A gap of the sockets that ends while a channel syncs a long history is acted on in the next round: the channel that syncs reads its
// pages for one poll alone, and the other channels' catch-up reads are made in the same round.
func TestPollLongSyncDoesNotDelayAGapCatchUp(t *testing.T) {
	pt := newPollTest(t)
	pt.f.setGraph(func(g *fakeGraph) { g.deltaPages[testChannel] = 1_000_000 })
	pt.bound(t, testChannel, testGraphChannel)
	pt.round(t)
	pt.next(t)
	pt.calls()
	pt.clk.advance(time.Second)
	gapStart := pt.clk.now()
	pt.f.putMessage(testGraphChannel, made(person("1794971800140", "", testUser, "<p>in the gap</p>"), gapStart.Add(time.Second)))
	pt.clk.advance(2 * time.Second)
	before := pt.clk.now()
	pt.p.gap(Gap{Start: gapStart, End: pt.clk.now()})
	pt.round(t)
	if got := deltaReads(pt.calls(), testChannel); got > pt.p.syncPages {
		t.Errorf("delta reads of the syncing channel in the gap's round = %d; want at most %d", got, pt.p.syncPages)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_catchups_total", catchUpGap, catchUpRead); got != 2 {
		t.Errorf("catchups{socket_gap,read} = %v; want 2, both channels in the gap's round", got)
	}
	if got := pt.sink.callsOf("1794971800140"); got != 1 {
		t.Errorf("the post of the gap was handed on %d times; want once", got)
	}
	// At most a sync's pages, the replies and a catch-up read for each channel: the round is not held by the history.
	if took := pt.clk.now().Sub(before); took > time.Duration(2*(pt.p.syncPages+2*pt.p.rootCap+2))*pt.p.callGap {
		t.Errorf("the gap's round took %v of the poller's paced reads; want it bounded by one poll of each channel", took)
	}
}

// A reply to a root that a new root pushes out of the newest pollRootCap in the same interval is read once more, at the poll that finds
// the new root: the replies of the roots kept before the delta read are read too, at most twice pollRootCap reads.
func TestPollReadsTheRepliesOfARootThatANewRootPushesOut(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	for _, id := range []string{"1794971800101", "1794971800102", "1794971800103"} {
		pt.f.putMessage(testChannel, made(person(id, "", testUser, "<p>root</p>"), pt.clk.now().Add(time.Second)))
	}
	pt.next(t)
	pt.calls()
	pt.f.putMessage(testChannel, made(person("1794971800110", "1794971800101", testUser, "<p>reply to R1</p>"), pt.clk.now().Add(time.Second)))
	pt.f.putMessage(testChannel, made(person("1794971800104", "", testUser, "<p>R4</p>"), pt.clk.now().Add(2*time.Second)))
	pt.next(t)
	replies := 0
	for _, c := range pt.calls() {
		if strings.Contains(c, "/replies?") {
			replies++
		}
	}
	if replies != 4 || replies > 2*pt.p.rootCap {
		t.Errorf("reads of replies at the poll that found R4 = %d; want 4 (R1 to R4), at most %d", replies, 2*pt.p.rootCap)
	}
	pt.next(t)
	pt.next(t)
	if got := pt.sink.callsOf("1794971800110"); got != 1 {
		t.Errorf("the reply to the root pushed out was handed on %d times; want once", got)
	}
	if _, ok := pt.p.chans[testChannel].roots["1794971800101"]; ok {
		t.Error("the root pushed out is still kept")
	}
}

// A reply that Graph shows after a newer one, with a lower ID, is handed on: a root's replies are bounded by when they were made, less
// the skew, and not by their IDs.
func TestPollHandsOnAReplyThatGraphShowsLate(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	pt.f.putMessage(testChannel, made(person("1794971800201", "", testUser, "<p>root</p>"), pt.clk.now().Add(time.Second)))
	pt.next(t)
	pt.f.putMessage(testChannel, made(person("1794971800211", "1794971800201", testUser, "<p>B</p>"), pt.clk.now().Add(time.Second)))
	pt.next(t)
	b := pt.p.chans[testChannel].roots["1794971800201"].newest // when B was made
	if b.IsZero() {
		t.Fatal("the read of the root's replies kept no newest reply")
	}
	pt.f.putMessage(testChannel, made(person("1794971800210", "1794971800201", testUser, "<p>A, seen late</p>"), pt.clk.now().Add(-time.Second)))
	pt.f.putMessage(testChannel, made(person("1794971800209", "1794971800201", testUser, "<p>made 20s before B</p>"), b.Add(-20*time.Second)))
	pt.f.putMessage(testChannel, made(person("1794971800208", "1794971800201", testUser, "<p>made 40s before B</p>"), b.Add(-40*time.Second)))
	pt.next(t)
	for id, want := range map[string]int{"1794971800210": 1, "1794971800209": 1, "1794971800208": 0} {
		if got := pt.sink.callsOf(id); got != want {
			t.Errorf("the late reply %s was handed on %d times; want %d: within the skew of the newest, and not past it", id, got, want)
		}
	}
}

// What a read of a root's replies gave is not handed on again, nor read as new, however long after (past the dedup's window), and a
// reply that a catch-up read found older than the root's newest, less the skew, is not handed on by a read of the replies.
func TestPollRepliesWatermarkHoldsPastTheDedupWindow(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.f.putMessage(testChannel, made(person("1794971800220", "", testUser, "<p>older root</p>"), pt.ago(30*time.Minute)))
	pt.f.putMessage(testChannel, made(person("1794971800221", "1794971800220", testUser, "<p>old reply</p>"), pt.ago(20*time.Minute)))
	pt.f.putMessage(testChannel, made(person("1794971800222", "1794971800220", testUser, "<p>recent reply</p>"), pt.ago(2*time.Minute)))
	pt.round(t)
	pt.f.putMessage(testChannel, made(person("1794971800223", "1794971800220", testUser, "<p>new reply</p>"), pt.clk.now().Add(time.Second)))
	pt.next(t)
	pt.clk.advance(dedupWindow + time.Minute)
	pt.round(t)
	pt.next(t)
	for id, want := range map[string]int{"1794971800221": 0, "1794971800222": 1, "1794971800223": 1} {
		if got := pt.sink.callsOf(id); got != want {
			t.Errorf("the sink was given %s %d times; want %d", id, got, want)
		}
	}
	if got := pt.read(t, notifiedDuplicate); got != 0 {
		t.Errorf("read_messages{duplicate} = %v; want 0: a reply read once is not read as new again", got)
	}
}

// A catch-up read that fails and is made again after the look-back has passed hands on nothing made before the look-back of the read
// that succeeds, which the dedup may have forgotten.
func TestPollCatchUpRetriedPastTheLookbackHandsOnNothingOlder(t *testing.T) {
	pt := newPollTest(t)
	failing := true
	pt.f.setGraph(func(g *fakeGraph) {
		g.fail = func(r *http.Request) (int, string, bool) {
			if failing && r.URL.Query().Has("$expand") {
				return http.StatusServiceUnavailable, `{"error":{"code":"ServiceUnavailable"}}`, true
			}
			return 0, "", false
		}
	})
	pt.bound(t, testChannel)
	pt.f.putMessage(testChannel, made(person("1794971800230", "", testUser, "<p>before the start</p>"), pt.ago(time.Minute)))
	for range 20 {
		pt.round(t)
		pt.clk.advance(time.Minute)
	}
	pt.f.setGraph(func(*fakeGraph) { failing = false })
	pt.clk.advance(pollFailMax)
	pt.round(t)
	if got := pt.obs.series(t, "agentkourier_teams_graph_catchups_total", catchUpStart, catchUpRead); got != 1 {
		t.Fatalf("catchups{start,read} = %v; want 1 at last", got)
	}
	if got := pt.sink.callsOf("1794971800230"); got != 0 {
		t.Errorf("a post made before the look-back of the read that succeeded was handed on %d times; want never", got)
	}
	if got := pt.read(t, readOld); got < 1 {
		t.Errorf("read_messages{old} = %v; want the post counted old", got)
	}
}

// A post made up to pollSkew before the last poll began, which that poll's delta did not give, is new to the next poll; one made before
// that is not.
func TestPollKeepsAPostMadeWithinTheSkew(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	prev := pt.p.chans[testChannel].prev
	pt.f.putMessage(testChannel, made(person("1794971800240", "", testUser, "<p>late by 20s</p>"), prev.Add(-20*time.Second)))
	pt.f.putMessage(testChannel, made(person("1794971800241", "", testUser, "<p>late by 40s</p>"), prev.Add(-40*time.Second)))
	pt.next(t)
	if got := pt.sink.callsOf("1794971800240"); got != 1 {
		t.Errorf("a post made 20s before the last poll was handed on %d times; want once", got)
	}
	if got := pt.sink.callsOf("1794971800241"); got != 0 {
		t.Errorf("a post made 40s before the last poll was handed on %d times; want never", got)
	}
}

// The catch-up read of a gap looks back no further than the look-back, however long ago the gap began.
func TestPollGapCatchUpIsBoundedByTheLookback(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.f.putMessage(testChannel, made(person("1794971800250", "", testUser, "<p>in a long gap, long ago</p>"), pt.ago(30*time.Minute)))
	pt.round(t)
	pt.clk.advance(time.Second)
	pt.p.gap(Gap{Start: pt.ago(time.Hour), End: pt.clk.now()})
	pt.round(t)
	if got := pt.obs.series(t, "agentkourier_teams_graph_catchups_total", catchUpGap, catchUpRead); got != 1 {
		t.Fatalf("catchups{socket_gap,read} = %v; want 1", got)
	}
	if got := pt.sink.callsOf("1794971800250"); got != 0 {
		t.Errorf("a post made before the look-back was handed on %d times by the gap's catch-up read; want never", got)
	}
}

// A run starts from nothing: what the last run read of a channel, and a gap told after it ended, are dropped.
func TestPollANewRunStartsFromNothing(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	pt.p.gap(Gap{Start: pt.ago(time.Minute), End: pt.clk.now()})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	pt.p.run(ctx, pt.sink)
	pt.p.mu.Lock()
	gapFrom := pt.p.gapFrom
	pt.p.mu.Unlock()
	if len(pt.p.chans) != 0 || !gapFrom.IsZero() || len(pt.p.wake) != 0 {
		t.Fatalf("after a run: channels %d, gap %v, wake %d; want none of them", len(pt.p.chans), gapFrom, len(pt.p.wake))
	}
	pt.round(t)
	if got := pt.polls(t, polledStarted); got != 2 {
		t.Errorf("polls{started} = %v; want 2: the second run syncs the channel anew", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_catchups_total", catchUpStart, catchUpRead); got != 2 {
		t.Errorf("catchups{start,read} = %v; want 2", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_catchups_total", catchUpGap, catchUpRead); got != 0 {
		t.Errorf("catchups{socket_gap,read} = %v; want 0: the gap was told before the run", got)
	}
}

// A poll keeps for their replies the roots that are posts alone (not a system event); drops a message that does not name itself by a
// message ID, counted ignored; takes a message whose createdDateTime it cannot read as old, logged at DEBUG; and forgets a root whose
// replies Graph refuses to read, while the channel is polled on.
func TestPollKeepsPostsAloneAndForgetsARefusedRoot(t *testing.T) {
	pt := newPollTest(t)
	pt.bound(t, testChannel)
	pt.round(t)
	at := pt.clk.now().Add(time.Second)
	event := made(person("1794971800260", "", testUser, "<p>joined</p>"), at)
	event["messageType"] = "systemEventMessage"
	pt.f.putMessage(testChannel, event)
	pt.f.putMessage(testChannel, made(person("1794971800261", "", testUser, "<p>a post</p>"), at))
	pt.f.putMessage(testChannel, made(person("not-an-id", "", testUser, "<p>a bad ID</p>"), at))
	pt.f.putMessage(testChannel, person("1794971800262", "", testUser, "<p>no date</p>"))
	pt.next(t)
	roots := pt.p.chans[testChannel].roots
	if _, ok := roots["1794971800260"]; ok {
		t.Error("a system event is kept as a root")
	}
	if _, ok := roots["1794971800261"]; !ok {
		t.Fatal("a post is not kept as a root")
	}
	if got := pt.read(t, notifiedIgnored); got != 2 {
		t.Errorf("read_messages{ignored} = %v; want 2: the system event and the message with a bad ID", got)
	}
	if pt.sink.has("not-an-id") || pt.sink.has("1794971800262") {
		t.Error("a message with a bad ID, or with no date, was handed on")
	}
	if !strings.Contains(pt.logs.String(), "has no createdDateTime that the broker reads") {
		t.Error("the message with no date was not logged")
	}
	pt.f.setGraph(func(g *fakeGraph) {
		g.fail = func(r *http.Request) (int, string, bool) {
			if strings.Contains(r.URL.Path, "/messages/1794971800261/replies") {
				return http.StatusForbidden, `{"error":{"code":"Forbidden"}}`, true
			}
			return 0, "", false
		}
	})
	pt.next(t)
	if _, ok := pt.p.chans[testChannel].roots["1794971800261"]; ok {
		t.Error("a root whose replies Graph refused is kept")
	}
	if got := pt.polls(t, polledRefused); got != 0 {
		t.Errorf("polls{refused} = %v; want 0: one root's refusal does not fail the channel", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelPolling); got != 1 {
		t.Errorf("channels{polling} = %v; want 1", got)
	}
}

package teams

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/lanes"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// flushPrefix begins the IDs of flush's posts, which got leaves out; no other post of the tests has an ID that begins so.
const flushPrefix = "1799999"

// recordingSink records what it is given, and counts every call by message ID. While block is open, Message waits on it for a message
// of blockChannel, or of any channel when blockChannel is "", or until its context ends, and then returns after unwind and records
// nothing. A message ID in failing is refused as many times as it says (failAlways: every time).
type recordingSink struct {
	mu           sync.Mutex
	messages     []chat.Inbound
	calls        map[string]int
	callTimes    map[string][]time.Time
	failing      map[string]int
	block        chan struct{}
	blockChannel string
	unwind       time.Duration
}

// failAlways is a count of failing that does not run out in a test.
const failAlways = 1 << 30

// errSinkDown is the error of a refused message.
var errSinkDown = errors.New("the sink is down")

func (s *recordingSink) Message(ctx context.Context, m chat.Inbound) error {
	s.mu.Lock()
	if s.calls == nil {
		s.calls, s.callTimes = map[string]int{}, map[string][]time.Time{}
	}
	s.calls[m.At.ID]++
	s.callTimes[m.At.ID] = append(s.callTimes[m.At.ID], time.Now())
	if s.failing[m.At.ID] > 0 {
		s.failing[m.At.ID]--
		s.mu.Unlock()
		return errSinkDown
	}
	s.mu.Unlock()
	if s.block != nil && (s.blockChannel == "" || s.blockChannel == m.At.Channel) {
		select {
		case <-s.block:
		case <-ctx.Done():
			time.Sleep(s.unwind)
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
	return nil
}

// fail makes the sink refuse the message id n more times.
func (s *recordingSink) fail(id string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing == nil {
		s.failing = map[string]int{}
	}
	s.failing[id] = n
}

// callsOf is how many times Message was called with the message id.
func (s *recordingSink) callsOf(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[id]
}

// timesOf is when Message was called with the message id, in order.
func (s *recordingSink) timesOf(id string) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.callTimes[id]...)
}

func (s *recordingSink) Interaction(context.Context, chat.Interaction) error { return nil }

// got is what the sink was given, flush's posts left out.
func (s *recordingSink) got() []chat.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []chat.Inbound
	for _, m := range s.messages {
		if !strings.HasPrefix(m.At.ID, flushPrefix) {
			out = append(out, m)
		}
	}
	return out
}

// has says whether the sink was given the message id.
func (s *recordingSink) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.messages {
		if m.At.ID == id {
			return true
		}
	}
	return false
}

// invokeRecorder is an InvokeHandler that records the invokes, and the sink each was given, and answers with answer.
type invokeRecorder struct {
	mu      sync.Mutex
	invokes []Invoke
	sinks   []chat.Sink
	answer  InvokeAnswer
}

func (h *invokeRecorder) Invoke(_ context.Context, sink chat.Sink, inv Invoke) InvokeAnswer {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.invokes = append(h.invokes, inv)
	h.sinks = append(h.sinks, sink)
	return h.answer
}

func (h *invokeRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.invokes)
}

// recordingStore is a MemoryConversations that also keeps every record it was given.
type recordingStore struct {
	*MemoryConversations
	mu   sync.Mutex
	recs []Conversation
}

func (s *recordingStore) Remember(ctx context.Context, conn string, c Conversation) error {
	s.mu.Lock()
	s.recs = append(s.recs, c)
	s.mu.Unlock()
	return s.MemoryConversations.Remember(ctx, conn, c)
}

func (s *recordingStore) given() []Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Conversation(nil), s.recs...)
}

// receiverTest is a Receiver of the test connection, with the fake Bot Framework's keys, and what it hands on.
type receiverTest struct {
	r       *Receiver
	f       *fakeBotFramework
	key     signingKey
	clock   *fakeClock
	sink    *recordingSink
	convs   *recordingStore
	flushes int
}

// newReceiverTest makes a Receiver; with serve, it also runs it with its sink until the test ends.
func newReceiverTest(t *testing.T, serve bool, invokes InvokeHandler) *receiverTest {
	t.Helper()
	key := newSigningKey(t, "k", "msteams")
	f := newFakeBotFramework(t, key)
	clock := newFakeClock()
	rt := &receiverTest{f: f, key: key, clock: clock, sink: &recordingSink{}, convs: &recordingStore{MemoryConversations: NewMemoryConversations()}}
	r, err := NewReceiver(ReceiverConfig{
		Connection: testConn, TenantID: testTenant, AppID: testAppID, MetadataURL: f.metadataURL(), HTTPClient: f.srv.Client(),
		Conversations: rt.convs, Invokes: invokes, Logger: testutil.DiscardLogger(), Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.sinkRetryBase = time.Millisecond
	rt.r = r
	if serve {
		rt.serve(t)
	}
	return rt
}

// serve runs the Receiver with the test's sink, and waits until it takes requests. stop ends it and returns what Serve returned; the
// test's end stops it too.
func (rt *receiverTest) serve(t *testing.T) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.r.Serve(ctx, rt.sink) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			err = <-done
		})
		return err
	}
	t.Cleanup(func() {
		if rt.sink.block != nil {
			select {
			case <-rt.sink.block:
			default:
				close(rt.sink.block)
			}
		}
		if err := stop(); !errors.Is(err, context.Canceled) {
			t.Errorf("Serve returned %v, want context.Canceled", err)
		}
	})
	testutil.Eventually(t, "the receiver takes requests", func() bool { return rt.r.live.Load() != nil })
	return stop
}

// post sends body to the Receiver with a token signed with the test key, or with authz when it is not "".
func (rt *receiverTest) post(t *testing.T, body []byte, authz string) *httptest.ResponseRecorder {
	t.Helper()
	if authz == "" {
		authz = bearer(t, rt.key, claimsAt(rt.clock.now()))
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat/teams/"+testConn, bytes.NewReader(body))
	req.Header.Set("Authorization", authz)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rt.r.ServeHTTP(rec, req)
	return rec
}

// inChannel is the fixture name moved to channel, with the ID id.
func inChannel(t *testing.T, name, channel, id string) []byte {
	t.Helper()
	return editFixture(t, name, func(m map[string]any) {
		m["id"] = id
		m["conversation"].(map[string]any)["id"] = channel + threadMarker + id
	})
}

// flush posts a person's message in each of channels and waits until the sink has them. A channel's lane hands on in the order it
// accepted, so whatever was put on those lanes before has reached the sink by then. It returns the sink's messages, flush's left out.
func (rt *receiverTest) flush(t *testing.T, channels ...string) []chat.Inbound {
	t.Helper()
	for _, channel := range channels {
		rt.flushes++
		id := fmt.Sprintf("%s%06d", flushPrefix, rt.flushes)
		if rec := rt.post(t, inChannel(t, "message_plain", channel, id), ""); rec.Code != http.StatusOK {
			t.Fatalf("flush: status %d", rec.Code)
		}
		testutil.Eventually(t, "the sink has the flush", func() bool { return rt.sink.has(id) })
	}
	return rt.sink.got()
}

// TestNewReceiverRefusesAnIncompleteConfig: a receiver with no connection, tenant or app would take any tenant's activities, or no
// token at all, so it is not made.
func TestNewReceiverRefusesAnIncompleteConfig(t *testing.T) {
	full := ReceiverConfig{Connection: testConn, TenantID: testTenant, AppID: testAppID}
	for name, edit := range map[string]func(c *ReceiverConfig){
		"no connection": func(c *ReceiverConfig) { c.Connection = "" },
		"no tenant":     func(c *ReceiverConfig) { c.TenantID = "" },
		"no app":        func(c *ReceiverConfig) { c.AppID = "" },
	} {
		cfg := full
		edit(&cfg)
		if r, err := NewReceiver(cfg); err == nil || r != nil {
			t.Errorf("%s: made a receiver (%v)", name, err)
		}
	}
	r, err := NewReceiver(full)
	if err != nil {
		t.Fatalf("a full config: %v", err)
	}
	if r.verifier.appID != testAppID {
		t.Errorf("the verifier's audience is %q, want the receiver's app", r.verifier.appID)
	}
}

// TestReceiverHandsAMessageToTheSink: a verified post of the connection's tenant is answered 200 and given to the sink, and its
// channel is recorded.
func TestReceiverHandsAMessageToTheSink(t *testing.T) {
	rt := newReceiverTest(t, true, nil)
	if rec := rt.post(t, fixture(t, "message_mention"), ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	testutil.Eventually(t, "the sink has the message", func() bool { return len(rt.sink.got()) == 1 })
	m := rt.sink.got()[0]
	if m.At.ID != "1794965322719" || m.At.Thread != "" || !m.Mentioned || m.User != testUser {
		t.Errorf("the sink got %+v", m)
	}
	c, ok, _ := rt.convs.Lookup(context.Background(), testConn, testChannel)
	if !ok || c.ServiceURL != testService || c.AADGroupID != testGroup {
		t.Errorf("the conversation record is %+v, %v", c, ok)
	}
}

// TestReceiverWarmsTheKeysWhenServing: Serve fetches the signing keys before any request comes.
func TestReceiverWarmsTheKeysWhenServing(t *testing.T) {
	rt := newReceiverTest(t, true, nil)
	testutil.Eventually(t, "the keys are fetched", func() bool { return rt.f.fetches() == 1 })
	if rec := rt.post(t, fixture(t, "message_mention"), ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if got := rt.f.fetches(); got != 1 {
		t.Errorf("fetches = %d, want 1", got)
	}
}

// TestReceiverRefreshesIdleKeys: a serving receiver fetches the keys again once they pass keysRefreshAfter, with no request to start it.
func TestReceiverRefreshesIdleKeys(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	rt.r.keysCheckEvery = time.Millisecond
	rt.serve(t)
	testutil.Eventually(t, "the keys are fetched", func() bool {
		rt.r.verifier.mu.Lock()
		defer rt.r.verifier.mu.Unlock()
		return rt.r.verifier.keys != nil
	})
	rt.clock.advance(keysRefreshAfter)
	testutil.Eventually(t, "the idle keys are fetched again", func() bool { return rt.f.fetches() == 2 })
}

// TestReceiverRefusesAnUnverifiedRequest: a request whose token fails is answered 401, and nothing of it is handed on or recorded.
func TestReceiverRefusesAnUnverifiedRequest(t *testing.T) {
	rt := newReceiverTest(t, true, nil)
	other := newSigningKey(t, "k", "msteams")
	mismatch := claimsAt(rt.clock.now())
	mismatch["serviceurl"] = "https://smba.trafficmanager.net/emea/"
	for name, authz := range map[string]string{
		"no token":          "none",
		"an impostor's key": bearer(t, other, claimsAt(rt.clock.now())),
		"a service URL that is not the activity's": bearer(t, rt.key, mismatch),
	} {
		if rec := rt.post(t, fixture(t, "message_mention"), authz); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
	}
	if got := rt.sink.got(); len(got) != 0 {
		t.Errorf("the sink got %d messages", len(got))
	}
	if got := rt.convs.given(); len(got) != 0 {
		t.Errorf("an unverified request was recorded: %+v", got)
	}
}

// TestReceiverRefusesAnotherTenant: a verified activity of a tenant that is not the connection's is answered 403 and not worked on: no
// message, no record, no invoke.
func TestReceiverRefusesAnotherTenant(t *testing.T) {
	h := &invokeRecorder{}
	rt := newReceiverTest(t, true, h)
	other := func(m map[string]any) {
		m["channelData"].(map[string]any)["tenant"] = map[string]any{"id": "00000000-0000-4000-8000-0000000000aa"}
	}
	none := func(m map[string]any) { delete(m["channelData"].(map[string]any), "tenant") }
	for _, name := range []string{"message_mention", "invoke_action", "installation_update"} {
		for label, edit := range map[string]func(map[string]any){"another tenant": other, "no tenant": none} {
			if rec := rt.post(t, editFixture(t, name, edit), ""); rec.Code != http.StatusForbidden {
				t.Errorf("%s, %s: status %d, want 403", name, label, rec.Code)
			}
		}
	}
	if got := rt.flush(t, testChannel); len(got) != 0 {
		t.Errorf("the sink got %d messages", len(got))
	}
	if n := h.count(); n != 0 {
		t.Errorf("the invoke handler was called %d times", n)
	}
	for _, c := range rt.convs.given() {
		if c.TenantID != testTenant {
			t.Errorf("another tenant's activity was recorded: %+v", c)
		}
	}
}

// TestReceiverDropsADuplicate: an activity that comes again within the dedup window is answered 200 and not handed on again; after
// it, it is a new one.
func TestReceiverDropsADuplicate(t *testing.T) {
	h := &invokeRecorder{}
	rt := newReceiverTest(t, true, h)
	for range 3 {
		if rec := rt.post(t, fixture(t, "message_plain"), ""); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if rec := rt.post(t, fixture(t, "invoke_action"), ""); rec.Code != http.StatusOK {
			t.Fatalf("invoke status %d", rec.Code)
		}
	}
	if got := len(rt.flush(t, testChannel)); got != 1 {
		t.Errorf("the sink got %d messages, want 1", got)
	}
	if n := h.count(); n != 1 {
		t.Errorf("the invoke handler was called %d times, want 1", n)
	}
	// The same message ID in another channel is another message.
	const otherChannel = "19:other@thread.tacv2"
	rt.post(t, inChannel(t, "message_plain", otherChannel, "1794965057510"), "")
	if got := len(rt.flush(t, otherChannel)); got != 2 {
		t.Errorf("with the other channel's, the sink got %d messages, want 2", got)
	}
	rt.clock.advance(2 * dedupWindow)
	rt.post(t, fixture(t, "message_plain"), "")
	if got := len(rt.flush(t, testChannel)); got != 3 {
		t.Errorf("past the window, the sink got %d messages, want 3", got)
	}
}

// TestReceiverAnswersBeforeTheSinkTakesIt: the answer does not wait on the sink, since Teams waits about 25 seconds for a message
// activity, does not send it again, and the chat listener allows its handler 3 seconds.
func TestReceiverAnswersBeforeTheSinkTakesIt(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	rt.sink.block = make(chan struct{})
	rt.serve(t)
	start := time.Now()
	rec := rt.post(t, fixture(t, "message_mention"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the answer took %s with the sink blocked", took)
	}
}

// TestReceiverRunsChannelsApart: a channel whose sink is held does not hold up another channel's posts.
func TestReceiverRunsChannelsApart(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	rt.sink.block, rt.sink.blockChannel = make(chan struct{}), testChannel
	rt.serve(t)
	if rec := rt.post(t, fixture(t, "message_mention"), ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	const quick = "19:quick@thread.tacv2"
	rt.post(t, inChannel(t, "message_plain", quick, "1794965000001"), "")
	testutil.Eventually(t, "the quick channel's post reaches the sink", func() bool { return rt.sink.has("1794965000001") })
	if rt.sink.has("1794965322719") {
		t.Error("the held channel's post reached the sink")
	}
}

// TestReceiverRefusesWhileNotServing: the route answers 503 before Serve runs and after it ends, and claims nothing for dedup, so a
// later delivery is taken.
func TestReceiverRefusesWhileNotServing(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	if rec := rt.post(t, fixture(t, "message_mention"), ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before Serve: status %d, want 503", rec.Code)
	}
	stop := rt.serve(t)
	if rec := rt.post(t, fixture(t, "message_mention"), ""); rec.Code != http.StatusOK {
		t.Fatalf("while serving: status %d, want 200", rec.Code)
	}
	testutil.Eventually(t, "the sink has the message", func() bool { return len(rt.sink.got()) == 1 })
	_ = stop()
	if rec := rt.post(t, fixture(t, "message_plain"), ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("after Serve: status %d, want 503", rec.Code)
	}
}

// TestReceiverDrainsWhatItAnsweredAsItStops: activities answered 200 and still on their lanes when Serve's context ends are handed on,
// within stopDrain, before Serve returns.
func TestReceiverDrainsWhatItAnsweredAsItStops(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	rt.sink.block = make(chan struct{})
	stop := rt.serve(t)
	ids := []string{"1794965000001", "1794965000002", "1794965000003"}
	for _, id := range ids {
		if rec := rt.post(t, inChannel(t, "message_plain", testChannel, id), ""); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	go func() {
		time.Sleep(stopDrain / 4)
		close(rt.sink.block)
	}()
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve returned %v", err)
	}
	for _, id := range ids {
		if !rt.sink.has(id) {
			t.Errorf("%s was answered 200 and not handed on", id)
		}
	}
}

// TestReceiverStopsDrainingAtItsBound: a sink that is still busy at stopDrain sees its context end, what waits behind it is dropped,
// and Serve returns only once the lanes have stopped, so that nothing calls the sink or the store after it, even a sink that takes a
// while to return.
func TestReceiverStopsDrainingAtItsBound(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	rt.sink.block, rt.sink.unwind = make(chan struct{}), 100*time.Millisecond
	stop := rt.serve(t)
	for _, id := range []string{"1794965000001", "1794965000002"} {
		if rec := rt.post(t, inChannel(t, "message_plain", testChannel, id), ""); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	s := rt.r.live.Load()
	start := time.Now()
	_ = stop()
	if took := time.Since(start); took < stopDrain+rt.sink.unwind || took > stopDrain+2*time.Second {
		t.Errorf("Serve returned %s after its context ended, want about %s and the sink's unwind", took, stopDrain)
	}
	if n := s.lanes.Held(); n != 0 {
		t.Errorf("Serve returned with %d activities still on the lanes", n)
	}
	if got := rt.sink.got(); len(got) != 0 {
		t.Errorf("the sink took %+v past the drain", got)
	}
	if n := rt.sink.callsOf("1794965000002"); n != 0 {
		t.Errorf("the sink was called %d times, after the drain, with a message that was still waiting", n)
	}
}

// TestReceiverRetriesASinkThatFails: a post the sink refuses is given to it again on its lane, and the channel's later posts come
// after it.
func TestReceiverRetriesASinkThatFails(t *testing.T) {
	rt := newReceiverTest(t, true, nil)
	rt.sink.fail("1794965000001", 1)
	for _, id := range []string{"1794965000001", "1794965000002"} {
		if rec := rt.post(t, inChannel(t, "message_plain", testChannel, id), ""); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	got := rt.flush(t, testChannel)
	if len(got) != 2 || got[0].At.ID != "1794965000001" || got[1].At.ID != "1794965000002" {
		t.Fatalf("the sink got %+v, want the two posts in order", got)
	}
	if n := rt.sink.callsOf("1794965000001"); n != 2 {
		t.Errorf("the sink was called %d times with the post it refused once, want 2", n)
	}
}

// TestReceiverReleasesAPostTheSinkNeverTakes: a post the sink refuses sinkAttempts times is dropped, and its dedup key released, so
// that the same activity is taken when it comes again.
func TestReceiverReleasesAPostTheSinkNeverTakes(t *testing.T) {
	rt := newReceiverTest(t, true, nil)
	const id = "1794965000001"
	rt.sink.fail(id, failAlways)
	body := inChannel(t, "message_plain", testChannel, id)
	if rec := rt.post(t, body, ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rt.flush(t, testChannel); len(got) != 0 {
		t.Fatalf("the sink took %+v", got)
	}
	if n := rt.sink.callsOf(id); n != sinkAttempts {
		t.Errorf("the sink was called %d times, want sinkAttempts (%d)", n, sinkAttempts)
	}
	rt.sink.fail(id, 0)
	if rec := rt.post(t, body, ""); rec.Code != http.StatusOK {
		t.Fatalf("again: status %d", rec.Code)
	}
	if got := rt.flush(t, testChannel); len(got) != 1 || got[0].At.ID != id {
		t.Errorf("the same activity again: the sink got %+v", got)
	}
}

// TestReceiverDoublesTheRetryWait: the wait before each further try of a post the sink refuses is twice the one before.
func TestReceiverDoublesTheRetryWait(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	const base = 40 * time.Millisecond
	rt.r.sinkRetryBase = base
	rt.serve(t)
	const id = "1794965000001"
	rt.sink.fail(id, sinkAttempts-1)
	if rec := rt.post(t, inChannel(t, "message_plain", testChannel, id), ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rt.flush(t, testChannel); len(got) != 1 || got[0].At.ID != id {
		t.Fatalf("the sink got %+v, want the post on its last try", got)
	}
	times := rt.sink.timesOf(id)
	if len(times) != sinkAttempts {
		t.Fatalf("the sink was called %d times, want sinkAttempts (%d)", len(times), sinkAttempts)
	}
	for i := 1; i < len(times); i++ {
		if gap, want := times[i].Sub(times[i-1]), base<<(i-1); gap < want {
			t.Errorf("try %d came %s after the one before, want at least %s", i+1, gap, want)
		}
	}
}

// TestReceiverCutsARetryWaitAtTheDrain: a post waiting to be tried again when the drain ends is dropped at once, so that Serve returns
// at stopDrain and not at the end of the wait.
func TestReceiverCutsARetryWaitAtTheDrain(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	rt.r.sinkRetryBase = sinkRetryMax
	stop := rt.serve(t)
	const id = "1794965000001"
	rt.sink.fail(id, failAlways)
	if rec := rt.post(t, inChannel(t, "message_plain", testChannel, id), ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	testutil.Eventually(t, "the sink refuses the post once", func() bool { return rt.sink.callsOf(id) == 1 })
	start := time.Now()
	_ = stop()
	if took := time.Since(start); took > stopDrain+sinkRetryMax/4 {
		t.Errorf("Serve returned %s after its context ended, want about %s: the retry wait of %s was not cut", took, stopDrain, sinkRetryMax)
	}
	if n := rt.sink.callsOf(id); n != 1 {
		t.Errorf("the sink was called %d times, want once: a wait cut by the drain is no try", n)
	}
}

// TestReceiverReleasesWhatTheDrainDrops: a post still waiting when the drain ends is dropped with its dedup key released, so that the
// same activity is taken if it comes again; an invoke dropped so keeps its key, since the InvokeHandler has answered it.
func TestReceiverReleasesWhatTheDrainDrops(t *testing.T) {
	h := &invokeRecorder{answer: InvokeAnswer{Status: http.StatusOK}}
	rt := newReceiverTest(t, false, h)
	rt.sink.block = make(chan struct{})
	stop := rt.serve(t)
	const held, waiting = "1794965000001", "1794965000002"
	for _, id := range []string{held, waiting} {
		if rec := rt.post(t, inChannel(t, "message_plain", testChannel, id), ""); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if rec := rt.post(t, fixture(t, "invoke_action"), ""); rec.Code != http.StatusOK || h.count() != 1 {
		t.Fatalf("the invoke: status %d and %d calls of the handler, want 200 and 1", rec.Code, h.count())
	}
	_ = stop()
	if rt.sink.callsOf(waiting) != 0 {
		t.Fatalf("the waiting post was handed on past the drain")
	}

	rt.sink.block = nil
	rt.serve(t)
	if rec := rt.post(t, inChannel(t, "message_plain", testChannel, waiting), ""); rec.Code != http.StatusOK {
		t.Fatalf("the dropped post again: status %d", rec.Code)
	}
	if rec := rt.post(t, fixture(t, "invoke_action"), ""); rec.Code != http.StatusOK {
		t.Fatalf("the invoke again: status %d", rec.Code)
	}
	if got := rt.flush(t, testChannel); len(got) != 1 || got[0].At.ID != waiting {
		t.Errorf("after the drain dropped it, the same post again: the sink got %+v", got)
	}
	if n := h.count(); n != 1 {
		t.Errorf("the handler answered the same invoke %d times, want once", n)
	}
}

// TestReceiverRefusesWhatIsNoActivity: a body that is no activity, a message whose ID holds ':', and an ID past maxActivityID are
// answered 400.
func TestReceiverRefusesWhatIsNoActivity(t *testing.T) {
	rt := newReceiverTest(t, true, nil)
	for name, body := range map[string][]byte{
		"not JSON":                       []byte("{"),
		"another channel":                editFixture(t, "message_mention", func(m map[string]any) { m["channelId"] = "webchat" }),
		"an ID with ':'":                 editFixture(t, "message_mention", func(m map[string]any) { m["id"] = "1:2" }),
		"an invoke ID long":              editFixture(t, "invoke_action", func(m map[string]any) { m["id"] = "f:" + strings.Repeat("a", maxActivityID) }),
		"an invoke's replyToId with ':'": editFixture(t, "invoke_action", func(m map[string]any) { m["replyToId"] = "1:2" }),
	} {
		if rec := rt.post(t, body, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
}

// TestReceiverAcknowledgesWhatIsNoPost: an edit, a deletion, an install event and a post that is no person's are answered 200 and give
// the sink nothing; the install events are recorded.
func TestReceiverAcknowledgesWhatIsNoPost(t *testing.T) {
	rt := newReceiverTest(t, true, nil)
	own := editFixture(t, "message_mention", func(m map[string]any) { m["from"] = map[string]any{"id": "28:" + testAppID} })
	for name, body := range map[string][]byte{
		"edit": fixture(t, "message_update"), "delete": fixture(t, "message_delete"), "own post": own,
		"conversationUpdate": fixture(t, "conversation_update"), "installationUpdate": fixture(t, "installation_update"),
	} {
		if rec := rt.post(t, body, ""); rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", name, rec.Code)
		}
	}
	if got := rt.flush(t, testChannel); len(got) != 0 {
		t.Errorf("the sink got %+v", got)
	}
	if c, ok, _ := rt.convs.Lookup(context.Background(), testConn, testChannel); !ok || c.AADGroupID != testGroup {
		t.Errorf("the install event is not recorded: %+v", c)
	}
}

// TestReceiverWritesARecordOnlyWhenItAddsSomething: the store is written for a channel's first record, and not again for a record that
// adds nothing to it (the same activity's fields, or an invoke's, which lack the group).
func TestReceiverWritesARecordOnlyWhenItAddsSomething(t *testing.T) {
	rt := newReceiverTest(t, true, nil)
	rt.post(t, fixture(t, "message_mention"), "")
	rt.post(t, fixture(t, "message_plain"), "")
	rt.post(t, fixture(t, "invoke_action"), "")
	rt.post(t, fixture(t, "message_reply"), "")
	rt.flush(t, testChannel)
	if got := rt.convs.given(); len(got) != 1 {
		t.Errorf("the store was written %d times, want once: %+v", len(got), got)
	}
}

// TestReceiverAnswersAnInvokeThroughItsSeam: an invoke goes to the InvokeHandler in the request, with the sink of the live Serve, and its
// answer is the response; with no handler, the invoke is answered 200 with no body. Neither reaches the sink as a message.
func TestReceiverAnswersAnInvokeThroughItsSeam(t *testing.T) {
	h := &invokeRecorder{answer: InvokeAnswer{Status: http.StatusOK, Body: []byte(`{"statusCode":200}`)}}
	rt := newReceiverTest(t, true, h)
	rec := rt.post(t, fixture(t, "invoke_action"), "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"statusCode":200}` || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("answer %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	if h.count() != 1 || h.invokes[0].Name != "adaptiveCard/action" || h.invokes[0].User != testUser {
		t.Errorf("the handler got %+v", h.invokes)
	}
	if h.count() == 1 && h.sinks[0] != chat.Sink(rt.sink) {
		t.Errorf("the handler was given the sink %v, want the live Serve's", h.sinks[0])
	}

	bare := newReceiverTest(t, true, nil)
	rec = bare.post(t, fixture(t, "invoke_action"), "")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("with no handler: %d %q, want 200 and no body", rec.Code, rec.Body.String())
	}
	if len(rt.flush(t, testChannel))+len(bare.flush(t, testChannel)) != 0 {
		t.Error("an invoke reached the sink as a message")
	}
}

// fillLane posts messages in channel, held by the sink, until its lane is full, and returns the next ID.
func (rt *receiverTest) fillLane(t *testing.T, channel string, first int) int {
	t.Helper()
	for i := range lanes.PerChannel {
		if rec := rt.post(t, inChannel(t, "message_plain", channel, fmt.Sprint(first+i)), ""); rec.Code != http.StatusOK {
			t.Fatalf("message %d of %s: status %d", i, channel, rec.Code)
		}
	}
	return first + lanes.PerChannel
}

// TestReceiverShedsPastAChannelsLane: a channel's lane takes lanes.PerChannel activities while its sink is held, and the next is answered
// 503 and not remembered as taken, so that the same activity is taken when it comes again.
func TestReceiverShedsPastAChannelsLane(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	rt.sink.block = make(chan struct{})
	rt.serve(t)
	next := rt.fillLane(t, testChannel, 1794960000000)
	shedBody := inChannel(t, "message_plain", testChannel, fmt.Sprint(next))
	if rec := rt.post(t, shedBody, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("past the lane: status %d, want 503", rec.Code)
	}
	close(rt.sink.block)
	testutil.Eventually(t, "the sink takes what the lane held", func() bool { return len(rt.sink.got()) == lanes.PerChannel })
	if rec := rt.post(t, shedBody, ""); rec.Code != http.StatusOK {
		t.Fatalf("the shed message again: status %d, want 200", rec.Code)
	}
	testutil.Eventually(t, "the sink takes the shed message", func() bool { return rt.sink.has(fmt.Sprint(next)) })
}

// TestReceiverShedsPastAllLanes: once the lanes hold lanes.Total, a channel that has one held is refused another, and a channel
// with none still takes its first.
func TestReceiverShedsPastAllLanes(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	rt.sink.block = make(chan struct{})
	rt.serve(t)
	id := 1794960000000
	for c := range lanes.Total / lanes.PerChannel {
		id = rt.fillLane(t, fmt.Sprintf("19:busy%d@thread.tacv2", c), id)
	}
	const fresh = "19:fresh@thread.tacv2"
	if rec := rt.post(t, inChannel(t, "message_plain", fresh, fmt.Sprint(id)), ""); rec.Code != http.StatusOK {
		t.Fatalf("a channel's first past the total: status %d, want 200", rec.Code)
	}
	if rec := rt.post(t, inChannel(t, "message_plain", fresh, fmt.Sprint(id+1)), ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a channel's second past the total: status %d, want 503", rec.Code)
	}
}

// TestReceiverAnswersAnInvokeOnAFullLane: an invoke whose channel's lane is full still gets the handler's answer: only its record is
// shed.
func TestReceiverAnswersAnInvokeOnAFullLane(t *testing.T) {
	h := &invokeRecorder{answer: InvokeAnswer{Status: http.StatusOK, Body: []byte(`{"statusCode":200}`)}}
	rt := newReceiverTest(t, false, h)
	rt.sink.block = make(chan struct{})
	rt.serve(t)
	rt.fillLane(t, testChannel, 1794960000000)
	rec := rt.post(t, fixture(t, "invoke_action"), "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"statusCode":200}` {
		t.Errorf("answer %d %q, want the handler's", rec.Code, rec.Body.String())
	}
	if h.count() != 1 {
		t.Errorf("the handler was called %d times, want 1", h.count())
	}
}

// warnLines is a log handler that keeps the WARN lines' messages and attributes.
type warnLines struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (w *warnLines) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (w *warnLines) WithAttrs([]slog.Attr) slog.Handler           { return w }
func (w *warnLines) WithGroup(string) slog.Handler                { return w }
func (w *warnLines) Handle(_ context.Context, r slog.Record) error {
	line := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		line[a.Key] = a.Value.Any()
		return true
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, line)
	return nil
}

// withReason is the lines that name reason.
func (w *warnLines) withReason(reason Reason) []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []map[string]any
	for _, l := range w.lines {
		if l["reason"] == string(reason) {
			out = append(out, l)
		}
	}
	return out
}

// TestReceiverWarnsOfASignedTokenItRefuses: a token the Bot Framework signed that names another app, or is out of the clock's skew, is
// logged at WARN, one line for each reason per refusalLogEvery, which counts those it did not log; a token anyone could forge is not.
func TestReceiverWarnsOfASignedTokenItRefuses(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	logs := &warnLines{}
	rt.r.log = slog.New(logs)
	rt.serve(t)
	otherApp := claimsAt(rt.clock.now())
	otherApp["aud"] = "00000000-0000-4000-8000-00000000000a"
	expired := claimsAt(rt.clock.now())
	expired["exp"] = rt.clock.now().Add(-time.Hour).Unix()
	impostor := newSigningKey(t, "k", "msteams")
	for range 3 {
		for _, authz := range []string{bearer(t, rt.key, otherApp), bearer(t, rt.key, expired), bearer(t, impostor, claimsAt(rt.clock.now()))} {
			if rec := rt.post(t, fixture(t, "message_mention"), authz); rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", rec.Code)
			}
		}
	}
	if n, m := len(logs.withReason(ReasonAudience)), len(logs.withReason(ReasonExpired)); n != 1 || m != 1 {
		t.Fatalf("WARN lines: %d for audience and %d for expired, want 1 each", n, m)
	}
	if n := len(logs.withReason(ReasonSignature)); n != 0 {
		t.Errorf("a forged token was logged at WARN %d times", n)
	}
	rt.clock.advance(refusalLogEvery)
	rt.post(t, fixture(t, "message_mention"), bearer(t, rt.key, otherApp))
	lines := logs.withReason(ReasonAudience)
	if len(lines) != 2 || lines[1]["alsoRefusedSinceLastLine"] != int64(2) {
		t.Errorf("after refusalLogEvery: %+v, want a second line that counts the 2 not logged", lines)
	}
}

// TestRefusalLogCountsSinceTheLastLine: a reason gets a line at once and then one each refusalLogEvery, which counts the refusals since
// the line before it, and not those before that; each reason is paced on its own.
func TestRefusalLogCountsSinceTheLastLine(t *testing.T) {
	l := refusalLog{last: map[Reason]time.Time{}, suppressed: map[Reason]int{}}
	t0 := time.Unix(1_800_000_000, 0)
	for i, step := range []struct {
		reason Reason
		at     time.Duration
		line   bool
		count  int
	}{
		{ReasonAudience, 0, true, 0},
		{ReasonAudience, time.Second, false, 0},
		{ReasonExpired, time.Second, true, 0},
		{ReasonAudience, 2 * time.Second, false, 0},
		{ReasonAudience, refusalLogEvery, true, 2},
		{ReasonAudience, refusalLogEvery + time.Second, false, 0},
		{ReasonAudience, 2 * refusalLogEvery, true, 1},
		{ReasonAudience, 3 * refusalLogEvery, true, 0},
		{ReasonExpired, 3 * refusalLogEvery, true, 0},
	} {
		if line, n := l.admit(step.reason, t0.Add(step.at)); line != step.line || n != step.count {
			t.Errorf("step %d (%s at %s): admit gives (%t, %d), want (%t, %d)", i, step.reason, step.at, line, n, step.line, step.count)
		}
	}
}

// failingStore is a ConversationStore whose Remember always fails.
type failingStore struct{ *MemoryConversations }

func (failingStore) Remember(context.Context, string, Conversation) error {
	return errors.New("the store is down")
}

// TestReceiverClipsTheChannelItLogs: the channel of an activity, which the request chose, is cut to quoteMax bytes in the WARN lines
// of a post the sink never takes and of a record the store refuses.
func TestReceiverClipsTheChannelItLogs(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	logs := &warnLines{}
	rt.r.log, rt.r.convs = slog.New(logs), failingStore{NewMemoryConversations()}
	rt.serve(t)
	channel := "19:" + strings.Repeat("x", 4096) + "@thread.tacv2"
	const id = "1794965000001"
	rt.sink.fail(id, failAlways)
	if rec := rt.post(t, inChannel(t, "message_plain", channel, id), ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	testutil.Eventually(t, "the sink gives up on the post", func() bool { return rt.sink.callsOf(id) == sinkAttempts })
	rt.flush(t, channel)
	logs.mu.Lock()
	defer logs.mu.Unlock()
	seen := 0
	for _, l := range logs.lines {
		c, ok := l["channel"].(string)
		if !ok {
			continue
		}
		seen++
		if len(c) > quoteMax+len("...") {
			t.Errorf("%q quotes %d bytes of the channel", l["msg"], len(c))
		}
	}
	if seen < 2 {
		t.Errorf("%d WARN lines name the channel, want the drop's and the store's: %v", seen, logs.lines)
	}
}

// TestReceiverAnswers503WhenTheKeysCannotBeRead: a token that cannot be checked because the Bot Framework's keys cannot be fetched is
// answered 503, which is no fault of the request, and not 401.
func TestReceiverAnswers503WhenTheKeysCannotBeRead(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	rt.f.setStatus(http.StatusInternalServerError)
	rt.serve(t)
	if rec := rt.post(t, fixture(t, "message_mention"), ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", rec.Code)
	}
}

// TestServingRefusesWorkOnceClosed: a request that found the Serve live, and submits after it began to stop, is not put on a lane that
// Serve no longer waits for.
func TestServingRefusesWorkOnceClosed(t *testing.T) {
	s := &serving{lanes: lanes.New(), closed: true}
	ran := false
	if _, open := s.submit(testChannel, func() { ran = true }); open {
		t.Error("a closed Serve took work")
	}
	s.lanes.Wait()
	if ran {
		t.Error("a closed Serve ran work")
	}
}

// TestReceiverDedupsByTypeToo: an edit that comes before its message under the same ID does not make the message a duplicate.
func TestReceiverDedupsByTypeToo(t *testing.T) {
	rt := newReceiverTest(t, true, nil)
	const id = "1794965057510"
	update := editFixture(t, "message_update", func(m map[string]any) { m["id"] = id })
	if rec := rt.post(t, update, ""); rec.Code != http.StatusOK {
		t.Fatalf("update: status %d", rec.Code)
	}
	if rec := rt.post(t, inChannel(t, "message_plain", testChannel, id), ""); rec.Code != http.StatusOK {
		t.Fatalf("message: status %d", rec.Code)
	}
	if got := rt.flush(t, testChannel); len(got) != 1 || got[0].At.ID != id {
		t.Errorf("the sink got %+v, want the message", got)
	}
}

// refusingStore is a ConversationStore that refuses every record, as the broker's store refuses the write of a replica that is not the
// leader, and counts what it was asked to record.
type refusingStore struct {
	*MemoryConversations
	mu    sync.Mutex
	asked int
}

func (s *refusingStore) Remember(context.Context, string, Conversation) error {
	s.mu.Lock()
	s.asked++
	s.mu.Unlock()
	return errors.New("store: deposed")
}

func (s *refusingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

// TestReceiverHandsOnAPostWhoseRecordTheStoreRefuses: a record the store refuses stops nothing. The post is still given to the sink, and the
// record, which the receiver does not take as written, is asked of the store again with the channel's next activity.
func TestReceiverHandsOnAPostWhoseRecordTheStoreRefuses(t *testing.T) {
	rt := newReceiverTest(t, false, nil)
	refusing := &refusingStore{MemoryConversations: NewMemoryConversations()}
	rt.r.convs = refusing
	rt.serve(t)
	for _, id := range []string{"1794965000101", "1794965000102"} {
		if rec := rt.post(t, inChannel(t, "message_plain", testChannel, id), ""); rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", rec.Code)
		}
		testutil.Eventually(t, "the sink has the post", func() bool { return rt.sink.has(id) })
	}
	if got := refusing.count(); got != 2 {
		t.Errorf("the store was asked for the record %d times, want twice: once for each activity, since the first was refused", got)
	}
}

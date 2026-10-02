package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// nextSent takes the messageId of the next Send, or fails the test if none comes.
func (f *fixture) nextSent() string {
	f.t.Helper()
	select {
	case id := <-f.agent.sent:
		return id
	case <-time.After(5 * time.Second):
		f.t.Fatal("no Send came")
		return ""
	}
}

// A reply that has to wait for the active task is marked (spec 8.2): Submit says it is queued
// and the sink is told, with the message to put the reaction on. The first turn, and a
// redelivery of it, are not queued behind anything, and each turn keeps its own messageId.
func TestReplyBehindAnActiveTaskIsQueued(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	mid, gate := make(chan struct{}), make(chan struct{})
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		if n == 0 {
			mid <- struct{}{}
			<-gate
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	first, err := f.submit(inbound("1"))
	if err != nil || first.Queued {
		t.Fatalf("first turn = %+v, %v; want it not queued", first, err)
	}
	<-mid                                        // the task runs
	for _, ts := range []string{"2", "3", "3"} { // the last is a redelivery
		if res, err := f.submit(inbound(ts)); err != nil || !res.Queued {
			t.Fatalf("reply %s = %+v, %v; want it queued", ts, res, err)
		}
	}
	sink.mu.Lock()
	var stamps []string
	for _, n := range sink.notices {
		if n.Kind != NoticeQueued || n.ID != inbound(n.MessageRef).MessageID+"/queued" {
			t.Errorf("notice %+v; want a queued notice named after its turn", n)
		}
		stamps = append(stamps, n.MessageRef)
	}
	sink.mu.Unlock()
	if !slices.Equal(stamps, []string{"2", "3", "3"}) {
		t.Errorf("notices for %v; want the replies 2 and 3, and 3 again for its redelivery", stamps)
	}
	close(gate)
	for _, ts := range []string{"1", "2", "3"} { // one per turn, in order, each with its own messageId
		if id, want := f.nextSent(), inbound(ts).MessageID; id != want {
			t.Errorf("sent %s; want %s", id, want)
		}
	}
}

// While a task waits for input, replies are held: neither sent nor taken as its answer. When
// the pause ends, they go out (spec 8.2, D21 as amended).
func TestRepliesAreHeldWhileAPauseLasts(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 0 {
			return turn(h, n, a2a.TaskStateInputRequired)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	f.nextSent()
	res, err := f.submit(inbound("2"))
	if err != nil || !res.Queued {
		t.Fatalf("reply = %+v, %v; want it queued behind the pause", res, err)
	}
	select {
	case id := <-f.agent.sent:
		t.Fatalf("%s was sent during the pause", id)
	case <-time.After(100 * time.Millisecond):
	}
	if err := f.m.endPause(t.Context(), key, "t1"); err != nil {
		t.Fatal(err)
	}
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the pause; want %s", id, want)
	}
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
}

// -32004 leaves the turn queued and resends it unchanged after a backoff, which doubles up to
// a cap. Before each resend, the turn's task is looked up, in case the agent recorded it after
// all (spec 8.2). Nothing is dropped.
func TestBusyKeepsTheTurnAndLooksItUpBeforeEachResend(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 1 || n == 2 {
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		<-sink.finals
	}
	want := inbound("2").MessageID
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	var ids []string
	for _, m := range f.agent.msgs[1:] {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{want, want, want}) {
		t.Errorf("turn 2 was sent as %v; want the same messageId three times", ids)
	}
	if lookups := []string{"c1 " + want, "c1 " + want}; !slices.Equal(f.agent.finds, lookups) {
		t.Errorf("lookups = %v; want one before each resend, none before the first send", f.agent.finds)
	}
}

func TestBackoffDoublesUpToTheCap(t *testing.T) {
	m := &Manager{cfg: Config{RetryBase: time.Second, RetryMax: time.Minute}}
	var got []time.Duration
	for tries := 1; tries <= 9; tries++ {
		got = append(got, m.backoff(tries))
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff by attempt = %v; want doubling from 1s to a cap of 60s", got)
		}
	}
}

// A lookup that finds the turn's task means the agent had recorded it: the runner follows
// that task and never sends the turn again. A lookup that fails means nothing is known, so
// nothing is sent: it backs off and asks again.
func TestLookupFindsTheTurnOrFailsWithoutSending(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 1 {
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	lookups := 0
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		if lookups++; lookups == 1 {
			return nil, nil, errors.New("agent unreachable")
		}
		return &a2a.Task{ID: "t9", ContextID: "c1"}, []a2a.TaskID{"t9"}, nil
	}
	f.agent.follow = func(id a2a.TaskID, h agent.Handler) error {
		h(agent.StateEvent{TaskID: id, ContextID: "c1", State: a2a.TaskStateWorking})
		h(agent.FinalEvent{Task: &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
		return nil
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		<-sink.finals
	}
	f.agent.mu.Lock()
	sends, finds, follows := len(f.agent.msgs), len(f.agent.finds), slices.Clone(f.agent.follows)
	f.agent.mu.Unlock()
	if sends != 2 || finds != 2 || !slices.Equal(follows, []a2a.TaskID{"t9"}) {
		t.Errorf("%d sends, %d lookups, followed %v; want 2 sends (the turn once, busy), 2 lookups (failed, found) and t9 followed", sends, finds, follows)
	}
	eventually(t, "the turn delivered and its task cleared", func() bool { return f.taskID() == "" })
	if _, err := f.st.NextReply(t.Context(), key); err == nil {
		t.Error("the followed turn is still queued")
	}
}

// endPause names the task whose pause ended. A call that arrives while the runner is not
// parked, that repeats a call already made, or that names another task is dropped: it must
// never release the next pause, or replies would go out while a task waits for its answer.
func TestEndPauseIgnoresStaleAndDuplicateCalls(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	mid, gate := make(chan struct{}), make(chan struct{})
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 0 {
			accepted(h, n)
			mid <- struct{}{}
			<-gate
			return turn(h, n, a2a.TaskStateInputRequired)
		}
		if n == 1 {
			return turn(h, n, a2a.TaskStateInputRequired)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	end := func(id a2a.TaskID, wantErr error) {
		t.Helper()
		if err := f.m.endPause(t.Context(), key, id); !errors.Is(err, wantErr) {
			t.Fatalf("endPause(%s) = %v; want %v", id, err, wantErr)
		}
	}
	silent := func(why string) {
		t.Helper()
		select {
		case id := <-f.agent.sent:
			t.Fatalf("%s was sent %s", id, why)
		case <-time.After(100 * time.Millisecond):
		}
	}
	for _, ts := range []string{"1", "2", "3"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	<-mid
	end("t1", ErrNotParked) // stale: turn 1 has not paused yet
	close(gate)
	<-sink.finals
	f.nextSent()
	silent("after a call made before the pause")
	end("t1", nil)
	end("t1", ErrNotParked) // a duplicate
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Fatalf("sent %s after the pause; want %s", id, want)
	}
	<-sink.finals           // turn 2 pauses on t2
	end("t1", ErrNotParked) // stale: t1's pause is over
	silent("on a call for the earlier pause")
	if got := f.taskID(); got != "t2" {
		t.Errorf("task = %q; want t2 still recorded", got)
	}
	end("t2", nil)
	if id, want := f.nextSent(), inbound("3").MessageID; id != want {
		t.Errorf("sent %s after the second pause; want %s", id, want)
	}
}

// After a restart there is no runner. A call for the recorded task clears it and starts one;
// a call for any other task does nothing.
func TestEndPauseWithNoRunnerClearsTheRecordedTask(t *testing.T) {
	f := newFixture(t)
	f.live()
	f.seed("ns/b1", 0) // task t1 in context c1
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the held runner to retire", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return len(f.m.convos) == 0 })
	if err := f.m.endPause(t.Context(), key, "t9"); !errors.Is(err, ErrNotParked) {
		t.Fatalf("endPause of another task = %v; want ErrNotParked", err)
	}
	select {
	case id := <-f.agent.sent:
		t.Fatalf("%s was sent on a call for another task", id)
	case <-time.After(100 * time.Millisecond):
	}
	if err := f.m.endPause(t.Context(), key, "t1"); err != nil {
		t.Fatal(err)
	}
	if id, want := f.nextSent(), inbound("1").MessageID; id != want {
		t.Errorf("sent %s; want the held turn %s", id, want)
	}
}

// Two -32004 in a row on the first turn do not slow the next turn: a turn the agent takes
// starts the backoff over, and a new head's first send is not a resend, so it has no lookup.
func TestRetryStateStartsOverAfterANamedTurn(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	var waits []time.Duration
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Second, time.Minute
	f.m.cfg.After = func(d time.Duration) <-chan time.Time { // no real wait; the delay asked for is recorded
		waits = append(waits, d) // the runner is the only caller, and the test reads it after the turns end
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n < 2 || n == 3 { // turn 1 twice, turn 2 once
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		<-sink.finals
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	// Turn 1 had no context, so its resends have no lookup; turn 2 had one, before its resend.
	if want := "[send send send send find send]"; fmt.Sprint(f.agent.seq) != want {
		t.Errorf("calls = %v; want %s", f.agent.seq, want)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, time.Second}; !slices.Equal(waits, want) {
		t.Errorf("waits = %v; want %v: turn 2's first wait is the base, not the third step turn 1's count would give", waits, want)
	}
}

// A turn sent once and not known to have been taken is looked up before any resend into its
// context, whatever failed the first time, and however the resend is triggered (spec 8.2).
func TestPreviouslySentTurnIsLookedUpBeforeAnyResend(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	waits := f.countWaits()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 1 {
			return errors.New("the stream ended in a way nothing classifies") // the runner stops, leaving the turn queued
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	f.retired(t)
	in := inbound("2")
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	f.nextSent()
	f.nextSent()
	f.retired(t) // nothing retries it by itself
	if sends, _, _ := f.agent.counts(); sends != 2 || waits.Load() != 0 {
		t.Fatalf("%d sends, %d retry waits; want turn 1 and turn 2 once, and no retry", sends, waits.Load())
	}
	if _, err := f.submit(in); err != nil { // the redelivered event
		t.Fatal(err)
	}
	<-sink.finals
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if want := "[send send find send]"; fmt.Sprint(f.agent.seq) != want {
		t.Errorf("calls = %v; want %s: the lookup comes before the resend", f.agent.seq, want)
	}
}

// The pause is rendered inside Send's handler, so a call to end it can come from the renderer
// before Send returns. It must be seen as a call for the pause the runner is about to park on:
// the conversation is not left parked for good.
func TestEndPauseFromInsideTheSinkReleasesThePause(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	var callErr error
	sink.onFinal = func(task *a2a.Task) {
		if task.Status.State == a2a.TaskStateInputRequired {
			callErr = f.m.endPause(context.Background(), key, task.ID)
		}
	}
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 0 {
			return turn(h, n, a2a.TaskStateInputRequired)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	f.nextSent()
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the pause; want %s", id, want)
	}
	<-sink.finals
	<-sink.finals
	if callErr != nil {
		t.Errorf("endPause from the handler = %v; want it accepted", callErr)
	}
}

// A conversation whose queue is empty keeps no resend record.
func TestAttemptedTurnIsForgottenWhenTheQueueEmpties(t *testing.T) {
	f := newFixture(t)
	f.live()
	f.agent.script = func(context.Context, int, agent.Handler) error { return errors.New("boom") } // stays queued
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	f.nextSent()
	eventually(t, "the runner to retire", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return len(f.m.convos) == 0 })
	f.m.mu.Lock()
	recorded := len(f.m.attempted)
	f.m.mu.Unlock()
	if recorded != 1 {
		t.Fatalf("%d attempted entries; want the queued turn's", recorded)
	}
	if err := f.st.MarkReplyDelivered(t.Context(), inbound("1").MessageID, t0); err != nil { // the queue empties
		t.Fatal(err)
	}
	f.m.start(key)
	eventually(t, "the entry to go", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return len(f.m.attempted) == 0 })
}

// After Close, no pause can be ended: the runner is gone, and a call must not look as if it
// released anything.
func TestEndPauseAfterCloseIsDropped(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error { return turn(h, n, a2a.TaskStateInputRequired) }
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	if err := f.m.endPause(t.Context(), key, "t1"); !errors.Is(err, ErrNotParked) {
		t.Errorf("endPause after Close = %v; want ErrNotParked", err)
	}
}

// A pause event is shown inside the stream, before the send has ended. If the stream then fails
// (here, is lost), the conversation was never parked, and a call to end that pause must be
// dropped: it must leave no stale state, and no buffered signal for a later pause to consume.
func TestPauseEventFollowedByALostStreamLeavesNoStalePause(t *testing.T) {
	f := newFixture(t)
	f.live()
	waits := f.countWaits()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		h(agent.FinalEvent{Task: &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}})
		return &agent.StreamLostError{TaskID: "t1", ContextID: "c1", Err: errors.New("connection reset")}
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the runner to wait to follow the lost stream", func() bool { return waits.Load() == 1 })
	if err := f.m.endPause(t.Context(), key, "t1"); !errors.Is(err, ErrNotParked) {
		t.Errorf("endPause = %v; want ErrNotParked: the runner is not parked", err)
	}
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if c := f.m.convos[key]; len(c.resume) != 0 || c.parked != "" {
		t.Errorf("parked %q, %d signals buffered; want none", c.parked, len(c.resume))
	}
}

// A pause that is ended from inside the renderer's handler is accepted, and its release is
// buffered for the runner to take. If the stream then fails, the park is wiped, and so is that
// buffered release: it must not wait to free a later pause.
func TestReleaseOfAPauseInAFailedStreamIsDrained(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	waits := f.countWaits()
	var callErr error
	sink.onFinal = func(task *a2a.Task) { callErr = f.m.endPause(context.Background(), key, task.ID) }
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		h(agent.FinalEvent{Task: &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}})
		return &agent.StreamLostError{TaskID: "t1", ContextID: "c1", Err: errors.New("connection reset")}
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the runner to wait to follow the lost stream", func() bool { return waits.Load() == 1 })
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if c := f.m.convos[key]; callErr != nil || len(c.resume) != 0 || c.parked != "" {
		t.Errorf("call = %v, %d signals buffered, parked %q; want it accepted, then nothing left over", callErr, len(c.resume), c.parked)
	}
}

// Ending a pause takes the runner a moment (it clears the recorded task), and in that moment a
// duplicate call must still be refused: one that found the runner neither parked nor driving, and
// the record still naming the task, would clear it itself and answer nil, which a caller reads as
// "I ended the pause".
func TestDuplicateEndPauseWhileThePauseIsBeingEnded(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	f.st.onSet = func() { // the first clearing of the task, by the runner
		if !first.Swap(true) {
			close(entered)
			<-release
		}
	}
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error { return turn(h, n, a2a.TaskStateInputRequired) }
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	eventually(t, "the runner to park", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return f.m.convos[key].parked == "t1" })
	if err := f.m.endPause(t.Context(), key, "t1"); err != nil {
		t.Fatal(err)
	}
	<-entered // the runner is ending the pause
	if err := f.m.endPause(t.Context(), key, "t1"); !errors.Is(err, ErrNotParked) {
		t.Errorf("duplicate endPause = %v; want ErrNotParked", err)
	}
	close(release)
}

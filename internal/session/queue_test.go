package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
		if n.Kind != NoticeQueued || n.ID != inbound(n.MessageTS).MessageID+"/queued" {
			t.Errorf("notice %+v; want a queued notice named after its turn", n)
		}
		stamps = append(stamps, n.MessageTS)
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
	end := func(id a2a.TaskID) {
		t.Helper()
		if err := f.m.endPause(t.Context(), key, id); err != nil {
			t.Fatal(err)
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
	end("t1") // stale: turn 1 has not paused yet
	close(gate)
	<-sink.finals
	f.nextSent()
	silent("after a call made before the pause")
	end("t1")
	end("t1") // the second is a duplicate
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Fatalf("sent %s after the pause; want %s", id, want)
	}
	<-sink.finals // turn 2 pauses on t2
	end("t1")     // stale: t1's pause is over
	silent("on a call for the earlier pause")
	if got := f.taskID(); got != "t2" {
		t.Errorf("task = %q; want t2 still recorded", got)
	}
	end("t2")
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
	if err := f.m.endPause(t.Context(), key, "t9"); err != nil {
		t.Fatal(err)
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
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = 50*time.Millisecond, time.Second
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
	if gap := f.agent.times[4].Sub(f.agent.times[3]); gap < 50*time.Millisecond || gap >= 150*time.Millisecond {
		t.Errorf("turn 2 waited %v after its first -32004; want the base 50ms, not the 200ms turn 1's count would give", gap)
	}
}

// A turn sent once and not known to have been taken is looked up before any resend into its
// context, whatever failed the first time, and however the resend is triggered (spec 8.2).
func TestPreviouslySentTurnIsLookedUpBeforeAnyResend(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 1 {
			return fmt.Errorf("%w: the stream ended", agent.ErrOutcomeUnknown)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	eventually(t, "the runner to retire", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return len(f.m.convos) == 0 })
	in := inbound("2")
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	<-f.agent.sent
	<-f.agent.sent
	select { // the runner stops on an outcome it cannot classify; nothing resends it by itself
	case id := <-f.agent.sent:
		t.Fatalf("%s was sent again with nothing to wake the runner", id)
	case <-time.After(100 * time.Millisecond):
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

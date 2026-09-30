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
	f.m.endPause(key)
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

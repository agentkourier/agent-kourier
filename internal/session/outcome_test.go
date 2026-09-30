package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
)

// notices returns the sink's notices of kind.
func (s *fakeSink) noticesOf(kind NoticeKind) (out []Notice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.notices {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// A send that never left (ErrNotSent) is retried, the same messageId, after a backoff, and
// the thread is told once that courier is retrying (spec 11).
func TestNotSentIsRetriedWithOneNotice(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n < 3 {
			return fmt.Errorf("%w: the agent is down", agent.ErrNotSent)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	in := inbound("1")
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.msgs) != 4 || len(f.agent.finds) != 0 {
		t.Fatalf("%d sends, %d lookups; want 4 sends of one turn and no lookup: nothing left, so nothing to find", len(f.agent.msgs), len(f.agent.finds))
	}
	for _, m := range f.agent.msgs {
		if m.ID != in.MessageID {
			t.Errorf("sent %s; want the same messageId %s each time", m.ID, in.MessageID)
		}
	}
	if got := sink.noticesOf(NoticeRetrying); len(got) != 1 || got[0].ID != in.MessageID+"/retrying" {
		t.Errorf("retrying notices = %+v; want one", got)
	}
}

// Past sendGiveUp the turn fails, with a notice, and the queue goes on.
func TestNotSentGivesUp(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 2 { // the agent has been down for 16 minutes
			f.now.Store(t0.Add(16 * time.Minute).UnixNano())
		}
		if n < 3 {
			return fmt.Errorf("%w: the agent is down", agent.ErrNotSent)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	<-sink.finals // the second turn, which the first one's failure did not stop
	failed := sink.noticesOf(NoticeFailed)
	if len(failed) != 1 || failed[0].ID != inbound("1").MessageID+"/failed" || failed[0].Detail == "" {
		t.Errorf("failed notices = %+v; want one for turn 1, with a reason", failed)
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if last := f.agent.msgs[len(f.agent.msgs)-1]; last.ID != inbound("2").MessageID || len(f.agent.msgs) != 4 {
		t.Errorf("%d sends, the last %s; want turn 1 three times, then turn 2 once", len(f.agent.msgs), last.ID)
	}
}

// A refused credential fails the turn at once, without a retry and without any other identity
// (D34, D14). The identity Source's Rejected is called, which writes the only audit row: the
// manager must not audit it again. The queue goes on.
func TestCredentialsRejectedFailsTheTurn(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 0 {
			return fmt.Errorf("%w: %w", agent.ErrNotSent, agent.ErrCredentialsRejected) // both, as Send returns it
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	<-sink.finals
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.rejected) != 1 || f.rejected[0].Op != audit.OpSend || f.rejected[0].MessageID != inbound("1").MessageID || !errors.Is(f.rejected[0].Err, agent.ErrCredentialsRejected) {
		t.Errorf("Rejected calls = %+v; want one, for the send of turn 1", f.rejected)
	}
	if len(f.audits) != 0 {
		t.Errorf("the manager audited %+v; Rejected writes the only row", f.audits)
	}
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].MessageTS != "1" {
		t.Errorf("failed notices = %+v; want one for turn 1", got)
	}
	if len(f.agent.msgs) != 2 {
		t.Errorf("%d sends; want the rejected turn once, then the next turn", len(f.agent.msgs))
	}
}

// A refused identity (-31403) also fails the turn without a retry, and is audited: nothing
// else records it.
func TestForbiddenFailsTheTurnAndIsAudited(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = func(context.Context, int, agent.Handler) error {
		return fmt.Errorf("send message: %w", agent.ErrForbidden)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	select {
	case <-f.agent.sent: // the one send
	case <-time.After(time.Second):
		t.Fatal("no send")
	}
	select {
	case id := <-f.agent.sent:
		t.Fatalf("%s was sent again", id)
	case <-time.After(100 * time.Millisecond):
	}
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.audits) != 1 || f.audits[0].Op != audit.OpSend || !errors.Is(f.audits[0].Err, agent.ErrForbidden) || len(f.rejected) != 0 {
		t.Errorf("audits %+v, Rejected %+v; want one send audited, and no Rejected", f.audits, f.rejected)
	}
}

// unknown makes send n of the agent fail with an unknown outcome, as a stream lost before the
// agent named a task does.
func unknown(n int) func(context.Context, int, agent.Handler) error {
	return func(_ context.Context, call int, h agent.Handler) error {
		if call == n {
			return fmt.Errorf("%w: the stream ended", agent.ErrOutcomeUnknown)
		}
		return turn(h, call, a2a.TaskStateCompleted)
	}
}

// After a send of unknown outcome into an existing context, the turn is looked up before
// anything is resent: a lookup that fails means nothing is known, so nothing is sent, and the
// runner backs off and asks again; absent means the agent never had it, so it is sent, once
// more, with the same messageId (spec 8.2). Never a blind resend.
func TestUnknownOutcomeIsLookedUpBeforeAnyResend(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	f.agent.script = unknown(1) // turn 2's first send
	lookups := 0
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		if lookups++; lookups <= 2 {
			return nil, nil, errors.New("agent unreachable")
		}
		return nil, nil, nil
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		<-sink.finals
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	want := []string{"send", "send", "find", "find", "find", "send"}
	if fmt.Sprint(f.agent.seq) != fmt.Sprint(want) {
		t.Errorf("calls = %v; want %v: turn 1, turn 2 (lost), three lookups (two fail), then turn 2 again", f.agent.seq, want)
	}
	if a, b := f.agent.msgs[1], f.agent.msgs[2]; a.ID != b.ID {
		t.Errorf("resent %s as %s; want the same messageId", a.ID, b.ID)
	}
}

// A first turn has no context, so nothing to look in: it is resent at once, and the agent
// deduplicates it by messageId (WP-02).
func TestUnknownOutcomeOfAFirstTurnIsResentWithoutALookup(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = unknown(0)
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.msgs) != 2 || len(f.agent.finds) != 0 || f.agent.msgs[0].ID != f.agent.msgs[1].ID || f.agent.msgs[1].ContextID != "" {
		t.Errorf("%d sends, %d lookups; want the same first turn twice, with no context and no lookup", len(f.agent.msgs), len(f.agent.finds))
	}
}

// If the agent did take the turn, it is followed, not sent again. When it took it more than
// once (a duplicate run), the thread is told and the audit log records the holders.
func TestUnknownOutcomeFoundIsFollowedAndDuplicatesAreReported(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = unknown(1)
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return &a2a.Task{ID: "t8", ContextID: "c1"}, []a2a.TaskID{"t8", "t9"}, nil
	}
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 7, a2a.TaskStateCompleted) }
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		<-sink.finals
	}
	if sends, _, follows := f.agent.counts(); sends != 2 || follows != 1 {
		t.Errorf("%d sends, %d follows; want turn 1 and turn 2 once, and the found task followed", sends, follows)
	}
	if got := sink.noticesOf(NoticeDuplicateRun); len(got) != 1 || got[0].MessageTS != "2" {
		t.Errorf("duplicate-run notices = %+v; want one for turn 2", got)
	}
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.audits) != 1 || f.audits[0].Op != audit.OpFindTask || len(f.audits[0].Holders) != 2 {
		t.Errorf("audits = %+v; want the lookup with both holders", f.audits)
	}
}

// A lookup that keeps failing does not go on for ever: past sendGiveUp the turn fails, with a
// notice, and is still not sent blind.
func TestUnknownOutcomeGivesUpWhenTheLookupNeverAnswers(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = unknown(1)
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		f.now.Store(t0.Add(16 * time.Minute).UnixNano())
		return nil, nil, errors.New("agent unreachable")
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the turn to fail", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	if sends, _, _ := f.agent.counts(); sends != 2 {
		t.Errorf("%d sends; want turn 1 and turn 2 once: a failed lookup never lets a resend through", sends)
	}
}

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

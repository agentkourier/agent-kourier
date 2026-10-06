package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	waitFinal(t, sink)
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

// The first failed send of a turn is retried silently, after the usual backoff: the first call of a
// new pod can fail and the next one work, and the thread need not hear of it. The notice that
// courier is retrying goes out once, when the second send in a row has failed too (spec 11), for a
// send that never left and for one of unknown outcome alike.
func TestFirstFailedSendIsRetriedSilently(t *testing.T) {
	notSent := func() error { return fmt.Errorf("%w: the agent is down", agent.ErrNotSent) }
	unknown := func() error { return fmt.Errorf("%w: the stream ended", agent.ErrOutcomeUnknown) }
	for _, tc := range []struct {
		name     string
		failures []func() error // the first sends fail like this, and the one after them works
		notices  int
	}{
		{"one not-sent", []func() error{notSent}, 0},
		{"two not-sent", []func() error{notSent, notSent}, 1},
		{"five not-sent", []func() error{notSent, notSent, notSent, notSent, notSent}, 1},
		{"one unknown outcome", []func() error{unknown}, 0},
		{"two unknown outcomes", []func() error{unknown, unknown}, 1},
		{"five unknown outcomes", []func() error{unknown, unknown, unknown, unknown, unknown}, 1},
		{"unknown outcome, then not-sent", []func() error{unknown, notSent}, 1},
		{"not-sent, then unknown outcome", []func() error{notSent, unknown}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
			f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
				// The notice follows the second failure and no other: none before send 2, one from it on.
				if got, want := len(sink.noticesOf(NoticeRetrying)), min(n/2, 1); got != want {
					t.Errorf("%d retrying notices before send %d; want %d", got, n, want)
				}
				if n < len(tc.failures) {
					return tc.failures[n]()
				}
				return turn(h, n, a2a.TaskStateCompleted)
			}
			in := inbound("1")
			if _, err := f.submit(in); err != nil {
				t.Fatal(err)
			}
			waitFinal(t, sink)
			f.agent.mu.Lock()
			sends := len(f.agent.msgs)
			f.agent.mu.Unlock()
			if sends != len(tc.failures)+1 {
				t.Errorf("%d sends; want %d: every failure retried, the turn never dropped", sends, len(tc.failures)+1)
			}
			got := sink.noticesOf(NoticeRetrying)
			if len(got) != tc.notices || (tc.notices == 1 && got[0].ID != in.MessageID+"/retrying") {
				t.Errorf("retrying notices = %+v; want %d", got, tc.notices)
			}
			if failed := sink.noticesOf(NoticeFailed); len(failed) != 0 {
				t.Errorf("failed notices = %+v; want none: the agent answered", failed)
			}
		})
	}
}

// A busy answer shows the agent is reachable, so it cancels a silent retry that is pending: one
// failed send, then -32004, then one more failed send is not "twice in a row", and the thread is
// not told. Once the notice has gone out, a busy answer does not bring a second one.
func TestBusyAnswerCancelsAPendingSilentRetry(t *testing.T) {
	const (
		down = iota // ErrNotSent
		busy
		ok
	)
	for _, tc := range []struct {
		name    string
		seq     []int
		notices int
	}{
		{"not-sent, busy, not-sent", []int{down, busy, down, ok}, 0},
		{"not-sent, not-sent, busy, not-sent", []int{down, down, busy, down, ok}, 1},
		{"not-sent, busy, not-sent, not-sent", []int{down, busy, down, down, ok}, 1},
		{"not-sent, not-sent, busy, not-sent, not-sent", []int{down, down, busy, down, down, ok}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
			f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
				switch tc.seq[n] {
				case down:
					return fmt.Errorf("%w: the agent is down", agent.ErrNotSent)
				case busy:
					return fmt.Errorf("send message: %w", agent.ErrBusy)
				}
				return turn(h, n, a2a.TaskStateCompleted)
			}
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			waitFinal(t, sink)
			if got := sink.noticesOf(NoticeRetrying); len(got) != tc.notices {
				t.Errorf("retrying notices = %+v; want %d", got, tc.notices)
			}
		})
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
	waitFinal(t, sink) // the second turn, which the first one's failure did not stop
	failed := sink.noticesOf(NoticeFailed)
	if len(failed) != 1 || failed[0].ID != inbound("1").MessageID+"/failed" || failed[0].Detail == "" {
		t.Errorf("failed notices = %+v; want one for turn 1, with a reason", failed)
	}
	if got := sink.noticesOf(NoticeRetrying); len(got) != 1 || got[0].MessageRef != "1" {
		t.Errorf("retrying notices = %+v; want one, for turn 1, before it was given up on", got)
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
	waitFinal(t, sink)
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.rejected) != 1 || f.rejected[0].Op != audit.OpSend || f.rejected[0].MessageID != inbound("1").MessageID || !errors.Is(f.rejected[0].Err, agent.ErrCredentialsRejected) {
		t.Errorf("Rejected calls = %+v; want one, for the send of turn 1", f.rejected)
	}
	if len(f.audits) != 0 {
		t.Errorf("the manager audited %+v; Rejected writes the only row", f.audits)
	}
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].MessageRef != "1" {
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
	waits := f.countWaits()
	f.agent.script = func(context.Context, int, agent.Handler) error {
		return fmt.Errorf("send message: %w", agent.ErrForbidden)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	f.retired(t)
	if sends, _, _ := f.agent.counts(); sends != 1 || waits.Load() != 0 {
		t.Errorf("%d sends, %d retry waits; want one send and no retry", sends, waits.Load())
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
		waitFinal(t, sink)
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
	waitFinal(t, sink)
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
		waitFinal(t, sink)
	}
	if sends, _, follows := f.agent.counts(); sends != 2 || follows != 1 {
		t.Errorf("%d sends, %d follows; want turn 1 and turn 2 once, and the found task followed", sends, follows)
	}
	if got := sink.noticesOf(NoticeDuplicateRun); len(got) != 1 || got[0].MessageRef != "2" {
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

// lost makes turn 0 lose its stream after the agent named task t1: Send returns the
// StreamLostError that says so.
func lost(cause error) func(context.Context, int, agent.Handler) error {
	return func(_ context.Context, n int, h agent.Handler) error {
		if n > 0 {
			return turn(h, n, a2a.TaskStateCompleted)
		}
		accepted(h, n)
		return &agent.StreamLostError{TaskID: "t1", ContextID: "c1", Err: cause}
	}
}

// working is the task t1 as GetTask shows it, in WORKING since at.
func working(at time.Time) func(a2a.TaskID) (*a2a.Task, error) {
	return func(id a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: &at}}, nil
	}
}

// A stream lost after the task was named is followed again, after a backoff, until the task
// settles (spec 8.3): the turn is not resent, and the queue waits for the task.
func TestLostStreamIsFollowedUntilTheTaskEnds(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 2*time.Millisecond
	f.agent.script = lost(errors.New("connection reset"))
	f.agent.get = working(t0)
	follows := 0
	f.agent.follow = func(id a2a.TaskID, h agent.Handler) error {
		if follows++; follows == 1 {
			return &agent.StreamLostError{TaskID: id, ContextID: "c1", Err: errors.New("still down")}
		}
		return turn(h, 0, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	waitFinal(t, sink) // turn 1 ends after two follows
	waitFinal(t, sink) // then turn 2
	sink.mu.Lock()
	var recovered []bool
	for _, ref := range sink.refs {
		recovered = append(recovered, ref.Recovered)
	}
	sink.mu.Unlock()
	if want := []bool{false, true, true, false}; !slices.Equal(recovered, want) { // send, two follows, then turn 2's send
		t.Errorf("Recovered of the turns' sinks = %v; want %v: a followed task continues its message", recovered, want)
	}
	if sends, _, fo := f.agent.counts(); sends != 2 || fo != 2 {
		t.Errorf("%d sends, %d follows; want turn 1 once and turn 2 once, and turn 1's task followed twice", sends, fo)
	}
}

// A task WORKING for longer than maxWorkingAge is presumed wedged (R17): it is cancelled, the
// thread is told, the call is audited, and the queue goes on. The age is the agent's own
// timestamp of the state, on courier's clock.
func TestWedgedTaskIsCancelledAfterMaxWorkingAge(t *testing.T) {
	for _, tc := range []struct {
		name string
		now  time.Duration // how far past t0 courier's clock is when the stream drops
		get  func(f *fixture) func(a2a.TaskID) (*a2a.Task, error)
	}{
		{"by the task's timestamp and its last event", 2 * time.Hour, func(*fixture) func(a2a.TaskID) (*a2a.Task, error) { return working(t0.Add(-2 * time.Hour)) }},
		{"when GetTask never answers, by the time since the loss", 0, func(f *fixture) func(a2a.TaskID) (*a2a.Task, error) {
			return func(a2a.TaskID) (*a2a.Task, error) {
				f.now.Store(t0.Add(2 * time.Hour).UnixNano())
				return nil, errors.New("agent unreachable")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 2*time.Millisecond
			inner := lost(errors.New("connection reset"))
			f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
				err := inner(ctx, n, h) // the task is named at t0, and its stream drops
				if n == 0 {
					f.now.Store(t0.Add(tc.now).UnixNano())
				}
				return err
			}
			f.agent.get = tc.get(f)
			f.agent.follow = func(a2a.TaskID, agent.Handler) error { return errors.New("follow must not run") }
			f.submitTurns("1", "2")
			waitFinal(t, sink) // turn 2, after turn 1's task was cancelled
			f.agent.mu.Lock()
			cancels := slices.Clone(f.agent.cancels)
			f.agent.mu.Unlock()
			if !slices.Equal(cancels, []a2a.TaskID{"t1"}) {
				t.Errorf("cancelled %v; want t1 once", cancels)
			}
			if got := sink.noticesOf(NoticeWedged); len(got) != 1 || got[0].MessageRef != "1" {
				t.Errorf("wedged notices = %+v; want one for turn 1", got)
			}
			f.amu.Lock()
			defer f.amu.Unlock()
			if len(f.audits) != 1 || f.audits[0].Op != audit.OpCancel || f.audits[0].TaskID != "t1" {
				t.Errorf("audits = %+v; want the cancel of t1", f.audits)
			}
		})
	}
}

// A reconnect the front door refused comes back as a StreamLostError that also matches
// ErrCredentialsRejected. The credential check comes first: the stream is not followed in a
// loop, the identity Source is told, and the thread hears the turn failed. The task stays
// recorded, for reconciliation.
func TestLostStreamWithRefusedCredentialsIsNotFollowed(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = lost(fmt.Errorf("reconnect: %w", agent.ErrCredentialsRejected))
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.rejected) != 1 || !errors.Is(f.rejected[0].Err, agent.ErrCredentialsRejected) || len(f.audits) != 0 {
		t.Errorf("Rejected calls %+v, audits %+v; want one Rejected call and no other audit", f.rejected, f.audits)
	}
	if _, _, fo := f.agent.counts(); fo != 0 || f.taskID() != "t1" {
		t.Errorf("%d follows, task %q; want no follow and t1 still recorded", fo, f.taskID())
	}
}

// The agent no longer knows the task, or the session: the thread is told, the courier's record
// of the agent's session is reset (and only that, so the thread stays owned), and the next turn
// starts without a contextId, a fresh session (spec 8.2). UNCONFIRMED against kagent: what it
// answers for a session it dropped.
func TestSessionGoneStartsAFreshContext(t *testing.T) {
	notFound := a2a.NewError(a2a.ErrTaskNotFound, "task not found")
	for _, tc := range []struct {
		name          string
		get, followTo error
	}{
		{"GetTask does not know the task", notFound, nil},
		{"Follow does not know the task", nil, notFound},
	} {
		for _, resetFails := range []bool{false, true} { // a failed reset is retried, not forgotten
			tc, resetFails := tc, resetFails
			name := tc.name
			if resetFails {
				name += ", and the reset fails once"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				if resetFails {
					f.st.fail["reset"] = errors.New("database is locked")
				}
				sink := f.live()
				f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 2*time.Millisecond
				f.agent.script = lost(errors.New("connection reset"))
				f.agent.get = func(id a2a.TaskID) (*a2a.Task, error) { return working(t0)(id) }
				if tc.get != nil {
					f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return nil, tc.get }
				}
				f.agent.follow = func(a2a.TaskID, agent.Handler) error { return tc.followTo }
				for _, ts := range []string{"1", "2"} {
					if _, err := f.submit(inbound(ts)); err != nil {
						t.Fatal(err)
					}
				}
				waitFinal(t, sink) // turn 2
				if got := sink.noticesOf(NoticeSessionGone); len(got) != 1 || got[0].MessageRef != "1" || got[0].ID != "task:slack:C1:1700.01:t1/session_gone" {
					t.Errorf("session-gone notices = %+v; want one, for turn 1, named after the task", got)
				}
				f.agent.mu.Lock()
				defer f.agent.mu.Unlock()
				if second := f.agent.msgs[1]; second.ContextID != "" {
					t.Errorf("turn 2 was sent in context %q; want none: a fresh session", second.ContextID)
				}
				if sess, _ := f.session(); sess.Binding != "ns/b1" || !sess.LastActivity.Equal(t0) {
					t.Errorf("session = %+v; want the Binding and activity kept", sess)
				}
			})
		}
	}
}

// If the agent answers a turn in a different context than the session's, it opened a new
// session for the old one's id (UNCONFIRMED: what kagent does with an unknown contextId): the
// thread is told, and the new context is recorded.
func TestReplacedContextIsAdoptedAndReported(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 1 {
			h(agent.StateEvent{TaskID: "t2", ContextID: "c2", State: a2a.TaskStateWorking})
			h(agent.FinalEvent{Task: &a2a.Task{ID: "t2", ContextID: "c2", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
			return nil
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
	}
	if got := sink.noticesOf(NoticeSessionGone); len(got) != 1 || got[0].MessageRef != "2" {
		t.Errorf("session-gone notices = %+v; want one, for turn 2", got)
	}
	if sess, _ := f.session(); sess.ContextID != "c2" {
		t.Errorf("context = %q; want the new one, c2", sess.ContextID)
	}
}

// Any answer from the agent, -32004 included, means it is reachable: the give-up clock starts
// over. A not-sent, a busy session 10 minutes later, and a not-sent 16 minutes after the first
// are not 15 minutes of an unreachable agent.
func TestBusyResetsTheGiveUpClock(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		switch n {
		case 0:
			return fmt.Errorf("%w: down", agent.ErrNotSent)
		case 1:
			f.now.Store(t0.Add(10 * time.Minute).UnixNano())
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		case 2:
			f.now.Store(t0.Add(16 * time.Minute).UnixNano())
			return fmt.Errorf("%w: down", agent.ErrNotSent)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if got := sink.noticesOf(NoticeFailed); len(got) != 0 {
		t.Errorf("failed notices = %+v; want none: the agent answered at +10 minutes", got)
	}
}

// The thread is told once that courier is retrying, whatever failed first: a busy session, then
// two not-sent, is one run of trouble with one notice; a later turn's trouble gets its own. The
// busy answer is no failed send to retry: the notice still waits for the second not-sent.
func TestRetryingNoticeIsSentOncePerTurn(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		switch n {
		case 0:
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		case 1, 2, 4, 5: // turn 1 twice, then turn 2 twice
			return fmt.Errorf("%w: down", agent.ErrNotSent)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
	}
	got := sink.noticesOf(NoticeRetrying)
	if len(got) != 2 || got[0].MessageRef != "1" || got[1].MessageRef != "2" {
		t.Errorf("retrying notices = %+v; want one for turn 1 and one for turn 2", got)
	}
}

// With no identity Source to call, a refused credential is still audited: no failure is
// dropped without a row (spec 8.6 step 7).
func TestRefusedCredentialWithoutASourceIsAudited(t *testing.T) {
	f := newFixture(t)
	f.noRejected = true
	sink := f.live()
	f.agent.script = func(context.Context, int, agent.Handler) error {
		return fmt.Errorf("%w: %w", agent.ErrNotSent, agent.ErrCredentialsRejected)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.audits) != 1 || !errors.Is(f.audits[0].Err, agent.ErrCredentialsRejected) {
		t.Errorf("audits = %+v; want the refused send", f.audits)
	}
}

// New refuses a Config without what the manager must have, and the error names the field: a wiring that drops one
// fails at start-up and not by doing nothing. TextReply is not in the list: it is set after construction.
func TestNewNamesTheFieldThatIsMissing(t *testing.T) {
	f := newFixture(t)
	full := f.m.cfg
	for _, tc := range []struct {
		field string
		clear func(*Config)
	}{
		{"Store", func(c *Config) { c.Store = nil }},
		{"Bindings", func(c *Config) { c.Bindings = nil }},
		{"Audit", func(c *Config) { c.Audit = nil }},
		{"Users", func(c *Config) { c.Users = nil }},
		{"Sink", func(c *Config) { c.Sink = nil }},
		{"Questions", func(c *Config) { c.Questions = nil }},
		{"ThreadContext", func(c *Config) { c.ThreadContext = nil }},
		{"OnTurnEnd", func(c *Config) { c.OnTurnEnd = nil }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			cfg := full
			tc.clear(&cfg)
			m, err := New(cfg)
			if err == nil || m != nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("New = %v, %v; want an error that names %s", m, err, tc.field)
			}
		})
	}
	if _, err := New(full); err != nil {
		t.Fatalf("New with every required field = %v, want none", err)
	}
}

// waitFinal waits for the sink's next FinalEvent, or fails the test after 10 s.
func waitFinal(t *testing.T, s *fakeSink) {
	t.Helper()
	select {
	case <-s.finals:
	case <-time.After(10 * time.Second):
		t.Fatal("no final event")
	}
}

// submitTurns queues the turns while the first Send is held back. The tests that call it jump the
// fake clock past the thread's TTL once the first turn is in flight, and a turn submitted after
// the jump is refused as Expired, without an error and without ever being sent: every turn is
// queued first, and the first Send goes on once the last is.
func (f *fixture) submitTurns(turns ...string) {
	f.t.Helper()
	f.agent.mu.Lock()
	f.agent.hold = make(chan struct{})
	hold := f.agent.hold
	f.agent.mu.Unlock()
	defer close(hold) // also when a Fatal leaves the loop: the runner must not wait for ever
	for _, ts := range turns {
		if _, err := f.submit(inbound(ts)); err != nil {
			f.t.Fatal(err)
		}
	}
}

// wedgedFixture is a conversation whose turn 1 lost its stream and whose task t1 has run for
// two hours since it was named, so that the next look at it cancels it (R17).
func wedgedFixture(t *testing.T, setup func(*fixture), turns ...string) (*fixture, *fakeSink) {
	t.Helper()
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	inner := lost(errors.New("connection reset"))
	f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
		err := inner(ctx, n, h) // the task is named at t0 ...
		if n == 0 {
			f.now.Store(t0.Add(2 * time.Hour).UnixNano()) // ... and two hours pass before anyone looks
		}
		return err
	}
	f.agent.get = working(t0.Add(-2 * time.Hour))
	f.agent.follow = func(a2a.TaskID, agent.Handler) error { return errors.New("follow must not run") }
	setup(f) // before any turn: the runner reads what it sets
	f.submitTurns(turns...)
	return f, sink
}

// A cancel that fails is classified, not retried for ever: the audit log gets one row for the
// decision, and the thread hears what happened.
func TestFailedCancelOfAWedgedTaskIsClassified(t *testing.T) {
	t.Run("the task is gone: the session is reset", func(t *testing.T) {
		f, sink := wedgedFixture(t, func(f *fixture) {
			f.agent.cancelErr = func(int) error { return a2a.NewError(a2a.ErrTaskNotFound, "task not found") }
		}, "1", "2")
		waitFinal(t, sink) // turn 2, sent in a fresh context
		if got := sink.noticesOf(NoticeSessionGone); len(got) != 1 {
			t.Errorf("session-gone notices = %+v; want one", got)
		}
		f.agent.mu.Lock()
		defer f.agent.mu.Unlock()
		if len(f.agent.cancels) != 1 || f.agent.msgs[1].ContextID != "" {
			t.Errorf("%d cancels, turn 2 in context %q; want one cancel and a fresh context", len(f.agent.cancels), f.agent.msgs[1].ContextID)
		}
	})
	t.Run("not cancelable: the task ended meanwhile, and its end is rendered", func(t *testing.T) {
		f, sink := wedgedFixture(t, func(f *fixture) {
			f.agent.cancelErr = func(int) error { return fmt.Errorf("cancel: %w", a2a.ErrTaskNotCancelable) }
			f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
		}, "1", "2")
		waitFinal(t, sink)
		if got := sink.noticesOf(NoticeWedged); len(got) != 0 {
			t.Errorf("wedged notices = %+v; want none: the task had ended", got)
		}
		if _, _, fo := f.agent.counts(); fo != 1 {
			t.Errorf("%d follows; want the task read once more, to render its end", fo)
		}
	})
	for _, tc := range []struct {
		name   string
		cause  error
		source bool // the identity Source is told, and writes the audit row
	}{
		{"the credentials are refused", fmt.Errorf("%w: rejected", agent.ErrCredentialsRejected), true},
		{"the identity is refused", fmt.Errorf("send message: %w", agent.ErrForbidden), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink := wedgedFixture(t, func(f *fixture) { f.agent.cancelErr = func(int) error { return tc.cause } }, "1")
			eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
			time.Sleep(50 * time.Millisecond) // a retry loop would show by now
			f.amu.Lock()
			defer f.amu.Unlock()
			rows := len(f.audits)
			if tc.source {
				rows = len(f.rejected)
			}
			if cancels := f.agent.cancels; len(cancels) != 1 || rows != 1 {
				t.Errorf("%d cancels, %d audit rows; want one of each", len(cancels), rows)
			}
		})
	}
	t.Run("a transient failure is retried for a bounded time, then reported", func(t *testing.T) {
		f, sink := wedgedFixture(t, func(f *fixture) {
			f.agent.cancelErr = func(n int) error {
				if n == 3 {
					f.now.Store(t0.Add(2*time.Hour + 16*time.Minute).UnixNano())
				}
				return errors.New("agent unreachable")
			}
		}, "1")
		eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
		f.amu.Lock()
		defer f.amu.Unlock()
		if len(f.audits) != 1 || f.audits[0].Op != audit.OpCancel {
			t.Errorf("audits = %+v; want one row, for the decision, not one per attempt", f.audits)
		}
	})
}

// The age of a WORKING task counts from the later of its status timestamp and the last event
// courier saw for it (R17): a healthy task that streamed for an hour and then dropped is
// followed, not cancelled. A state with no timestamp, or one in the future, says nothing about
// age, and is not cancelled either.
func TestTaskWithRecentLifeIsNotPresumedWedged(t *testing.T) {
	for _, tc := range []struct {
		name string
		get  func(a2a.TaskID) (*a2a.Task, error)
	}{
		{"its status is old but an event is recent", working(t0.Add(-2 * time.Hour))},
		{"no timestamp", func(id a2a.TaskID) (*a2a.Task, error) {
			return &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}, nil
		}},
		{"a timestamp in the future", working(t0.Add(24 * time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
			f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
				accepted(h, n)
				f.now.Store(t0.Add(90 * time.Minute).UnixNano())
				h(agent.TextEvent{ArtifactID: "a", Text: "progress"}) // the task is alive, an hour and a half in
				return &agent.StreamLostError{TaskID: "t1", ContextID: "c1", Err: errors.New("connection reset")}
			}
			f.agent.get = tc.get
			f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			waitFinal(t, sink)
			if got := f.cancelled(); len(got) != 0 {
				t.Errorf("cancelled %v; want the task followed to its end", got)
			}
		})
	}
}

// A reply that keeps getting -32004 from a task courier never recorded is queued behind it,
// though nothing recorded says so (Submit answered Queued=false): the thread is told once, with
// the same stable ID Submit's own queued notice has, so a renderer posts it at most once.
func TestBusyFromAnUnrecordedBlockerTellsTheThread(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n < 4 {
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	in := inbound("1")
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	got := sink.noticesOf(NoticeQueued)
	if len(got) != 1 || got[0].ID != in.MessageID+"/queued" || got[0].MessageRef != "1" {
		t.Errorf("queued notices = %+v; want one, named like Submit's own, after four -32004", got)
	}
}

// An unknown outcome is not settled by a lookup that finds nothing: the send may still be
// recorded late. So a later not-sent does not make the next attempt blind: it looks the turn up
// first, however the earlier attempts ended.
func TestNotSentDoesNotForgetAnEarlierUnknownOutcome(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		switch n {
		case 1:
			return fmt.Errorf("%w: the stream ended", agent.ErrOutcomeUnknown)
		case 2:
			return fmt.Errorf("%w: down", agent.ErrNotSent)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if want := "[send send find send find send]"; fmt.Sprint(f.agent.seq) != want {
		t.Errorf("calls = %v; want %s: a lookup before each resend of turn 2", f.agent.seq, want)
	}
}

// A turn the agent may have taken is never given up on: however long the lookups fail, it is not
// failed, and is followed when the agent answers.
func TestHeldTurnNeverGivesUp(t *testing.T) {
	t.Run("a held turn whose lookups keep failing", func(t *testing.T) {
		f := newFixture(t)
		sink := f.live()
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
		f.seed("ns/b1", 0) // a task t1 is recorded, so the head is held until it is looked up
		lookups := 0
		f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
			if lookups++; lookups < 4 {
				f.now.Store(t0.Add(time.Duration(lookups) * 10 * time.Minute).UnixNano())
				return nil, nil, errors.New("agent unreachable")
			}
			return &a2a.Task{ID: "t1", ContextID: "c1"}, []a2a.TaskID{"t1"}, nil
		}
		f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
		if got := sink.noticesOf(NoticeFailed); len(got) != 0 {
			t.Errorf("failed notices = %+v; a turn the agent has is never given up on", got)
		}
	})
}

// One -32004 must not make an unreachable agent patient for ever: a busy session means the agent
// answered, which starts the give-up clock over (and the thread is told the reply is queued), but
// a turn that then cannot reach the agent is failed after sendGiveUp, as section 11 says.
func TestBusyThenUnreachableStillGivesUp(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(_ context.Context, n int, _ agent.Handler) error {
		if n == 0 {
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		}
		f.now.Store(t0.Add(time.Duration(n) * time.Hour).UnixNano()) // an hour for every attempt
		return fmt.Errorf("%w: down", agent.ErrNotSent)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) >= 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 {
		t.Errorf("failed notices = %+v; want exactly one", got)
	}
}

// A failed reset is retried, also for a thread with a single turn, where nothing else keeps the
// runner alive: the notice and the move on wait for the reset, whichever way the loss is found.
func TestSessionGoneSurvivesAFailedResetWithOneTurn(t *testing.T) {
	notFound := a2a.NewError(a2a.ErrTaskNotFound, "task not found")
	for _, tc := range []struct {
		name   string
		script func(context.Context, int, agent.Handler) error
	}{
		{"found while following the lost stream", lost(errors.New("connection reset"))},
		{"found in the lost stream's own error", func(_ context.Context, n int, h agent.Handler) error {
			accepted(h, n)
			return &agent.StreamLostError{TaskID: "t1", ContextID: "c1", Err: notFound}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
			f.st.fail["reset"] = errors.New("database is locked")
			f.agent.script = tc.script
			f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return nil, notFound }
			f.agent.follow = func(a2a.TaskID, agent.Handler) error { return notFound }
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the session to be reset", func() bool {
				sess, _ := f.session()
				return sess.ContextID == "" && sess.TaskID == "" && len(sink.noticesOf(NoticeSessionGone)) == 1
			})
		})
	}
}

// A silent task that is WORKING but does nothing is presumed wedged even though every follow
// begins by replaying its snapshot (R17). What a snapshot replays is not new life: only an event
// that differs from what courier already holds (a new state, new text, a new tool call) is.
func TestSilentTaskIsCancelledDespiteSnapshotsReplayedByEachFollow(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		h(agent.TextEvent{ArtifactID: "a", Text: "so far"})
		return &agent.StreamLostError{TaskID: "t1", ContextID: "c1", Err: errors.New("connection reset")}
	}
	f.agent.get = working(t0.Add(-2 * time.Hour)) // WORKING for ever, says nothing
	f.agent.follow = func(id a2a.TaskID, h agent.Handler) error {
		f.now.Store(time.Unix(0, f.now.Load()).Add(10 * time.Minute).UnixNano())      // each look is ten minutes on
		h(agent.StateEvent{TaskID: id, ContextID: "c1", State: a2a.TaskStateWorking}) // the snapshot, replayed
		h(agent.TextEvent{ArtifactID: "a", Text: "so far"})
		return &agent.StreamLostError{TaskID: id, ContextID: "c1", Err: errors.New("connection reset")}
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the silent task to be presumed wedged", func() bool { return len(sink.noticesOf(NoticeWedged)) == 1 })
	if got := f.cancelled(); len(got) != 1 || got[0] != "t1" {
		t.Errorf("cancelled %v; want t1 once", got)
	}
}

// A state with no timestamp counts from the last event of the task, else from the loss. A
// timestamp in the future stops protecting a task once it has passed.
func TestAgeWithoutAUsableTimestamp(t *testing.T) {
	for _, tc := range []struct {
		name string
		get  func(a2a.TaskID) (*a2a.Task, error)
	}{
		{"no timestamp", func(id a2a.TaskID) (*a2a.Task, error) {
			return &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}, nil
		}},
		{"a timestamp in the future, since passed", working(t0.Add(30 * time.Minute))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
			inner := lost(errors.New("connection reset")) // the task is named at t0
			f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
				err := inner(ctx, n, h)
				f.now.Store(t0.Add(2 * time.Hour).UnixNano()) // and nothing is heard for two hours
				return err
			}
			f.agent.get = tc.get
			f.agent.follow = func(a2a.TaskID, agent.Handler) error { return errors.New("follow must not run") }
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the task to be presumed wedged", func() bool { return len(sink.noticesOf(NoticeWedged)) == 1 })
		})
	}
}

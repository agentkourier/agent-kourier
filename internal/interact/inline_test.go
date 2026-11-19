package interact_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// A platform that answers a press in its own request (chat.InlineDecider) waits for the press worker's decision through
// DecidePress. The press is decided by the worker alone, once, whether anyone waits or not; the wait reads what it came to.

// gatedAuthz is an authz whose IsMember waits for open to be closed, so that a decision takes as long as a test wants.
type gatedAuthz struct {
	*authz
	open chan struct{}
}

func (g gatedAuthz) IsMember(ctx context.Context, channel, user string) (bool, error) {
	select {
	case <-g.open:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return g.authz.IsMember(ctx, channel, user)
}

// inline makes r's pipeline, over the Binding b and the authorizer az (r.az if nil), publish what its presses come to and hand them to a
// running press worker, as the broker's does. now is the pipeline's clock (r.now if nil). It returns the worker's processor, which counts
// the tries.
func inline(t *testing.T, r *pressRig, b interact.Binding, az interact.Authorizer, now func() time.Time) *countingProc {
	t.Helper()
	if az == nil {
		az = r.az
	}
	if now == nil {
		now = func() time.Time { return r.now }
	}
	proc := &countingProc{}
	var w *interact.Worker
	pl, err := interact.NewPipeline(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{b}, Authorizers: chattest.Same(az),
		Ephemerals: chattest.Same[interact.Ephemeral](r.eph), Decider: r.d, Auditor: r.aud, Limits: store.PressLimits{Total: 10, PerUser: 5},
		Logger: testutil.DiscardLogger(), Now: now, Notify: func(p store.Press) { w.Submit(p) }, Outcomes: interact.NewOutcomes(4)})
	if err != nil {
		t.Fatal(err)
	}
	r.pl, proc.PressProcessor = pl, pl
	w, _, _ = startWorker(t, proc, func(c *interact.WorkerConfig) {
		c.Presses, c.Backoff, c.MaxBackoff, c.Now = r.st, 20*time.Millisecond, 100*time.Millisecond, now
	})
	return proc
}

// decide is user's press i decided while its platform waits up to a second.
func (r *pressRig) decide(t *testing.T, i chat.Interaction) chat.PressOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	out, err := r.pl.DecidePress(ctx, i)
	if err != nil {
		t.Fatalf("DecidePress: %v", err)
	}
	return out
}

// A press decided within the wait is told what it came to: that the answer was recorded, or why it was refused, in the words a
// platform with ephemeral messages tells the person alone (the decider's and the pipeline's). The decision is the worker's, once.
func TestAPressDecidedInTheWaitIsToldItsOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		b     interact.Binding
		setup func(t *testing.T, r *pressRig)
		press func(r *pressRig) chat.Interaction
		want  string
		sent  int // answers the session is given in all
	}{
		"taken": {
			setup: func(t *testing.T, r *pressRig) { r.open(t) },
			press: func(r *pressRig) chat.Interaction { return r.submit(t, "UALICE1", 1) },
			want:  "Your answer was recorded.", sent: 1,
		},
		"not a member": {
			setup: func(t *testing.T, r *pressRig) { r.open(t) },
			press: func(r *pressRig) chat.Interaction { return r.submit(t, "UEVE555", 1) },
			want:  "Only members of this channel can answer this question.",
		},
		"gone": {
			setup: func(*testing.T, *pressRig) {},
			press: func(r *pressRig) chat.Interaction { return r.submit(t, "UALICE1", 1) },
			want:  "This question is no longer available.",
		},
		"invalid": {
			setup: func(t *testing.T, r *pressRig) { r.open(t) },
			press: func(r *pressRig) chat.Interaction { return r.submit(t, "UALICE1", -1) },
			want:  "Please answer question 1, then press Submit again.",
		},
		"answered by another": {
			setup: func(t *testing.T, r *pressRig) {
				r.open(t)
				if out := r.decide(t, r.submit(t, "UBOB22", 0)); !out.Decided {
					t.Fatalf("the first press: %+v", out)
				}
			},
			press: func(r *pressRig) chat.Interaction { return r.submit(t, "UALICE1", 1) },
			want:  "Someone else answered this question first.", sent: 1,
		},
		"already recorded": {
			setup: func(t *testing.T, r *pressRig) {
				r.open(t)
				if out := r.decide(t, r.submit(t, "UALICE1", 0)); !out.Decided {
					t.Fatalf("the first press: %+v", out)
				}
			},
			press: func(r *pressRig) chat.Interaction { return r.submit(t, "UALICE1", 1) },
			want:  "Your answer to this question is already recorded.", sent: 2, // the recorded answer is sent again, as after a crash
		},
		"not recorded": {
			setup: func(t *testing.T, r *pressRig) {
				r.open(t)
				r.aud.setError(fmt.Errorf("the row cannot be built: %w", store.ErrInvalid))
			},
			press: func(r *pressRig) chat.Interaction { return r.submit(t, "UALICE1", 1) },
			want:  "Your answer could not be recorded, so it was not taken. An operator must fix this.",
		},
		"not an approver": {
			b: groupBinding(),
			setup: func(t *testing.T, r *pressRig) {
				r.az.groups = map[string][]string{"S2": {"UBOB22"}}
				openApproval(t, r, store.InteractionToolApproval, approvalRequest(t))
			},
			press: func(*pressRig) chat.Interaction { return tap("UALICE1", chat.ActionApproveAll) },
			want:  "Only approvers of this Binding can decide this request.",
		},
		"groups broken": {
			b: groupBinding(),
			setup: func(t *testing.T, r *pressRig) {
				r.az.unreadable = map[string]error{"S1": errNoScope}
				openApproval(t, r, store.InteractionToolApproval, approvalRequest(t))
			},
			press: func(*pressRig) chat.Interaction { return tap("UALICE1", chat.ActionApproveAll) },
			want:  groupsBroken,
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := tc.b
			if b.Dialect == nil {
				b = newPressBinding()
			}
			r := newPressRigFor(t, b)
			proc := inline(t, r, b, nil, nil)
			tc.setup(t, r)
			before := proc.tries.Load()
			if got, want := r.decide(t, tc.press(r)), (chat.PressOutcome{Decided: true, Text: tc.want}); got != want {
				t.Errorf("told %+v; want %+v", got, want)
			}
			if n := proc.tries.Load() - before; n != 1 || r.waiting(t) != 0 || len(r.sess.answers()) != tc.sent {
				t.Errorf("%d tries, %d presses waiting, %d answers sent; want the press decided once, by the worker, and %d sent", n, r.waiting(t), len(r.sess.answers()), tc.sent)
			}
		})
	}
}

// newPressBinding is the Binding of newPressRig.
func newPressBinding() interact.Binding {
	return interact.Binding{Dialect: kagentv1.New(time.Second), AskUser: true, Timeout: time.Minute, Identity: "secret:ns/tok"}
}

// A question whose prompt is not recorded yet is tried again until EarlyFor has passed, and is then told it is not ready: when that
// is final within the wait, the presser is told so.
func TestAPressOnAPromptNeverRecordedIsToldNotReady(t *testing.T) {
	r := newPressRig(t)
	var ticks atomic.Int64 // each read of the clock is five minutes after the last, so the press is past EarlyFor when it is processed
	now := func() time.Time { return d0.Add(time.Duration(ticks.Add(1)) * 5 * time.Minute) }
	inline(t, r, newPressBinding(), nil, now)
	r.pending(t) // no message recorded
	want := chat.PressOutcome{Decided: true, Text: "This question is not ready yet. Please press Submit again in a moment."}
	if got := r.decide(t, r.submit(t, "UALICE1", 1)); got != want {
		t.Errorf("told %+v; want %+v", got, want)
	}
}

// A press whose first try cannot decide it (a check that fails, which may pass) is not decided in the wait, which returns at once with
// no outcome, and the worker decides it later, once.
func TestAPressToBeTriedAgainIsNotDecidedInTheWait(t *testing.T) {
	r := newPressRig(t)
	proc := inline(t, r, newPressBinding(), nil, nil)
	r.open(t)
	r.az.mu.Lock()
	r.az.memberErr = errBoom
	r.az.mu.Unlock()
	start := time.Now()
	if got := r.decide(t, r.submit(t, "UALICE1", 1)); got != (chat.PressOutcome{}) {
		t.Errorf("told %+v; want no outcome", got)
	}
	if waited := time.Since(start); waited >= time.Second {
		t.Errorf("the wait took %v; want it to end at the first try, not at its deadline", waited)
	}
	r.az.mu.Lock()
	r.az.memberErr = nil
	r.az.mu.Unlock()
	testutil.Eventually(t, "the worker to decide the press", func() bool { return r.waiting(t) == 0 })
	if row := r.row(t); row.State != store.InteractionAnswered || len(r.sess.answers()) != 1 || proc.tries.Load() != 2 {
		t.Errorf("state %s, %d answers, %d tries; want it answered once, at the second try", row.State, len(r.sess.answers()), proc.tries.Load())
	}
}

// A press whose decision takes longer than the wait is answered with no outcome when the wait ends, and the worker goes on to decide
// it, once: the wait does not cut the decision, and nothing decides it a second time.
func TestAPressDecidedAfterTheWaitIsDecidedOnceByTheWorker(t *testing.T) {
	r := newPressRig(t)
	gate := gatedAuthz{authz: r.az, open: make(chan struct{})}
	proc := inline(t, r, newPressBinding(), gate, nil)
	r.open(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	out, err := r.pl.DecidePress(ctx, r.submit(t, "UALICE1", 1))
	if err != nil || out != (chat.PressOutcome{}) {
		t.Fatalf("DecidePress = %+v, %v; want no outcome once the wait ran out", out, err)
	}
	if r.row(t).State != store.InteractionPending {
		t.Fatal("the press was decided before its check of the person passed")
	}
	close(gate.open)
	testutil.Eventually(t, "the worker to decide the press", func() bool { return r.waiting(t) == 0 })
	time.Sleep(50 * time.Millisecond) // a second decision, if one were coming
	if row := r.row(t); row.State != store.InteractionAnswered || row.Decider != "UALICE1" || len(r.sess.answers()) != 1 || proc.tries.Load() != 1 {
		t.Errorf("state %s by %q, %d answers, %d tries; want it answered by the press once, in one try", row.State, row.Decider, len(r.sess.answers()), proc.tries.Load())
	}
}

// Press, the path of a platform that does not wait (Slack), records the press, hands it on and returns, as it always has, with Outcomes
// set: nothing waits for the decision, which the worker makes after.
func TestAPressNobodyWaitsForReturnsAtOnce(t *testing.T) {
	r := newPressRig(t)
	gate := gatedAuthz{authz: r.az, open: make(chan struct{})}
	proc := inline(t, r, newPressBinding(), gate, nil)
	r.open(t)
	done := make(chan error, 1)
	go func() { done <- r.pl.Press(t.Context(), r.submit(t, "UALICE1", 1)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Press waited for the decision")
	}
	if r.row(t).State != store.InteractionPending {
		t.Fatal("the press was decided in Press")
	}
	close(gate.open)
	testutil.Eventually(t, "the worker to decide the press", func() bool { return r.waiting(t) == 0 })
	if row := r.row(t); row.State != store.InteractionAnswered || len(r.sess.answers()) != 1 || proc.tries.Load() != 1 {
		t.Errorf("state %s, %d answers, %d tries; want it answered once", row.State, len(r.sess.answers()), proc.tries.Load())
	}
}

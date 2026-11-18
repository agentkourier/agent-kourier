package feedback_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// inline makes r's handler publish what its votes come to and hand them to a running press worker, as the broker's does.
func (r *rig) inline(t *testing.T) {
	t.Helper()
	var w *interact.Worker
	h, err := feedback.New(feedback.Config{
		Store: r.st, Sessions: r.st, Authorizers: chattest.Same[interact.Authorizer](r.az), Ephemerals: chattest.Same[interact.Ephemeral](r.eph), Auditor: r.aud,
		Limits: store.PressLimits{Total: 10, PerUser: 3}, Logger: testutil.DiscardLogger(), Now: func() time.Time { return r.now },
		Notify: func(p store.Press) { w.Submit(p) }, Outcomes: interact.NewOutcomes(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.h = h
	w, err = interact.NewWorker(interact.WorkerConfig{Processor: h, Presses: r.st, Backoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
		Now: func() time.Time { return r.now }, Logger: testutil.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// decide is the vote i decided while its platform waits up to a second.
func (r *rig) decide(t *testing.T, i chat.Interaction) chat.PressOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.tel.Ctx, time.Second)
	defer cancel()
	out, err := r.h.DecidePress(ctx, i)
	if err != nil {
		t.Fatalf("DecidePress: %v", err)
	}
	return out
}

// A vote decided within the wait is told what it came to: recorded, already recorded, or why it was refused. A person outside the
// channel, whom Slack cannot tell alone, is told that only members can rate the message, and nothing is sent to them alone.
func TestAVoteDecidedInTheWaitIsToldItsOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		before []chat.Interaction // decided first, each in its own wait
		press  chat.Interaction
		want   string
		votes  int
		alone  int // messages told to the voter alone, in all
	}{
		"recorded": {
			press: press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful),
			want:  "Thanks, your vote is recorded as useful. To change it, press the other button.", votes: 1, alone: 1,
		},
		"already recorded": {
			before: []chat.Interaction{press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)},
			press:  press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful),
			want:   "Your vote is already recorded as useful.", votes: 1, alone: 2,
		},
		"not a member": {
			press: press("UEVE555", store.FeedbackInvestigation, store.VerdictUseful),
			want:  "Only members of this channel can rate this message.",
		},
		"gone": {
			press: func() chat.Interaction {
				i := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
				i.PromptID = feedback.Value{Kind: store.FeedbackInvestigation, Verdict: store.VerdictUseful, Thread: "999.9", Turn: turn}.String()
				return i
			}(),
			want: "This message can no longer be rated.", alone: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.inline(t)
			for _, i := range tc.before {
				if out := r.decide(t, i); !out.Decided {
					t.Fatalf("the first vote: %+v", out)
				}
			}
			if got, want := r.decide(t, tc.press), (chat.PressOutcome{Decided: true, Text: tc.want}); got != want {
				t.Errorf("told %+v; want %+v", got, want)
			}
			if len(r.votes(t)) != tc.votes || r.waiting(t) != 0 || len(r.eph.list()) != tc.alone {
				t.Errorf("%d votes, %d presses waiting, told alone %+v; want %d votes, none waiting, %d told alone", len(r.votes(t)), r.waiting(t), r.eph.list(), tc.votes, tc.alone)
			}
		})
	}
}

// A vote whose first try cannot decide it is not decided in the wait, which returns at once with no outcome; the worker records it
// later, once.
func TestAVoteToBeTriedAgainIsNotDecidedInTheWait(t *testing.T) {
	r := newRig(t)
	r.inline(t)
	r.az.mu.Lock()
	r.az.memberErr = errors.New("the roster read timed out")
	r.az.mu.Unlock()
	start := time.Now()
	if got := r.decide(t, press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)); got != (chat.PressOutcome{}) {
		t.Errorf("told %+v; want no outcome", got)
	}
	if waited := time.Since(start); waited >= time.Second {
		t.Errorf("the wait took %v; want it to end at the first try", waited)
	}
	r.az.mu.Lock()
	r.az.memberErr = nil
	r.az.mu.Unlock()
	testutil.Eventually(t, "the worker to record the vote", func() bool { return r.waiting(t) == 0 })
	if n := len(r.votes(t)); n != 1 {
		t.Errorf("%d votes; want one", n)
	}
}

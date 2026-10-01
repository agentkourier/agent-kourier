package interact_test

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// pressRig is the decider's rig with a pipeline in front of it, on a clock the test moves.
type pressRig struct {
	*decideRig
	pl  *interact.Pipeline
	now time.Time
	got []store.Press // what Notify was told
}

func newPressRig(t *testing.T) *pressRig {
	t.Helper()
	r := &pressRig{decideRig: newDecideRig(t), now: d0.Add(time.Minute)}
	var err error
	r.pl, err = interact.NewPipeline(interact.PressConfig{Store: r.st, Ephemerals: r.eph, Limits: store.PressLimits{Total: 10, PerUser: 2},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return r.now }, Notify: func(p store.Press) { r.got = append(r.got, p) }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// open is a pending interaction whose prompt is posted, as message 100.1 of the thread.
func (r *pressRig) open(t *testing.T) store.Interaction {
	t.Helper()
	r.pending(t)
	if err := r.st.SetInteractionMessageRef(t.Context(), "ix-1", "100.1"); err != nil {
		t.Fatal(err)
	}
	return r.row(t)
}

// submit is user's press of the Submit button on the prompt.
func (r *pressRig) submit(user string) slack.Interaction {
	return slack.Interaction{User: user, Channel: key.Channel, MessageTS: "100.1", ThreadTS: key.ThreadTS, ActionID: "submit", Value: "ix-1"}
}

// process records the press and processes it, as the worker will.
func (r *pressRig) process(t *testing.T, i slack.Interaction) (interact.Result, error) {
	t.Helper()
	if err := r.pl.Press(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	return r.pl.Process(t.Context(), presses[len(presses)-1])
}

func (r *pressRig) waiting(t *testing.T) int {
	t.Helper()
	p, err := r.st.ListPresses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return len(p)
}

func TestPressRecordsAtCouriersTimeAndReturns(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	i := r.submit("UALICE1")
	if err := r.pl.Press(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if len(presses) != 1 || presses[0].InteractionID != "ix-1" || presses[0].User != "UALICE1" || !presses[0].PressedAt.Equal(r.now) || len(r.got) != 1 {
		t.Fatalf("presses %+v, notified %d times; want one, at courier's own time, announced", presses, len(r.got))
	}
	if len(r.eph.list()) != 0 || len(r.sess.answers()) != 0 || r.row(t).State != store.InteractionPending {
		t.Error("recording the press did the work of processing it, which keeps Slack waiting for its acknowledgement")
	}
	if err := r.pl.Press(t.Context(), slack.Interaction{User: "U1", Channel: "C1", MessageTS: "1.2", Value: "feedback-up"}); !errors.Is(err, interact.ErrNotInteraction) {
		t.Errorf("a button that is not an interaction's = %v; want ErrNotInteraction", err)
	}
	_ = r.pl.Press(t.Context(), i)
	if err := r.pl.Press(t.Context(), i); !errors.Is(err, store.ErrLimit) {
		t.Errorf("a third press by one person = %v; want store.ErrLimit", err)
	}
}

func TestAPressOnADecidedQuestionIsToldWhoDecided(t *testing.T) {
	for name, tc := range map[string]struct {
		state store.InteractionState
		want  string
	}{
		"answered":  {store.InteractionAnswered, "UBOB22: <@UALICE1> answered this question first."},
		"timed out": {store.InteractionTimedOut, "UBOB22: This question has timed out."},
		"cancelled": {store.InteractionCancelled, "UBOB22: This question is closed."},
	} {
		t.Run(name, func(t *testing.T) {
			r := newPressRig(t)
			r.open(t)
			if tc.state == store.InteractionAnswered {
				_ = r.st.SubmitInteraction(t.Context(), "ix-1", []byte(`{"request_id":"req-1","answers":[["a"]]}`), "UALICE1", d0.Add(30*time.Second))
			} else {
				_ = r.st.ResolveInteraction(t.Context(), "ix-1", tc.state, deadline)
			}
			if _, err := r.process(t, r.submit("UBOB22")); err != nil || r.waiting(t) != 0 {
				t.Fatalf("Process = %v with %d presses left", err, r.waiting(t))
			}
			if got := r.eph.list(); len(got) != 1 || got[0] != tc.want || len(r.sess.answers()) != 0 {
				t.Errorf("told %q after %d sends; want %q, and nothing else", got, len(r.sess.answers()), tc.want)
			}
		})
	}
}

func TestAnEarlyPressIsTriedAgainForTwoMinutes(t *testing.T) {
	r := newPressRig(t)
	r.pending(t) // the prompt's message is not recorded yet
	if err := r.pl.Press(t.Context(), r.submit("UALICE1")); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	r.now = r.now.Add(2*time.Minute - time.Second)
	if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Retry || r.waiting(t) != 1 || len(r.eph.list()) != 0 {
		t.Fatalf("Process just before 2 minutes = %v, %v, %d waiting, told %q; want Retry with the press kept and no one told", res, err, r.waiting(t), r.eph.list())
	}
	r.now = r.now.Add(time.Second)
	if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done || len(r.eph.list()) != 1 || r.waiting(t) != 0 {
		t.Errorf("Process at 2 minutes = %v, %v, told %q; want the person told to press again, and the press done", res, err, r.eph.list())
	}
}

func TestAPressForNothingOrForAnotherMessageIsDropped(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	other := r.submit("UALICE1")
	other.MessageTS = "999.9" // the value is the interaction's, but the button is on another message
	if res, err := r.process(t, other); err != nil || res != interact.Done || len(r.eph.list()) != 0 || r.waiting(t) != 0 {
		t.Errorf("a press on another message = %v, %v, told %q; want it dropped quietly", res, err, r.eph.list())
	}
	none := r.submit("UALICE1")
	none.Value = "ix-nosuch"
	if _, err := r.process(t, none); err != nil || len(r.eph.list()) != 1 || r.waiting(t) != 0 {
		t.Errorf("a press for no interaction: told %q, %v; want it told the question is gone", r.eph.list(), err)
	}
}

// TestAnOpenPressWaitsForTheChecks: until WP-33d1c2 an open press is kept, undecided.
func TestAnOpenPressWaitsForTheChecks(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	if res, err := r.process(t, r.submit("UALICE1")); err == nil || res != interact.Retry || r.waiting(t) != 1 || r.row(t).State != store.InteractionPending {
		t.Errorf("Process = %v, %v with %d waiting; want Retry, an error, the press kept and nothing decided", res, err, r.waiting(t))
	}
}

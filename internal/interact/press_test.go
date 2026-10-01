package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// authz is who may answer. The zero value says nobody is a member; members and bots are by user.
type authz struct {
	mu                sync.Mutex
	members, bots     map[string]bool
	memberErr, botErr error
	asked             int
}

func (a *authz) IsChannelMember(_ context.Context, _, user string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	return a.members[user] && a.memberErr == nil, a.memberErr
}

func (a *authz) IsBot(_ context.Context, user string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	return a.bots[user] || a.botErr != nil, a.botErr
}

// pressRig is the decider's rig with a pipeline in front of it, on a clock the test moves.
type pressRig struct {
	*decideRig
	az  *authz
	pl  *interact.Pipeline
	now time.Time
	got []store.Press // what Notify was told
}

func newPressRig(t *testing.T) *pressRig {
	t.Helper()
	r := &pressRig{decideRig: newDecideRig(t), az: &authz{members: map[string]bool{"UALICE1": true, "UBOB22": true}}, now: d0.Add(time.Minute)}
	if err := r.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/b", LastActivity: d0}); err != nil {
		t.Fatal(err)
	}
	b := interact.Binding{Dialect: kagentv1.New(time.Second), AskUser: true, Timeout: time.Minute, Identity: "secret:ns/tok"}
	var err error
	r.pl, err = interact.NewPipeline(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{b}, Authorizer: r.az, Ephemerals: r.eph, Decider: r.d, Auditor: r.aud,
		Limits: store.PressLimits{Total: 10, PerUser: 2}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return r.now }, Notify: func(p store.Press) { r.got = append(r.got, p) }})
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

// submit is user's press of the Submit button with choice index idx chosen (-1: nothing chosen).
func (r *pressRig) submit(t *testing.T, user string, idx int) slack.Interaction {
	t.Helper()
	controls, err := interact.Controls("ix-1", question)
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]map[string]slack.Input{}
	if idx >= 0 {
		inputs[controls[0].BlockID] = map[string]slack.Input{controls[0].ActionID: {Type: controls[0].Element, Values: []string{string(rune('0' + idx))}}}
	}
	return slack.Interaction{User: user, Channel: key.Channel, MessageTS: "100.1", ThreadTS: key.ThreadTS, ActionID: "submit", Value: "ix-1", Inputs: inputs}
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
	i := r.submit(t, "UALICE1", 0)
	if err := r.pl.Press(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if len(presses) != 1 || presses[0].InteractionID != "ix-1" || presses[0].User != "UALICE1" || !presses[0].PressedAt.Equal(r.now) || len(r.got) != 1 {
		t.Fatalf("presses %+v, notified %d times; want one, at courier's own time, announced", presses, len(r.got))
	}
	if r.az.asked != 0 || len(r.eph.list()) != 0 || len(r.sess.answers()) != 0 {
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

func TestACompleteAnswerFromAMemberIsDecided(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	if res, err := r.process(t, r.submit(t, "UALICE1", 1)); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v", res, err)
	}
	row := r.row(t)
	var answer struct{ Answers [][]string }
	_ = json.Unmarshal(row.Answer, &answer)
	if row.State != store.InteractionAnswered || row.Decider != "UALICE1" || !row.DecidedAt.Equal(r.now) || len(answer.Answers) != 1 || answer.Answers[0][0] != "b" {
		t.Errorf("interaction = %+v; want answered by UALICE1 at the press's time, with choice 1, which is b", row)
	}
	if len(r.sess.answers()) != 1 || r.waiting(t) != 0 || len(r.eph.list()) != 0 {
		t.Errorf("%d sends, %d presses waiting, told %q; want the answer sent, the press deleted, no one told anything", len(r.sess.answers()), r.waiting(t), r.eph.list())
	}
}

// TestAPressRecordedInTimeIsDecidedEvenIfProcessedLate: the deadline is the press's to meet, not the worker's.
func TestAPressRecordedInTimeIsDecidedEvenIfProcessedLate(t *testing.T) {
	r := newPressRig(t)
	r.open(t) // due at d0 + 10 minutes
	i := r.submit(t, "UALICE1", 0)
	if err := r.pl.Press(t.Context(), i); err != nil { // recorded at d0 + 1 minute
		t.Fatal(err)
	}
	r.now = deadline.Add(time.Hour) // the worker gets to it long after
	presses, _ := r.st.ListPresses(t.Context())
	if _, err := r.pl.Process(t.Context(), presses[0]); err != nil || r.row(t).State != store.InteractionAnswered || len(r.sess.answers()) != 1 {
		t.Errorf("Process = %v, state %s, %d sends; want the in-time press decided and sent", err, r.row(t).State, len(r.sess.answers()))
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
			if _, err := r.process(t, r.submit(t, "UBOB22", 0)); err != nil || r.waiting(t) != 0 {
				t.Fatalf("Process = %v with %d presses left", err, r.waiting(t))
			}
			if got := r.eph.list(); len(got) != 1 || got[0] != tc.want || r.az.asked != 0 || len(r.sess.answers()) != 0 || len(r.aud.got) != 0 {
				t.Errorf("told %q after %d authorization checks, %d sends and %d audit rows; want %q, and nothing else: a press that lost is no decision to audit", got, r.az.asked, len(r.sess.answers()), len(r.aud.got), tc.want)
			}
		})
	}
}

// TestTheWinnerPressingAgainSendsAgain: after a crash the same person's press finds its answer recorded,
// and goes to the decider, which sends it again without a second decision.
func TestTheWinnerPressingAgainSendsAgain(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	if _, err := r.process(t, r.submit(t, "UALICE1", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.process(t, r.submit(t, "UALICE1", 0)); err != nil || len(r.sess.answers()) != 2 || len(r.eph.list()) != 0 || len(r.aud.got) != 1 {
		t.Errorf("Process = %v, %d sends, told %q, %d audit rows; want the answer sent again, nobody told, one decision audited", err, len(r.sess.answers()), r.eph.list(), len(r.aud.got))
	}
}

func TestOnlyMembersWhoAreNotBotsAreDecided(t *testing.T) {
	for name, tc := range map[string]struct {
		mod    func(*authz)
		user   string
		want   string // what the person is told; empty: nothing
		denied int    // audit rows: a refusal of a person is a decision
	}{
		"not a member": {func(*authz) {}, "UOUTSIDE", "UOUTSIDE: Only members of this channel can answer this question.", 1},
		"a bot":        {func(a *authz) { a.bots = map[string]bool{"UALICE1": true} }, "UALICE1", "", 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newPressRig(t)
			r.open(t)
			tc.mod(r.az)
			if res, err := r.process(t, r.submit(t, tc.user, 0)); err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v", res, err)
			}
			got := strings.Join(r.eph.list(), "|")
			if got != tc.want || r.row(t).State != store.InteractionPending || len(r.sess.answers()) != 0 || len(r.aud.got) != tc.denied || r.waiting(t) != 0 {
				t.Errorf("told %q, state %s, %d sends, %d audit rows; want %q, the question still open, nothing sent, %d rows, the press done", got, r.row(t).State, len(r.sess.answers()), len(r.aud.got), tc.want, tc.denied)
			}
			if tc.denied == 1 && (r.aud.got[0].Result != audit.DecisionDenied || r.aud.act[0] != (audit.Actor{User: tc.user, Binding: "ns/b", Identity: "secret:ns/tok"}) || !r.aud.got[0].At.Equal(r.now)) {
				t.Errorf("audit row %+v by %+v; want a denied decision by the person, at the press's time, under the Binding", r.aud.got[0], r.aud.act[0])
			}
		})
	}
}

// TestACheckThatFailsKeepsThePressWhateverItsAge: a check that cannot be made is no decision. A users-API
// outage that straddles the deadline must not lose an answer that came in time, so the press is kept for the
// worker to try again, at any age: ending it is the worker's cap (GiveUp), and not a window of the pipeline's
// own, which would end it, with the person told the check failed, long before the cap.
func TestACheckThatFailsKeepsThePressWhateverItsAge(t *testing.T) {
	for name, mod := range map[string]func(*authz){
		"membership cannot be read": func(a *authz) { a.memberErr = errBoom },
		"bot status cannot be read": func(a *authz) { a.botErr = errBoom },
	} {
		t.Run(name, func(t *testing.T) {
			r := newPressRig(t)
			r.open(t)
			mod(r.az)
			if err := r.pl.Press(t.Context(), r.submit(t, "UALICE1", 0)); err != nil {
				t.Fatal(err)
			}
			presses, _ := r.st.ListPresses(t.Context())
			pressed := r.now
			for _, age := range []time.Duration{2*time.Minute - time.Second, 2*time.Minute + time.Second, 59 * time.Minute, 24 * time.Hour} {
				r.now = pressed.Add(age)
				if res, err := r.pl.Process(t.Context(), presses[0]); !errors.Is(err, errBoom) || res != interact.Retry || r.waiting(t) != 1 || len(r.eph.list()) != 0 || len(r.aud.got) != 0 {
					t.Fatalf("Process at %v = %v, %v, %d waiting, told %q; want Retry with the error, the press kept, no one told and nothing audited", age, res, err, r.waiting(t), r.eph.list())
				}
			}
			if len(r.sess.answers()) != 0 {
				t.Errorf("%d answers sent; want none while the person cannot be checked", len(r.sess.answers()))
			}
		})
	}
}

// TestAnIncompleteAnswerIsToldAndNeverUpdatesThePrompt: there is no way for the pipeline to edit a message
// (it holds no Updater), so an incomplete press is only an ephemeral and the others' inputs are kept.
func TestAnIncompleteAnswerIsToldAndNeverUpdatesThePrompt(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	if _, err := r.process(t, r.submit(t, "UALICE1", -1)); err != nil {
		t.Fatal(err)
	}
	if got := r.eph.list(); len(got) != 1 || got[0] != "UALICE1: Please answer question 1, then press Submit again." || r.row(t).State != store.InteractionPending || r.waiting(t) != 0 || len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionIncomplete || r.aud.got[0].Answer == nil || len(r.aud.got[0].Answer.Answers) != 0 {
		t.Errorf("told %q, state %s, %d presses waiting, audit %+v; want the question number told, the question open, the press done, an incomplete decision with its shape and no values", got, r.row(t).State, r.waiting(t), r.aud.got)
	}
	if _, err := r.process(t, r.submit(t, "UALICE1", 7)); err != nil || !strings.Contains(r.eph.list()[1], "Question 1 could not be read") {
		t.Errorf("an index that is not a choice: told %q, %v; want it called unreadable, by number", r.eph.list(), err)
	}
}

func TestAnEarlyPressIsTriedAgainForTwoMinutes(t *testing.T) {
	r := newPressRig(t)
	r.pending(t) // the prompt's message is not recorded yet
	if err := r.pl.Press(t.Context(), r.submit(t, "UALICE1", 0)); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	r.now = r.now.Add(2*time.Minute - time.Second)
	if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Retry || r.waiting(t) != 1 || len(r.eph.list()) != 0 {
		t.Fatalf("Process just before 2 minutes = %v, %v, %d waiting, told %q; want Retry with the press kept and no one told", res, err, r.waiting(t), r.eph.list())
	}
	_ = r.st.SetInteractionMessageRef(t.Context(), "ix-1", "100.1") // the ref is recorded in time: it is decided
	if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done || r.row(t).State != store.InteractionAnswered {
		t.Errorf("Process once the ref is recorded = %v, %v, state %s; want it decided", res, err, r.row(t).State)
	}
	r2 := newPressRig(t)
	r2.pending(t)
	_ = r2.pl.Press(t.Context(), r2.submit(t, "UALICE1", 0))
	presses, _ = r2.st.ListPresses(t.Context())
	r2.now = r2.now.Add(2 * time.Minute)
	if res, err := r2.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done || len(r2.eph.list()) != 1 || r2.waiting(t) != 0 {
		t.Errorf("Process at 2 minutes = %v, %v, told %q; want the person told to press again, and the press done", res, err, r2.eph.list())
	}
}

func TestAPressForNothingOrForAnotherMessageIsDropped(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	other := r.submit(t, "UALICE1", 0)
	other.MessageTS = "999.9" // the value is the interaction's, but the button is on another message
	if res, err := r.process(t, other); err != nil || res != interact.Done || len(r.eph.list()) != 0 || len(r.sess.answers()) != 0 || r.waiting(t) != 0 {
		t.Errorf("a press on another message = %v, %v, told %q; want it dropped quietly", res, err, r.eph.list())
	}
	elsewhere := r.submit(t, "UALICE1", 0)
	elsewhere.Channel = "C9" // the message's ts is the prompt's, but it is in another channel
	if res, err := r.process(t, elsewhere); err != nil || res != interact.Done || len(r.eph.list()) != 0 || len(r.sess.answers()) != 0 || r.waiting(t) != 0 {
		t.Errorf("a press in another channel = %v, %v, told %q; want it dropped quietly", res, err, r.eph.list())
	}
	none := r.submit(t, "UALICE1", 0)
	none.Value = "ix-nosuch"
	if _, err := r.process(t, none); err != nil || len(r.eph.list()) != 1 || !strings.Contains(r.eph.list()[0], "no longer available") {
		t.Errorf("a press for no interaction: told %q, %v; want it told the question is gone", r.eph.list(), err)
	}
}

// TestAFailedDecisionLeavesThePressToBeDecidedAgain: the press is deleted only once the decider has
// returned nil, so a send that fails is tried again, as the same person's retry, without a second decision.
func TestAFailedDecisionLeavesThePressToBeDecidedAgain(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	r.sess.errs = []error{errBoom}
	i := r.submit(t, "UALICE1", 0)
	if err := r.pl.Press(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if res, err := r.pl.Process(t.Context(), presses[0]); !errors.Is(err, errBoom) || res != interact.Retry || r.waiting(t) != 1 {
		t.Fatalf("Process with a failing send = %v, %v, %d presses waiting; want Retry, the error, and the press kept", res, err, r.waiting(t))
	}
	if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done || r.waiting(t) != 0 || len(r.sess.answers()) != 2 || len(r.aud.got) != 1 {
		t.Errorf("Process again = %v, %v, %d waiting, %d sends, %d audit rows; want it done, the answer sent again, one decision audited", res, err, r.waiting(t), len(r.sess.answers()), len(r.aud.got))
	}
}

// TestARefusalIsAuditedBeforeThePersonIsToldAndNotWithoutIt: the audit comes first, and a failed write
// returns Retry and keeps the press, so the person is told only once the refusal is on record.
func TestARefusalIsAuditedBeforeThePersonIsToldAndNotWithoutIt(t *testing.T) {
	for name, tc := range map[string]struct {
		user  string
		input int
	}{"not a member": {"UOUTSIDE", 0}, "incomplete": {"UALICE1", -1}} {
		t.Run(name, func(t *testing.T) {
			r := newPressRig(t)
			r.open(t)
			r.aud.err = errBoom
			if res, err := r.process(t, r.submit(t, tc.user, tc.input)); !errors.Is(err, errBoom) || res != interact.Retry || r.waiting(t) != 1 || len(r.eph.list()) != 0 {
				t.Fatalf("Process with a failing audit = %v, %v, %d waiting, told %q; want Retry, the error, the press kept and no one told", res, err, r.waiting(t), r.eph.list())
			}
			r.aud.err = nil
			presses, _ := r.st.ListPresses(t.Context())
			if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done || len(r.eph.list()) != 1 || len(r.aud.got) != 2 {
				t.Errorf("Process again = %v, %v, told %q, %d audit attempts; want it done and told once", res, err, r.eph.list(), len(r.aud.got))
			}
		})
	}
}

// TestThePressLimitsDefaultToAThousandInAllAndTenForOnePerson.
func TestThePressLimitsDefaultToAThousandInAllAndTenForOnePerson(t *testing.T) {
	r := newPressRig(t)
	pl, err := interact.NewPipeline(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{}, Authorizer: r.az, Ephemerals: r.eph, Decider: r.d, Auditor: r.aud})
	if err != nil {
		t.Fatal(err)
	}
	press := func(user string) error {
		return pl.Press(t.Context(), slack.Interaction{User: user, Channel: "C1", MessageTS: "100.1", Value: "ix-1"})
	}
	for i := range 10 {
		if err := press("U0"); err != nil {
			t.Fatalf("press %d by one person: %v", i+1, err)
		}
	}
	if err := press("U0"); !errors.Is(err, store.ErrLimit) {
		t.Errorf("an eleventh press by one person = %v; want store.ErrLimit", err)
	}
	for u := 1; u < 100; u++ {
		for range 10 {
			if err := press(fmt.Sprintf("U%d", u)); err != nil {
				t.Fatalf("a press by U%d: %v", u, err)
			}
		}
	}
	if err := press("UOTHER"); !errors.Is(err, store.ErrLimit) {
		t.Errorf("the 1001st press = %v; want store.ErrLimit", err)
	}
}

// TestGivingUpTellsThePersonToPressAgainAndDeletesThePress: a press the worker cannot get through is not
// kept for ever, and pressing again is the way on: an answer already recorded is sent again, one that is
// not is decided.
func TestGivingUpTellsThePersonToPressAgainAndDeletesThePress(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	gone := r.submit(t, "UBOB22", 0)
	gone.Value = "ix-nosuch"
	for _, i := range []slack.Interaction{r.submit(t, "UALICE1", 0), gone} {
		if err := r.pl.Press(t.Context(), i); err != nil {
			t.Fatal(err)
		}
	}
	presses, _ := r.st.ListPresses(t.Context())
	for _, p := range presses {
		if err := r.pl.GiveUp(t.Context(), p); err != nil {
			t.Fatalf("GiveUp = %v", err)
		}
	}
	want := []string{"UALICE1: Couldn't process your answer. Please press Submit again.", "UBOB22: Couldn't process your answer. Please press Submit again."}
	if got := r.eph.list(); !slices.Equal(got, want) || r.waiting(t) != 0 || r.row(t).State != store.InteractionPending || len(r.sess.answers()) != 0 {
		t.Errorf("told %q, %d presses waiting; want both told to press again, both presses deleted, nothing decided or sent", got, r.waiting(t))
	}
}

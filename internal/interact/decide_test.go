package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// These tests run far in the past, so that the real clock is always past every deadline: a decision
// that took its time from the clock and not from the press would be refused as expired.
var (
	d0       = time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	deadline = d0.Add(10 * time.Minute)
)

// orderedStore logs the submits, so the order of the steps can be asserted against the audit rows.
type orderedStore struct {
	*sqlite.Store
	log *callLog
}

func (s *orderedStore) SubmitInteraction(ctx context.Context, id string, answer []byte, decider string, now time.Time) error {
	s.log.add("submit")
	return s.Store.SubmitInteraction(ctx, id, answer, decider, now)
}

// orderedAudit logs each decision written, with its result.
type orderedAudit struct {
	*audits
	log *callLog
}

func (a *orderedAudit) Decision(ctx context.Context, actor audit.Actor, d audit.Decision) error {
	a.log.add("audit:" + string(d.Result))
	return a.audits.Decision(ctx, actor, d)
}

// ephemerals records what each person was told alone.
type ephemerals struct {
	mu  sync.Mutex
	got []string // "user: text"
	err error
}

func (e *ephemerals) PostEphemeral(_ context.Context, _, _, user string, c slack.Content) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, user+": "+c.Text)
	return e.err
}

func (e *ephemerals) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.got...)
}

type decideRig struct {
	st    *orderedStore
	aud   *orderedAudit
	eph   *ephemerals
	calls *callLog
	d     *interact.Decider
	mu    sync.Mutex
	hooks []string
}

func newDecideRig(t *testing.T) *decideRig {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &decideRig{calls: &callLog{}, eph: &ephemerals{}}
	r.st = &orderedStore{Store: st, log: r.calls}
	r.aud = &orderedAudit{audits: &audits{}, log: r.calls}
	r.d, err = interact.NewDecider(interact.DecideConfig{
		Store: r.st, Auditor: r.aud, Ephemerals: r.eph, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnDecision: func(result string) { r.mu.Lock(); r.hooks = append(r.hooks, result); r.mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *decideRig) hooked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hooks...)
}

// question is the interaction every test decides: one question with two choices.
var question = dialect.Interaction{Kind: dialect.AskUser, TaskID: "task-1", RequestID: "req-1",
	Questions: []dialect.Question{{Text: "Which?", Choices: []string{"a", "b"}}}}

func (r *decideRig) pending(t *testing.T) store.Interaction {
	t.Helper()
	req, _ := json.Marshal(question)
	row := store.Interaction{ID: "ix-1", Key: key, TaskID: "task-1", Kind: store.InteractionAskUser, Request: req, CreatedAt: d0, Deadline: deadline}
	if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	return r.row(t)
}

func (r *decideRig) row(t *testing.T) store.Interaction {
	t.Helper()
	row, err := r.st.GetInteraction(t.Context(), "ix-1")
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// press is a decision by user, who pressed at pressedAt and chose a.
func (r *decideRig) press(t *testing.T, user string, pressedAt time.Time, requestID string) interact.PressDecision {
	t.Helper()
	return interact.PressDecision{
		Interaction: r.row(t), In: question, Answer: dialect.Answer{RequestID: requestID, Answers: [][]string{{"a"}}},
		User: user, PressedAt: pressedAt, BindingName: "ns/b",
		Binding: interact.Binding{Dialect: kagentv1.New(time.Second), AskUser: true, Timeout: time.Minute, Identity: "secret:ns/tok"},
	}
}

func TestDecideBuildsThenAuditsThenSubmits(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	at := d0.Add(time.Minute)
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", at, "req-1")); err != nil {
		t.Fatal(err)
	}
	if got, want := r.calls.list(), []string{"audit:answered", "submit"}; !slices.Equal(got, want) {
		t.Errorf("calls %q; want the decision audited before it takes effect: %q", got, want)
	}
	row := r.row(t)
	var stored dialect.Answer
	_ = json.Unmarshal(row.Answer, &stored)
	if row.State != store.InteractionAnswered || row.Decider != "UALICE1" || !row.DecidedAt.Equal(at) || len(stored.Answers) != 1 || stored.Answers[0][0] != "a" {
		t.Errorf("interaction = %+v; want it answered by UALICE1 at the press's time with the answer", row)
	}
	if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionAnswered || r.aud.got[0].TaskID != "task-1" || !r.aud.got[0].At.Equal(at) || r.aud.got[0].Answer == nil ||
		r.aud.act[0] != (audit.Actor{User: "UALICE1", Binding: "ns/b", Identity: "secret:ns/tok"}) {
		t.Errorf("audit %+v by %+v; want one answered decision by UALICE1 under the Binding and its identity, with the answer's shape", r.aud.got, r.aud.act)
	}
	if got := r.hooked(); !slices.Equal(got, []string{"answered"}) || len(r.eph.list()) != 0 {
		t.Errorf("hooks %q, ephemerals %q; want one answered and nothing said", got, r.eph.list())
	}
}

// TestAnInTimePressDecidedAfterTheDeadlineStillWins: contract (b) with the sweep. The press was
// recorded before the deadline, and is decided after it (the real clock is years later).
func TestAnInTimePressDecidedAfterTheDeadlineStillWins(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", deadline.Add(-time.Second), "req-1")); err != nil {
		t.Fatal(err)
	}
	if r.row(t).State != store.InteractionAnswered {
		t.Fatal("a press made before the deadline lost because it was decided after it")
	}
}

// TestALatePressIsToldSoAndNothingElse: made at or after the deadline, it is no decision at all,
// so it is not audited as one, and nothing is submitted.
func TestALatePressIsToldSoAndNothingElse(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", deadline, "req-1")); err != nil {
		t.Fatal(err)
	}
	if len(r.calls.list()) != 0 || r.row(t).State != store.InteractionPending || len(r.aud.got) != 0 {
		t.Errorf("calls %q, state %v; want no audit and no submit", r.calls.list(), r.row(t).State)
	}
	if got := r.eph.list(); len(got) != 1 || !strings.HasPrefix(got[0], "UALICE1: ") || !strings.Contains(got[0], "timed out") || !slices.Equal(r.hooked(), []string{"late"}) {
		t.Errorf("ephemerals %q, hooks %q; want UALICE1 told it timed out, and one late", got, r.hooked())
	}
}

// TestAnAnswerTheDialectRefusesIsNotRecorded: the message is built first, so an answer that cannot
// be sent leaves the interaction pending, and the person can answer again.
func TestAnAnswerTheDialectRefusesIsNotRecorded(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "another-request")); err != nil {
		t.Fatal(err)
	}
	if len(r.calls.list()) != 0 || r.row(t).State != store.InteractionPending {
		t.Errorf("calls %q, state %v; want nothing audited or submitted for an answer that cannot be sent", r.calls.list(), r.row(t).State)
	}
	if got := r.eph.list(); len(got) != 1 || !strings.Contains(got[0], "could not be used") || !slices.Equal(r.hooked(), []string{"invalid"}) {
		t.Errorf("ephemerals %q, hooks %q; want UALICE1 told to try again, and one invalid", got, r.hooked())
	}
}

// TestAFailedAuditStopsTheDecision: an unaudited decision never takes effect, so the press is left
// to be tried again, and nothing was submitted or said.
func TestAFailedAuditStopsTheDecision(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	r.aud.err = errBoom
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); !errors.Is(err, errBoom) {
		t.Fatalf("Decide = %v; want the audit's error, for the press to be tried again", err)
	}
	if got := r.calls.list(); !slices.Equal(got, []string{"audit:answered"}) || r.row(t).State != store.InteractionPending || len(r.hooked()) != 0 || len(r.eph.list()) != 0 {
		t.Errorf("calls %q, state %v, hooks %q; want only the failed audit", got, r.row(t).State, r.hooked())
	}
	r.aud.err = nil // the retry
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil || r.row(t).State != store.InteractionAnswered {
		t.Fatalf("the retry = %v, state %v; want it answered", err, r.row(t).State)
	}
}

// TestAPressAfterAnotherAnswerIsSuperseded names the winner to the loser, audits the loss, submits
// nothing, and sends nothing for it.
func TestAPressAfterAnotherAnswerIsSuperseded(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	_ = r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1"))
	if err := r.d.Decide(t.Context(), r.press(t, "UBOB22", d0.Add(2*time.Minute), "req-1")); err != nil {
		t.Fatal(err)
	}
	if got := r.eph.list(); len(got) != 1 || got[0] != "UBOB22: <@UALICE1> answered this question first." {
		t.Errorf("ephemerals %q; want U2 told that UALICE1 answered first", got)
	}
	if got, want := r.calls.list(), []string{"audit:answered", "submit", "audit:superseded"}; !slices.Equal(got, want) {
		t.Errorf("calls %q; want the loser audited as superseded and nothing submitted for it: %q", got, want)
	}
	if r.row(t).Decider != "UALICE1" || !slices.Equal(r.hooked(), []string{"answered", "superseded"}) {
		t.Errorf("decider %q, hooks %q; want UALICE1 kept, and one answered and one superseded", r.row(t).Decider, r.hooked())
	}
}

// TestTwoPressesAtOnceGiveOneAnswer: decisions are taken one at a time, so the second reads the
// first's answer: one answered row, one answered audit and one superseded audit.
func TestTwoPressesAtOnceGiveOneAnswer(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	var wg sync.WaitGroup
	for _, user := range []string{"UALICE1", "UBOB22"} {
		pd := r.press(t, user, d0.Add(time.Minute), "req-1")
		wg.Go(func() {
			if err := r.d.Decide(t.Context(), pd); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	row := r.row(t)
	results := map[audit.DecisionResult]int{}
	for _, d := range r.aud.got {
		results[d.Result]++
	}
	if row.State != store.InteractionAnswered || results[audit.DecisionAnswered] != 1 || results[audit.DecisionSuperseded] != 1 || len(r.aud.got) != 2 {
		t.Errorf("state %v, audits %v; want one answered row, one answered audit and one superseded", row.State, results)
	}
	loser := "UALICE1"
	if row.Decider == "UALICE1" {
		loser = "UBOB22"
	}
	if got := r.eph.list(); len(got) != 1 || got[0] != loser+": <@"+row.Decider+"> answered this question first." {
		t.Errorf("ephemerals %q; want the loser %s told who won", got, loser)
	}
}

// TestTheSameUserAgainIsARetryNotALoss: after a crash the press is decided again. The person already
// won, so they are not told they lost, nothing is audited or counted again, and the call goes on (to
// the send, in WP-33d2b).
func TestTheSameUserAgainIsARetryNotALoss(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	_ = r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1"))
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil {
		t.Fatal(err)
	}
	if len(r.aud.got) != 1 || len(r.eph.list()) != 0 || !slices.Equal(r.hooked(), []string{"answered"}) || len(r.calls.list()) != 2 {
		t.Errorf("audits %d, ephemerals %q, hooks %q, calls %q; want the first decision alone", len(r.aud.got), r.eph.list(), r.hooked(), r.calls.list())
	}
}

// TestAQuestionThatIsClosedSaysSo: the sweep timed it out, or it was cancelled.
func TestAQuestionThatIsClosedSaysSo(t *testing.T) {
	for state, want := range map[store.InteractionState]string{store.InteractionTimedOut: "timed out", store.InteractionCancelled: "closed"} {
		r := newDecideRig(t)
		r.pending(t)
		if err := r.st.ResolveInteraction(t.Context(), "ix-1", state, deadline); err != nil {
			t.Fatal(err)
		}
		if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil {
			t.Fatal(err)
		}
		if got := r.eph.list(); len(got) != 1 || !strings.Contains(got[0], want) || !slices.Equal(r.calls.list(), []string{"audit:superseded"}) {
			t.Errorf("%s: ephemerals %q, calls %q; want it said to be %q, audited as superseded, and nothing submitted", state, got, r.calls.list(), want)
		}
	}
}

// TestAnEphemeralThatFailsDoesNotFailTheDecision: it is told to one person and kept nowhere.
func TestAnEphemeralThatFailsDoesNotFailTheDecision(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	r.eph.err = errBoom
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", deadline, "req-1")); err != nil {
		t.Fatalf("Decide = %v; want a refused late press to be done even if it could not be told", err)
	}
}

func TestNewDeciderNeedsItsParts(t *testing.T) {
	if _, err := interact.NewDecider(interact.DecideConfig{}); err == nil {
		t.Error("NewDecider accepted no Store, Auditor or Ephemerals")
	}
}

package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
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
	log          *callLog
	all          *callLog // every step of the decision, for its order
	beforeSubmit func()   // runs between the decider's read and its write, as another process or the sweep would
}

func (s *orderedStore) SubmitInteraction(ctx context.Context, id string, answer []byte, decider string, now time.Time) error {
	s.log.add("submit")
	s.all.add("submit")
	if s.beforeSubmit != nil {
		s.beforeSubmit()
	}
	return s.Store.SubmitInteraction(ctx, id, answer, decider, now)
}

// orderedAudit logs each decision written, with its result.
type orderedAudit struct {
	*audits
	log *callLog
	all *callLog
	// gate, if set, holds the first Decision inside until it is closed, after entered is closed.
	gate, entered chan struct{}
	first         atomic.Bool
}

func (a *orderedAudit) Decision(ctx context.Context, actor audit.Actor, d audit.Decision) error {
	a.log.add("audit:" + string(d.Result))
	a.all.add("audit:" + string(d.Result))
	if a.gate != nil && a.first.CompareAndSwap(false, true) {
		close(a.entered)
		<-a.gate
	}
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

// answerSess is the session manager that takes the answers. err is returned by the next Answers, one each.
type answerSess struct {
	mu   sync.Mutex
	got  []*a2a.Message
	keys []store.ConversationKey
	errs []error
	log  *callLog
}

func (s *answerSess) Answer(_ context.Context, k store.ConversationKey, m *a2a.Message) error {
	s.log.add("send")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got, s.keys = append(s.got, m), append(s.keys, k)
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return err
	}
	return nil
}

func (s *answerSess) Cancel(context.Context, store.ConversationKey, a2a.TaskID, string) error {
	return nil
}

func (s *answerSess) answers() []*a2a.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*a2a.Message(nil), s.got...)
}

// enqueued records the posts the decider enqueues.
type enqueued struct {
	mu  sync.Mutex
	got []outbox.Post
}

func (e *enqueued) Enqueue(_ context.Context, p outbox.Post) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, p)
	return outbox.ID(p.Source, p.Role), nil
}

func (e *enqueued) list() []outbox.Post {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]outbox.Post(nil), e.got...)
}

// promptReplacer records the prompts the decider has replaced; err fails them.
type promptReplacer struct {
	mu  sync.Mutex
	ids []string
	err error
	log *callLog
}

func (p *promptReplacer) Replace(_ context.Context, id string) error {
	p.log.add("replace")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, id)
	return p.err
}

func (p *promptReplacer) replaced() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ids...)
}

type decideRig struct {
	st    *orderedStore
	aud   *orderedAudit
	eph   *ephemerals
	calls *callLog
	flow  *callLog // calls with the send and the replacement too
	logs  *logBuf
	sess  *answerSess
	rep   *promptReplacer
	out   *enqueued
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
	r := &decideRig{calls: &callLog{}, flow: &callLog{}, logs: &logBuf{}, eph: &ephemerals{}, sess: &answerSess{}, out: &enqueued{}, rep: &promptReplacer{}}
	r.sess.log, r.rep.log = r.flow, r.flow
	r.st = &orderedStore{Store: st, log: r.calls, all: r.flow}
	r.aud = &orderedAudit{audits: &audits{}, log: r.calls, all: r.flow}
	r.d, err = interact.NewDecider(interact.DecideConfig{
		Store: r.st, Auditor: r.aud, Ephemerals: r.eph, Session: r.sess, Sessions: r.st, Replacer: r.rep, Outbox: r.out, Logger: slog.New(slog.NewTextHandler(r.logs, nil)),
		OnDecision: func(result string) { r.mu.Lock(); r.hooks = append(r.hooks, result); r.mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// task records which task the thread's session is on now: "" once it has ended.
func (r *decideRig) task(t *testing.T, id string) {
	t.Helper()
	if err := r.st.UpsertSession(t.Context(), store.Session{Key: key, ContextID: "ctx-1", TaskID: id, Binding: "ns/b", LastActivity: d0}); err != nil {
		t.Fatal(err)
	}
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
	if got := len(r.sess.answers()); got != 1 {
		t.Errorf("%d answers sent; want exactly one", got)
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
	if len(r.aud.got) != 1 || len(r.eph.list()) != 0 || !slices.Equal(r.hooked(), []string{"answered"}) || !slices.Equal(r.flow.list(), []string{"audit:answered", "submit", "send", "replace", "send", "replace"}) {
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
		t.Error("NewDecider accepted no Store, Auditor, Ephemerals, Session or Outbox")
	}
}

// TestTheWinnersAnswerIsSentOnce: to the paused task of the thread, with the interaction's message ID, so
// that sending it again is the same message; and only the winner's, however many press.
func TestTheWinnersAnswerIsSentOnce(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	_ = r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1"))
	_ = r.d.Decide(t.Context(), r.press(t, "UBOB22", d0.Add(2*time.Minute), "req-1")) // lost
	_ = r.d.Decide(t.Context(), r.press(t, "UCAROL3", deadline, "req-1"))             // late
	_ = r.d.Decide(t.Context(), r.press(t, "UDAVE44", d0.Add(time.Minute), "wrong"))  // refused
	got := r.sess.answers()
	if len(got) != 1 || got[0].ID != "ix-1/answer" || got[0].TaskID != "task-1" || r.sess.keys[0] != key || got[0].Metadata[kagentv1.HITLExtension] == nil {
		t.Fatalf("answers sent %+v; want the winner's alone, for task-1 in the thread, under the interaction's message ID", got)
	}
	if len(r.out.list()) != 0 {
		t.Errorf("notices %+v; want none for an answer that was sent", r.out.list())
	}
}

// TestAnAnswerForATaskThatMovedOnIsToldInTheThread: the question was closed before the answer arrived.
// It is one notice, with an ID from the interaction, however often the press is decided again.
func TestAnAnswerForATaskThatMovedOnIsToldInTheThread(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	r.task(t, "task-2") // the thread has moved on to another task
	r.sess.errs = []error{session.ErrNotPaused}
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil {
		t.Fatalf("Decide = %v; want the press done: the task is not paused, and no retry would change that", err)
	}
	posts := r.out.list()
	if len(posts) != 1 || posts[0].Source != "ix-1/answer" || posts[0].Role != store.OutboxNotice || posts[0].Channel != key.Channel || posts[0].ThreadTS != key.ThreadTS || posts[0].Content.Text == "" {
		t.Errorf("notices %+v; want one, in the thread, from the interaction", posts)
	}
	if r.row(t).State != store.InteractionAnswered {
		t.Error("the interaction is no longer answered")
	}
}

// TestASendThatFailsIsRetriedWithoutAnotherDecision: the answer was recorded and the send failed. The press
// is decided again by the same person, who is not told they lost, and the send is made, with no second
// audit row and no second count.
func TestASendThatFailsIsRetriedWithoutAnotherDecision(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	r.sess.errs = []error{errBoom}
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); !errors.Is(err, errBoom) {
		t.Fatalf("Decide = %v; want the send's error, for the press to be tried again", err)
	}
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil {
		t.Fatal(err)
	}
	if got := r.sess.answers(); len(got) != 2 || got[0].ID != got[1].ID {
		t.Errorf("sends %d; want the failed one and its retry, as the same message", len(got))
	}
	if len(r.aud.got) != 1 || len(r.eph.list()) != 0 || !slices.Equal(r.hooked(), []string{"answered"}) || len(r.out.list()) != 0 {
		t.Errorf("audits %d, ephemerals %q, hooks %q, notices %d; want the decision once and nothing said", len(r.aud.got), r.eph.list(), r.hooked(), len(r.out.list()))
	}
}

// TestARetriedSendForATaskThatMovedOnSaysNothing: after a crash it cannot be told whether the answer got
// through before, so no notice claims that it did not.
func TestARetriedSendForATaskThatMovedOnSaysNothing(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	r.sess.errs = []error{errBoom, session.ErrNotPaused}
	_ = r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1"))
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil || len(r.out.list()) != 0 {
		t.Errorf("Decide = %v with %d notices; want it done, and silent", err, len(r.out.list()))
	}
}

// TestALatePressOnAQuestionTheSweepTimedOutIsJustLate: the sweep resolved it, and the press, made after
// the deadline, is no decision: it is told so and not audited as one lost.
func TestALatePressOnAQuestionTheSweepTimedOutIsJustLate(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	if err := r.st.ResolveInteraction(t.Context(), "ix-1", store.InteractionTimedOut, deadline); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", deadline.Add(time.Minute), "req-1")); err != nil {
		t.Fatal(err)
	}
	if len(r.calls.list()) != 0 || !slices.Equal(r.hooked(), []string{"late"}) || len(r.eph.list()) != 1 {
		t.Errorf("calls %q, hooks %q, ephemerals %q; want one late, told, and no audit", r.calls.list(), r.hooked(), r.eph.list())
	}
}

// TestTwoPressesAreTakenOneAtATime holds the first press inside its audit write. The second arrives
// and must wait for it: without that, both audit as the winner.
func TestTwoPressesAreTakenOneAtATime(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	r.aud.gate, r.aud.entered = make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { _ = r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")) })
	<-r.aud.entered
	wg.Go(func() { _ = r.d.Decide(t.Context(), r.press(t, "UBOB22", d0.Add(time.Minute), "req-1")) })
	time.Sleep(50 * time.Millisecond) // the second gets as far as it can while the first is held
	close(r.aud.gate)
	wg.Wait()
	results := map[audit.DecisionResult]int{}
	for _, d := range r.aud.got {
		results[d.Result]++
	}
	if results[audit.DecisionAnswered] != 1 || results[audit.DecisionSuperseded] != 1 || len(r.aud.got) != 2 || r.row(t).Decider != "UALICE1" {
		t.Errorf("audits %v, decider %q; want the first press the winner, and one answered and one superseded", results, r.row(t).Decider)
	}
}

// TestAWinnerThatIsNoSlackIDIsNotNamed: what names the winner to the loser is a mention, so only
// something shaped like a Slack user ID is put in one.
func TestAWinnerThatIsNoSlackIDIsNotNamed(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	_ = r.d.Decide(t.Context(), r.press(t, "<!channel>", d0.Add(time.Minute), "req-1"))
	_ = r.d.Decide(t.Context(), r.press(t, "UBOB22", d0.Add(2*time.Minute), "req-1"))
	if got := r.eph.list(); len(got) != 1 || got[0] != "UBOB22: Someone else answered this question first." {
		t.Errorf("ephemerals %q; want the winner unnamed", got)
	}
}

// TestADecisionTakenBetweenTheReadAndTheWrite: another process answered, or the sweep timed the question
// out, after the decider read it pending. The write is refused, and the press lost: it is audited as
// superseded (after the answered row written before the write, which stands for a decision that never took
// effect) and told so.
func TestADecisionTakenBetweenTheReadAndTheWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		intervene func(*decideRig, *testing.T)
		wantTold  string
	}{
		"another answer": {func(r *decideRig, t *testing.T) {
			if err := r.st.Store.SubmitInteraction(t.Context(), "ix-1", []byte(`{"RequestID":"req-1"}`), "UROB333", d0.Add(30*time.Second)); err != nil {
				t.Fatal(err)
			}
		}, "<@UROB333> answered this question first."},
		"the sweep": {func(r *decideRig, t *testing.T) {
			if err := r.st.ResolveInteraction(t.Context(), "ix-1", store.InteractionTimedOut, deadline); err != nil {
				t.Fatal(err)
			}
		}, "This question has timed out."},
	} {
		t.Run(name, func(t *testing.T) {
			r := newDecideRig(t)
			r.pending(t)
			r.st.beforeSubmit = func() { r.st.beforeSubmit = nil; tc.intervene(r, t) }
			if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil {
				t.Fatal(err)
			}
			if got, want := r.calls.list(), []string{"audit:answered", "submit", "audit:superseded"}; !slices.Equal(got, want) {
				t.Errorf("calls %q; want %q", got, want)
			}
			if got := r.eph.list(); len(got) != 1 || got[0] != "UALICE1: "+tc.wantTold || !slices.Equal(r.hooked(), []string{"superseded"}) || len(r.sess.answers()) != 0 {
				t.Errorf("ephemerals %q, hooks %q, sends %d; want it told %q, counted superseded, and not sent", got, r.hooked(), len(r.sess.answers()), tc.wantTold)
			}
		})
	}
}

// TestASessionThatHasEndedTheTaskGetsTheNotice: no task is recorded for the thread, or another one is.
func TestASessionThatHasEndedTheTaskGetsTheNotice(t *testing.T) {
	for name, task := range map[string]string{"no task": "", "another task": "task-9", "no session": "absent"} {
		r := newDecideRig(t)
		r.pending(t)
		if task != "absent" {
			r.task(t, task)
		}
		r.sess.errs = []error{session.ErrNotPaused}
		if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil || len(r.out.list()) != 1 {
			t.Errorf("%s: Decide = %v with %d notices; want it done, and the thread told", name, err, len(r.out.list()))
		}
	}
}

// TestAnAnswerBeforeTheTaskIsParkedAgainIsRetriedAndNotDenied: right after a restart the session still
// records the task but has not parked on it, so the answer is refused as not paused. The thread is not
// told it was not sent: the press is decided again, and the recovery resends the recorded answer.
func TestAnAnswerBeforeTheTaskIsParkedAgainIsRetriedAndNotDenied(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	r.task(t, "task-1") // still the thread's task
	r.sess.errs = []error{session.ErrNotPaused, session.ErrNotPaused}
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err == nil {
		t.Fatal("Decide = nil; want a retryable error, since the task may yet be parked")
	}
	if len(r.out.list()) != 0 {
		t.Fatalf("notices %+v; want none while the task may yet be parked", r.out.list())
	}
	// Decided again as a retry, and still refused: it cannot be known, so it stays silent.
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil || len(r.out.list()) != 0 {
		t.Errorf("the retry = %v with %d notices; want it done, and silent", err, len(r.out.list()))
	}
}

// TestAnAnswerTheSessionCallsInvalidIsFinal: no retry would change it, so it is logged at Error and the
// press is done; returning it would decide the press again for ever.
func TestAnAnswerTheSessionCallsInvalidIsFinal(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	r.sess.errs = []error{fmt.Errorf("answer: a message with an ID and a task is required: %w", store.ErrInvalid)}
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil {
		t.Fatalf("Decide = %v; want it done", err)
	}
	if out := r.logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "ix-1") {
		t.Errorf("log = %q; want an error naming ix-1", out)
	}
}

// failingSessions is a session store that cannot be read.
type failingSessions struct{ err error }

func (f failingSessions) GetSession(context.Context, store.ConversationKey, time.Duration, time.Time) (store.Session, error) {
	return store.Session{}, f.err
}

// TestASessionThatCannotBeReadRetriesTheAnswer: whether the task has ended is not known, so no notice
// says the answer was not sent, and the error is returned for the press to be decided again.
func TestASessionThatCannotBeReadRetriesTheAnswer(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	d, err := interact.NewDecider(interact.DecideConfig{Store: r.st, Auditor: r.aud, Ephemerals: r.eph, Session: r.sess, Sessions: failingSessions{errBoom},
		Outbox: r.out, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	r.sess.errs = []error{session.ErrNotPaused}
	if err := d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); !errors.Is(err, errBoom) {
		t.Fatalf("Decide = %v; want the session read's error, for the press to be tried again", err)
	}
	if got := r.out.list(); len(got) != 0 {
		t.Errorf("notices %+v; want none while it cannot be known that the task has ended", got)
	}
}

// TestTheWinnersPromptIsReplacedOnceTheAnswerIsSent: the controls come off after the answer is on its way,
// and only for the press that won.
func TestTheWinnersPromptIsReplacedOnceTheAnswerIsSent(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	_ = r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1"))
	if got, want := r.flow.list(), []string{"audit:answered", "submit", "send", "replace"}; !slices.Equal(got, want) {
		t.Errorf("calls %q; want the prompt replaced after the answer is sent: %q", got, want)
	}
	_ = r.d.Decide(t.Context(), r.press(t, "UBOB22", d0.Add(2*time.Minute), "req-1")) // lost
	_ = r.d.Decide(t.Context(), r.press(t, "UCAROL3", deadline, "req-1"))             // late
	_ = r.d.Decide(t.Context(), r.press(t, "UDAVE44", d0.Add(time.Minute), "wrong"))  // refused
	if got := r.rep.replaced(); !slices.Equal(got, []string{"ix-1"}) {
		t.Errorf("replaced %q; want the prompt once, for the winner", got)
	}
}

// TestAPromptThatCannotBeReplacedDoesNotUndoTheDecision: the answer is on its way, so a failed edit is
// logged for the repair and does not fail the decision (a retry of it would send the answer again).
func TestAPromptThatCannotBeReplacedDoesNotUndoTheDecision(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	r.rep.err = errBoom
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil {
		t.Fatalf("Decide = %v; want the decision done", err)
	}
	if out := r.logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "ix-1") || len(r.sess.answers()) != 1 {
		t.Errorf("log = %q, sends %d; want a warning naming ix-1, and the answer sent once", out, len(r.sess.answers()))
	}
}

// TestAHungEditDoesNotHoldUpThePress: the replacement is made on the worker that decided the press, and an
// edit that never answers must not keep it for ever. The decision stands, and is logged for the repair.
func TestAHungEditDoesNotHoldUpThePress(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	if err := r.st.SetInteractionMessageRef(t.Context(), "ix-1", "100.001"); err != nil {
		t.Fatal(err)
	}
	h := &hangs{}
	rp, err := interact.NewReplacer(interact.ReplacerConfig{Store: r.st, Updater: h, EditTimeout: 20 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	d, err := interact.NewDecider(interact.DecideConfig{Store: r.st, Auditor: r.aud, Ephemerals: r.eph, Session: r.sess, Sessions: r.st, Replacer: rp, Outbox: r.out,
		Logger: slog.New(slog.NewTextHandler(r.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	withinASecond(t, "Decide", func() { err = d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")) })
	if err != nil || len(r.sess.answers()) != 1 || h.count() != 1 {
		t.Errorf("Decide = %v, %d answers, %d edits; want it done, the answer sent once and the edit tried once", err, len(r.sess.answers()), h.count())
	}
	if out := r.logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "ix-1") {
		t.Errorf("log = %q; want a warning naming ix-1", out)
	}
}

// The answer is marked delivered once the session has taken it, and not before or otherwise: that mark is
// all that tells a delivered answer from one that never arrived, which the backstop (WP-33d4) acts on.
func TestAnAnswerIsMarkedDeliveredWhenTheSessionTakesIt(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	if got := r.row(t); !got.DeliveredAt.IsZero() {
		t.Fatalf("a pending interaction is delivered at %v", got.DeliveredAt)
	}
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil {
		t.Fatal(err)
	}
	if got := r.row(t); got.State != store.InteractionAnswered || got.DeliveredAt.IsZero() {
		t.Errorf("after a send the session took: state %s, delivered at %v; want answered and marked", got.State, got.DeliveredAt)
	}
}

func TestAnAnswerTheSessionDidNotTakeIsNotMarked(t *testing.T) {
	for name, tc := range map[string]struct {
		errs      []error
		sessionOn string // the task the session records at the time
		wantErrs  []bool // whether each Decide returns an error
	}{
		"a send that failed":                       {[]error{errBoom}, "", []bool{true}},
		"a task the session has not parked on yet": {[]error{session.ErrNotPaused, session.ErrNotPaused}, "task-1", []bool{true, false}},
		"a task that has moved on":                 {[]error{session.ErrNotPaused}, "task-2", []bool{false}},
		"a refusal for good":                       {[]error{store.ErrInvalid}, "", []bool{false}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newDecideRig(t)
			r.pending(t)
			if tc.sessionOn != "" {
				r.task(t, tc.sessionOn)
			}
			r.sess.errs = tc.errs
			for i, wantErr := range tc.wantErrs {
				if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); (err != nil) != wantErr {
					t.Fatalf("Decide %d = %v; want an error: %t", i+1, err, wantErr)
				}
			}
			if got := r.row(t); got.State != store.InteractionAnswered || !got.DeliveredAt.IsZero() {
				t.Errorf("state %s, delivered at %v; want answered and not marked: the session did not take it", got.State, got.DeliveredAt)
			}
		})
	}
}

// A send that failed and was retried, and then taken, is marked at the retry.
func TestARetriedAnswerTheSessionTakesIsMarked(t *testing.T) {
	r := newDecideRig(t)
	r.pending(t)
	r.sess.errs = []error{errBoom}
	_ = r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1"))
	if err := r.d.Decide(t.Context(), r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")); err != nil {
		t.Fatal(err)
	}
	if got := r.row(t); got.DeliveredAt.IsZero() {
		t.Error("the answer the retry delivered is not marked")
	}
}

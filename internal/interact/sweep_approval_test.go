package interact_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// A tool approval that nobody decides is denied through the dialect when the sweep times it out, in place of the cancel a
// question gets (spec 8.4 step 6). The tests below put one through the sweep rig of sweep_test.go.

const (
	timeoutReason  = "denied by agent-kourier: no one decided in time"
	deniedNotice   = "No one decided this request in time, so Agent Kourier is denying it."
	closedNotice   = "No one decided this request in time, so it was closed."
	cancelNotice   = "No one decided this request in time, so Agent Kourier is cancelling the task."
	nothingNotice  = "No one decided this request in time, and Agent Kourier could not act on it."
	questionNotice = "No one answered this question in time, so it was closed." // main's, for anything that is not a denied approval
)

// flagFalse makes the Binding of the rig not allow tool approvals, as every Binding is today.
func flagFalse(r *sweepRig) {
	r.look.bindings["ns/b"] = interact.Binding{Dialect: kagentv1.New(0), Timeout: timeout}
}

// failingDialect is a kagentv1 dialect that cannot build any answer.
type failingDialect struct{ *kagentv1.Dialect }

func (failingDialect) BuildResponse(dialect.Interaction, dialect.Answer) (a2a.Message, error) {
	return a2a.Message{}, errBoom
}

// approvalOfTask is the request of an approval of two tools (t1 delete_everything, t2 add_numbers) paused on task.
func approvalOfTask(t *testing.T, task string) []byte {
	t.Helper()
	raw, err := json.Marshal(dialect.Interaction{Kind: dialect.ToolApproval, TaskID: a2a.TaskID(task), RequestID: "req-" + task, Hint: "delete?", Tools: []dialect.Tool{
		{ID: "t1", Name: "delete_everything", Args: map[string]any{"path": "/"}}, {ID: "t2", Name: "add_numbers"}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// nestedRequest is the request of a sub-agent's approval as the dialect parses it, which the intake stores with the kind nested.
func nestedRequest(t *testing.T) []byte {
	t.Helper()
	task := pausedOn(map[string]any{
		"type": "tool_approval_request", "hint": "Approval is required for tool(s): delete_file",
		"tools": []any{map[string]any{"id": "adk-parent-tool", "call_id": "call-parent", "name": "k8s-agent", "args": map[string]any{}}},
		"nested": map[string]any{
			"subagent_name": "k8s-agent", "task_id": "sub-task", "context_id": "sub-ctx",
			"tools": []any{map[string]any{"id": "inner-1", "call_id": "c9", "name": "delete_file", "args": map[string]any{"path": "/x"}}},
		},
	})
	in, err := (&kagentv1.Dialect{}).ParseInteraction(task)
	if err != nil || in == nil || in.Nested == nil {
		t.Fatalf("the nested pause did not parse: %+v, %v", in, err)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// approvalRow adds a pending interaction of kind whose request is request, due at deadline, with its prompt posted as 100.001.
func (r *sweepRig) approvalRow(t *testing.T, id, task string, kind store.InteractionKind, request []byte, deadline time.Time) {
	t.Helper()
	r.approvalRowIn(t, key, id, task, kind, request, deadline)
}

// approvalRowIn is approvalRow in the thread k.
func (r *sweepRig) approvalRowIn(t *testing.T, k store.ConversationKey, id, task string, kind store.InteractionKind, request []byte, deadline time.Time) {
	t.Helper()
	row := store.Interaction{ID: id, Key: k, TaskID: task, Kind: kind, Request: request, CreatedAt: t0, Deadline: deadline}
	if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetInteractionMessageRef(t.Context(), id, "100.001"); err != nil {
		t.Fatal(err)
	}
}

// onlyApproval is a rig with one pending approval, ix-1, of task-1, whose request is the tools of approvalOfTask.
func onlyApproval(t *testing.T) *sweepRig {
	t.Helper()
	r := newSweepRig(t)
	r.approvalRow(t, "ix-1", "task-1", store.InteractionToolApproval, approvalOfTask(t, "task-1"), t0.Add(timeout))
	return r
}

// noticeTexts is the payload of each notice waiting in the outbox.
func (r *sweepRig) noticeTexts(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, row := range r.notices(t) {
		out = append(out, row.Payload)
	}
	return out
}

// TestAnApprovalThatTimesOutIsDeniedAndNotCancelled: the agent gets a denial of every tool, under the Binding, from Agent
// Kourier, with the interaction's answer message ID, and the task is not cancelled. The prompt becomes a timed-out ending that
// shows the tools and no verdict, the thread gets the approval's notice once, and the row is timed out and nobody decided it.
// It is audited as timed out once, and counted once, with the Binding.
func TestAnApprovalThatTimesOutIsDeniedAndNotCancelled(t *testing.T) {
	for name, tc := range map[string]struct {
		kind    store.InteractionKind
		request func(*testing.T) []byte
		tools   []wireApproval
		shown   int
	}{
		"a tool approval": {store.InteractionToolApproval, func(t *testing.T) []byte { return approvalOfTask(t, "task-1") }, []wireApproval{{"t1", false, timeoutReason}, {"t2", false, timeoutReason}}, 2},
		"a nested one, stored with the kind nested": {store.InteractionNested, nestedRequest, []wireApproval{{"inner-1", false, timeoutReason}}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSweepRig(t)
			r.approvalRow(t, "ix-1", "task-1", tc.kind, tc.request(t), t0.Add(timeout))
			sw := r.sweeper(t, nil)
			for range 2 { // the second sweep finds nothing to do
				if err := sw.Sweep(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if got := r.sess.cancelled(); len(got) != 0 {
				t.Errorf("cancelled %v; want the tools denied and the task left to go on", got)
			}
			sent := r.sess.sent()
			if len(sent) != 1 {
				t.Fatalf("%d answers; want one denial", len(sent))
			}
			approvals, actor := approvalsSent(t, sent[0])
			if got := platformSent(sent[0]); got != "slack" {
				t.Errorf("denial is from a user on platform %q; want the Binding's, slack", got)
			}
			if !slices.Equal(approvals, tc.tools) || actor != "agent-kourier" || sent[0].ID != "ix-1/answer" || sent[0].TaskID != "task-1" {
				t.Errorf("answer %+v by %q with ID %q for task %q; want every tool denied with the fixed reason, by agent-kourier, as the interaction's answer for task-1", approvals, actor, sent[0].ID, sent[0].TaskID)
			}
			if got := r.upd.list(); !slices.Equal(got, []string{"C1 100.001 timed_out"}) {
				t.Errorf("updates %q; want the prompt replaced once, as timed out", got)
			}
			e := r.upd.ends[0]
			if e.Validate() != nil || e.Outcome != chat.TimedOut || e.By != "" || e.Verdicts != nil || len(e.Prompt.Tools) != tc.shown || !e.At.Equal(t0.Add(timeout)) {
				t.Errorf("ending %+v, %v; want a valid timed-out ending at the deadline of %d tools, with no verdict and no one named", e, e.Validate(), tc.shown)
			}
			if notices := r.noticeTexts(t); len(notices) != 1 || !strings.Contains(notices[0], deniedNotice) || strings.Contains(notices[0], "question") {
				t.Errorf("notices %q; want the approval's, once", notices)
			}
			row, err := r.st.GetInteraction(t.Context(), "ix-1")
			if err != nil || row.State != store.InteractionTimedOut || row.Decider != "" || len(row.Answer) != 0 {
				t.Errorf("row %+v, %v; want it timed out, with no decider and no stored answer: the denial is rebuilt from the request", row, err)
			}
			if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionTimedOut || r.aud.got[0].InteractionID != "ix-1" || r.aud.act[0].Binding != "ns/b" || r.aud.act[0].User != "" {
				t.Errorf("audit %+v by %+v; want one timed_out decision by no person, under the Binding", r.aud.got, r.aud.act)
			}
			if got := r.outcomes(); !slices.Equal(got, []string{"ns/b timed_out"}) {
				t.Errorf("approval outcomes %q; want one timed_out with the Binding", got)
			}
			if got := r.hooked(); !slices.Equal(got, []string{"ix-1"}) {
				t.Errorf("timeout hook calls %q; want one", got)
			}
		})
	}
}

// TestTheStepsOfAnApprovalRunInOrder: the denial follows its timeout audit (the audit fake is not in the call log); the
// timeout is recorded last.
func TestTheStepsOfAnApprovalRunInOrder(t *testing.T) {
	r := onlyApproval(t)
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := r.calls.list(), []string{"deny", "edit", "notice", "resolve"}; !slices.Equal(got, want) {
		t.Errorf("calls %q; want %q", got, want)
	}
	if len(r.aud.got) != 1 {
		t.Errorf("audits %d; want one timeout audit before the denial", len(r.aud.got))
	}
}

// TestASweepThatFailsAtAnyStepOfAnApprovalIsFinishedByTheNext: each step in turn fails once. Until the timeout is recorded
// the timeout audit is written but nothing is counted; the next sweep finishes the steps, and the timeout is counted once,
// however many sweeps it took. A denial the session took is sent once: the later sweeps that finish the other steps do not
// send it again.
func TestASweepThatFailsAtAnyStepOfAnApprovalIsFinishedByTheNext(t *testing.T) {
	for name, tc := range map[string]struct {
		breakIt func(*sweepRig)
		fix     func(*sweepRig)
		calls   []string // the calls of the failing sweep
		sent    int      // the denials sent in all: one that the session took is not sent again
	}{
		"the denial": {func(r *sweepRig) { r.sess.answerAlways = errBoom }, func(r *sweepRig) { r.sess.mu.Lock(); r.sess.answerAlways = nil; r.sess.mu.Unlock() },
			[]string{"deny"}, 2},
		"the edit":   {func(r *sweepRig) { r.upd.err = errBoom }, func(r *sweepRig) { r.upd.mu.Lock(); r.upd.err = nil; r.upd.mu.Unlock() }, []string{"deny", "edit"}, 1},
		"the notice": {func(r *sweepRig) { r.enq.n = 1 }, func(*sweepRig) {}, []string{"deny", "edit", "notice"}, 1},
		"the record": {func(r *sweepRig) { r.st.fail = true }, func(*sweepRig) {}, []string{"deny", "edit", "notice", "resolve"}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := onlyApproval(t)
			tc.breakIt(r)
			sw := r.sweeper(t, nil)
			if err := sw.Sweep(t.Context()); err == nil {
				t.Fatal("a sweep that failed a step reported success")
			}
			if got := r.calls.list(); !slices.Equal(got, tc.calls) {
				t.Errorf("calls of the failing sweep %q; want %q", got, tc.calls)
			}
			if r.state(t, "ix-1") != store.InteractionPending || len(r.aud.got) != 1 || len(r.outcomes()) != 0 || len(r.hooked()) != 0 {
				t.Fatal("a timeout that was not recorded was counted, or its audit was not written before the failed step")
			}
			tc.fix(r)
			for range 2 {
				if err := sw.Sweep(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if r.state(t, "ix-1") != store.InteractionTimedOut || len(r.aud.got) != 1 || len(r.outcomes()) != 1 || len(r.hooked()) != 1 || len(r.notices(t)) != 1 {
				t.Errorf("after the sweeps: state %v, %d audits, %v counted, %d hooks, %d notices; want it timed out, and audited and counted once",
					r.state(t, "ix-1"), len(r.aud.got), r.outcomes(), len(r.hooked()), len(r.notices(t)))
			}
			if got := len(r.sess.sent()); got != tc.sent {
				t.Errorf("%d denials sent; want %d: a denial the session took is not sent again", got, tc.sent)
			}
			if notices := r.noticeTexts(t); len(notices) != 1 || !strings.Contains(notices[0], deniedNotice) {
				t.Errorf("notices %q; want the one for a denial the session took, however many sweeps it took", notices)
			}
			for _, m := range r.sess.sent() { // the same message each time
				if approvals, _ := approvalsSent(t, m); m.ID != "ix-1/answer" || len(approvals) != 2 || approvals[0].Approved || approvals[1].Approved {
					t.Errorf("answer %+v with ID %q; want the one denial each time", approvals, m.ID)
				}
			}
			if len(r.sess.cancelled()) != 0 {
				t.Errorf("cancelled %v; want none", r.sess.cancelled())
			}
		})
	}
}

// TestADenialThatFailsOtherwiseIsNeverGivenUpOn: the session answers nil, ErrNotPaused or ErrInvalid; any other error says
// nothing of the task, which may still be paused, and only this sweep would end the pause, so the denial, like the cancel, is
// retried however many sweeps fail; the edit and the notice are given up on. The manager the sweeper is given answers none of
// those today, so this pins a branch that is defensive: it fails only for a session that answers an error of its own.
func TestADenialThatFailsOtherwiseIsNeverGivenUpOn(t *testing.T) {
	r := onlyApproval(t)
	r.sess.answerAlways = errBoom
	sw := r.sweeper(t, nil)
	for i := 1; i <= 12; i++ {
		if err := sw.Sweep(t.Context()); err == nil {
			t.Fatalf("sweep %d with a failing denial reported success", i)
		}
		if r.state(t, "ix-1") != store.InteractionPending || len(r.upd.list()) != 0 || len(r.notices(t)) != 0 || len(r.aud.got) != 1 || len(r.outcomes()) != 0 {
			t.Fatalf("after sweep %d the approval was timed out, edited, noticed or counted with its task still paused", i)
		}
	}
	r.sess.mu.Lock()
	r.sess.answerAlways = nil
	r.sess.mu.Unlock()
	if err := sw.Sweep(t.Context()); err != nil || r.state(t, "ix-1") != store.InteractionTimedOut || len(r.outcomes()) != 1 {
		t.Fatalf("the sweep after the fix = %v, state %v, counted %v; want it timed out and counted once", err, r.state(t, "ix-1"), r.outcomes())
	}
}

// TestATaskThatIsNoLongerPausedIsFineForADenial: the session answers ErrNotPaused for a task it is not paused on, as it does
// for the cancel of a question, and the timeout stands.
func TestATaskThatIsNoLongerPausedIsFineForADenial(t *testing.T) {
	r := onlyApproval(t)
	r.sess.current = "task-2" // the thread has moved on
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep = %v; want a task that has moved on to be fine", err)
	}
	if got := r.sess.sent(); len(got) != 1 || got[0].TaskID != "task-1" {
		t.Errorf("answers %v; want the denial to name task-1, so that task-2 is left alone", got)
	}
	if r.state(t, "ix-1") != store.InteractionTimedOut || len(r.aud.got) != 1 || len(r.outcomes()) != 1 {
		t.Errorf("state %v, %d audits, counted %v; want it timed out, audited and counted", r.state(t, "ix-1"), len(r.aud.got), r.outcomes())
	}
	if notices := r.noticeTexts(t); len(notices) != 1 || !strings.Contains(notices[0], closedNotice) || strings.Contains(notices[0], "denying") {
		t.Errorf("notices %q; want the one that says nothing was sent: the request was closed", notices)
	}
}

// TestAnApprovalWithAPressWaitingIsNotTimedOut: a press recorded by the deadline and not yet decided holds the sweep off, so
// nothing is denied under it, and the approval is timed out once the press is decided.
func TestAnApprovalWithAPressWaitingIsNotTimedOut(t *testing.T) {
	r := onlyApproval(t)
	r.press.waiting["ix-1"] = true
	sw := r.sweeper(t, nil)
	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.state(t, "ix-1") != store.InteractionPending || len(r.sess.sent()) != 0 || len(r.sess.cancelled()) != 0 || len(r.upd.list()) != 0 || len(r.notices(t)) != 0 || len(r.aud.got) != 0 || len(r.outcomes()) != 0 {
		t.Fatal("an approval with a press waiting was denied, cancelled, edited, noticed, audited or counted")
	}
	r.press.waiting["ix-1"] = false
	if err := sw.Sweep(t.Context()); err != nil || r.state(t, "ix-1") != store.InteractionTimedOut || len(r.sess.sent()) != 1 {
		t.Fatalf("Sweep after the press was decided = %v, state %v, %d denials; want it timed out", err, r.state(t, "ix-1"), len(r.sess.sent()))
	}
}

// TestAPressRecordedBeforeTheDeadlineHoldsTheSweepOfAnApproval: against the store's own account of the presses. A press made
// before the deadline and not decided holds the sweep, and one made after it does not: that one is late.
func TestAPressRecordedBeforeTheDeadlineHoldsTheSweepOfAnApproval(t *testing.T) {
	for name, tc := range map[string]struct {
		pressed time.Time
		held    bool
	}{
		"before the deadline": {t0.Add(timeout - time.Second), true},
		"after the deadline":  {t0.Add(timeout + time.Second), false},
	} {
		t.Run(name, func(t *testing.T) {
			r := onlyApproval(t)
			if _, err := r.st.RecordPress(t.Context(), store.Press{InteractionID: "ix-1", User: "UALICE1", Channel: key.Channel, MessageRef: "100.001", PressedAt: tc.pressed}, store.PressLimits{Total: 10, PerUser: 2}); err != nil {
				t.Fatal(err)
			}
			if err := r.sweeper(t, func(c *interact.SweepConfig) { c.Presses = r.st }).Sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			if held := r.state(t, "ix-1") == store.InteractionPending; held != tc.held || (len(r.sess.sent()) == 0) != tc.held {
				t.Errorf("held = %v with %d denials; want held = %v", held, len(r.sess.sent()), tc.held)
			}
		})
	}
}

// TestAnApprovalWhoseBindingIsNotKnownIsEndedAsAQuestionIs: the thread's session names a Binding the config no longer has, a
// thread has no session, or the Binding does not allow approvals (flag false, as every Binding is today). The sweep does what it
// did before approvals could be decided: it cancels the task, tells the thread with a question's notice, audits the timeout and
// calls the timeout hook, and counts no approval outcome.
func TestAnApprovalWhoseBindingIsNotKnownIsEndedAsAQuestionIs(t *testing.T) {
	for name, tc := range map[string]struct {
		thread  string // the thread of the approval; "1.1" is the one with a session
		prepare func(*sweepRig)
		binding string // the Binding the audit names
		warned  bool
	}{
		"the Binding is not in the config":     {"1.1", func(r *sweepRig) { r.look.bindings = nil }, "ns/b", true},
		"the thread has no session":            {"9.9", func(*sweepRig) {}, "", true},
		"the Binding does not allow approvals": {"1.1", flagFalse, "ns/b", false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSweepRig(t)
			r.approvalRowIn(t, store.ConversationKey{Connection: key.Connection, Channel: key.Channel, Thread: tc.thread}, "ix-1", "task-1", store.InteractionToolApproval, approvalOfTask(t, "task-1"), t0.Add(timeout))
			tc.prepare(r)
			if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := r.sess.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) || len(r.sess.sent()) != 0 {
				t.Errorf("cancelled %v, %d denials; want the task cancelled and nothing sent", got, len(r.sess.sent()))
			}
			if notices := r.noticeTexts(t); len(notices) != 1 || !strings.Contains(notices[0], questionNotice) {
				t.Errorf("notices %q; want a question's, as before approvals could be decided", notices)
			}
			if warned := strings.Contains(r.logs.String(), "level=WARN"); warned != tc.warned {
				t.Errorf("log %q; want a warning: %v", r.logs.String(), tc.warned)
			}
			if r.state(t, "ix-1") != store.InteractionTimedOut || len(r.aud.got) != 1 || r.aud.act[0].Binding != tc.binding || len(r.outcomes()) != 0 || !slices.Equal(r.hooked(), []string{"ix-1"}) {
				t.Errorf("state %v, audits %+v by %+v, counted %q, hooks %q; want it timed out, audited once, the hook called, and no approval outcome counted", r.state(t, "ix-1"), r.aud.got, r.aud.act, r.outcomes(), r.hooked())
			}
			if got := r.calls.list(); !slices.Equal(got, []string{"cancel", "edit", "notice", "resolve"}) {
				t.Errorf("calls %q; want the question's, in order", got)
			}
		})
	}
}

// TestAnApprovalOnAFlagFalseBindingIsCancelledAgainWhenItsPauseComesAgain: the pause of an approval the sweep timed out under a
// Binding that does not allow approvals comes again (a re-emit after a restart). Nothing is denied, and the task is cancelled
// again each time, as a question's is; before, such a pause was neither denied nor cancelled.
func TestAnApprovalOnAFlagFalseBindingIsCancelledAgainWhenItsPauseComesAgain(t *testing.T) {
	r := newRig(t, nil) // AskUser true, ToolApprovals false
	in, err := (&kagentv1.Dialect{}).ParseInteraction(approval())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	row := store.Interaction{ID: interact.InteractionID(*in), Key: key, TaskID: "task-1", Kind: store.InteractionToolApproval, Request: raw, CreatedAt: t0, Deadline: t0.Add(timeout)}
	if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := r.st.ResolveInteraction(t.Context(), row.ID, store.InteractionTimedOut, t0.Add(timeout)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if err := r.paused(t, approval()); err != nil || len(r.sess.answers) != 0 || len(r.sess.cancels) != i {
			t.Fatalf("Paused %d = %v with %d answers, %d cancels; want the task cancelled each time and nothing denied", i, err, len(r.sess.answers), len(r.sess.cancels))
		}
	}
	if rows := r.auditRows(t); len(rows) != 0 || len(r.hook) != 0 {
		t.Errorf("audit rows %q, hook %v; want none: the timeout was the sweep's", rows, r.hook)
	}
}

// TestTheNoticeOfAnApprovalIsWhatTheSweepDid: on a Binding that allows approvals, by what the session said of the denial. Taken:
// it is denying it. Not paused: nothing was sent. Refused as invalid, or not buildable: the task is cancelled instead, and the
// notice says so, or, if the cancel finds it not paused too, that nothing could be done. The timeout is counted in each.
func TestTheNoticeOfAnApprovalIsWhatTheSweepDid(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(*sweepRig)
		notice  string
		denials int
		cancels int
	}{
		"the session took the denial":       {func(*sweepRig) {}, deniedNotice, 1, 0},
		"the task was not paused":           {func(r *sweepRig) { r.sess.current = "task-9" }, closedNotice, 1, 0},
		"the session refused it as invalid": {func(r *sweepRig) { r.sess.answerErrs = []error{fmt.Errorf("answer: %w", store.ErrInvalid)} }, cancelNotice, 1, 1},
		"the dialect would not build it": {func(r *sweepRig) {
			r.look.bindings["ns/b"] = interact.Binding{Dialect: failingDialect{kagentv1.New(0)}, Timeout: timeout, ToolApprovals: true}
		}, cancelNotice, 0, 1},
		"refused, and the cancel finds it not paused": {func(r *sweepRig) {
			r.sess.answerErrs, r.sess.errs = []error{store.ErrInvalid}, []error{session.ErrNotPaused}
		}, nothingNotice, 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := onlyApproval(t)
			tc.prepare(r)
			if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			if notices := r.noticeTexts(t); len(notices) != 1 || !strings.Contains(notices[0], tc.notice) {
				t.Errorf("notices %q; want %q", notices, tc.notice)
			}
			if len(r.sess.sent()) != tc.denials || len(r.sess.cancelled()) != tc.cancels {
				t.Errorf("%d denials, %d cancels; want %d, %d", len(r.sess.sent()), len(r.sess.cancelled()), tc.denials, tc.cancels)
			}
			if r.state(t, "ix-1") != store.InteractionTimedOut || !slices.Equal(r.outcomes(), []string{"ns/b timed_out"}) || len(r.aud.got) != 1 {
				t.Errorf("state %v, counted %q, %d audits; want it timed out, counted once with the Binding, audited once", r.state(t, "ix-1"), r.outcomes(), len(r.aud.got))
			}
		})
	}
}

// TestAnApprovalThatCannotBeReadIsEndedAsAQuestionIs: a request that cannot be read, or has been cleared, is not known to be an
// approval, so it is cancelled with a question's notice and not counted as one.
func TestAnApprovalThatCannotBeReadIsEndedAsAQuestionIs(t *testing.T) {
	for name, request := range map[string]string{"a request not readable": `not json`, "a request cleared": ``} {
		t.Run(name, func(t *testing.T) {
			r := newSweepRig(t)
			r.approvalRow(t, "ix-1", "task-1", store.InteractionToolApproval, []byte(request), t0.Add(timeout))
			if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := r.sess.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) || len(r.sess.sent()) != 0 {
				t.Errorf("cancelled %v, %d denials; want the task cancelled and nothing sent", got, len(r.sess.sent()))
			}
			if notices := r.noticeTexts(t); len(notices) != 1 || !strings.Contains(notices[0], questionNotice) || r.state(t, "ix-1") != store.InteractionTimedOut || len(r.outcomes()) != 0 {
				t.Errorf("notices %q, state %v, counted %q; want a question's notice, a timeout, and no approval outcome", notices, r.state(t, "ix-1"), r.outcomes())
			}
		})
	}
}

// TestTheBindingNameIsCarriedFromTheDenialToTheCount: a read of the session that fails after the denial (a second one, at the
// report) leaves the audit and the count with the name the denial step found, and not an empty label.
func TestTheBindingNameIsCarriedFromTheDenialToTheCount(t *testing.T) {
	r := onlyApproval(t)
	r.sess.onAnswer = func() { r.st.sessionErr = errBoom } // the store goes down once the denial is sent
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.outcomes(), []string{"ns/b timed_out"}) || len(r.aud.act) != 1 || r.aud.act[0].Binding != "ns/b" {
		t.Errorf("counted %q, audit by %+v; want the Binding's name in both", r.outcomes(), r.aud.act)
	}
}

// TestASessionThatCannotBeReadHoldsTheDenial: a failed read says nothing of whether there is a Binding, so the task is not
// cancelled on it: the sweep fails and tries again.
func TestASessionThatCannotBeReadHoldsTheDenial(t *testing.T) {
	r := onlyApproval(t)
	r.st.sessionErr = errBoom
	sw := r.sweeper(t, nil)
	if err := sw.Sweep(t.Context()); err == nil || len(r.sess.cancelled()) != 0 || len(r.sess.sent()) != 0 || r.state(t, "ix-1") != store.InteractionPending {
		t.Fatalf("Sweep = %v with %d cancels, %d denials, state %v; want an error and nothing done", err, len(r.sess.cancelled()), len(r.sess.sent()), r.state(t, "ix-1"))
	}
	r.st.sessionErr = nil
	if err := sw.Sweep(t.Context()); err != nil || len(r.sess.sent()) != 1 || r.state(t, "ix-1") != store.InteractionTimedOut {
		t.Fatalf("Sweep after the read works = %v, %d denials, state %v; want it denied", err, len(r.sess.sent()), r.state(t, "ix-1"))
	}
}

// TestAQuestionThatTimesOutIsCancelledAndNotCounted: unchanged. Its task is cancelled, nothing is sent, its notice is its own,
// and the approval-outcome metric is left alone, though the timeout hook is called.
func TestAQuestionThatTimesOutIsCancelledAndNotCounted(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.sess.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) || len(r.sess.sent()) != 0 {
		t.Errorf("cancelled %v, %d answers; want the task cancelled and nothing sent", got, len(r.sess.sent()))
	}
	if notices := r.noticeTexts(t); len(notices) != 1 || !strings.Contains(notices[0], "No one answered this question in time, so it was closed.") {
		t.Errorf("notices %q; want the question's", notices)
	}
	if len(r.outcomes()) != 0 || !slices.Equal(r.hooked(), []string{"ix-1"}) || len(r.aud.got) != 1 {
		t.Errorf("counted %q, hooked %q, %d audits; want no approval outcome, one timeout hook, one audit", r.outcomes(), r.hooked(), len(r.aud.got))
	}
}

// A timed-out approval whose pause comes again is denied again, from its stored request, and not cancelled: the denial may
// not have reached the agent before a crash. The tests below use the intake's rig.

// timedOutApproval pauses on task, a tool approval on a Binding that allows them, and times its interaction out.
func timedOutApproval(t *testing.T, task a2a.Task) *rig {
	t.Helper()
	return timedOutApprovalOn(t, task, allowApprovals)
}

// timedOutApprovalOn is timedOutApproval on the Binding that mod makes.
func timedOutApprovalOn(t *testing.T, task a2a.Task, mod func(*interact.Binding)) *rig {
	t.Helper()
	r := newRig(t, mod)
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, task)
	if err := r.audit.Decision(t.Context(), audit.Actor{Binding: "ns/b"}, audit.Decision{InteractionID: row.ID, TaskID: a2a.TaskID(row.TaskID), Result: audit.DecisionTimedOut, At: t0.Add(timeout)}); err != nil {
		t.Fatal(err)
	}
	if err := r.st.ResolveInteraction(t.Context(), row.ID, store.InteractionTimedOut, t0.Add(timeout)); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestAPauseThatComesAgainAfterAnApprovalTimedOutIsDeniedAgain: the denial is the sweep's, rebuilt from the stored request: every
// tool, the same reason and message ID, from Agent Kourier. The task is not cancelled, and nothing is counted or told: the
// timeout was. Each re-emission retries the denial without another audit: the timed-out row already has its durable timeout
// decision, and a process restart does not change that.
func TestAPauseThatComesAgainAfterAnApprovalTimedOutIsDeniedAgain(t *testing.T) {
	r, task := timedOutApproval(t, approval()), approval()
	_ = task
	notices := len(r.notices(t))
	for range 2 {
		if err := r.paused(t, approval()); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.sess.cancels) != 0 || len(r.sess.answers) != 2 {
		t.Fatalf("%d cancels, %d answers; want the denial sent each time and the task not cancelled", len(r.sess.cancels), len(r.sess.answers))
	}
	for _, m := range r.sess.answers {
		approvals, actor := approvalsSent(t, m)
		if got := platformSent(m); got != "slack" {
			t.Errorf("denial is from a user on platform %q; want the Binding's, slack", got)
		}
		want := []wireApproval{{"t1", false, timeoutReason}, {"t2", false, timeoutReason}}
		if !slices.Equal(approvals, want) || actor != "agent-kourier" || m.ID != r.row(t, approval()).ID+"/answer" || m.TaskID != "task-1" {
			t.Errorf("answer %+v by %q with ID %q; want every tool denied with the fixed reason, by agent-kourier", approvals, actor, m.ID)
		}
	}
	if rows := r.auditRows(t); len(rows) != 1 || rows[0] != "timed_out" || len(r.hook) != 0 || len(r.notices(t)) != notices {
		t.Errorf("audit rows %q, hook %v, %d notices (was %d); want one durable timeout audit and no other effects", rows, r.hook, len(r.notices(t)), notices)
	}
}

// TestARecoveredTimeoutDenialDoesNotReauditBeforeSending: a timed-out approval that is re-emitted after a restart already has
// its durable timeout audit. An unavailable audit writer does not hold up retrying the denial or cause a duplicate row.
func TestARecoveredTimeoutDenialDoesNotReauditBeforeSending(t *testing.T) {
	r := timedOutApproval(t, approval())
	r.audit.fail = true
	if err := r.paused(t, approval()); err != nil || len(r.sess.answers) != 1 || len(r.sess.cancels) != 0 {
		t.Fatalf("Paused = %v with %d answers and %d cancels; want one denial without another audit", err, len(r.sess.answers), len(r.sess.cancels))
	}
	if rows := r.auditRows(t); len(rows) != 1 || rows[0] != "timed_out" {
		t.Errorf("audit rows %q; want one timeout audit", rows)
	}
}

// TestARecoveredTimeoutDenialRepairsAnUnauditedTimedOutRow: a fallback timeout can resolve its row before the post-timeout
// audit and leave no decision row when that audit fails. Recovery restores the audit-before-denial guarantee before sending.
func TestARecoveredTimeoutDenialRepairsAnUnauditedTimedOutRow(t *testing.T) {
	r := newRig(t, allowApprovals)
	if err := r.paused(t, approval()); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, approval())
	if err := r.st.ResolveInteraction(t.Context(), row.ID, store.InteractionTimedOut, t0.Add(timeout)); err != nil {
		t.Fatal(err)
	}
	r.audit.fail = true
	if err := r.paused(t, approval()); err == nil || len(r.sess.answers) != 0 || len(r.sess.cancels) != 0 {
		t.Fatalf("Paused = %v with %d answers and %d cancels; want the missing audit to block the denial", err, len(r.sess.answers), len(r.sess.cancels))
	}
	r.audit.fail = false
	if err := r.paused(t, approval()); err != nil || len(r.sess.answers) != 1 || len(r.sess.cancels) != 0 {
		t.Fatalf("Paused after audit recovery = %v with %d answers and %d cancels; want one denial", err, len(r.sess.answers), len(r.sess.cancels))
	}
	if rows := r.auditRows(t); len(rows) != 1 || rows[0] != "timed_out" {
		t.Errorf("audit rows %q; want one repaired timeout audit", rows)
	}
}

// TestARecoveredTimeoutDenialUsesAnExplicitAuditReader: an interactions-only store wrapper can recover a timed-out
// row when the durable audit reader is wired separately.
func TestARecoveredTimeoutDenialUsesAnExplicitAuditReader(t *testing.T) {
	r := newRig(t, allowApprovals)
	task := approval()
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, task)
	if err := r.st.ResolveInteraction(t.Context(), row.ID, store.InteractionTimedOut, t0.Add(timeout)); err != nil {
		t.Fatal(err)
	}
	b := interact.Binding{Dialect: kagentv1.New(30 * time.Second), ToolApprovals: true, Timeout: timeout}
	in, err := interact.New(intakeCfg(interact.Config{
		Store: interactionsOnlyStore{Interactions: r.st}, AuditReader: r.st, Lookup: lookup{b}, Platform: fitter{r}, Outbox: r.outbox,
		Session: r.sess, Audit: r.audit, Now: func() time.Time { return t0.Add(time.Hour) },
	}))
	if err != nil {
		t.Fatal(err)
	}
	err = in.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Task: task})
	if err != nil || len(r.sess.answers) != 1 || len(r.sess.cancels) != 0 {
		t.Fatalf("Paused = %v with %d answers, %d cancels; want one denial and no cancellation", err, len(r.sess.answers), len(r.sess.cancels))
	}
	if rows := r.auditRows(t); len(rows) != 1 || rows[0] != "timed_out" {
		t.Fatalf("audit rows %q; want one repaired timeout audit", rows)
	}
}

type interactionsOnlyStore struct{ store.Interactions }

// TestADenialThatComesAgainToAFinishedTaskOrAFailingSession: a task that has moved on is fine, and a failed send is returned, for
// the pause to be tried again.
func TestADenialThatComesAgainToAFinishedTaskOrAFailingSession(t *testing.T) {
	r := timedOutApproval(t, approval())
	r.sess.answerErr = session.ErrNotPaused
	if err := r.paused(t, approval()); err != nil || len(r.sess.cancels) != 0 {
		t.Errorf("Paused = %v with %d cancels; want a task that has moved on to be fine, and not cancelled", err, len(r.sess.cancels))
	}
	r.sess.answerErr = errBoom
	if err := r.paused(t, approval()); err == nil || len(r.sess.cancels) != 0 {
		t.Errorf("Paused = %v with %d cancels; want the failed send returned, and not turned into a cancel", err, len(r.sess.cancels))
	}
}

// TestAPausedNestedApprovalThatTimedOutIsDeniedAgain: the row's kind is nested, and the request's kind decides.
func TestAPausedNestedApprovalThatTimedOutIsDeniedAgain(t *testing.T) {
	r := newRig(t, allowApprovals)
	task := pausedOn(map[string]any{
		"type": "tool_approval_request", "hint": "Approval is required for tool(s): delete_file",
		"tools": []any{map[string]any{"id": "adk-parent-tool", "call_id": "call-parent", "name": "k8s-agent", "args": map[string]any{}}},
		"nested": map[string]any{
			"subagent_name": "k8s-agent", "task_id": "sub-task", "context_id": "sub-ctx",
			"tools": []any{map[string]any{"id": "inner-1", "call_id": "c9", "name": "delete_file", "args": map[string]any{"path": "/x"}}},
		},
	})
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, task)
	if row.Kind != store.InteractionNested {
		t.Fatalf("row kind %s; want nested", row.Kind)
	}
	if err := r.st.ResolveInteraction(t.Context(), row.ID, store.InteractionTimedOut, t0.Add(timeout)); err != nil {
		t.Fatal(err)
	}
	if err := r.paused(t, task); err != nil || len(r.sess.cancels) != 0 || len(r.sess.answers) != 1 {
		t.Fatalf("Paused = %v with %d cancels, %d answers; want one denial and no cancel", err, len(r.sess.cancels), len(r.sess.answers))
	}
	if approvals, _ := approvalsSent(t, r.sess.answers[0]); !slices.Equal(approvals, []wireApproval{{"inner-1", false, timeoutReason}}) {
		t.Errorf("approvals %+v; want the inner tool denied", approvals)
	}
}

// TestAPauseThatComesAgainAfterAnApprovalWasCancelledOrClearedIsCancelled: only a timed-out approval is denied again. A
// cancelled one, and a timed-out one whose request retention has cleared, are cancelled again as before.
func TestAPauseThatComesAgainAfterAnApprovalWasCancelledOrClearedIsCancelled(t *testing.T) {
	for name, tc := range map[string]struct {
		state store.InteractionState
		clear bool
	}{
		"cancelled":               {store.InteractionCancelled, false},
		"timed out, then cleared": {store.InteractionTimedOut, true},
		"cancelled, then cleared": {store.InteractionCancelled, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, allowApprovals)
			if err := r.paused(t, approval()); err != nil {
				t.Fatal(err)
			}
			if err := r.st.ResolveInteraction(t.Context(), r.row(t, approval()).ID, tc.state, t0.Add(timeout)); err != nil {
				t.Fatal(err)
			}
			if tc.clear {
				if n, err := r.st.ClearSettledInteractions(t.Context(), t0.Add(1000*time.Hour), time.Hour, time.Hour); err != nil || n != 1 {
					t.Fatalf("cleared %d, %v; want the interaction", n, err)
				}
			}
			if err := r.paused(t, approval()); err != nil || len(r.sess.answers) != 0 || len(r.sess.cancels) != 1 {
				t.Errorf("Paused = %v with %d answers, %d cancels; want the task cancelled again and nothing sent", err, len(r.sess.answers), len(r.sess.cancels))
			}
		})
	}
}

// TestAPauseThatComesAgainAfterAQuestionTimedOutIsCancelledAgain: unchanged, and with no denial in it.
func TestAPauseThatComesAgainAfterAQuestionTimedOutIsCancelledAgain(t *testing.T) {
	r, task := newRig(t, nil), askUser("req-1")
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	if err := r.st.ResolveInteraction(t.Context(), r.row(t, task).ID, store.InteractionTimedOut, t0.Add(timeout)); err != nil {
		t.Fatal(err)
	}
	if err := r.paused(t, task); err != nil || len(r.sess.answers) != 0 || len(r.sess.cancels) != 1 {
		t.Errorf("Paused = %v with %d answers, %d cancels; want the task cancelled again and nothing sent", err, len(r.sess.answers), len(r.sess.cancels))
	}
}

// TestTheBackstopLeavesATimedOutApprovalAlone: the backstop tells a thread that an answered interaction's answer never arrived.
// A timed-out approval has no answer, only a denial the sweep rebuilds from the request, and the thread has been told
// that the request is being denied; so it says nothing, whether or not the task is over.
func TestTheBackstopLeavesATimedOutApprovalAlone(t *testing.T) {
	r := onlyApproval(t)
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.st.UpsertSession(t.Context(), store.Session{Key: key, ContextID: "ctx-1", TaskID: "task-2", Binding: "ns/b", LastActivity: t0}); err != nil { // the task is over
		t.Fatal(err)
	}
	later := r.clock.Add(time.Hour)
	b, err := interact.NewBackstop(interact.BackstopConfig{Store: r.st, Sessions: r.st, Outbox: r.enq, Now: func() time.Time { return later }})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if notices := r.noticeTexts(t); len(notices) != 1 || strings.Contains(notices[0], "not sent") {
		t.Errorf("notices %q; want only the sweep's", notices)
	}
}

// TestADenialThatCannotBeSentAgainIsCancelledInstead: the dialect will not build it, or the session refuses it as invalid
// (final: no retry would mend either). The task is cancelled, as it is for a question. A failure that may pass is returned.
func TestADenialThatCannotBeSentAgainIsCancelledInstead(t *testing.T) {
	for name, tc := range map[string]struct {
		mod       func(*interact.Binding)
		answerErr error
	}{
		"the dialect will not build it": {func(b *interact.Binding) {
			allowApprovals(b)
			b.Dialect = failingDialect{kagentv1.New(0)}
		}, nil},
		"the session refuses it as invalid": {allowApprovals, store.ErrInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			r := timedOutApprovalOn(t, approval(), tc.mod)
			r.sess.answerErr = tc.answerErr
			if err := r.paused(t, approval()); err != nil || len(r.sess.cancels) != 1 {
				t.Errorf("Paused = %v with %d cancels; want the task cancelled", err, len(r.sess.cancels))
			}
			if tc.answerErr == nil && len(r.sess.answers) != 0 {
				t.Errorf("%d answers; want none sent", len(r.sess.answers))
			}
		})
	}
}

// TestADenialByPolicyCarriesItsOwnReason: autoDeny's denial says the Binding does not allow approvals, and the timeout's says
// that no one decided; neither is sent with the other's reason.
func TestADenialByPolicyCarriesItsOwnReason(t *testing.T) {
	r := newRig(t, nil)
	if err := r.paused(t, approval()); err != nil {
		t.Fatal(err)
	}
	approvals, actor := approvalsSent(t, r.sess.answers[0])
	want := []wireApproval{{"t1", false, "denied by agent-kourier: this Binding does not allow tool approvals"}, {"t2", false, "denied by agent-kourier: this Binding does not allow tool approvals"}}
	if !slices.Equal(approvals, want) || actor != "agent-kourier" {
		t.Errorf("approvals %+v by %q; want every tool denied with the policy's reason, by agent-kourier", approvals, actor)
	}
}

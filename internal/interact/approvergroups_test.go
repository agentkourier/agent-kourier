package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// Approver groups (spec 8.4 step 3): a tool approval on a Binding that names groups is decided by a channel member who is in
// at least one of them. UALICE1 and UBOB22 are members of the channel; only UBOB22 is an approver in these tests.

const notApprover = "Only approvers of this Binding can decide this request."

var approverGroups = []string{"S1", "S2"}

// groupBinding is a Binding that allows tool approvals and names approverGroups.
func groupBinding() interact.Binding {
	b := approvalBinding()
	b.ApproverGroups = slices.Clone(approverGroups)
	return b
}

// newGroupRig is a press rig for b with the approval of approvalRequest posted, of kind kind, in which UBOB22 is the one
// approver (in the second group, S2).
func newGroupRig(t *testing.T, b interact.Binding, kind store.InteractionKind) *pressRig {
	t.Helper()
	r := newPressRigFor(t, b)
	r.az.groups = map[string][]string{"S1": {"UCAROL1"}, "S2": {"UBOB22"}}
	openApproval(t, r, kind, approvalRequest(t))
	return r
}

// TestAnApproverInAGroupDecidesAnApproval: a member in the second of two groups decides as they would with no groups, and the
// platform is asked once, about the Binding's groups in their order.
func TestAnApproverInAGroupDecidesAnApproval(t *testing.T) {
	r := newGroupRig(t, groupBinding(), store.InteractionToolApproval)
	if res, err := r.process(t, tap("UBOB22", chat.ActionApproveAll)); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v", res, err)
	}
	if got := r.az.groupAsked(); len(got) != 1 || !slices.Equal(got[0], approverGroups) {
		t.Errorf("groups asked %v; want one call about %v", got, approverGroups)
	}
	got, actor := approvalsSent(t, r.sess.answers()[0])
	if row := r.row(t); row.State != store.InteractionAnswered || row.Decider != "UBOB22" || actor != "UBOB22" || len(got) != 2 || !got[0].Approved || !got[1].Approved ||
		len(r.eph.list()) != 0 || r.waiting(t) != 0 || len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionAnswered {
		t.Errorf("state %s, decider %q, sent %+v as %q, told %q, %d audit rows; want UBOB22's approval decided and audited, and nobody told", row.State, row.Decider, got, actor, r.eph.list(), len(r.aud.got))
	}
}

// TestAMemberInNoApproverGroupIsDeniedAndAudited: the refusal is a decision of its own, audited as denied_not_approver (not denied, which is a non-member or a bot) with the person as
// actor, and then told in fixed words that name no group. Nothing is sent or counted, the press is gone, and the approval is
// still pending and decided by an approver. An answer that is not complete is refused for who pressed first, not for its shape.
func TestAMemberInNoApproverGroupIsDeniedAndAudited(t *testing.T) {
	for name, press := range map[string]chat.Interaction{
		"approve all":          tap("UALICE1", chat.ActionApproveAll),
		"deny all":             tap("UALICE1", chat.ActionDenyAll),
		"submit, incomplete":   tap("UALICE1", chat.ActionSubmit, verdict(0, yes)),
		"submit, nothing sent": tap("UALICE1", chat.ActionSubmit),
	} {
		t.Run(name, func(t *testing.T) {
			r := newGroupRig(t, groupBinding(), store.InteractionToolApproval)
			if res, err := r.process(t, press); err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v", res, err)
			}
			if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: " + notApprover}) {
				t.Errorf("told %q; want the fixed words, once", got)
			}
			if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionNotApprover || r.aud.got[0].Answer != nil || r.aud.got[0].Shape != nil || !r.aud.got[0].At.Equal(r.now) ||
				r.aud.act[0] != (audit.Actor{User: "UALICE1", Binding: "ns/b", Identity: "secret:ns/tok"}) {
				t.Errorf("audit %+v by %+v; want one denied_not_approver decision by UALICE1 at the press's time, with no answer or shape", r.aud.got, r.aud.act)
			}
			if row := r.row(t); row.State != store.InteractionPending || len(r.sess.answers()) != 0 || len(r.counted()) != 0 || r.waiting(t) != 0 || len(r.rep.replaced()) != 0 {
				t.Errorf("state %s, %d sends, counted %q, %d waiting; want it pending, nothing sent, counted or replaced, and the press done", row.State, len(r.sess.answers()), r.counted(), r.waiting(t))
			}
			if res, err := r.process(t, tap("UBOB22", chat.ActionApproveAll)); err != nil || res != interact.Done || r.row(t).State != store.InteractionAnswered || r.row(t).Decider != "UBOB22" {
				t.Errorf("the approver's press = %v, %v, state %s; want it decided", res, err, r.row(t).State)
			}
		})
	}
}

// TestAFailedAuditOfAnApproverDenialKeepsThePress: as every refusal, the audit comes before the person is told, and without it
// nothing is told and the press stays for another try.
func TestAFailedAuditOfAnApproverDenialKeepsThePress(t *testing.T) {
	r := newGroupRig(t, groupBinding(), store.InteractionToolApproval)
	r.aud.err = errBoom
	if err := r.pl.Press(t.Context(), tap("UALICE1", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if res, err := r.pl.Process(t.Context(), presses[0]); !errors.Is(err, errBoom) || res != interact.Retry {
		t.Fatalf("Process = %v, %v; want Retry with the error", res, err)
	}
	if told := r.eph.list(); len(told) != 0 || r.waiting(t) != 1 || r.row(t).State != store.InteractionPending || len(r.sess.answers()) != 0 {
		t.Errorf("told %q, %d waiting, state %s; want nobody told and the press kept", told, r.waiting(t), r.row(t).State)
	}
	r.aud.err = nil
	// The fake log notes the attempt that failed too, so the second try is the second row; both are the denial.
	if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done || len(r.aud.got) != 2 || r.aud.got[1].Result != audit.DecisionNotApprover ||
		!slices.Equal(r.eph.list(), []string{"UALICE1: " + notApprover}) || r.waiting(t) != 0 {
		t.Errorf("Process again = %v, %v; audit %+v, told %q; want the denial audited again, then told, once", res, err, r.aud.got, r.eph.list())
	}
}

// TestAFailedApproverGroupCheckIsNoDecision: a check that cannot be made is not a denial. The error is returned, the press is
// kept for the worker to try again, nothing is audited, told or sent, and the failure is logged at Error naming the Binding and
// the interaction. Once the check works the same press is decided, whoever it came from.
func TestAFailedApproverGroupCheckIsNoDecision(t *testing.T) {
	for name, tc := range map[string]struct {
		user    string
		decided bool
		told    []string
	}{
		"an approver":     {"UBOB22", true, nil},
		"not an approver": {"UALICE1", false, []string{"UALICE1: " + notApprover}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newGroupRig(t, groupBinding(), store.InteractionToolApproval)
			var out logBuf
			pl, err := interact.NewPipeline(pressCfg(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{groupBinding()}, Authorizer: r.az, Ephemerals: r.eph, Decider: r.d, Auditor: r.aud,
				Logger: slog.New(telemetry.NewJSONHandler(&out, slog.LevelDebug)), Now: func() time.Time { return r.now }}))
			if err != nil {
				t.Fatal(err)
			}
			r.az.groupErr = errBoom
			if err := pl.Press(t.Context(), tap(tc.user, chat.ActionApproveAll)); err != nil {
				t.Fatal(err)
			}
			presses, _ := r.st.ListPresses(t.Context())
			if res, err := pl.Process(t.Context(), presses[0]); !errors.Is(err, errBoom) || res != interact.Retry {
				t.Fatalf("Process = %v, %v; want Retry with the error", res, err)
			}
			if told := r.eph.list(); len(told) != 0 || len(r.aud.got) != 0 || r.waiting(t) != 1 || r.row(t).State != store.InteractionPending || len(r.sess.answers()) != 0 || len(r.counted()) != 0 {
				t.Errorf("told %q, %d audit rows, %d waiting, state %s; want nothing told or audited, the press kept and the approval open", told, len(r.aud.got), r.waiting(t), r.row(t).State)
			}
			var line map[string]any
			if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &line) != nil {
				t.Fatalf("log:\n%s\nwant one line", out.String())
			}
			if line["level"] != "ERROR" || line["binding"] != "ns/b" || line["interaction"] != "ix-1" || !strings.Contains(line["msg"].(string), "approver-group check failed") {
				t.Errorf("line = %v; want an Error naming the Binding and the interaction and saying the approver-group check failed", line)
			}
			r.az.groupErr = nil
			if res, err := pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done || (r.row(t).State == store.InteractionAnswered) != tc.decided || !slices.Equal(r.eph.list(), tc.told) {
				t.Errorf("Process again = %v, %v; state %s, told %q; want decided %t, told %q", res, err, r.row(t).State, r.eph.list(), tc.decided, tc.told)
			}
		})
	}
}

// TestAGroupCheckIsNeverMadeWhenItCannotMatter: with no groups, any member that is not a bot decides and the platform is not
// asked. A person who is not a member or is a bot is refused before the groups are asked about, and so is a press on an
// approval the Binding no longer allows.
func TestAGroupCheckIsNeverMadeWhenItCannotMatter(t *testing.T) {
	noGroups := approvalBinding()
	off := groupBinding()
	off.ToolApprovals = false
	for name, tc := range map[string]struct {
		b       interact.Binding
		user    string
		bot     bool
		decided bool
		told    string
	}{
		"empty list, any member":       {noGroups, "UALICE1", false, true, ""},
		"not a member":                 {groupBinding(), "UOUTSIDE", false, false, "UOUTSIDE: Only members of this channel can decide this request."},
		"a bot":                        {groupBinding(), "UALICE1", true, false, ""},
		"the Binding no longer asks":   {off, "UALICE1", false, false, "UALICE1: This request is no longer available."},
		"the Binding no longer asks 2": {off, "UBOB22", false, false, "UBOB22: This request is no longer available."},
	} {
		t.Run(name, func(t *testing.T) {
			r := newGroupRig(t, tc.b, store.InteractionToolApproval)
			if tc.bot {
				r.az.bots = map[string]bool{tc.user: true}
			}
			if res, err := r.process(t, tap(tc.user, chat.ActionApproveAll)); err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v", res, err)
			}
			if got := r.az.groupAsked(); len(got) != 0 {
				t.Errorf("groups asked %v; want none", got)
			}
			if told := strings.Join(r.eph.list(), "|"); told != tc.told || (r.row(t).State == store.InteractionAnswered) != tc.decided {
				t.Errorf("told %q, state %s; want told %q, decided %t", told, r.row(t).State, tc.told, tc.decided)
			}
		})
	}
}

// TestAQuestionIsNotGatedByApproverGroups: the groups are for tool approvals only (decision D-a). On a Binding that names
// them, a member who is in none answers a question, and the platform is not asked about any group.
func TestAQuestionIsNotGatedByApproverGroups(t *testing.T) {
	b := interact.Binding{Dialect: kagentv1.New(time.Second), AskUser: true, ToolApprovals: true, ApproverGroups: approverGroups, Timeout: time.Minute, Identity: "secret:ns/tok"}
	r := newPressRigFor(t, b)
	r.az.groups = map[string][]string{"S2": {"UBOB22"}}
	r.open(t)
	if res, err := r.process(t, r.submit(t, "UALICE1", 1)); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v", res, err)
	}
	if got := r.az.groupAsked(); len(got) != 0 {
		t.Errorf("groups asked %v; want none for a question", got)
	}
	if row := r.row(t); row.State != store.InteractionAnswered || row.Decider != "UALICE1" || len(r.sess.answers()) != 1 || len(r.eph.list()) != 0 {
		t.Errorf("state %s, decider %q, %d sends, told %q; want the question answered by a member who is no approver", row.State, row.Decider, len(r.sess.answers()), r.eph.list())
	}
}

// TestANestedApprovalIsGatedByApproverGroups: a sub-agent's approval is stored with the kind nested and is the same request.
func TestANestedApprovalIsGatedByApproverGroups(t *testing.T) {
	r := newGroupRig(t, groupBinding(), store.InteractionNested)
	if _, err := r.process(t, tap("UALICE1", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: " + notApprover}) || r.row(t).State != store.InteractionPending || len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionNotApprover {
		t.Errorf("told %q, state %s, audit %+v; want the non-approver denied and told", got, r.row(t).State, r.aud.got)
	}
	if res, err := r.process(t, tap("UBOB22", chat.ActionApproveAll)); err != nil || res != interact.Done || r.row(t).State != store.InteractionAnswered || r.row(t).Decider != "UBOB22" {
		t.Errorf("the approver's press = %v, %v, state %s; want it decided", res, err, r.row(t).State)
	}
}

// TestTheWinnersRetryAfterLeavingTheGroupSendsTheRecordedAnswer: a person whose approval is recorded, and whose send failed, is
// not asked about the groups again when the press is retried. The approval was authorized when it was made; the retry only sends
// it again, with no second audit row (and no denied one after the answered one), no count and nobody told.
func TestTheWinnersRetryAfterLeavingTheGroupSendsTheRecordedAnswer(t *testing.T) {
	r := newGroupRig(t, groupBinding(), store.InteractionToolApproval)
	r.sess.errs = []error{errBoom}
	if err := r.pl.Press(t.Context(), tap("UBOB22", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if res, err := r.pl.Process(t.Context(), presses[0]); !errors.Is(err, errBoom) || res != interact.Retry {
		t.Fatalf("Process with a failing send = %v, %v; want Retry with the error", res, err)
	}
	r.az.groups = nil // removed from the group
	if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done {
		t.Fatalf("Process again = %v, %v", res, err)
	}
	sent := r.sess.answers()
	if len(sent) != 2 || len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionAnswered || len(r.eph.list()) != 0 || r.waiting(t) != 0 || !slices.Equal(r.counted(), []string{"ns/b: approved"}) {
		t.Errorf("%d sends, audit %+v, told %q, %d waiting, counted %q; want the answer sent again under the one answered row, nobody told",
			len(sent), r.aud.got, r.eph.list(), r.waiting(t), r.counted())
	}
	if got := r.az.groupAsked(); len(got) != 1 {
		t.Errorf("groups asked %d times; want once, for the decision and not for its retry", len(got))
	}
}

// TestAPersonRemovedFromTheGroupIsDeniedOnAPendingApproval: leaving the group takes the right to decide, so the same press
// that a member of the group could make is denied while the approval is still open.
func TestAPersonRemovedFromTheGroupIsDeniedOnAPendingApproval(t *testing.T) {
	r := newGroupRig(t, groupBinding(), store.InteractionToolApproval)
	r.az.groups = nil
	if _, err := r.process(t, tap("UBOB22", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionNotApprover || r.row(t).State != store.InteractionPending || !slices.Equal(r.eph.list(), []string{"UBOB22: " + notApprover}) {
		t.Errorf("audit %+v, state %s, told %q; want denied_not_approver, still pending, told", r.aud.got, r.row(t).State, r.eph.list())
	}
}

// TestSomeoneElsesPressOnADecidedApprovalIsToldWhoDecidedWithoutAGroupCheck: unchanged by the groups.
func TestSomeoneElsesPressOnADecidedApprovalIsToldWhoDecidedWithoutAGroupCheck(t *testing.T) {
	r := newGroupRig(t, groupBinding(), store.InteractionToolApproval)
	if _, err := r.process(t, tap("UBOB22", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.process(t, tap("UALICE1", chat.ActionDenyAll)); err != nil {
		t.Fatal(err)
	}
	if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: @UBOB22 decided this request first."}) || len(r.az.groupAsked()) != 1 || len(r.aud.got) != 1 {
		t.Errorf("told %q, groups asked %d times, %d audit rows; want who decided, and no further group check or audit", got, len(r.az.groupAsked()), len(r.aud.got))
	}
}

// TestACancelledGroupCheckIsRetriedWithoutAnErrorLine: a shutdown with a press in flight is not a fault of the groups, so it
// is kept for retry and writes no Error line.
func TestACancelledGroupCheckIsRetriedWithoutAnErrorLine(t *testing.T) {
	r := newGroupRig(t, groupBinding(), store.InteractionToolApproval)
	var out logBuf
	pl, err := interact.NewPipeline(pressCfg(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{groupBinding()}, Authorizer: r.az, Ephemerals: r.eph, Decider: r.d, Auditor: r.aud,
		Logger: slog.New(telemetry.NewJSONHandler(&out, slog.LevelDebug)), Now: func() time.Time { return r.now }}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	r.az.onGroup, r.az.groupErr = cancel, context.Canceled
	if err := pl.Press(t.Context(), tap("UALICE1", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if res, err := pl.Process(ctx, presses[0]); !errors.Is(err, context.Canceled) || res != interact.Retry {
		t.Fatalf("Process = %v, %v; want Retry with the cancellation", res, err)
	}
	if out.String() != "" || len(r.aud.got) != 0 || len(r.eph.list()) != 0 || r.waiting(t) != 1 {
		t.Errorf("log %q, %d audit rows, told %q, %d waiting; want no log line, nothing audited or told, and the press kept", out.String(), len(r.aud.got), r.eph.list(), r.waiting(t))
	}
}

package interact_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// A press decides an approval: the tests below put a press through the pipeline and the decider against the rigs the
// package has (the press rig, with the fake audit log, the ordered store and the recording session), and the replacement
// against the chat fake.

// The tools of approvalRequest, in its order: t1 (delete_everything) and t2 (add_numbers).
var toolNamesOfTheRequest = []string{"delete_everything", "add_numbers"}

// newApprovalRig is a press rig for a Binding that allows tool approvals, with the approval of approvalRequest posted as
// message 100.1 of the thread.
func newApprovalRig(t *testing.T) *pressRig {
	t.Helper()
	return newApprovalRigOf(t, approvalRequest(t))
}

// newApprovalRigOf is newApprovalRig for the approval whose request is request.
func newApprovalRigOf(t *testing.T, request []byte) *pressRig {
	t.Helper()
	r := newPressRigFor(t, approvalBinding())
	openApproval(t, r, store.InteractionToolApproval, request)
	return r
}

// openApproval records an interaction of kind whose request is request, with its prompt posted as message 100.1.
func openApproval(t *testing.T, r *pressRig, kind store.InteractionKind, request []byte) {
	t.Helper()
	row := store.Interaction{ID: "ix-1", Key: key, TaskID: "task-1", Kind: kind, Request: request, CreatedAt: d0, Deadline: deadline}
	if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetInteractionMessageRef(t.Context(), "ix-1", "100.1"); err != nil {
		t.Fatal(err)
	}
}

// oneToolRequest is the request of an approval of one tool, which has an Approve and a Deny button and no Submit.
func oneToolRequest(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "task-1", Tools: []dialect.Tool{{ID: "t1", Name: "delete_everything", Args: map[string]any{"path": "/"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// tap is user's press of a button of the approval prompt, as the platform's adapter hands it over.
func tap(user string, action chat.Action, answers ...chat.Answer) chat.Interaction {
	return chat.Interaction{User: user, At: chat.Ref{Platform: "slack", Conn: key.Connection, Channel: key.Channel, Thread: key.Thread, ID: "100.1"}, PromptID: "ix-1", Action: action, Answers: answers}
}

// wireApproval is one decision as the kagent message carries it.
type wireApproval struct {
	ID       string
	Approved bool
	Reason   string
}

// approvalsSent reads the tool_approval_response of a message the rig's session was given, and the user it is attributed to.
func approvalsSent(t *testing.T, msg *a2a.Message) (approvals []wireApproval, actor string) {
	t.Helper()
	payload, _ := msg.Metadata[kagentv1.HITLExtension].(map[string]any)
	if payload["type"] != "tool_approval_response" {
		t.Fatalf("payload %v; want a tool_approval_response", payload)
	}
	list, _ := payload["approvals"].([]any)
	for _, x := range list {
		m, _ := x.(map[string]any)
		approvals = append(approvals, wireApproval{ID: m["id"].(string), Approved: m["approved"].(bool), Reason: stringOf(m["rejection_reason"])})
	}
	who, _ := msg.Metadata[kagentv1.ActorMetadataKey].(map[string]any)
	return approvals, stringOf(who["id"])
}

// hasPrefix says whether a text part of msg starts with the attribution prefix, which a structured answer never gets:
// its actor is in its metadata alone.
func hasPrefix(msg *a2a.Message) bool {
	for _, p := range msg.Parts {
		if text, ok := p.Content.(a2a.Text); ok && strings.HasPrefix(string(text), "[Slack: ") {
			return true
		}
	}
	return false
}

// platformSent is the platform the actor of a message the rig's session was given is on ("" when it names none).
func platformSent(msg *a2a.Message) string {
	who, _ := msg.Metadata[kagentv1.ActorMetadataKey].(map[string]any)
	return stringOf(who["platform"])
}

func stringOf(v any) string { s, _ := v.(string); return s }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// approvalBinding is a Binding that allows tool approvals.
func approvalBinding() interact.Binding {
	return interact.Binding{Dialect: kagentv1.New(time.Second), ToolApprovals: true, Timeout: time.Minute, Identity: "secret:ns/tok", Platform: dialect.Platform{ID: "slack", Name: "Slack"}}
}

const (
	denied = "denied by an approver in chat" // the one reason a denial carries
)

// TestAPressDecidesAnApproval: each way to decide one, through the pipeline. The decision is audited, with the names of the
// tools and the verdict of each, before it takes effect; then it is sent under the Binding with the person as attribution,
// one decision for each tool by its ID; the prompt is replaced; the outcome is counted once; and nobody is told anything.
func TestAPressDecidesAnApproval(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func(*testing.T) []byte
		press   chat.Interaction
		stored  string // what the press holds in the store
		want    []wireApproval
		outcome string
	}{
		{"approve all", approvalRequest, tap("UALICE1", chat.ActionApproveAll), `{"action":"approve_all"}`,
			[]wireApproval{{"t1", true, ""}, {"t2", true, ""}}, "ns/b: approved"},
		{"deny all", approvalRequest, tap("UALICE1", chat.ActionDenyAll), `{"action":"deny_all"}`,
			[]wireApproval{{"t1", false, denied}, {"t2", false, denied}}, "ns/b: denied"},
		{"submit, both approved", approvalRequest, tap("UALICE1", chat.ActionSubmit, verdict(0, yes), verdict(1, yes)), `[{"Question":0,"Values":["0"]},{"Question":1,"Values":["0"]}]`,
			[]wireApproval{{"t1", true, ""}, {"t2", true, ""}}, "ns/b: approved"},
		{"submit, both denied", approvalRequest, tap("UALICE1", chat.ActionSubmit, verdict(0, no), verdict(1, no)), `[{"Question":0,"Values":["1"]},{"Question":1,"Values":["1"]}]`,
			[]wireApproval{{"t1", false, denied}, {"t2", false, denied}}, "ns/b: denied"},
		{"submit, mixed", approvalRequest, tap("UALICE1", chat.ActionSubmit, verdict(1, yes), verdict(0, no)), `[{"Question":1,"Values":["0"]},{"Question":0,"Values":["1"]}]`,
			[]wireApproval{{"t1", false, denied}, {"t2", true, ""}}, "ns/b: partially_approved"},
		{"one tool, approved", oneToolRequest, tap("UALICE1", chat.ActionApproveAll), `{"action":"approve_all"}`,
			[]wireApproval{{"t1", true, ""}}, "ns/b: approved"},
		{"one tool, denied", oneToolRequest, tap("UALICE1", chat.ActionDenyAll), `{"action":"deny_all"}`,
			[]wireApproval{{"t1", false, denied}}, "ns/b: denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newApprovalRigOf(t, tc.request(t))
			if err := r.pl.Press(t.Context(), tc.press); err != nil {
				t.Fatal(err)
			}
			presses, _ := r.st.ListPresses(t.Context())
			if len(presses) != 1 || string(presses[0].Inputs) != tc.stored {
				t.Fatalf("presses %+v; want one holding %s", presses, tc.stored)
			}
			if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v", res, err)
			}
			if got, want := r.flow.list(), []string{"audit:answered", "submit", "send", "replace"}; !slices.Equal(got, want) {
				t.Errorf("steps %q; want the decision audited before it takes effect, then sent, then the prompt replaced: %q", got, want)
			}
			row := r.row(t)
			if row.State != store.InteractionAnswered || row.Decider != "UALICE1" || !row.DecidedAt.Equal(r.now) {
				t.Errorf("interaction = %+v; want answered by UALICE1 at the press's time", row)
			}
			sent := r.sess.answers()
			if len(sent) != 1 || sent[0].ID != "ix-1/answer" {
				t.Fatalf("%d messages sent; want one with the interaction's answer ID", len(sent))
			}
			if got, actor := approvalsSent(t, sent[0]); !slices.Equal(got, tc.want) || actor != "UALICE1" {
				t.Errorf("sent %+v as %q; want %+v as UALICE1", got, actor, tc.want)
			}
			// The answer names the platform the Binding's ChatConnection is on, and its text gets no prefix.
			if who, _ := sent[0].Metadata[kagentv1.ActorMetadataKey].(map[string]any); who["platform"] != "slack" || hasPrefix(sent[0]) {
				t.Errorf("answer actor = %v (prefixed: %v); want platform slack and no prefix in the text", who, hasPrefix(sent[0]))
			}
			if r.sess.keys[0] != key {
				t.Errorf("sent to %+v; want the thread %+v", r.sess.keys[0], key)
			}
			if len(r.aud.got) != 1 {
				t.Fatalf("%d audit rows; want one", len(r.aud.got))
			}
			d := r.aud.got[0]
			names := toolNamesOfTheRequest
			if len(tc.want) == 1 {
				names = names[:1]
			}
			if d.Result != audit.DecisionAnswered || d.TaskID != "task-1" || !d.At.Equal(r.now) || d.Answer == nil || len(d.Answer.Decisions) != len(tc.want) ||
				!slices.Equal(d.ToolNames, names) || d.Shape != nil || r.aud.act[0] != (audit.Actor{User: "UALICE1", Binding: "ns/b", Identity: "secret:ns/tok"}) {
				t.Errorf("audit row %+v by %+v; want answered by UALICE1, with the answer's decisions and the tools' names %q", d, r.aud.act[0], names)
			}
			if got := r.counted(); !slices.Equal(got, []string{tc.outcome}) || !slices.Equal(r.hooked(), []string{"answered"}) {
				t.Errorf("counted %q, hooks %q; want %q once, and one answered", got, r.hooked(), tc.outcome)
			}
			if told := r.eph.list(); len(told) != 0 || r.waiting(t) != 0 {
				t.Errorf("told %q, %d presses waiting; want nobody told and the press done", told, r.waiting(t))
			}
			if got := r.rep.replaced(); !slices.Equal(got, []string{"ix-1"}) {
				t.Errorf("replaced %q; want the prompt of ix-1", got)
			}
		})
	}
}

// TestTheAuditRowOfAnApprovalNamesTheToolsAndNotTheirArguments goes through the real audit writer: the row says who decided
// what, by tool, and holds none of the arguments.
func TestTheAuditRowOfAnApprovalNamesTheToolsAndNotTheirArguments(t *testing.T) {
	r := newApprovalRig(t)
	w := newWriter(t, r.st, audit.Options{Logger: quietLogger()})
	d, err := interact.NewDecider(decideCfg(interact.DecideConfig{Store: r.st, Auditor: w, Ephemerals: r.eph, Session: r.sess, Sessions: r.st, Replacer: r.rep, Outbox: r.out}))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := interact.NewPipeline(pressCfg(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{approvalBinding()}, Authorizer: r.az, Ephemerals: r.eph, Decider: d, Auditor: w,
		Logger: quietLogger(), Now: func() time.Time { return r.now }}))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []chat.Interaction{tap("UALICE1", chat.ActionSubmit, verdict(0, no), verdict(1, yes)), tap("UBOB22", chat.ActionSubmit, verdict(0, yes))} {
		if err := pl.Press(t.Context(), i); err != nil {
			t.Fatal(err)
		}
		presses, _ := r.st.ListPresses(t.Context())
		if _, err := pl.Process(t.Context(), presses[len(presses)-1]); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := r.st.ListAudit(t.Context(), time.Time{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.User+" "+row.Outcome)
	}
	want := []string{`UALICE1 answered approved=1 denied=1 approved_tools=["add_numbers"] denied_tools=["delete_everything"]`}
	if !slices.Equal(got, want) {
		t.Errorf("audit rows %q; want %q: the second press found the approval decided, and a press on one that is decided is no decision to audit", got, want)
	}
	if strings.Contains(strings.Join(got, "|"), `"/"`) || strings.Contains(strings.Join(got, "|"), "path") {
		t.Errorf("audit rows %q hold an argument", got)
	}
}

// TestAFailedAuditWriteSendsNothingAndKeepsThePress: an unaudited approval never takes effect (spec 8.6 step 7). The error is
// returned and the press stays recorded; nothing is recorded as decided, sent or counted; and the press, decided again once
// the log is back, is the decision.
func TestAFailedAuditWriteSendsNothingAndKeepsThePress(t *testing.T) {
	r := newApprovalRig(t)
	r.aud.err = errBoom
	if err := r.pl.Press(t.Context(), tap("UALICE1", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if res, err := r.pl.Process(t.Context(), presses[0]); !errors.Is(err, errBoom) || res != interact.Retry {
		t.Fatalf("Process = %v, %v; want Retry with the error", res, err)
	}
	if row := r.row(t); row.State != store.InteractionPending || len(row.Answer) != 0 || len(r.sess.answers()) != 0 || len(r.counted()) != 0 || r.waiting(t) != 1 ||
		len(r.hooked()) != 0 || len(r.rep.replaced()) != 0 || len(r.eph.list()) != 0 {
		t.Errorf("state %s, answer %q, %d sends, counted %q, %d waiting, hooks %q; want it pending, nothing sent, recorded, counted or told, and the press kept",
			row.State, row.Answer, len(r.sess.answers()), r.counted(), r.waiting(t), r.hooked())
	}
	if got := r.flow.list(); !slices.Equal(got, []string{"audit:answered"}) {
		t.Errorf("steps %q; want the audit alone: the decision was not taken", got)
	}
	r.aud.err = nil
	if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done || r.row(t).State != store.InteractionAnswered || len(r.sess.answers()) != 1 || !slices.Equal(r.counted(), []string{"ns/b: approved"}) {
		t.Errorf("Process again = %v, %v; state %s, %d sends, counted %q; want the press decided, once", res, err, r.row(t).State, len(r.sess.answers()), r.counted())
	}
}

// TestAnIncompleteSubmitIsRefusedAndAudited: a tool left without exactly one verdict is a refusal and not a decision. The person
// is told which tools need one, by number only; the audit row says how many were decided and nothing of which or how; nothing is
// sent or counted, and the interaction stays open.
func TestAnIncompleteSubmitIsRefusedAndAudited(t *testing.T) {
	for _, tc := range []struct {
		name  string
		one   bool // an approval of one tool
		press chat.Interaction
		told  string
		shape audit.ToolShape
	}{
		{"one tool of two left", false, tap("UALICE1", chat.ActionSubmit, verdict(0, yes)), "UALICE1: Please choose Approve or Deny for tool 2, then press Submit again.", audit.ToolShape{Decided: 1, Total: 2}},
		{"the first tool left", false, tap("UALICE1", chat.ActionSubmit, verdict(1, no)), "UALICE1: Please choose Approve or Deny for tool 1, then press Submit again.", audit.ToolShape{Decided: 1, Total: 2}},
		{"nothing chosen", false, tap("UALICE1", chat.ActionSubmit), "UALICE1: Please choose Approve or Deny for tool 1, 2, then press Submit again.", audit.ToolShape{Decided: 0, Total: 2}},
		{"a tool answered twice", false, tap("UALICE1", chat.ActionSubmit, verdict(0, yes), verdict(0, yes), verdict(1, no)), "UALICE1: Tool 1 could not be read. Please check it and press Submit again.", audit.ToolShape{Decided: 1, Total: 2}},
		{"a value that is no verdict", false, tap("UALICE1", chat.ActionSubmit, verdict(0, "7"), verdict(1, no)), "UALICE1: Tool 1 could not be read. Please check it and press Submit again.", audit.ToolShape{Decided: 1, Total: 2}},
		{"a tool the prompt does not have", false, tap("UALICE1", chat.ActionSubmit, verdict(0, yes), verdict(1, yes), verdict(5, yes)), "UALICE1: Tool 1, 2 could not be read. Please check it and press Submit again.", audit.ToolShape{Decided: 0, Total: 2}},
		// A prompt of one tool has no Submit and no radio, so a Submit of it holds no verdict: never a decision.
		{"a submit of one tool with no verdict", true, tap("UALICE1", chat.ActionSubmit), "UALICE1: Please choose Approve or Deny for tool 1, then press Submit again.", audit.ToolShape{Decided: 0, Total: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := approvalRequest(t)
			if tc.one {
				request = oneToolRequest(t)
			}
			r := newApprovalRigOf(t, request)
			if res, err := r.process(t, tc.press); err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v", res, err)
			}
			if got := strings.Join(r.eph.list(), "|"); got != tc.told {
				t.Errorf("told %q; want %q", got, tc.told)
			}
			if row := r.row(t); row.State != store.InteractionPending || len(r.sess.answers()) != 0 || len(r.counted()) != 0 || len(r.hooked()) != 0 || r.waiting(t) != 0 || len(r.rep.replaced()) != 0 {
				t.Errorf("state %s, %d sends, counted %q, hooks %q, %d waiting; want it open, nothing sent or counted, and the press done", row.State, len(r.sess.answers()), r.counted(), r.hooked(), r.waiting(t))
			}
			if got := r.calls.list(); !slices.Equal(got, []string{"audit:incomplete"}) || len(r.aud.got) != 1 {
				t.Fatalf("calls %q; want one incomplete decision audited and nothing submitted", got)
			}
			d := r.aud.got[0]
			if d.Result != audit.DecisionIncomplete || d.Shape == nil || *d.Shape != tc.shape || d.Answer != nil || d.ToolNames != nil || !d.At.Equal(r.now) ||
				r.aud.act[0] != (audit.Actor{User: "UALICE1", Binding: "ns/b", Identity: "secret:ns/tok"}) {
				t.Errorf("audit row %+v by %+v; want incomplete by UALICE1 with the shape %+v alone: no tool, no verdict", d, r.aud.act[0], tc.shape)
			}
		})
	}
}

// supersede is a press of user that was read while the approval was still open, as a press on another worker is, and is decided
// after snapshot's interaction has been decided by someone else: the decider finds it lost.
func supersede(t *testing.T, r *pressRig, snapshot store.Interaction, user string, action chat.Action) {
	t.Helper()
	var in dialect.Interaction
	if err := json.Unmarshal(snapshot.Request, &in); err != nil {
		t.Fatal(err)
	}
	answer, problems, err := interact.ExtractDecisions(in, action, nil)
	if err != nil || len(problems) != 0 {
		t.Fatalf("ExtractDecisions = %+v, %v", problems, err)
	}
	pd := interact.PressDecision{Interaction: snapshot, In: in, Answer: answer, User: user, PressedAt: r.now.Add(time.Second), BindingName: "ns/b", Binding: approvalBinding()}
	if err := r.d.Decide(t.Context(), pd); err != nil {
		t.Fatalf("Decide = %v", err)
	}
}

// TestASecondCompletePressIsSupersededAndToldWhoDecided: the first complete decision wins. The loser is told, in the words of an
// approval, who decided; their press is audited superseded; the outcome and the send are not repeated.
func TestASecondCompletePressIsSupersededAndToldWhoDecided(t *testing.T) {
	r := newApprovalRig(t)
	snapshot := r.row(t)
	if _, err := r.process(t, tap("UALICE1", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	supersede(t, r, snapshot, "UBOB22", chat.ActionDenyAll)
	if got := r.eph.list(); !slices.Equal(got, []string{"UBOB22: @UALICE1 decided this request first."}) {
		t.Errorf("told %q; want UBOB22 told, by mention, that UALICE1 decided first", got)
	}
	var results []audit.DecisionResult
	for _, d := range r.aud.got {
		results = append(results, d.Result)
	}
	if !slices.Equal(results, []audit.DecisionResult{audit.DecisionAnswered, audit.DecisionSuperseded}) || r.aud.act[1].User != "UBOB22" || r.aud.got[1].Answer != nil || r.aud.got[1].ToolNames != nil {
		t.Errorf("audit %+v by %+v; want answered by UALICE1 and superseded by UBOB22, the loser's with no answer", results, r.aud.act)
	}
	if got := r.counted(); !slices.Equal(got, []string{"ns/b: approved"}) || !slices.Equal(r.hooked(), []string{"answered", "superseded"}) || len(r.sess.answers()) != 1 || r.row(t).Decider != "UALICE1" {
		t.Errorf("counted %q, hooks %q, %d sends, decider %q; want the first decision's outcome once, and one send", got, r.hooked(), len(r.sess.answers()), r.row(t).Decider)
	}
}

// TestTheSamePersonsRetryAfterACrashSendsAgainWithoutASecondAuditOrCount: the answer is recorded, the send failed or the process
// died; the person's press, decided again, resends the recorded answer, whatever that press holds (see
// TestTheWinnersSecondPressSendsTheRecordedAnswerAndNotItsOwn). It is the same decision: audited and counted once.
func TestTheSamePersonsRetryAfterACrashSendsAgainWithoutASecondAuditOrCount(t *testing.T) {
	for name, failFirstSend := range map[string]bool{"a press decided again": false, "a send that failed": true} {
		t.Run(name, func(t *testing.T) {
			r := newApprovalRig(t)
			if failFirstSend {
				r.sess.errs = []error{errBoom}
				if err := r.pl.Press(t.Context(), tap("UALICE1", chat.ActionSubmit, verdict(0, no), verdict(1, yes))); err != nil {
					t.Fatal(err)
				}
				presses, _ := r.st.ListPresses(t.Context())
				if res, err := r.pl.Process(t.Context(), presses[0]); !errors.Is(err, errBoom) || res != interact.Retry {
					t.Fatalf("Process with a failing send = %v, %v; want Retry with the error", res, err)
				}
				if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done {
					t.Fatalf("Process again = %v, %v", res, err)
				}
			} else {
				for range 2 {
					if _, err := r.process(t, tap("UALICE1", chat.ActionSubmit, verdict(0, no), verdict(1, yes))); err != nil {
						t.Fatal(err)
					}
				}
			}
			sent := r.sess.answers()
			if len(sent) != 2 || len(r.aud.got) != 1 || !slices.Equal(r.counted(), []string{"ns/b: partially_approved"}) || len(r.eph.list()) != 0 || r.waiting(t) != 0 {
				t.Fatalf("%d sends, %d audit rows, counted %q, told %q, %d waiting; want the answer sent twice, one decision audited and counted, nobody told",
					len(sent), len(r.aud.got), r.counted(), r.eph.list(), r.waiting(t))
			}
			a, _ := approvalsSent(t, sent[0])
			b, _ := approvalsSent(t, sent[1])
			if !slices.Equal(a, b) || sent[0].ID != sent[1].ID {
				t.Errorf("the two sends differ: %+v and %+v; want the same message", a, b)
			}
		})
	}
}

// TestANonMemberAndABotAreRefusedAndAudited: neither decides, the refusal is a decision of its own and is audited, in the
// words of an approval.
func TestANonMemberAndABotAreRefusedAndAudited(t *testing.T) {
	for name, tc := range map[string]struct {
		user, told string
		bot        bool
	}{
		"not a member": {"UOUTSIDE", "UOUTSIDE: Only members of this channel can decide this request.", false},
		"a bot":        {"UALICE1", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newApprovalRig(t)
			if tc.bot {
				r.az.bots = map[string]bool{tc.user: true}
			}
			if res, err := r.process(t, tap(tc.user, chat.ActionApproveAll)); err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v", res, err)
			}
			if got := strings.Join(r.eph.list(), "|"); got != tc.told {
				t.Errorf("told %q; want %q", got, tc.told)
			}
			if row := r.row(t); row.State != store.InteractionPending || len(r.sess.answers()) != 0 || len(r.counted()) != 0 || r.waiting(t) != 0 {
				t.Errorf("state %s, %d sends, counted %q; want it open, nothing sent or counted", row.State, len(r.sess.answers()), r.counted())
			}
			if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionDenied || r.aud.got[0].Answer != nil || r.aud.act[0].User != tc.user || !r.aud.got[0].At.Equal(r.now) {
				t.Errorf("audit %+v by %+v; want one denied decision by %s at the press's time", r.aud.got, r.aud.act, tc.user)
			}
		})
	}
}

// TestALateApprovalPressIsToldAndNotAudited: at or after the deadline the press is no decision at all, and is told so in the
// words of an approval.
func TestALateApprovalPressIsToldAndNotAudited(t *testing.T) {
	r := newApprovalRig(t)
	r.now = deadline
	if _, err := r.process(t, tap("UALICE1", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: This request has timed out, so your decision was not taken."}) {
		t.Errorf("told %q", got)
	}
	if len(r.aud.got) != 0 || len(r.counted()) != 0 || !slices.Equal(r.hooked(), []string{"late"}) || r.row(t).State != store.InteractionPending || len(r.sess.answers()) != 0 {
		t.Errorf("%d audit rows, counted %q, hooks %q; want none audited or counted, and one late", len(r.aud.got), r.counted(), r.hooked())
	}
}

// TestANestedApprovalWorksEndToEnd: a sub-agent's approval is stored with the kind nested, whatever the request is. What a press
// means, and the words it is told in, come from the decoded request's kind: it is decided, counted and named, and a person
// who cannot decide it or who lost is told in the words of an approval.
func TestANestedApprovalWorksEndToEnd(t *testing.T) {
	r := newPressRigFor(t, approvalBinding())
	openApproval(t, r, store.InteractionNested, approvalRequest(t))
	snapshot := r.row(t)
	if snapshot.Kind != store.InteractionNested {
		t.Fatalf("row kind %q; want nested", snapshot.Kind)
	}
	if _, err := r.process(t, tap("UOUTSIDE", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	if res, err := r.process(t, tap("UALICE1", chat.ActionSubmit, verdict(0, yes), verdict(1, no))); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v", res, err)
	}
	r.now = r.now.Add(time.Second)
	if _, err := r.process(t, tap("UBOB22", chat.ActionDenyAll)); err != nil { // found decided: told, and no decision of its own
		t.Fatal(err)
	}
	supersede(t, r, snapshot, "UCAROL1", chat.ActionDenyAll) // found decided only in the decider
	if got := r.eph.list(); !slices.Equal(got, []string{"UOUTSIDE: Only members of this channel can decide this request.", "UBOB22: @UALICE1 decided this request first.", "UCAROL1: @UALICE1 decided this request first."}) {
		t.Errorf("told %q; want all three in the words of an approval", got)
	}
	var results []audit.DecisionResult
	for _, d := range r.aud.got {
		results = append(results, d.Result)
	}
	if !slices.Equal(results, []audit.DecisionResult{audit.DecisionDenied, audit.DecisionAnswered, audit.DecisionSuperseded}) || !slices.Equal(r.aud.got[1].ToolNames, toolNamesOfTheRequest) {
		t.Errorf("audit %q, names %q; want denied, answered and superseded, the answer naming the tools", results, r.aud.got[1].ToolNames)
	}
	got, actor := approvalsSent(t, r.sess.answers()[0])
	if !slices.Equal(got, []wireApproval{{"t1", true, ""}, {"t2", false, denied}}) || actor != "UALICE1" || !slices.Equal(r.counted(), []string{"ns/b: partially_approved"}) {
		t.Errorf("sent %+v as %q, counted %q; want the mixed decision sent as UALICE1 and counted once", got, actor, r.counted())
	}
}

// TestAPressOnADecidedApprovalIsToldInTheWordsOfAnApproval: what the person is told when the approval is no longer open, by how
// it ended, and when the prompt is not ready, is gone, or the worker gave up on the press.
func TestAPressOnADecidedApprovalIsToldInTheWordsOfAnApproval(t *testing.T) {
	for name, tc := range map[string]struct {
		state store.InteractionState
		want  string
	}{
		"answered":  {store.InteractionAnswered, "UBOB22: @UALICE1 decided this request first."},
		"timed out": {store.InteractionTimedOut, "UBOB22: This request has timed out."},
		"cancelled": {store.InteractionCancelled, "UBOB22: This request is closed."},
	} {
		t.Run(name, func(t *testing.T) {
			r := newApprovalRig(t)
			if tc.state == store.InteractionAnswered {
				_ = r.st.SubmitInteraction(t.Context(), "ix-1", []byte(`{"decisions":[{"tool_id":"t1","approved":true},{"tool_id":"t2","approved":true}]}`), "UALICE1", d0.Add(30*time.Second))
			} else {
				_ = r.st.ResolveInteraction(t.Context(), "ix-1", tc.state, deadline)
			}
			if _, err := r.process(t, tap("UBOB22", chat.ActionApproveAll)); err != nil || r.waiting(t) != 0 {
				t.Fatalf("Process = %v with %d presses left", err, r.waiting(t))
			}
			if got := r.eph.list(); !slices.Equal(got, []string{tc.want}) || len(r.aud.got) != 0 || len(r.sess.answers()) != 0 {
				t.Errorf("told %q, %d audit rows, %d sends; want %q and nothing else", got, len(r.aud.got), len(r.sess.answers()), tc.want)
			}
		})
	}
	t.Run("not ready", func(t *testing.T) {
		r := newPressRigFor(t, approvalBinding())
		row := store.Interaction{ID: "ix-1", Key: key, TaskID: "task-1", Kind: store.InteractionNested, Request: approvalRequest(t), CreatedAt: d0, Deadline: deadline}
		if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
			t.Fatal(err)
		}
		if err := r.pl.Press(t.Context(), tap("UALICE1", chat.ActionApproveAll)); err != nil {
			t.Fatal(err)
		}
		presses, _ := r.st.ListPresses(t.Context())
		r.now = r.now.Add(3 * time.Minute) // past EarlyFor, and no prompt recorded yet
		if _, err := r.pl.Process(t.Context(), presses[0]); err != nil {
			t.Fatal(err)
		}
		if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: This request is not ready yet. Please press again in a moment."}) {
			t.Errorf("told %q", got)
		}
	})
	t.Run("the worker gave up", func(t *testing.T) {
		r := newApprovalRig(t)
		if err := r.pl.Press(t.Context(), tap("UALICE1", chat.ActionApproveAll)); err != nil {
			t.Fatal(err)
		}
		presses, _ := r.st.ListPresses(t.Context())
		if err := r.pl.GiveUp(t.Context(), presses[0]); err != nil {
			t.Fatal(err)
		}
		if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: Couldn't process your decision. Please press again."}) || r.waiting(t) != 0 {
			t.Errorf("told %q, %d waiting", got, r.waiting(t))
		}
	})
}

// TestAPressOnAnApprovalWhoseBindingNoLongerAllowsThemDecidesNothing: the flag was turned off after the prompt was posted. A
// press must not approve a tool call the Binding now forbids; until the intake records the policy denial, the press only says
// the request is unavailable.
func TestAPressOnAnApprovalWhoseBindingNoLongerAllowsThemDecidesNothing(t *testing.T) {
	r := newPressRigFor(t, interact.Binding{Dialect: kagentv1.New(time.Second), ToolApprovals: false, AskUser: true, Timeout: time.Minute})
	openApproval(t, r, store.InteractionToolApproval, approvalRequest(t))
	if _, err := r.process(t, tap("UALICE1", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: This request is no longer available."}) || r.row(t).State != store.InteractionPending ||
		len(r.sess.answers()) != 0 || len(r.aud.got) != 0 || len(r.counted()) != 0 || r.waiting(t) != 0 {
		t.Errorf("told %q, state %s, %d sends, %d audit rows; want it told unavailable and nothing decided", got, r.row(t).State, len(r.sess.answers()), len(r.aud.got))
	}
}

// TestAPressByTheWinnerResendsAnUndeliveredApprovalWhenBindingIsDisabled: a human verdict already recorded while approvals
// were enabled remains the winner. The same person's retry goes through the decider and resends that verdict, even though the
// reloaded Binding now denies new approvals.
func TestAPressByTheWinnerResendsAnUndeliveredApprovalWhenBindingIsDisabled(t *testing.T) {
	r := newPressRigFor(t, interact.Binding{Dialect: kagentv1.New(time.Second), ToolApprovals: false, AskUser: true, Timeout: time.Minute})
	openApproval(t, r, store.InteractionToolApproval, approvalRequest(t))
	answer, err := json.Marshal(dialect.Answer{Decisions: []dialect.Decision{{ToolID: "t1", Approved: false, Reason: denied}, {ToolID: "t2", Approved: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.SubmitInteraction(t.Context(), "ix-1", answer, "UALICE1", r.now); err != nil {
		t.Fatal(err)
	}
	if res, err := r.process(t, tap("UALICE1", chat.ActionApproveAll)); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v; want the winner's retry finished", res, err)
	}
	sent := r.sess.answers()
	if len(sent) != 1 || len(r.eph.list()) != 0 || len(r.aud.got) != 0 || len(r.counted()) != 0 || r.waiting(t) != 0 {
		t.Fatalf("%d sends, told %q, %d audit rows, counted %q, %d waiting; want the recorded verdict resent without a false denial", len(sent), r.eph.list(), len(r.aud.got), r.counted(), r.waiting(t))
	}
	if got, actor := approvalsSent(t, sent[0]); !slices.Equal(got, []wireApproval{{"t1", false, denied}, {"t2", true, ""}}) || actor != "UALICE1" {
		t.Errorf("resent %+v as %q; want the recorded human verdict as UALICE1", got, actor)
	}
}

func TestAPressLosingToPolicyDenialIsToldThePolicyDecision(t *testing.T) {
	r := newPressRigFor(t, interact.Binding{Dialect: kagentv1.New(time.Second), ToolApprovals: false, AskUser: true, Timeout: time.Minute})
	openApproval(t, r, store.InteractionToolApproval, approvalRequest(t))
	if err := r.st.SubmitInteraction(t.Context(), "ix-1", []byte(`{"decisions":[{"tool_id":"t1","approved":false},{"tool_id":"t2","approved":false}]}`), "agent-kourier", r.now); err != nil {
		t.Fatal(err)
	}
	if res, err := r.process(t, tap("UALICE1", chat.ActionApproveAll)); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v; want the losing press finished", res, err)
	}
	if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: This request was denied because this Binding does not allow approvals."}) {
		t.Errorf("told %q; want the policy decision text", got)
	}
}

// TestPressesRecordedInEachStoredFormAreRead: literal Inputs, as each version wrote them. The bare array of the version on
// main is a Submit; the object names its action; a form this version cannot read is told the request is gone.
func TestPressesRecordedInEachStoredFormAreRead(t *testing.T) {
	for _, tc := range []struct {
		name, inputs string
		want         []wireApproval // nil: nothing is decided
		told         string
	}{
		{"main's array, two verdicts", `[{"Question":0,"Values":["0"]},{"Question":1,"Values":["1"]}]`, []wireApproval{{"t1", true, ""}, {"t2", false, denied}}, ""},
		{"main's array, null", `null`, nil, "UALICE1: Please choose Approve or Deny for tool 1, 2, then press Submit again."},
		{"main's array, one verdict", `[{"Question":1,"Values":["0"]}]`, nil, "UALICE1: Please choose Approve or Deny for tool 1, then press Submit again."},
		{"approve all", `{"action":"approve_all"}`, []wireApproval{{"t1", true, ""}, {"t2", true, ""}}, ""},
		{"deny all", `{"action":"deny_all"}`, []wireApproval{{"t1", false, denied}, {"t2", false, denied}}, ""},
		{"submit, named", `{"action":"submit","answers":[{"Question":0,"Values":["1"]},{"Question":1,"Values":["1"]}]}`, []wireApproval{{"t1", false, denied}, {"t2", false, denied}}, ""},
		{"slack's state of the version before", `{"q0":{"a0":{"Type":"plain_text_input","Values":["x"]}}}`, nil, "UALICE1: This request is no longer available."},
		{"an action of the future", `{"action":"abstain"}`, nil, "UALICE1: This request is no longer available."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newApprovalRig(t)
			press := store.Press{InteractionID: "ix-1", User: "UALICE1", Channel: key.Channel, MessageRef: "100.1", Inputs: []byte(tc.inputs), PressedAt: r.now}
			seq, err := r.st.RecordPress(t.Context(), press, store.PressLimits{Total: 10, PerUser: 2})
			if err != nil {
				t.Fatal(err)
			}
			press.Seq = seq
			if res, err := r.pl.Process(t.Context(), press); err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v", res, err)
			}
			if tc.want == nil {
				if got := strings.Join(r.eph.list(), "|"); got != tc.told || r.row(t).State != store.InteractionPending || len(r.sess.answers()) != 0 {
					t.Errorf("told %q, state %s, %d sends; want %q, and nothing decided", got, r.row(t).State, len(r.sess.answers()), tc.told)
				}
				return
			}
			sent := r.sess.answers()
			if len(sent) != 1 {
				t.Fatalf("%d sends; want one", len(sent))
			}
			if got, _ := approvalsSent(t, sent[0]); !slices.Equal(got, tc.want) {
				t.Errorf("sent %+v; want %+v", got, tc.want)
			}
		})
	}
}

// TestAPressRecordedNowIsStoredAsTheDocumentedForms: Press writes the literal forms the tests above read.
func TestAPressRecordedNowIsStoredAsTheDocumentedForms(t *testing.T) {
	r := newApprovalRig(t)
	for _, tc := range []struct {
		i    chat.Interaction
		want string
	}{
		{tap("UALICE1", chat.ActionApproveAll), `{"action":"approve_all"}`},
		{tap("UALICE1", chat.ActionDenyAll), `{"action":"deny_all"}`},
		{tap("UBOB22", chat.ActionSubmit, verdict(0, yes)), `[{"Question":0,"Values":["0"]}]`},
	} {
		if err := r.pl.Press(t.Context(), tc.i); err != nil {
			t.Fatal(err)
		}
	}
	presses, _ := r.st.ListPresses(t.Context())
	var got []string
	for _, p := range presses {
		got = append(got, string(p.Inputs))
	}
	if want := []string{`{"action":"approve_all"}`, `{"action":"deny_all"}`, `[{"Question":0,"Values":["0"]}]`}; !slices.Equal(got, want) {
		t.Errorf("stored %q; want %q", got, want)
	}
	if err := r.pl.Press(t.Context(), tap("UBOB22", chat.Action(9))); err == nil {
		t.Error("a press of an action no version could read was recorded")
	}
	if presses, _ := r.st.ListPresses(t.Context()); len(presses) != 3 {
		t.Errorf("%d presses recorded; want the three", len(presses))
	}
}

// TestAQuestionPressedWithAnotherButtonIsRefusedAsInvalid: a question has one button, Submit. A press of the buttons of an approval
// on it is no honest press of its controls: every question is refused as unreadable, audited incomplete with the shape, and nothing
// is decided.
func TestAQuestionPressedWithAnotherButtonIsRefusedAsInvalid(t *testing.T) {
	for _, action := range []chat.Action{chat.ActionApproveAll, chat.ActionDenyAll} {
		r := newPressRig(t)
		r.open(t)
		i := r.submit(t, "UALICE1", 0)
		i.Action = action
		if res, err := r.process(t, i); err != nil || res != interact.Done {
			t.Fatalf("Process = %v, %v", res, err)
		}
		if got := strings.Join(r.eph.list(), "|"); got != "UALICE1: Question 1 could not be read. Please check it and press Submit again." ||
			r.row(t).State != store.InteractionPending || len(r.sess.answers()) != 0 {
			t.Errorf("%v: told %q, state %s, %d sends; want the question refused and nothing decided", action, got, r.row(t).State, len(r.sess.answers()))
		}
		if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionIncomplete || r.aud.got[0].Shape != nil || len(r.aud.got[0].Answer.Answers) != 0 {
			t.Errorf("audit %+v; want incomplete with a shape of no question answered", r.aud.got)
		}
	}
}

// TestTheReplacementOfADecidedApproval: the prompt is replaced by an ending that holds the tools as asked and the verdict of
// each, in the order of the tools and by tool ID, who decided and when. Through the chat fake, which refuses an ending that is
// not valid.
func TestTheReplacementOfADecidedApproval(t *testing.T) {
	r := newApprovalRig(t)
	if _, err := r.process(t, tap("UALICE1", chat.ActionSubmit, verdict(0, no), verdict(1, yes))); err != nil {
		t.Fatal(err)
	}
	row := r.row(t)
	out, err := interact.ReplacementOutgoing(row)
	if err != nil || out.Ending == nil {
		t.Fatalf("ReplacementOutgoing = %+v, %v", out, err)
	}
	e := *out.Ending
	if e.Validate() != nil || e.Outcome != chat.Answered || e.By != "UALICE1" || !e.At.Equal(r.now) || e.Prompt.Subagent != "planner" || len(e.Prompt.Tools) != 2 ||
		e.Prompt.Tools[0].Name != "delete_everything" || e.Prompt.Tools[1].Name != "add_numbers" || !slices.Equal(e.Verdicts, []chat.ToolVerdict{chat.ToolDenied, chat.ToolApproved}) {
		t.Errorf("ending %+v, %v; want answered by UALICE1 at the press's time, delete_everything denied and add_numbers approved", e, e.Validate())
	}

	// The prompt is posted to the chat fake, and a Replacer edits it into the ending from the row the pipeline left.
	fake := chattest.NewFake()
	ref, err := fake.Post(t.Context(), chat.Target{Channel: key.Channel, Thread: key.Thread}, chat.Ask(chat.Prompt{ID: "ix-2", Tools: []chat.Tool{{Name: "delete_everything"}, {Name: "add_numbers"}}}), "ix-2")
	if err != nil {
		t.Fatal(err)
	}
	rr := newReplaceRigWith(t, func(c *interact.ReplacerConfig) { c.Updater = fake })
	copied := store.Interaction{ID: "ix-2", Key: key, TaskID: "task-1", Kind: store.InteractionNested, Request: row.Request, CreatedAt: d0, Deadline: deadline}
	if _, err := rr.st.CreateInteraction(t.Context(), copied); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(rr.st.SetInteractionMessageRef(t.Context(), "ix-2", ref.ID), rr.st.SubmitInteraction(t.Context(), "ix-2", row.Answer, row.Decider, row.DecidedAt)); err != nil {
		t.Fatal(err)
	}
	if err := rr.r.Replace(t.Context(), "ix-2"); err != nil {
		t.Fatalf("Replace = %v; want the fake to take the ending", err)
	}
	if msgs := fake.Messages(); len(msgs) != 1 || msgs[0].Updates != 1 || msgs[0].Out.Ending == nil || !slices.Equal(msgs[0].Out.Ending.Verdicts, e.Verdicts) || msgs[0].Out.Ending.By != "UALICE1" {
		t.Errorf("messages %+v; want the prompt replaced once by the ending", msgs)
	}
}

// TestTheEndingOfAnApprovalTheBrokerDeniedOnItsOwn: a Binding whose flag is turned off with a prompt pending leaves a row denied
// by "agent-kourier" that has a message ref, and its prompt must be replaced: the ending is valid, all denied, and names no
// person. A reorderd answer is matched by tool ID.
func TestTheEndingOfAnApprovalTheBrokerDeniedOnItsOwn(t *testing.T) {
	row := store.Interaction{ID: "ix-1", Key: key, TaskID: "task-1", Kind: store.InteractionToolApproval, Request: approvalRequest(t), MessageRef: "100.1",
		State: store.InteractionAnswered, Decider: "agent-kourier", DecidedAt: d0,
		Answer: []byte(`{"request_id":"","answers":null,"decisions":[{"tool_id":"t2","approved":false,"reason":"x"},{"tool_id":"t1","approved":false,"reason":"x"}]}`)}
	out, err := interact.ReplacementOutgoing(row)
	if err != nil || out.Ending == nil {
		t.Fatalf("ReplacementOutgoing = %+v, %v", out, err)
	}
	if e := *out.Ending; e.Validate() != nil || e.By != "" || !slices.Equal(e.Verdicts, []chat.ToolVerdict{chat.ToolDenied, chat.ToolDenied}) || e.Outcome != chat.Answered {
		t.Errorf("ending %+v, %v; want a valid answered ending, both tools denied, no one named", e, e.Validate())
	}
	// The decisions are matched by ID and not by position.
	row.Decider = "UALICE1"
	row.Answer = []byte(`{"decisions":[{"tool_id":"t2","approved":true},{"tool_id":"t1","approved":false}]}`)
	if out, err := interact.ReplacementOutgoing(row); err != nil || !slices.Equal(out.Ending.Verdicts, []chat.ToolVerdict{chat.ToolDenied, chat.ToolApproved}) || out.Ending.By != "UALICE1" {
		t.Errorf("ReplacementOutgoing = %+v, %v; want t1 denied and t2 approved, by UALICE1", out, err)
	}
}

// TestAnAnswerThatDoesNotMatchTheRequestHasNoEnding: a stored answer that decides other tools than the request asked about, or
// not all of them, or one twice, is an error and not an ending guessed from it.
func TestAnAnswerThatDoesNotMatchTheRequestHasNoEnding(t *testing.T) {
	row := store.Interaction{ID: "ix-1", Key: key, TaskID: "task-1", Kind: store.InteractionToolApproval, Request: approvalRequest(t), MessageRef: "100.1",
		State: store.InteractionAnswered, Decider: "UALICE1", DecidedAt: d0}
	for name, answer := range map[string]string{
		"one decision of two":      `{"decisions":[{"tool_id":"t1","approved":true}]}`,
		"a tool that is not asked": `{"decisions":[{"tool_id":"t1","approved":true},{"tool_id":"zz","approved":true}]}`,
		"a tool decided twice":     `{"decisions":[{"tool_id":"t1","approved":true},{"tool_id":"t1","approved":false}]}`,
		"three decisions":          `{"decisions":[{"tool_id":"t1","approved":true},{"tool_id":"t2","approved":true},{"tool_id":"t3","approved":true}]}`,
		"no decisions":             `{"request_id":"r","answers":[["a"]]}`,
		"an answer to questions":   `{"answers":[["a"],["b"]]}`,
	} {
		row.Answer = []byte(answer)
		if out, err := interact.ReplacementOutgoing(row); err == nil {
			t.Errorf("%s: ReplacementOutgoing = %+v; want an error", name, out)
		}
	}
}

// askUserSent is the answer an ask_user_response message carries, for the first question.
func askUserSent(t *testing.T, msg *a2a.Message) []string {
	t.Helper()
	payload, _ := msg.Metadata[kagentv1.HITLExtension].(map[string]any)
	list, _ := payload["answers"].([]any)
	if len(list) != 1 {
		t.Fatalf("payload %v; want one answer", payload)
	}
	first, _ := list[0].(map[string]any)
	out, _ := first["answer"].([]string)
	return out
}

// TestTheWinnersSecondPressSendsTheRecordedAnswerAndNotItsOwn: once a person's decision is recorded, the message that reaches the
// agent is the recorded one, whatever the same person presses next (the prompt may still show its controls). It is the
// decision that was audited, counted, stored and shown in the ending; a different message would take effect unaudited
// (spec 8.6 step 7). The second press may even be late: it only resends.
func TestTheWinnersSecondPressSendsTheRecordedAnswerAndNotItsOwn(t *testing.T) {
	bothDenied := []wireApproval{{"t1", false, denied}, {"t2", false, denied}}
	bothApproved := []wireApproval{{"t1", true, ""}, {"t2", true, ""}}
	mixed := []wireApproval{{"t1", true, ""}, {"t2", false, denied}}
	for _, tc := range []struct {
		name          string
		first, second chat.Interaction
		recorded      []wireApproval
		late          bool
	}{
		{"deny all, then approve all", tap("UALICE1", chat.ActionDenyAll), tap("UALICE1", chat.ActionApproveAll), bothDenied, false},
		{"approve all, then deny all", tap("UALICE1", chat.ActionApproveAll), tap("UALICE1", chat.ActionDenyAll), bothApproved, false},
		{"a mixed submit, then another", tap("UALICE1", chat.ActionSubmit, verdict(0, yes), verdict(1, no)), tap("UALICE1", chat.ActionSubmit, verdict(0, no), verdict(1, yes)), mixed, false},
		{"a mixed submit, then approve all", tap("UALICE1", chat.ActionSubmit, verdict(0, yes), verdict(1, no)), tap("UALICE1", chat.ActionApproveAll), mixed, false},
		{"deny all, then approve all, too late", tap("UALICE1", chat.ActionDenyAll), tap("UALICE1", chat.ActionApproveAll), bothDenied, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newApprovalRig(t)
			if _, err := r.process(t, tc.first); err != nil {
				t.Fatal(err)
			}
			if tc.late {
				r.now = deadline.Add(time.Second)
			}
			if res, err := r.process(t, tc.second); err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v", res, err)
			}
			sent := r.sess.answers()
			if len(sent) != 2 {
				t.Fatalf("%d messages sent; want the recorded answer sent twice", len(sent))
			}
			for i, m := range sent {
				if got, _ := approvalsSent(t, m); !slices.Equal(got, tc.recorded) {
					t.Errorf("message %d carries %+v; want the recorded %+v", i+1, got, tc.recorded)
				}
				// The second press rebuilds the recorded answer (Decider.recorded), which names the Binding's platform too.
				if got := platformSent(m); got != "slack" {
					t.Errorf("message %d is from a user on platform %q; want slack", i+1, got)
				}
			}
			if len(r.aud.got) != 1 || len(r.counted()) != 1 {
				t.Errorf("%d audit rows, counted %q; want the one decision, audited and counted once", len(r.aud.got), r.counted())
			}
		})
	}
}

// TestTheWinnersSecondPressOnAQuestionSendsTheRecordedAnswer is the same rule for a question: the answer is the recorded one.
func TestTheWinnersSecondPressOnAQuestionSendsTheRecordedAnswer(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	for _, idx := range []int{0, 1} { // a, then b
		if _, err := r.process(t, r.submit(t, "UALICE1", idx)); err != nil {
			t.Fatal(err)
		}
	}
	sent := r.sess.answers()
	if len(sent) != 2 || len(r.aud.got) != 1 {
		t.Fatalf("%d messages, %d audit rows; want two sends of one decision", len(sent), len(r.aud.got))
	}
	for i, m := range sent {
		if got := askUserSent(t, m); !slices.Equal(got, []string{"a"}) {
			t.Errorf("message %d carries %q; want the recorded answer a", i+1, got)
		}
	}
}

// TestTheWinnersPressAfterTheAnswerWasClearedSendsNothing: retention has cleared the recorded answer, so there is nothing that
// was audited to send. The press is done, and nothing else is.
func TestTheWinnersPressAfterTheAnswerWasClearedSendsNothing(t *testing.T) {
	r := newApprovalRig(t)
	snapshot := r.row(t) // as a press that read the approval while it was open has it
	if _, err := r.process(t, tap("UALICE1", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	if n, err := r.st.ClearSettledInteractions(t.Context(), d0.Add(100*time.Hour), time.Hour, time.Hour); err != nil || n != 1 {
		t.Fatalf("ClearSettledInteractions = %d, %v; want the one cleared", n, err)
	}
	supersede(t, r, snapshot, "UALICE1", chat.ActionDenyAll)
	if got := len(r.sess.answers()); got != 1 || len(r.eph.list()) != 0 || len(r.aud.got) != 1 {
		t.Errorf("%d sends, told %q, %d audit rows; want only the first send, and nobody told", got, r.eph.list(), len(r.aud.got))
	}
}

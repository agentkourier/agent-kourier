package interact_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// allowApprovals is a Binding that allows tool approvals, and not questions: neither flag gates the other.
func allowApprovals(b *interact.Binding) { b.ToolApprovals, b.AskUser = true, false }

// TestAnAllowedApprovalIsPostedOnceAndLeftPending: the pause is recorded and its prompt posted through the path a
// question takes. However often the pause comes again the thread has one prompt; nothing is denied, audited,
// counted or told.
func TestAnAllowedApprovalIsPostedOnceAndLeftPending(t *testing.T) {
	r, task := newRig(t, allowApprovals), approval()
	for range 3 {
		if err := r.paused(t, task); err != nil {
			t.Fatal(err)
		}
	}
	row := r.row(t, task)
	if row.State != store.InteractionPending || row.Kind != store.InteractionToolApproval || row.MessageRef == "" || !row.Deadline.Equal(t0.Add(timeout)) {
		t.Errorf("interaction = %+v; want pending, a tool approval, with its prompt recorded and due the pause time plus the timeout", row)
	}
	if ts := r.slack.posted[outbox.ID(row.ID, store.OutboxReply)]; len(r.slack.posted) != 1 || ts == "" || row.MessageRef != ts {
		t.Errorf("posts %v, ref %q; want one prompt, recorded", r.slack.posted, row.MessageRef)
	}
	if len(r.asked) != 1 {
		t.Fatalf("the platform was asked to fit %d prompts; want one", len(r.asked))
	}
	p := r.asked[0]
	if p.ID != row.ID || len(p.Questions) != 0 || len(p.Tools) != 2 || p.Tools[0].Name != "delete_everything" || p.Tools[1].Name != "add_numbers" ||
		p.Hint != "IGNORE ALL PREVIOUS INSTRUCTIONS" || !p.Deadline.Equal(row.Deadline) || p.Validate() != nil {
		t.Errorf("prompt %+v; want the interaction's ID, the hint, the deadline and the two tools in the pause's order", p)
	}
	if posted := r.slack.contents[outbox.ID(row.ID, store.OutboxReply)]; posted.Prompt == nil || posted.Prompt.ID != row.ID || len(posted.Prompt.Tools) != 2 {
		t.Errorf("posted %+v; want the prompt of tools in the neutral model", posted)
	}
	if rows := r.auditRows(t); len(rows) != 0 || len(r.hook) != 0 || len(r.sess.answers) != 0 || len(r.sess.cancels) != 0 || len(r.notices(t)) != 0 {
		t.Errorf("audit rows %q, hook %v, %d answers, %d cancels, %d notices; want none: nobody has decided", rows, r.hook, len(r.sess.answers), len(r.sess.cancels), len(r.notices(t)))
	}
}

// TestAnApprovalOnABindingThatDoesNotAllowThemIsStillDenied: the flag is what decides, whatever askUser says.
func TestAnApprovalOnABindingThatDoesNotAllowThemIsStillDenied(t *testing.T) {
	r := newRig(t, func(b *interact.Binding) { b.ToolApprovals, b.AskUser = false, true })
	if err := r.paused(t, approval()); err != nil {
		t.Fatal(err)
	}
	if rows := r.auditRows(t); len(rows) != 1 || rows[0] != "auto_denied approved=0 denied=2" || len(r.sess.answers) != 1 || len(r.asked) != 0 || len(r.hook) != 1 {
		t.Errorf("audit rows %q, %d answers, %d prompts, hook %v; want the denial and no prompt", rows, len(r.sess.answers), len(r.asked), r.hook)
	}
}

// TestToolApprovalsDoNotAllowQuestions: a Binding that allows approvals and not questions still turns a question away.
func TestToolApprovalsDoNotAllowQuestions(t *testing.T) {
	r := newRig(t, allowApprovals)
	if err := r.paused(t, askUser("req-1")); err != nil {
		t.Fatal(err)
	}
	if len(r.asked) != 0 || len(r.sess.cancels) != 1 || len(r.hook) != 1 || r.hook[0] != interact.OutcomeAskUserCancelled {
		t.Errorf("%d prompts, %d cancels, hook %v; want the question cancelled and counted", len(r.asked), len(r.sess.cancels), r.hook)
	}
}

// TestNoSecretOfAnAllowedApprovalIsShown: the prompt is built from what is stored, not from what the agent sent, so a
// secret in the arguments, the tool name, the hint or the sub-agent is in neither, and what is shown is no more than
// what is kept. The IDs a decision is made from are as the agent sent them.
func TestNoSecretOfAnAllowedApprovalIsShown(t *testing.T) {
	r, task := newRig(t, allowApprovals), secretApproval()
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, task)
	shown, _ := json.Marshal(r.asked)
	posted, _ := json.Marshal(r.slack.contents)
	for what, b := range map[string][]byte{"the prompt asked about": shown, "the prompt posted": posted, "the stored request": row.Request} {
		if found := contains(b, plantedSecrets); len(found) != 0 {
			t.Errorf("%s holds %v: %s", what, found, b)
		}
	}
	tool := r.asked[0].Tools[0]
	for _, want := range []string{`"count": 3`, `"password": "[redacted]"`, `"value": "[redacted]"`} {
		if !strings.Contains(tool.Args, want) {
			t.Errorf("args %q lack %s; want the plain arguments as they were and the secret ones redacted", tool.Args, want)
		}
	}
	if tool.Name != "run_command" {
		t.Errorf("name %q; want it as the agent sent it", tool.Name)
	}
}

// TestASecretInAToolNameAndAHintIsNotShown: a name and a hint are redacted as the stored request redacts them.
func TestASecretInAToolNameAndAHintIsNotShown(t *testing.T) {
	r := newRig(t, allowApprovals)
	task := pausedOn(map[string]any{"type": "tool_approval_request", "hint": "token=" + plantedHint, "tools": []map[string]any{
		{"id": "t1", "name": "run password=" + plantedPassword, "args": map[string]any{}}}})
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	shown, _ := json.Marshal(r.asked)
	if found := contains(shown, plantedSecrets); len(found) != 0 || len(r.asked) != 1 {
		t.Errorf("the prompt holds %v (%d prompts): %s", found, len(r.asked), shown)
	}
}

// TestANestedApprovalCarriesItsSubagent: the tools are the sub-agent's own, and the prompt says whose.
func TestANestedApprovalCarriesItsSubagent(t *testing.T) {
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
	if len(r.asked) != 1 || r.asked[0].Subagent != "k8s-agent" || len(r.asked[0].Tools) != 1 || r.asked[0].Tools[0].Name != "delete_file" || r.asked[0].Tools[0].Args != "{\n  \"path\": \"/x\"\n}" {
		t.Errorf("prompt %+v; want the sub-agent's name and its one inner tool", r.asked)
	}
	if row := r.row(t, task); row.State != store.InteractionPending || len(r.sess.answers) != 0 {
		t.Errorf("state %s, %d answers; want it pending", row.State, len(r.sess.answers))
	}
}

// TestASecretInASubagentNameIsNotShown: the sub-agent's name is redacted as the stored request redacts it.
func TestASecretInASubagentNameIsNotShown(t *testing.T) {
	r := newRig(t, allowApprovals)
	task := pausedOn(map[string]any{
		"type":  "tool_approval_request",
		"tools": []any{map[string]any{"id": "parent", "name": "k8s-agent"}},
		"nested": map[string]any{"subagent_name": "planner token=" + plantedHint, "task_id": "sub-task", "context_id": "sub-ctx",
			"tools": []any{map[string]any{"id": "inner-1", "name": "delete_file"}}},
	})
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	shown, _ := json.Marshal(r.asked)
	if found := contains(shown, plantedSecrets); len(found) != 0 || len(r.asked) != 1 || !strings.HasPrefix(r.asked[0].Subagent, "planner") {
		t.Errorf("the prompt holds %v (%d prompts): %s", found, len(r.asked), shown)
	}
}

// TestAToolWithNoNameIsStillShown: the dialect refuses a tool with no ID and not one with no name. A decision
// names the ID, and the person sees the arguments, so the tool is shown; the adapter says it has no name.
func TestAToolWithNoNameIsStillShown(t *testing.T) {
	r := newRig(t, allowApprovals)
	task := pausedOn(map[string]any{"type": "tool_approval_request", "tools": []map[string]any{{"id": "t1", "args": map[string]any{"path": "/x"}}}})
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	if len(r.asked) != 1 || r.asked[0].Tools[0].Name != "" || r.asked[0].Tools[0].Args == "" || len(r.sess.cancels) != 0 {
		t.Errorf("prompts %+v, %d cancels; want the nameless tool shown with its arguments", r.asked, len(r.sess.cancels))
	}
}

// fakeFits makes the rig ask the chattest fake whether a prompt fits, which refuses a tool whose name or
// arguments are longer than its place allows, and more tools than it shows.
func fakeFits(r *rig) { r.fits = chattest.NewFake().Fits }

// The reasons the audit row of a refused approval gives, each the start of one fixed text. The row keeps the first 120
// bytes of the cause, so the reason comes first in it and holds nothing of the pause.
const (
	reasonCut     = "a tool's name or arguments are longer than can be shown whole"
	reasonControl = "a tool's name or arguments hold a control character or invalid UTF-8"
	reasonKey     = "a tool's argument key would be changed by cleaning"
	reasonNumber  = "a tool's arguments hold a number that cannot be shown exactly"
	reasonTooBig  = "the prompt does not fit in one message"
)

// auditedRefusal asserts that the audit holds the unusable pause once, saying reason in the row itself, and its cancel once.
func auditedRefusal(t *testing.T, r *rig, reason string) {
	t.Helper()
	rows := r.auditRows(t)
	if len(rows) != 2 || !strings.HasPrefix(rows[0], "unusable_pause: ") || !strings.Contains(rows[0], reason) || rows[1] != "unusable_pause_cancel: ok" {
		t.Errorf("audit %q; want the unusable pause, saying %q, and its cancel, once", rows, reason)
	}
}

// refusedBeforeRecording asserts that an approval the prompt could not show exactly was refused before anything was
// recorded: no interaction, nothing posted, the task cancelled, the thread told, the pause audited with reason, and no
// denial sent or counted; and that the pause coming again does none of that twice (its task is no longer paused).
func refusedBeforeRecording(t *testing.T, r *rig, task a2a.Task, reason string) {
	t.Helper()
	if err := r.paused(t, task); err != nil {
		t.Fatalf("Paused = %v; want the pause refused, which is not an error", err)
	}
	in, err := (&kagentv1.Dialect{}).ParseInteraction(task)
	if err != nil || in == nil {
		t.Fatal(in, err)
	}
	if _, err := r.st.GetInteraction(t.Context(), interact.InteractionID(*in)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetInteraction = %v; want no interaction made for a pause that is refused", err)
	}
	if len(r.slack.posted) != 0 || len(r.asked) != 0 || len(r.sess.cancels) != 1 || len(r.sess.answers) != 0 || len(r.hook) != 0 || len(r.notices(t)) != 1 {
		t.Errorf("%d posts, %d prompts asked, %d cancels, %d answers, hook %v, %d notices; want the task cancelled once, the thread told once, and nothing shown or denied",
			len(r.slack.posted), len(r.asked), len(r.sess.cancels), len(r.sess.answers), r.hook, len(r.notices(t)))
	}
	auditedRefusal(t, r, reason)
	r.sess.cancelErr = session.ErrNotPaused
	if err := r.paused(t, task); err != nil || len(r.slack.posted) != 0 || len(r.auditRows(t)) != 2 || len(r.notices(t)) != 1 {
		t.Errorf("Paused again = %v with %d posts, %d audit rows, %d notices; want no new refusal", err, len(r.slack.posted), len(r.auditRows(t)), len(r.notices(t)))
	}
}

// refusedAndClosed asserts that an approval the platform refused as too large was refused as unusable, with its
// interaction closed and no prompt posted, and that the pause coming again does none of that twice.
func refusedAndClosed(t *testing.T, r *rig, task a2a.Task) {
	t.Helper()
	if err := r.paused(t, task); err != nil {
		t.Fatalf("Paused = %v; want the pause refused, which is not an error", err)
	}
	row := r.row(t, task)
	if row.State != store.InteractionCancelled || row.MessageRef != "" || len(r.slack.posted) != 0 {
		t.Errorf("interaction %+v, %d posts; want it closed with no prompt posted", row, len(r.slack.posted))
	}
	if len(r.sess.cancels) != 1 || len(r.sess.answers) != 0 || len(r.hook) != 0 || len(r.notices(t)) != 1 {
		t.Errorf("%d cancels, %d answers, hook %v, %d notices; want the task cancelled and told once, and no denial", len(r.sess.cancels), len(r.sess.answers), r.hook, len(r.notices(t)))
	}
	auditedRefusal(t, r, reasonTooBig)
	shown := len(r.asked)
	// The pause comes again: the closed interaction has its cancel made once more, and nothing else is done twice.
	if err := r.paused(t, task); err != nil || len(r.asked) != shown || len(r.auditRows(t)) != 2 || len(r.notices(t)) != 1 || len(r.slack.posted) != 0 {
		t.Errorf("Paused again = %v with %d prompts asked (was %d), %d audit rows, %d notices; want no new refusal", err, len(r.asked), shown, len(r.auditRows(t)), len(r.notices(t)))
	}
}

// approvalOf is a pause for one tool with the given name and arguments.
func approvalOf(name string, args map[string]any) a2a.Task {
	return pausedOn(map[string]any{"type": "tool_approval_request", "tools": []map[string]any{{"id": "t1", "name": name, "args": args}}})
}

// TestAnApprovalThatDoesNotFitThePlatformIsRefusedAsUnusable: arguments that cannot be shown whole are never shown cut (a
// person cannot approve what they cannot read). This one is under the redaction's bound (4 KiB) and over the
// platform's limit, so it is the platform that refuses it: the interaction was made, and is closed.
func TestAnApprovalThatDoesNotFitThePlatformIsRefusedAsUnusable(t *testing.T) {
	r := newRig(t, allowApprovals)
	fakeFits(r)
	task := approvalOf("run_command", map[string]any{"script": strings.Repeat("x", 4050)})
	refusedAndClosed(t, r, task)
	if len(r.asked) != 1 {
		t.Errorf("the platform was asked %d times; want it asked, and the prompt refused", len(r.asked))
	}
}

// TestAToolTheRedactionWouldCutIsRefusedAsUnusable: the prompt is made from the redacted pause, and the redaction
// keeps at most 4 KiB of each string (it cuts at white space and marks the cut, if a rule does not take the mark). What is
// cut is not shown, and might be what the tool does. So the pause is refused before it is recorded, whatever the
// platform would take, and the platform is never asked.
func TestAToolTheRedactionWouldCutIsRefusedAsUnusable(t *testing.T) {
	cjk := strings.Repeat("字 ", 1157) + "rm -rf /important" // 4627 bytes, 2300 UTF-16 units: the platform takes it; the redaction does not
	// 4061 bytes as sent, and over the bound once the token layer has written "[redacted]" for each value: the cut is made
	// after the text grows, and the mark is taken by the rule of the last value.
	grows := strings.Repeat("字 ", 900) + strings.Repeat("access_token=x ", 30) + "; rm -rf /x"
	for name, task := range map[string]a2a.Task{
		"an unbroken string":          approvalOf("run", map[string]any{"script": strings.Repeat("A", 4990) + ";rm -rf /x"}),
		"a string of words":           approvalOf("run", map[string]any{"script": strings.Repeat("word ", 1000) + "rm -rf /x"}),
		"multi-byte, under the limit": approvalOf("run", map[string]any{"script": cjk}),
		"a string just over":          approvalOf("run", map[string]any{"script": strings.Repeat("x", 4097)}),
		"a string that grows past":    approvalOf("run", map[string]any{"script": grows}),
		"many secrets that grow past": approvalOf("run", map[string]any{"script": strings.Repeat("access_token=x ", 250)}),
		"secret names that grow past": approvalOf("run", map[string]any{"script": strings.Repeat("client_secret=a ", 250)}),
		"a name":                      approvalOf(strings.Repeat("字 ", 1157)+"rm", nil),
		"a name that grows past":      approvalOf(grows, nil),
		"a key":                       approvalOf("run", map[string]any{strings.Repeat("k ", 2100) + "tail": "v"}),
		"a string in a map in a list": approvalOf("run", map[string]any{"a": []any{map[string]any{"b": cjk}}}),
		"a key in a map in a map":     approvalOf("run", map[string]any{"a": map[string]any{"b": map[string]any{strings.Repeat("k ", 2100) + "tail": 1}}}),
		"a string in the second tool": pausedOn(map[string]any{"type": "tool_approval_request", "tools": []map[string]any{{"id": "t1", "name": "ok"}, {"id": "t2", "name": "run", "args": map[string]any{"s": cjk}}}}),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, allowApprovals)
			fakeFits(r)
			refusedBeforeRecording(t, r, task, reasonCut)
		})
	}
}

// TestATextThatIsNotWholeOnceCleanedIsRefusedAsUnusable: the prompt shows a text without its invisible characters, and
// that is all it may leave out. A control character other than tab and the line breaks has no honest use, and the
// cleaning deletes it silently ("rm -rf /\x00tmp/build" would read "rm -rf /tmp/build"), so it is refused. So is an argument
// key that cleaning changes in any way, for a key is an identifier and not prose.
func TestATextThatIsNotWholeOnceCleanedIsRefusedAsUnusable(t *testing.T) {
	for name, tc := range map[string]struct {
		task   a2a.Task
		reason string
	}{
		"NUL in a string":                   {approvalOf("run", map[string]any{"cmd": "rm -rf /\x00tmp/build"}), reasonControl},
		"ESC in a string":                   {approvalOf("run", map[string]any{"cmd": "echo \x1b[2Jhello"}), reasonControl},
		"DEL in a string":                   {approvalOf("run", map[string]any{"cmd": "a\x7fb"}), reasonControl},
		"a C1 control":                      {approvalOf("run", map[string]any{"cmd": "a\u009bb"}), reasonControl},
		"in a list in a map":                {approvalOf("run", map[string]any{"a": []any{map[string]any{"b": "x\x00y"}}}), reasonControl},
		"NUL in a name":                     {approvalOf("ru\x00n", nil), reasonControl},
		"a control in a key":                {approvalOf("run", map[string]any{"c\x00md": 1}), reasonControl},
		"a zero-width in a key":             {approvalOf("run", map[string]any{"cmd\u200b": "ls"}), reasonKey},
		"a key with margins":                {approvalOf("run", map[string]any{" force ": true}), reasonKey},
		"a key with a tab and a line break": {approvalOf("run", map[string]any{"a\tb\nc": 1}), reasonKey},
		"a key with two spaces":             {approvalOf("run", map[string]any{"a  b": 1}), reasonKey},
		"a key that is a secret":            {approvalOf("run", map[string]any{"ghp_" + strings.Repeat("a1B2", 9): 1}), reasonKey},
		"keys that clean to one":            {approvalOf("run", map[string]any{"cmd": "ls", "cmd\u200b": "rm -rf /"}), reasonKey},
		"in a map":                          {approvalOf("run", map[string]any{"outer": map[string]any{"cmd": "ls", "c\u200bmd": "rm -rf /"}}), reasonKey},
		"in a list":                         {approvalOf("run", map[string]any{"list": []any{map[string]any{"cmd": "ls", "cmd\u200b": "rm -rf /"}}}), reasonKey},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, allowApprovals)
			refusedBeforeRecording(t, r, tc.task, tc.reason)
		})
	}
}

// TestInvisibleCharactersAndLineEndsInAValueAreShownWithoutThem: what the cleaning leaves out of a value is the
// invisible format characters (zero-width, variation selectors), and its line ends are LF; tab, LF and CR are no
// control characters to refuse, and text in other scripts and an emoji are shown whole.
func TestInvisibleCharactersAndLineEndsInAValueAreShownWithoutThem(t *testing.T) {
	r := newRig(t, allowApprovals)
	task := approvalOf("run", map[string]any{"s": "a\u200bb\r\nc\td \U0001F44D\ufe0f \u0645\u0631\u062d\u0628\u0627 \u0928\u092e\u0938\u094d\u0924\u0947\n"})
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"s\": \"ab\\nc\\td \U0001F44D \u0645\u0631\u062d\u0628\u0627 \u0928\u092e\u0938\u094d\u0924\u0947\\n\"\n}"
	if len(r.asked) != 1 || r.asked[0].Tools[0].Args != want {
		t.Errorf("prompts %+v; want the value without its invisible characters and with LF line ends:\n%s", r.asked, want)
	}
}

// TestAToolWithANumberThatIsNotExactIsRefusedAsUnusable: arguments arrive as float64, so 2^53 + 1 is shown as 2^53 and
// the agent's tool acts on another number than the one approved.
func TestAToolWithANumberThatIsNotExactIsRefusedAsUnusable(t *testing.T) {
	for name, n := range map[string]float64{"2^53": 1 << 53, "ten to the 18": 1e18, "negative": -(1 << 60), "huge": 1e300} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, allowApprovals)
			refusedBeforeRecording(t, r, approvalOf("run", map[string]any{"a": []any{map[string]any{"id": n}}}), reasonNumber)
		})
	}
}

// TestNumbersThatAreExactAreShownAsTheyAre: 2^53 - 1 is the last integer a float64 holds exactly, and ordinary numbers,
// fractions among them, are shown, as the nearest float64 where they are decimals.
func TestNumbersThatAreExactAreShownAsTheyAre(t *testing.T) {
	r := newRig(t, allowApprovals)
	task := approvalOf("run", map[string]any{"a": 3, "b": float64(1<<53 - 1), "c": 1.5, "d": -7, "e": 1234567890123, "f": 0.1, "g": true, "h": nil})
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"a\": 3,\n  \"b\": 9007199254740991,\n  \"c\": 1.5,\n  \"d\": -7,\n  \"e\": 1234567890123,\n  \"f\": 0.1,\n  \"g\": true,\n  \"h\": null\n}"
	if len(r.asked) != 1 || r.asked[0].Tools[0].Args != want {
		t.Errorf("prompts %+v; want the numbers exactly as sent:\n%s", r.asked, want)
	}
}

// TestAnApprovalOfTooManyToolsForThePlatformIsRefusedAsUnusable: the platform's own cap (a smaller one than the
// dialect's) is the same refusal.
func TestAnApprovalOfTooManyToolsForThePlatformIsRefusedAsUnusable(t *testing.T) {
	r := newRig(t, allowApprovals)
	fake := chattest.NewFake()
	fake.MaxTools = 1
	r.fits = fake.Fits
	if err := r.paused(t, approval()); err != nil {
		t.Fatal(err)
	}
	if row := r.row(t, approval()); row.State != store.InteractionCancelled || len(r.sess.cancels) != 1 || len(r.slack.posted) != 0 {
		t.Errorf("state %s, %d cancels, %d posts; want it refused and closed", row.State, len(r.sess.cancels), len(r.slack.posted))
	}
}

// TestAPlatformThatCannotFitForAnotherReasonIsNotARefusal: only a prompt that is too large is refused as unusable. Any
// other error is returned, for the pause to be tried again.
func TestAPlatformThatCannotFitForAnotherReasonIsNotARefusal(t *testing.T) {
	r := newRig(t, allowApprovals)
	r.promptErr = errors.New("the platform does not show tool approvals")
	if err := r.paused(t, approval()); err == nil || len(r.sess.cancels) != 0 {
		t.Errorf("Paused = %v with %d cancels; want the error returned and nothing cancelled", err, len(r.sess.cancels))
	}
}

// TestASecondDifferentApprovalUnderASeenIDIsRefusedAsUnusable: the ID names the tools, and a model may reuse an ID
// (UNCONFIRMED(WP-02)), so another call under it is not the pending one again: refused as unusable, with the first
// prompt standing.
func TestASecondDifferentApprovalUnderASeenIDIsRefusedAsUnusable(t *testing.T) {
	r := newRig(t, allowApprovals)
	if err := r.paused(t, approval()); err != nil {
		t.Fatal(err)
	}
	other := pausedOn(map[string]any{"type": "tool_approval_request", "tools": []map[string]any{
		{"id": "t1", "call_id": "c1", "name": "something_else"}, {"id": "t2", "call_id": "c2", "name": "add_numbers"}}})
	if err := r.paused(t, other); err != nil {
		t.Fatal(err)
	}
	if len(r.slack.posted) != 1 || len(r.sess.cancels) != 1 || len(r.sess.answers) != 0 || r.row(t, approval()).State != store.InteractionPending {
		t.Errorf("%d posts, %d cancels, %d answers; want the first prompt alone and the second pause cancelled", len(r.slack.posted), len(r.sess.cancels), len(r.sess.answers))
	}
	if rows := strings.Join(r.auditRows(t), "|"); strings.Count(rows, "unusable_pause: ") != 1 || strings.Count(rows, "unusable_pause_cancel: ok") != 1 {
		t.Errorf("audit %q; want the unusable pause and its cancel audited once", rows)
	}
}

// TestACrashBetweenRecordingAndPostingAnApprovalLeavesOnePrompt: as for a question, the pause that comes again posts
// the prompt that the first did not.
func TestACrashBetweenRecordingAndPostingAnApprovalLeavesOnePrompt(t *testing.T) {
	r, task := newRig(t, allowApprovals), approval()
	r.slack.fault = "after"
	if err := r.paused(t, task); err == nil {
		t.Fatal("the first Paused succeeded through a crash")
	}
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	if row := r.row(t, task); len(r.slack.posted) != 1 || row.MessageRef == "" || row.State != store.InteractionPending {
		t.Errorf("posts %v, interaction %+v; want one prompt, recorded", r.slack.posted, row)
	}
}

// The tools a task is paused on, for the tests below that build the row by hand.
func approvalRequest(t *testing.T) []byte {
	t.Helper()
	in := dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "task-1", Hint: "delete?", Nested: &dialect.Nested{Subagent: "planner"}, Tools: []dialect.Tool{
		{ID: "t1", Name: "delete_everything", Args: map[string]any{"path": "/"}}, {ID: "t2", Name: "add_numbers"}}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAPressOnAPendingApprovalDecidesNothing: a press is not yet decidable (the controls of an approval are read
// elsewhere). It must fail safe: the person is told the question is not available, nothing is sent to the agent, and
// the interaction stays pending for the timeout.
func TestAPressOnAPendingApprovalDecidesNothing(t *testing.T) {
	for name, inputs := range map[string][]chat.Answer{
		"approve all":        nil,
		"a verdict per tool": {{Question: 0, Values: []string{chat.ToolApproveValue}}, {Question: 1, Values: []string{chat.ToolApproveValue}}},
		"a deny":             {{Question: 0, Values: []string{chat.ToolDenyValue}}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newPressRig(t)
			row := store.Interaction{ID: "ix-1", Key: key, TaskID: "task-1", Kind: store.InteractionToolApproval, Request: approvalRequest(t), CreatedAt: d0, Deadline: deadline}
			if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
				t.Fatal(err)
			}
			if err := r.st.SetInteractionMessageRef(t.Context(), "ix-1", "100.1"); err != nil {
				t.Fatal(err)
			}
			i := r.submit(t, "UALICE1", -1)
			i.Answers = inputs
			if res, err := r.process(t, i); err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v; want the press finished", res, err)
			}
			got := r.row(t)
			if got.State != store.InteractionPending || len(got.Answer) != 0 || len(r.sess.answers()) != 0 || len(r.aud.got) != 0 || len(r.hooked()) != 0 || r.waiting(t) != 0 {
				t.Errorf("state %s, answer %q, %d sends, %d audit rows, hooks %v, %d presses waiting; want it pending, nothing sent, audited or counted, and the press gone", got.State, got.Answer, len(r.sess.answers()), len(r.aud.got), r.hooked(), r.waiting(t))
			}
			if told := r.eph.list(); len(told) != 1 || !strings.Contains(told[0], "no longer available") {
				t.Errorf("told %q; want the person told the question is not available", told)
			}
		})
	}
}

// TestAPendingApprovalThatTimesOutIsClosedWithoutAnError: the sweep builds the ending of any pending interaction
// from its stored request, and an approval's is its tools, with no verdicts.
func TestAPendingApprovalThatTimesOutIsClosedWithoutAnError(t *testing.T) {
	r := newSweepRig(t)
	row := store.Interaction{ID: "ix-1", Key: key, TaskID: "task-1", Kind: store.InteractionToolApproval, Request: approvalRequest(t), CreatedAt: t0, Deadline: t0.Add(timeout)}
	if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetInteractionMessageRef(t.Context(), "ix-1", "100.001"); err != nil {
		t.Fatal(err)
	}
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.state(t, "ix-1") != store.InteractionTimedOut || len(r.upd.ends) != 1 {
		t.Fatalf("state %s, %d edits; want it timed out and its prompt replaced", r.state(t, "ix-1"), len(r.upd.ends))
	}
	e := r.upd.ends[0]
	if e.Validate() != nil || e.Outcome != chat.TimedOut || e.Verdicts != nil || len(e.Prompt.Tools) != 2 || e.Prompt.Tools[0].Name != "delete_everything" || e.Prompt.Subagent != "planner" || e.Prompt.Hint != "delete?" {
		t.Errorf("ending %+v, %v; want a valid timed-out ending of the two tools, with no verdicts", e, e.Validate())
	}
	if strings.Contains(r.logs.String(), "level=ERROR") || strings.Contains(r.logs.String(), "level=WARN") {
		t.Errorf("the sweep logged a problem: %s", r.logs.String())
	}
}

// TestTheReplacementOfAnApprovalThatEndedWithoutAnAnswer: cancelled or timed out, from the row alone. An answered one
// has no ending yet, and says so rather than show a wrong one.
func TestTheReplacementOfAnApprovalThatEndedWithoutAnAnswer(t *testing.T) {
	row := store.Interaction{ID: "ix-1", Key: key, TaskID: "task-1", Kind: store.InteractionToolApproval, Request: approvalRequest(t), MessageRef: "100.1", DecidedAt: deadline}
	for _, state := range []store.InteractionState{store.InteractionTimedOut, store.InteractionCancelled} {
		row.State = state
		out, err := interact.ReplacementOutgoing(row)
		if err != nil || out.Ending == nil || out.Ending.Validate() != nil || len(out.Ending.Prompt.Tools) != 2 {
			t.Errorf("%s: %+v, %v; want a valid ending of the two tools", state, out, err)
		}
	}
	row.State, row.Decider, row.Answer = store.InteractionAnswered, "UALICE1", []byte(`{"decisions":[{"tool_id":"t1","approved":true},{"tool_id":"t2"}]}`)
	if _, err := interact.ReplacementOutgoing(row); err == nil {
		t.Error("an answered approval was given an ending")
	}
}

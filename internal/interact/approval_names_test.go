package interact_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
)

// A tool name is shown, stored and audited as it is, or the pause is refused: the redaction rewrites a credential-shaped
// word whole, so a tool named BearerTokenValidator would read [redacted] in the prompt, the stored request and the audit row,
// and two such tools would read alike; and the audit row writes a name whole only up to a limit, and as it is only when its own
// redaction leaves it alone.

// TestAToolNameThatCleaningChangesIsRefusedAsUnusable: a name that redact.FreeText changes at all, as storable would store
// it, is refused before anything is recorded, whatever the platform would take: a word that reads as a credential, a secret
// in the name, invisible characters, and line ends.
func TestAToolNameThatCleaningChangesIsRefusedAsUnusable(t *testing.T) {
	for name, tool := range map[string]string{
		"a word that holds a credential word": "BearerTokenValidator",
		"a secret in the name":                "run password=" + plantedPassword,
		"a zero-width character":              "del\u200bete",
		"a variation selector":                "ok\ufe0f",
		"a bidirectional control":             "ab\u202ecd",
		"a CR LF":                             "a\r\nb",
		"a CR":                                "a\rb",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, allowApprovals)
			fakeFits(r)
			refusedBeforeRecording(t, r, approvalOf(tool, nil), reasonName)
			if rows := strings.Join(r.auditRows(t), "|") + strings.Join(r.notices(t), "|"); strings.Contains(rows, plantedPassword) {
				t.Errorf("the audit or the notice holds the planted secret: %s", rows)
			}
		})
	}
}

// TestAToolNameTheAuditRowWouldNotWriteWholeIsRefusedAsUnusable: a name that cleaning leaves alone but the audit row of its
// decision would rewrite or cut is refused too, so that every tool an approver can decide is named whole in the record.
func TestAToolNameTheAuditRowWouldNotWriteWholeIsRefusedAsUnusable(t *testing.T) {
	for name, tc := range map[string]struct {
		tool   string
		reason string
	}{
		"ending as a credential scheme":  {"check_Bearer:", reasonNameLog},
		"ending as an assignment":        {"check_id_token=", reasonNameLog},
		"a tab":                          {"a\tb", reasonNameLog},
		"a line break":                   {"a\nb", reasonNameLog},
		"two spaces":                     {"a  b", reasonNameLog},
		"white space at the end":         {"delete ", reasonNameLog},
		"301 ASCII bytes":                {strings.Repeat("n", 301), reasonNameLen},
		"101 CJK characters":             {strings.Repeat("あ", 101), reasonNameLen},
		"151 two-byte characters":        {strings.Repeat("é", 151), reasonNameLen},
		"76 emoji":                       {strings.Repeat("\U0001F600", 76), reasonNameLen},
		"over the length and a gap":      {strings.Repeat("n", 301) + "Bearer:", reasonNameLen},
		"3000 characters, under the cut": {strings.Repeat("n", 3000), reasonNameLen},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, allowApprovals)
			fakeFits(r)
			refusedBeforeRecording(t, r, approvalOf(tc.tool, nil), tc.reason)
		})
	}
}

// TestTheReasonsOfAToolNameFitTheAuditCause: the audit keeps the first 120 bytes of "unusable_pause: " and the cause, so each
// reason must be whole in it.
func TestTheReasonsOfAToolNameFitTheAuditCause(t *testing.T) {
	for _, reason := range []string{reasonName, reasonNameLog, reasonNameLen} {
		if n := len("unusable_pause: " + "unusable pause: " + reason); n > 120 {
			t.Errorf("%q: %d bytes in the row; want at most 120", reason, n)
		}
	}
}

// TestToolNamesOfOrdinaryShapesAreShownAndStoredUnchanged: identifiers in several scripts, at the limit of their kind, are
// shown as the agent sent them, and stored so, and the audit row of their decision holds them whole.
func TestToolNamesOfOrdinaryShapesAreShownAndStoredUnchanged(t *testing.T) {
	for name, tool := range map[string]string{
		"ascii":                 "delete_pod",
		"with dots and dashes":  "k8s.io/get-pods_v2",
		"with single spaces":    "list pods in namespace",
		"latin accents":         "créer_dépôt",
		"cyrillic":              "удалить_под",
		"cjk":                   "删除容器",
		"arabic":                "مرحبا",
		"devanagari":            "नमस्ते",
		"an emoji":              "deploy_\U0001F680",
		"300 ASCII bytes":       strings.Repeat("n", 300),
		"150 two-byte":          strings.Repeat("é", 150),
		"100 CJK":               strings.Repeat("あ", 100),
		"75 emoji":              strings.Repeat("\U0001F600", 75),
		"a word near a keyword": "TokenCounter",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, allowApprovals)
			task := approvalOf(tool, map[string]any{"x": 1})
			if err := r.paused(t, task); err != nil {
				t.Fatal(err)
			}
			if len(r.asked) != 1 || r.asked[0].Tools[0].Name != tool || len(r.sess.cancels) != 0 {
				t.Fatalf("prompts %+v, %d cancels; want one prompt naming %.40q as it was sent", r.asked, len(r.sess.cancels), tool)
			}
			var stored dialect.Interaction
			if err := json.Unmarshal(r.row(t, task).Request, &stored); err != nil || stored.Tools[0].Name != tool {
				t.Errorf("stored name %.40q (%v); want %.40q", stored.Tools[0].Name, err, tool)
			}
			if got := audit.ToolNameFit(tool); got != audit.NameWhole {
				t.Errorf("ToolNameFit = %d; want the audit row to write it whole", got)
			}
		})
	}
}

// TestTwoToolsWithDistinctOrdinaryNamesStayDistinctInThePromptTheRequestAndTheAuditRow: the intake shows and stores both
// names, and the real audit writer's row of the decision names both, each with its verdict.
func TestTwoToolsWithDistinctOrdinaryNamesStayDistinctInThePromptTheRequestAndTheAuditRow(t *testing.T) {
	ir := newRig(t, allowApprovals)
	task := pausedOn(map[string]any{"type": "tool_approval_request", "tools": []map[string]any{
		{"id": "t1", "name": "get_secret", "args": map[string]any{}}, {"id": "t2", "name": "get_secrets", "args": map[string]any{}}}})
	if err := ir.paused(t, task); err != nil {
		t.Fatal(err)
	}
	if len(ir.asked) != 1 || len(ir.asked[0].Tools) != 2 || ir.asked[0].Tools[0].Name != "get_secret" || ir.asked[0].Tools[1].Name != "get_secrets" {
		t.Fatalf("prompts %+v; want the two names as sent", ir.asked)
	}
	request := ir.row(t, task).Request
	var stored dialect.Interaction
	if err := json.Unmarshal(request, &stored); err != nil || stored.Tools[0].Name != "get_secret" || stored.Tools[1].Name != "get_secrets" {
		t.Fatalf("stored %+v (%v); want the two names as sent", stored.Tools, err)
	}

	r := newApprovalRigOf(t, request)
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
	if err := pl.Press(t.Context(), tap("UALICE1", chat.ActionSubmit, verdict(0, yes), verdict(1, no))); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if _, err := pl.Process(t.Context(), presses[0]); err != nil {
		t.Fatal(err)
	}
	rows, err := r.st.ListAudit(t.Context(), time.Time{}, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows %v (%v); want one", rows, err)
	}
	if want := `answered approved=1 denied=1 approved_tools=["get_secret"] denied_tools=["get_secrets"]`; rows[0].Outcome != want {
		t.Errorf("audit row %q; want %q", rows[0].Outcome, want)
	}
}

// TestTheNameCheckRunsOnlyWhereAnApprovalIsAllowed: on a Binding that does not allow approvals the pause is denied as it
// was, whatever the tool is called; the name check is part of the allowed path alone.
func TestTheNameCheckRunsOnlyWhereAnApprovalIsAllowed(t *testing.T) {
	r := newRig(t, func(b *interact.Binding) { b.ToolApprovals, b.AskUser = false, true })
	if err := r.paused(t, approvalOf("BearerTokenValidator", nil)); err != nil {
		t.Fatal(err)
	}
	if len(r.hook) != 1 || len(r.sess.answers) != 1 || len(r.sess.cancels) != 0 || len(r.asked) != 0 {
		t.Errorf("hook %v, %d answers, %d cancels, %d prompts; want the approval denied by policy, as it is for any name", r.hook, len(r.sess.answers), len(r.sess.cancels), len(r.asked))
	}
}

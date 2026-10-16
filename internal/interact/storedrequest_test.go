package interact_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// The planted secrets: every one of them must be missing from what the store keeps. They are obviously fake.
const (
	plantedPassword = "hunter2-planted-password"
	plantedEnv      = "pw-planted-in-an-env-var"
	plantedBearer   = "planted.bearer.credential"
	plantedHint     = "planted-token-in-the-hint"
	plantedQuestion = "planted-secret-in-the-question"
)

var plantedSecrets = []string{plantedPassword, plantedEnv, plantedBearer, plantedHint, plantedQuestion}

// secretApproval is a tool approval whose arguments carry secrets in the ways render.Tool takes out for display:
// under a secret name, in an env var as name and value, and in a header of a command line.
func secretApproval() a2a.Task {
	return pausedOn(map[string]any{"type": "tool_approval_request", "hint": "run it, token=" + plantedHint, "tools": []map[string]any{
		{"id": "t1", "call_id": "c1", "name": "run_command", "args": map[string]any{
			"password": plantedPassword,
			"env":      []any{map[string]any{"name": "DB_PASSWORD", "value": plantedEnv}},
			"command":  "curl -H 'Authorization: Bearer " + plantedBearer + "' https://example.test/api",
			"count":    3,
		}},
	}})
}

func contains(b []byte, secrets []string) []string {
	var found []string
	for _, s := range secrets {
		if strings.Contains(string(b), s) {
			found = append(found, s)
		}
	}
	return found
}

// TestNoSecretOfAToolCallIsStored: the arguments of a tool call, and a secret anywhere in the hint, are redacted
// before the request is stored (what is stored is never more than render.Tool shows), while the answer the agent
// receives is the one the original pause makes: nothing Agent Kourier sends changes.
func TestNoSecretOfAToolCallIsStored(t *testing.T) {
	r, task := newRig(t, nil), secretApproval()
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, task)
	if found := contains(row.Request, plantedSecrets); len(found) != 0 {
		t.Fatalf("the stored request holds %v: %s", found, row.Request)
	}
	var stored dialect.Interaction
	if err := json.Unmarshal(row.Request, &stored); err != nil {
		t.Fatal(err)
	}
	args := stored.Tools[0].Args
	if stored.Tools[0].ID != "t1" || stored.Tools[0].Name != "run_command" || args["count"] != float64(3) || args["password"] != "[redacted]" {
		t.Errorf("stored tool %+v; want the ID, name and plain arguments as they were, and the secret one redacted", stored.Tools[0])
	}
	// Nothing sent to the agent changed: the denial is built from the original pause, and from what is stored.
	original, err := (&kagentv1.Dialect{}).ParseInteraction(task)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.sess.answers) != 1 {
		t.Fatalf("%d answers sent; want the denial", len(r.sess.answers))
	}
	got := r.sess.answers[0]
	payload, _ := got.Metadata[kagentv1.HITLExtension].(map[string]any)
	approvals, _ := payload["approvals"].([]any)
	if len(approvals) != 1 {
		t.Fatalf("the answer sent = %v; want one decision", payload)
	}
	reason, _ := approvals[0].(map[string]any)["rejection_reason"].(string) // Agent Kourier's own words, whatever they are
	answer := dialect.Answer{RequestID: original.RequestID, Decisions: []dialect.Decision{{ToolID: "t1", Reason: reason}}}
	want, err := interact.AnswerMessage(kagentv1.New(0), row.ID, *original, answer, "", dialect.Platform{})
	if err != nil {
		t.Fatal(err)
	}
	wantPayload, _ := want.Metadata[kagentv1.HITLExtension].(map[string]any)
	if got.TaskID != want.TaskID || !reflect.DeepEqual(payload["approvals"], wantPayload["approvals"]) {
		t.Errorf("the answer sent = %v; want the one the original pause makes, %v", payload, wantPayload)
	}
}

// TestASecretInAQuestionIsNotStoredButItsChoicesAre: a question's text is redacted like a hint, but a choice is kept
// exactly: the answer a person picks it by is sent to the agent as it is.
func TestASecretInAQuestionIsNotStoredButItsChoicesAre(t *testing.T) {
	r := newRig(t, nil)
	task := pausedOn(map[string]any{"type": "ask_user_request", "id": "req-s", "questions": []map[string]any{
		{"question": "Is password=" + plantedQuestion + " right?", "choices": []string{"yes", "no, use Bearer sk.choice.kept"}},
	}})
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, task)
	if found := contains(row.Request, []string{plantedQuestion}); len(found) != 0 {
		t.Fatalf("the stored request holds %v: %s", found, row.Request)
	}
	var stored dialect.Interaction
	if err := json.Unmarshal(row.Request, &stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Questions[0].Choices; !reflect.DeepEqual(got, []string{"yes", "no, use Bearer sk.choice.kept"}) {
		t.Errorf("stored choices %q; want them exactly as the agent sent them", got)
	}
	// The prompt is shown as it always was, and what was stored is no more than that.
	if len(r.asked) != 1 || !strings.Contains(r.asked[0].Questions[0].Text, plantedQuestion) {
		t.Errorf("prompt %+v; want the question as the agent wrote it", r.asked)
	}
}

// TestAQuestionStoredBeforeRedactionIsStillThePauseWhenItComesAgain: a row an older version wrote holds the question
// as it was; the same pause arriving after an upgrade is recognised, not refused as a second pause under the ID.
func TestAQuestionStoredBeforeRedactionIsStillThePauseWhenItComesAgain(t *testing.T) {
	r := newRig(t, nil)
	task := pausedOn(map[string]any{"type": "ask_user_request", "id": "req-s", "questions": []map[string]any{
		{"question": "Is password=" + plantedQuestion + " right?", "choices": []string{"yes", "no"}},
	}})
	in, err := (&kagentv1.Dialect{}).ParseInteraction(task)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	legacy := store.Interaction{ID: interact.InteractionID(*in), Key: key, TaskID: "task-1", Kind: store.InteractionAskUser, Request: raw, CreatedAt: t0, Deadline: t0.Add(timeout)}
	if _, err := r.st.CreateInteraction(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	if err := r.paused(t, task); err != nil {
		t.Fatalf("Paused = %v; want the old row taken for this pause", err)
	}
	if len(r.sess.cancels) != 0 || len(r.notices(t)) != 0 || len(r.asked) != 1 {
		t.Errorf("%d cancels, %d notices, %d prompts; want the old row's pause posted as any other, and nothing refused", len(r.sess.cancels), len(r.notices(t)), len(r.asked))
	}
}

// TestAnApprovalStoredBeforeRedactionIsRefusedAsUnusable: no approval row was written before the arguments were redacted,
// so an approval row that holds them as they were is not one this version wrote, and a pause that equals it only once
// redacted is not taken for it: the second reading of samePause is for a question.
func TestAnApprovalStoredBeforeRedactionIsRefusedAsUnusable(t *testing.T) {
	r, task := newRig(t, nil), secretApproval()
	in, err := (&kagentv1.Dialect{}).ParseInteraction(task)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	legacy := store.Interaction{ID: interact.InteractionID(*in), Key: key, TaskID: "task-1", Kind: store.InteractionToolApproval, Request: raw, CreatedAt: t0, Deadline: t0.Add(timeout)}
	if _, err := r.st.CreateInteraction(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	if err := r.paused(t, task); err != nil {
		t.Fatalf("Paused = %v; want the pause refused as unusable, which is not an error", err)
	}
	if got := r.row(t, task); got.State != store.InteractionPending || len(r.sess.answers) != 0 || len(r.sess.cancels) != 1 || len(r.notices(t)) != 1 {
		t.Errorf("state %s, %d answers, %d cancels, %d notices; want nothing denied, and the task cancelled with one notice", got.State, len(r.sess.answers), len(r.sess.cancels), len(r.notices(t)))
	}
}

// TestADifferentPauseUnderASeenIDIsStillRefused: redacting before the comparison must not let a pause with other
// plain arguments pass for the one that was stored.
func TestADifferentPauseUnderASeenIDIsStillRefused(t *testing.T) {
	r := newRig(t, nil)
	if err := r.paused(t, secretApproval()); err != nil {
		t.Fatal(err)
	}
	other := pausedOn(map[string]any{"type": "tool_approval_request", "tools": []map[string]any{
		{"id": "t1", "call_id": "c1", "name": "run_command", "args": map[string]any{"count": 4}},
	}})
	before := len(r.sess.answers)
	_ = r.paused(t, other)
	if len(r.notices(t)) != 2 { // the denial's notice, and the refusal's
		t.Errorf("%d notices, %d answers (was %d); want the second pause refused as unusable", len(r.notices(t)), len(r.sess.answers), before)
	}
}

// TestAPauseThatComesAgainForAnAnsweredClearedInteractionIsCancelledForItsOwnReason: once retention has cleared an
// answered interaction (after a day or more) its answer is gone, and the stored one cannot be sent again. The task is
// cancelled and the thread told, in words of their own, and the audit says the answer was cleared: not that the
// agent's pause was unusable, which it was not.
func TestAPauseThatComesAgainForAnAnsweredClearedInteractionIsCancelledForItsOwnReason(t *testing.T) {
	r, task := newRig(t, nil), approval()
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	sent, notices := len(r.sess.answers), len(r.notices(t))
	if n, err := r.st.ClearSettledInteractions(t.Context(), t0.Add(1000*time.Hour), time.Hour, time.Hour); err != nil || n != 1 {
		t.Fatalf("cleared %d, %v; want the denied interaction", n, err)
	}
	if err := r.paused(t, task); err != nil {
		t.Fatalf("Paused = %v; want the task cancelled, which is not an error", err)
	}
	if len(r.sess.answers) != sent || len(r.sess.cancels) != 1 {
		t.Errorf("%d sends (was %d), %d cancels; want no answer sent and the task cancelled once", len(r.sess.answers), sent, len(r.sess.cancels))
	}
	rows := strings.Join(r.auditRows(t), "|")
	if !strings.Contains(rows, "answer_cleared") || strings.Contains(rows, "unusable_pause") {
		t.Errorf("audit rows %q; want answer_cleared, and no unusable pause", rows)
	}
	got := strings.Join(r.notices(t), "|")
	if len(r.notices(t)) != notices+1 || strings.Count(got, "was not sent before it was cleared") != 1 || strings.Contains(got, "cannot use") {
		t.Errorf("notices %q; want one more, in its own words and not the unusable pause's", got)
	}
	if err := r.paused(t, task); err != nil || len(r.notices(t)) != notices+1 || len(r.sess.cancels) != 2 {
		t.Errorf("a third Paused = %v, %d notices, %d cancels; want one notice however often it comes, and the task cancelled again", err, len(r.notices(t)), len(r.sess.cancels))
	}
}

// TestAPauseThatComesAgainForATimedOutOrCancelledClearedInteractionIsCancelledAsBefore: a timeout whose cancel could not be
// made (the agent was down for a day) is finished when the pause comes again, from the row alone. Nothing is said of an
// unusable pause, and nothing is audited as one: the agent did nothing wrong.
func TestAPauseThatComesAgainForATimedOutOrCancelledClearedInteractionIsCancelledAsBefore(t *testing.T) {
	for _, state := range []store.InteractionState{store.InteractionTimedOut, store.InteractionCancelled} {
		t.Run(string(state), func(t *testing.T) {
			r, task := newRig(t, nil), askUser("req-1")
			if err := r.paused(t, task); err != nil {
				t.Fatal(err)
			}
			row := r.row(t, task)
			if err := r.st.ResolveInteraction(t.Context(), row.ID, state, t0.Add(timeout)); err != nil {
				t.Fatal(err)
			}
			if n, err := r.st.ClearSettledInteractions(t.Context(), t0.Add(1000*time.Hour), time.Hour, time.Hour); err != nil || n != 1 {
				t.Fatalf("cleared %d, %v; want the interaction", n, err)
			}
			r.sess.cancelErr = errBoom
			if err := r.paused(t, task); err == nil {
				t.Fatal("a failed cancel was not returned, for the pause to be tried again")
			}
			r.sess.cancelErr = nil
			if err := r.paused(t, task); err != nil {
				t.Fatal(err)
			}
			if len(r.sess.cancels) != 2 || len(r.sess.answers) != 0 {
				t.Errorf("%d cancels, %d answers; want the task cancelled again, and nothing sent", len(r.sess.cancels), len(r.sess.answers))
			}
			if rows, notices := r.auditRows(t), r.notices(t); len(rows) != 0 || len(notices) != 0 {
				t.Errorf("audit rows %q, notices %q; want none: no pause was unusable", rows, notices)
			}
		})
	}
}

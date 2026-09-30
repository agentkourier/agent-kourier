package kagentv1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// plain decodes v through JSON, so a typed payload and a recorded one compare equal.
func plain(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestBuildResponseMatchesAcceptedRequests parses each recorded pause, answers it, and
// compares the message with the request kagent accepted after that pause (WP-02). The
// text part is not compared: kagent ignores it when the payload is present.
func TestBuildResponseMatchesAcceptedRequests(t *testing.T) {
	for _, tc := range []struct {
		name, pause, accepted string
		answer                func(in *dialect.Interaction) dialect.Answer
	}{
		{"single-select", "askuser/03-pause-good-answer.response.sse", "askuser/04-good-answer.request.json",
			func(in *dialect.Interaction) dialect.Answer {
				return dialect.Answer{RequestID: in.RequestID, Answers: [][]string{{"PostgreSQL"}}}
			}},
		{"multi-select and free text", "askuser/21-pause-good-two-answers.response.sse", "askuser/22-good-two-answers.request.json",
			func(in *dialect.Interaction) dialect.Answer {
				return dialect.Answer{RequestID: in.RequestID, Answers: [][]string{{"PostgreSQL", "MySQL"}, {"blue"}}}
			}},
		{"sub-agent question (flat at alpha5)", "nested/01-pause.response.sse", "nested/02-answer.request.json",
			func(in *dialect.Interaction) dialect.Answer {
				return dialect.Answer{RequestID: in.RequestID, Answers: [][]string{{"east"}}}
			}},
		{"approve", "approval/01-pause-approve.response.sse", "approval/02-approve.request.json",
			func(in *dialect.Interaction) dialect.Answer {
				return dialect.Answer{Decisions: []dialect.Decision{{ToolID: in.Tools[0].ID, Approved: true, Reason: "ignored when approved"}}}
			}},
		{"reject with a reason", "approval/04-pause-reject-with-reason.response.sse", "approval/05-reject-with-reason.request.json",
			func(in *dialect.Interaction) dialect.Answer {
				return dialect.Answer{Decisions: []dialect.Decision{{ToolID: in.Tools[0].ID, Reason: "not today"}}}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Dialect{}
			in, ok := d.ParseInteraction(fixtureTask(t, tc.pause))
			if !ok {
				t.Fatal("the recorded pause did not parse")
			}
			got, err := d.BuildResponse(*in, tc.answer(in))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "kagent", tc.accepted))
			if err != nil {
				t.Fatal(err)
			}
			var req struct {
				Params struct {
					Message a2a.Message `json:"message"`
				} `json:"params"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Fatal(err)
			}
			want := req.Params.Message
			if got.TaskID != want.TaskID || got.ContextID != want.ContextID || got.Role != a2a.MessageRoleUser ||
				!reflect.DeepEqual(got.Extensions, want.Extensions) || len(got.Parts) != 1 || got.ID == "" {
				t.Errorf("message = %+v, want task %s, context %s, extensions %v, one part", got, want.TaskID, want.ContextID, want.Extensions)
			}
			if !reflect.DeepEqual(plain(t, got.Metadata), plain(t, want.Metadata)) {
				t.Errorf("metadata = %v\nwant       %v", plain(t, got.Metadata), plain(t, want.Metadata))
			}
		})
	}
}

func TestBuildResponseRefusesIncompleteAnswers(t *testing.T) {
	d := &Dialect{}
	two, _ := d.ParseInteraction(fixtureTask(t, "askuser/21-pause-good-two-answers.response.sse")) // multi-select, then free text
	one, _ := d.ParseInteraction(fixtureTask(t, "askuser/02-ask.response.sse"))                    // single-select
	tools, _ := d.ParseInteraction(pausedOn(map[string]any{"type": "tool_approval_request",
		"tools": []any{map[string]any{"id": "a", "name": "x"}, map[string]any{"id": "b", "name": "y"}}}))
	ans := func(in *dialect.Interaction, a ...[]string) dialect.Answer {
		return dialect.Answer{RequestID: in.RequestID, Answers: a}
	}
	yes := func(id string) dialect.Decision { return dialect.Decision{ToolID: id, Approved: true} }
	noID := *one
	noID.RequestID = ""
	noTask := *one
	noTask.TaskID = ""
	other := ans(one, []string{"PostgreSQL"})
	other.RequestID = "adk-some-other-pause"
	mixed := ans(one, []string{"PostgreSQL"})
	mixed.Decisions = []dialect.Decision{yes("a")}
	wrongKind := dialect.Answer{Answers: [][]string{{"x"}}, Decisions: []dialect.Decision{yes("a"), yes("b")}}

	for _, tc := range []struct {
		name string
		in   dialect.Interaction
		a    dialect.Answer
	}{
		{"no answers (kagent wedges)", *two, ans(two)},
		{"one of two questions", *two, ans(two, []string{"PostgreSQL"})},
		{"a question left empty", *two, ans(two, nil, []string{"blue"})},
		{"not one of the choices", *two, ans(two, []string{"Oracle"}, []string{"blue"})},
		{"a choice twice", *two, ans(two, []string{"MySQL", "MySQL"}, []string{"blue"})},
		{"blank free text", *two, ans(two, []string{"MySQL"}, []string{"  "})},
		{"two answers to a single-select", *one, ans(one, []string{"PostgreSQL", "MySQL"})},
		{"more answers than questions", *one, ans(one, []string{"PostgreSQL"}, []string{"blue"})},
		{"another pause's request id (kagent wedges)", *one, other},
		{"no request id in the answer", *one, dialect.Answer{Answers: [][]string{{"PostgreSQL"}}}},
		{"interaction without a request id", noID, ans(one, []string{"PostgreSQL"})},
		{"interaction without a task", noTask, ans(one, []string{"PostgreSQL"})},
		{"decisions for a question", *one, mixed},
		{"no decisions", *tools, dialect.Answer{}},
		{"one of two tools decided", *tools, dialect.Answer{Decisions: []dialect.Decision{yes("a")}}},
		{"a tool decided twice", *tools, dialect.Answer{Decisions: []dialect.Decision{yes("a"), yes("a")}}},
		{"an unknown tool (kagent wedges)", *tools, dialect.Answer{Decisions: []dialect.Decision{yes("a"), yes("nope")}}},
		{"answers for a tool approval", *tools, wrongKind},
		{"an ask-user with no questions (answers: null wedges kagent)", dialect.Interaction{Kind: dialect.AskUser, TaskID: "t", RequestID: "r"}, dialect.Answer{RequestID: "r"}},
		{"a tool approval with no tools (approvals: null is plain text)", dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "t"}, dialect.Answer{}},
		{"unknown kind", dialect.Interaction{Kind: "surprise", TaskID: "t"}, dialect.Answer{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := d.BuildResponse(tc.in, tc.a)
			if err == nil || msg.ID != "" || msg.Metadata != nil {
				t.Errorf("built %+v, %v; want an error and nothing built", msg, err)
			}
		})
	}
}

func TestBuildResponseToolApprovalsFollowTheRequestOrder(t *testing.T) {
	d := &Dialect{}
	in, _ := d.ParseInteraction(pausedOn(map[string]any{"type": "tool_approval_request",
		"tools": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}}))
	msg, err := d.BuildResponse(*in, dialect.Answer{Decisions: []dialect.Decision{{ToolID: "b", Reason: "no"}, {ToolID: "a", Approved: true}}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "tool_approval_response", "approvals": []any{
		map[string]any{"id": "a", "approved": true},
		map[string]any{"id": "b", "approved": false, "rejection_reason": "no"},
	}}
	if got := plain(t, msg.Metadata[HITLExtension]); !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %v\nwant      %v", got, want)
	}
}

// The ids a nested answer carries are the ones kagent v1.0.0-alpha5 matches
// (processNestedAskUser, processNestedApproval), on payloads shaped as its source builds
// them, since no nested capture exists.
func TestBuildResponseNestedIDs(t *testing.T) {
	d := &Dialect{}
	ask, _ := d.ParseInteraction(pausedOn(adkNestedAskUser()))
	msg, err := d.BuildResponse(*ask, dialect.Answer{RequestID: ask.RequestID, Answers: [][]string{{"east"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "ask_user_response", "id": "adk-child-ask", "answers": []any{map[string]any{"answer": []any{"east"}}}}
	if got := plain(t, msg.Metadata[HITLExtension]); !reflect.DeepEqual(got, want) {
		t.Errorf("ask-user payload = %v\nwant             %v", got, want)
	}

	approval, _ := d.ParseInteraction(pausedOn(adkNestedApproval()))
	msg, err = d.BuildResponse(*approval, dialect.Answer{Decisions: []dialect.Decision{{ToolID: "inner-1", Approved: true}}})
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"type": "tool_approval_response", "approvals": []any{map[string]any{"id": "inner-1", "approved": true}}}
	if got := plain(t, msg.Metadata[HITLExtension]); !reflect.DeepEqual(got, want) {
		t.Errorf("approval payload = %v\nwant                %v", got, want)
	}
	if _, err := d.BuildResponse(*approval, dialect.Answer{Decisions: []dialect.Decision{{ToolID: "adk-parent-tool", Approved: true}}}); err == nil {
		t.Error("a decision naming the outer tool was built; kagent matches only the inner tools")
	}
}

// An answer is attributed in metadata but gets no visible prefix: its text is ignored by
// kagent, and an extra part is unmeasured on a message whose only accepted shape is one part.
func TestAttributeLeavesAnswersWithoutAPrefix(t *testing.T) {
	d := &Dialect{}
	in, _ := d.ParseInteraction(fixtureTask(t, "askuser/02-ask.response.sse"))
	msg, err := d.BuildResponse(*in, dialect.Answer{RequestID: in.RequestID, Answers: [][]string{{"MySQL"}}})
	if err != nil {
		t.Fatal(err)
	}
	before := plain(t, msg.Metadata[HITLExtension])
	d.Attribute(&msg, dialect.User{ID: "U123", Email: "jane@example.com"})
	actor, _ := msg.Metadata[ActorMetadataKey].(map[string]any)
	if len(msg.Parts) != 1 || actor["id"] != "U123" || !reflect.DeepEqual(plain(t, msg.Metadata[HITLExtension]), before) {
		t.Errorf("parts = %d, actor = %v; want one part, the actor in metadata, the payload unchanged", len(msg.Parts), actor)
	}
}

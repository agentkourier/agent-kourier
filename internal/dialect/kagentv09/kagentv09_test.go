package kagentv09_test

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv09"
)

// The endpoint is the URL as written, trailing slash kept: kagent 0.9.x serves /api/a2a/{ns}/{name}/ and redirects
// a POST without the slash, which the dialect does not follow.
func TestEndpointIsTheURLAsWritten(t *testing.T) {
	const url = "http://kagent-controller.kagent.svc.cluster.local:8083/api/a2a/kagent/sre-investigator/"
	got, err := kagentv09.New(0).Endpoint(&v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{URL: url}}, v1alpha1.AgentRef{Namespace: "kagent", Name: "sre-investigator"})
	if err != nil || got != url {
		t.Errorf("Endpoint = %q, %v; want %q", got, err, url)
	}
}

func TestToolPartReadsKagentV09Parts(t *testing.T) {
	part := func(kind string, data map[string]any) *a2a.Part {
		p := a2a.NewDataPart(data)
		p.Metadata = map[string]any{"kagent_type": kind}
		return p
	}
	d := kagentv09.New(0)
	call, ok := d.ToolPart(part("function_call", map[string]any{"id": "c1", "name": "k8s_get_resources", "args": map[string]any{"resource_type": "namespace"}}))
	if !ok || call.Kind != dialect.ToolCall || call.ID != "c1" || call.Name != "k8s_get_resources" || call.Args["resource_type"] != "namespace" {
		t.Errorf("function_call = %+v, %v; want a call of k8s_get_resources", call, ok)
	}
	result, ok := d.ToolPart(part("function_response", map[string]any{"id": "c1", "name": "k8s_get_resources", "response": "ok"}))
	if !ok || result.Kind != dialect.ToolResult || result.ID != "c1" || result.Result != "ok" {
		t.Errorf("function_response = %+v, %v; want the result of c1", result, ok)
	}
	if _, ok := d.ToolPart(a2a.NewTextPart("the answer")); ok {
		t.Error("a text part was read as a tool part")
	}
}

// kagent 0.9.x has no HITL extension, so a pause is the generic dialect's: a plain-text question that takes a text reply.
func TestParseInteractionIsAPlainTextQuestion(t *testing.T) {
	task := a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired,
		Message: &a2a.Message{ID: "m1", Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{a2a.NewTextPart("Which namespace?")}}}}
	in, err := kagentv09.New(0).ParseInteraction(task)
	if err != nil || in == nil || !in.TextReplies || in.Kind != dialect.AskUser || len(in.Questions) != 1 || in.Questions[0].Text != "Which namespace?" {
		t.Errorf("ParseInteraction = %+v, %v; want one text question that takes text replies", in, err)
	}
}

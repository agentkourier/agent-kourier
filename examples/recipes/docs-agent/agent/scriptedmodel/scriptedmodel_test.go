package scriptedmodel_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/scriptedmodel"
)

func userText(s string) *genai.Content {
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: s}}}
}

func toolResponse(name string, out map[string]any) *genai.Content {
	return &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: name, Response: map[string]any{"output": out}}}}}
}

// step runs the model on the conversation and returns its one final response.
func step(t *testing.T, contents ...*genai.Content) *genai.Content {
	t.Helper()
	var got []*model.LLMResponse
	for resp, err := range (&scriptedmodel.Model{}).GenerateContent(context.Background(), &model.LLMRequest{Contents: contents}, true) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, resp)
	}
	if len(got) != 1 || got[0].Partial || !got[0].TurnComplete {
		t.Fatalf("responses = %+v, want one complete response", got)
	}
	return got[0].Content
}

func TestFirstStepSearchesForTheQuestion(t *testing.T) {
	c := step(t, userText("[Slack: Ada (U1) <ada@example.com>]\nHow do I connect an agent?"))
	call := c.Parts[0].FunctionCall
	if call == nil || call.Name != "search_docs" {
		t.Fatalf("first step = %+v, want a search_docs call", c.Parts)
	}
	if q, _ := call.Args["query"].(string); q != "How do I connect an agent?" {
		t.Errorf("query = %q, want the question without the actor line", q)
	}
}

func TestSecondStepReadsTheBestHit(t *testing.T) {
	hits := map[string]any{"results": []any{
		map[string]any{"path": "how-to/a.md", "anchor": "sec"},
		map[string]any{"path": "how-to/b.md"},
	}}
	c := step(t, userText("q"), toolResponse("search_docs", hits))
	call := c.Parts[0].FunctionCall
	if call == nil || call.Name != "read_page" || call.Args["path"] != "how-to/a.md" || call.Args["section"] != "sec" {
		t.Fatalf("second step = %+v, want read_page of the first hit's section", c.Parts)
	}
}

func TestNoHitsIsSaidPlainly(t *testing.T) {
	c := step(t, userText("q"), toolResponse("search_docs", map[string]any{"results": []any{}}))
	text := c.Parts[0].Text
	if !strings.Contains(text, "does not cover") || strings.Contains(text, ".md") {
		t.Errorf("answer = %q, want a plain statement that the docs do not cover it, with no source", text)
	}
}

func TestFinalAnswerCitesThePage(t *testing.T) {
	page := map[string]any{
		"path": "how-to/a.md", "title": "Connect it", "url": "https://d.example/how-to/a/",
		"text": "# Connect it\n\nFor a team with an agent: at the end, it works.\n\n## Next\n\nmore\n",
	}
	c := step(t, userText("q"), toolResponse("read_page", page))
	text := c.Parts[0].Text
	for _, want := range []string{"Connect it", "at the end, it works", "how-to/a.md", "https://d.example/how-to/a/"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer %q lacks %q", text, want)
		}
	}
	if strings.Contains(text, "# Connect it") {
		t.Errorf("answer repeats the heading marks: %q", text)
	}
}

func TestToolErrorsAreReported(t *testing.T) {
	errResp := &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "search_docs", Response: map[string]any{"error": "boom"}}}}}
	c := step(t, userText("q"), errResp)
	if text := c.Parts[0].Text; !strings.Contains(text, "could not") {
		t.Errorf("answer = %q", text)
	}
}

func TestRefusesAnEmptyConversation(t *testing.T) {
	for _, err := range (&scriptedmodel.Model{}).GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err == nil {
			t.Fatal("no error for an empty conversation")
		}
		return
	}
	t.Fatal("no response")
}

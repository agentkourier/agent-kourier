// Package scriptedmodel is a model that needs no network and no credentials. It follows the same three steps every
// time, which is what makes a smoke test of the docs agent repeatable: search the documentation for the user's
// question, read the best hit, then answer with the first sentence of its text and cite the page. When the search
// finds nothing it says that the documentation does not cover the question and cites nothing.
//
// It is not a language model and does not judge relevance. It exists to test the agent's wiring: ADK, the MCP tools,
// the A2A server and, through them, the corpus.
package scriptedmodel

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"strings"
	"sync/atomic"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Model is the scripted model. The zero value is ready to use.
type Model struct {
	calls atomic.Int64
}

var _ model.LLM = (*Model)(nil)

// Name identifies the model.
func (*Model) Name() string { return "scripted-docs" }

// actorLine is the line Agent Kourier puts before a person's message: "[Slack: Name (U123) <email>]".
var actorLine = regexp.MustCompile(`^\[[^\]\n]*\]\n`)

// GenerateContent answers one step of the script, chosen by what the last message of the conversation holds.
func (m *Model) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if req == nil || len(req.Contents) == 0 {
			yield(nil, errors.New("scriptedmodel: no conversation"))
			return
		}
		parts := req.Contents[len(req.Contents)-1].Parts
		for _, p := range parts {
			if fr := p.FunctionResponse; fr != nil {
				yield(m.respond(fr), nil)
				return
			}
		}
		var text strings.Builder
		for _, p := range parts {
			text.WriteString(p.Text)
		}
		query := strings.TrimSpace(actorLine.ReplaceAllString(text.String(), ""))
		yield(m.call("search_docs", map[string]any{"query": query}), nil)
	}
}

// respond answers a tool's result.
func (m *Model) respond(fr *genai.FunctionResponse) *model.LLMResponse {
	out, _ := fr.Response["output"].(map[string]any)
	if out == nil {
		return say("I could not search the documentation just now.")
	}
	switch fr.Name {
	case "search_docs":
		results, _ := out["results"].([]any)
		if len(results) == 0 {
			return say("The documentation I can search does not cover this. No page matches the question.")
		}
		best, _ := results[0].(map[string]any)
		args := map[string]any{"path": best["path"]}
		if anchor, _ := best["anchor"].(string); anchor != "" {
			args["section"] = anchor
		}
		return m.call("read_page", args)
	case "read_page":
		return say(answer(out))
	}
	return say("I could not read the documentation just now.")
}

// answer composes the final text from a page: its first sentence, then the source.
func answer(page map[string]any) string {
	title, _ := page["title"].(string)
	path, _ := page["path"].(string)
	url, _ := page["url"].(string)
	text, _ := page["text"].(string)
	source := path
	if url != "" {
		source = url + " (" + path + ")"
	}
	return fmt.Sprintf("According to the page \"%s\": %s\n\nSources:\n- %s", title, firstSentence(text), source)
}

// firstSentence returns the first sentence of the first paragraph of Markdown text that is not a heading.
func firstSentence(md string) string {
	for _, para := range strings.Split(md, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" || strings.HasPrefix(para, "#") {
			continue
		}
		para = strings.Join(strings.Fields(para), " ")
		if i := strings.Index(para, ". "); i >= 0 {
			return para[:i+1]
		}
		return para
	}
	return "the page has no text."
}

// call asks for a tool. Each call gets an ID of its own, since a session keeps every turn's calls.
func (m *Model) call(name string, args map[string]any) *model.LLMResponse {
	id := fmt.Sprintf("scripted-%d", m.calls.Add(1))
	return &model.LLMResponse{
		Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}}},
		TurnComplete: true,
	}
}

func say(text string) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}}, TurnComplete: true}
}

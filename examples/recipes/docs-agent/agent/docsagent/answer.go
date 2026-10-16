package docsagent

import (
	"context"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// answerParts is the executor's GenAIPartConverter. It makes the text of the model's final response, the one after
// the last tool call, the only text of the answer.
//
// Agent Kourier shows every artifact's text, one after the other, and a model writes text before and between its tool
// calls ("I'll search the documentation..."). With OutputArtifactPerEvent each model response is an artifact: its
// text streams in as partial events, and the aggregated response then replaces what streamed. Nothing says while a
// response streams whether tool calls follow. holdBack keeps the start of the text back, which is all of a short
// lead-in; answerParts retracts what got through anyway, when the response turns out not to be the answer: on an
// aggregated response that carries a tool call, each text part becomes an empty text part. An empty replacement is
// needed because Agent Kourier ignores an update without a text part, and a replace is what clears an artifact. The
// tool call itself stays, as a data part Agent Kourier does not show.
//
// Thoughts are never part of an answer, so they are dropped from every response.
func answerParts(_ context.Context, ev *session.Event, part *genai.Part) (*a2a.Part, error) {
	if part == nil {
		return nil, nil
	}
	if part.Text != "" {
		if part.Thought {
			return nil, nil
		}
		if !ev.Partial && hasToolCall(ev) {
			return a2a.NewTextPart(""), nil
		}
	}
	return adka2a.ToA2APart(part, ev.LongRunningToolIDs)
}

// hasToolCall reports whether the event asks for a tool.
func hasToolCall(ev *session.Event) bool {
	if ev.Content == nil {
		return false
	}
	for _, p := range ev.Content.Parts {
		if p != nil && p.FunctionCall != nil {
			return true
		}
	}
	return false
}

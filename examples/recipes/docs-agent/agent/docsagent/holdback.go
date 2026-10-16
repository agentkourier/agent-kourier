package docsagent

import (
	"context"
	"iter"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// holdBackBytes is how much text of a model response is held back before any of it is passed on. A model writes a
// sentence about what it is about to do before a tool call, usually well under this, and nothing says that a tool
// call follows until the response ends. Held text that turns out to be followed by a call is never seen. A response
// that is the answer passes the held text on, in one piece, once it holds this much, and the rest as it arrives: so a
// short answer arrives when its response ends, and a long one starts to stream after a moment.
//
// A longer narration than this still gets through. answerParts then retracts it, which a stream that cannot take text
// back, such as a Slack stream, shows as a gap: once the text streamed is no longer a prefix of the body, Agent
// Kourier appends nothing more for the whole turn (internal/render/live/stream.go), and replaces the message with the
// final text at the end of the turn.
//
// Known gap: if such a narration was streamed and the A2A stream drops before the emptying replace arrives, the
// GetTask or SubscribeToTask snapshot does not clear it, because Agent Kourier skips a snapshot whose text is a prefix
// of the text it holds (internal/agent/assemble.go). The narration then stays in the final message.
const holdBackBytes = 400

// holdBack is a model.LLM that holds back the first holdBackBytes of the streamed text of each response (see
// holdBackBytes). The aggregated response at the end of a stream carries the whole text, so holding text back loses
// nothing: it only decides when the text is first seen.
type holdBack struct{ model.LLM }

// GenerateContent streams the wrapped model's responses, holding back the start of a response's text. Anything that
// is not a partial response of plain text, such as the aggregated response, an error or a partial response with a
// thought or a tool call in it, passes on at once. Held text is not flushed ahead of such a response, though that
// would keep the order: text held while a tool call arrives is the lead-in this exists to hide, and the aggregated
// response carries whatever text is the answer.
func (h holdBack) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var held strings.Builder
		released := false
		for resp, err := range h.LLM.GenerateContent(ctx, req, stream) {
			text, plain := partialText(resp)
			if err != nil || !plain || released {
				if !yield(resp, err) {
					return
				}
				continue
			}
			held.WriteString(text)
			if held.Len() < holdBackBytes {
				continue
			}
			released = true
			merged := *resp
			merged.Content = &genai.Content{Role: resp.Content.Role, Parts: []*genai.Part{{Text: held.String()}}}
			if !yield(&merged, nil) {
				return
			}
		}
	}
}

// partialText returns the text of resp, if resp is a partial response made of plain text and nothing else.
func partialText(resp *model.LLMResponse) (string, bool) {
	if resp == nil || !resp.Partial || resp.Content == nil || len(resp.Content.Parts) == 0 {
		return "", false
	}
	var b strings.Builder
	for _, p := range resp.Content.Parts {
		if p == nil || p.Text == "" || p.Thought ||
			p.FunctionCall != nil || p.FunctionResponse != nil || p.InlineData != nil || p.FileData != nil {
			return "", false
		}
		b.WriteString(p.Text)
	}
	return b.String(), true
}

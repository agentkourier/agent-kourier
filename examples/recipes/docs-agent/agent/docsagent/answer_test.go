package docsagent_test

import (
	"context"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// streamingModel plays one fixed list of model turns, a turn per call, the way a streaming model does: the text of a
// turn arrives as partial chunks, and the turn ends with the aggregated response, which also carries its tool calls.
type streamingModel struct {
	turns [][]*model.LLMResponse
	next  atomic.Int64
}

func (*streamingModel) Name() string { return "scripted-streaming" }

func (m *streamingModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		i := int(m.next.Add(1)) - 1
		if i >= len(m.turns) {
			yield(nil, context.DeadlineExceeded) // a loop the script did not expect
			return
		}
		for _, r := range m.turns[i] {
			if !yield(r, nil) {
				return
			}
		}
	}
}

func chunk(text string) *model.LLMResponse {
	return &model.LLMResponse{Partial: true, Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}}}
}

// pieces is text as partial responses of at most size bytes each.
func pieces(text string, size int) []*model.LLMResponse {
	var out []*model.LLMResponse
	for len(text) > 0 {
		n := min(size, len(text))
		out = append(out, chunk(text[:n]))
		text = text[n:]
	}
	return out
}

func aggregated(text string, calls ...*genai.FunctionCall) *model.LLMResponse {
	var parts []*genai.Part
	if text != "" {
		parts = append(parts, &genai.Part{Text: text})
	}
	for _, c := range calls {
		parts = append(parts, &genai.Part{FunctionCall: c})
	}
	return &model.LLMResponse{TurnComplete: len(calls) == 0, Content: &genai.Content{Role: "model", Parts: parts}}
}

// shownText folds artifact updates the way Agent Kourier does (internal/agent assembler): an update with append adds
// to the artifact's text, any other replaces it, and an update with no text part changes nothing. It records every
// text an artifact has held, in order.
type shownText struct {
	text    map[a2a.ArtifactID]string
	order   []a2a.ArtifactID
	history map[a2a.ArtifactID][]string
}

func (s *shownText) apply(e *a2a.TaskArtifactUpdateEvent) {
	var b strings.Builder
	hasText := false
	for _, p := range e.Artifact.Parts {
		if t, ok := p.Content.(a2a.Text); ok {
			b.WriteString(string(t))
			hasText = true
		}
	}
	if !hasText {
		return
	}
	if s.text == nil {
		s.text, s.history = map[a2a.ArtifactID]string{}, map[a2a.ArtifactID][]string{}
	}
	id := e.Artifact.ID
	if _, seen := s.text[id]; !seen {
		s.order = append(s.order, id)
	}
	next := b.String()
	if e.Append {
		next = s.text[id] + next
	}
	s.text[id] = next
	s.history[id] = append(s.history[id], next)
}

// nonEmpty is what a reader sees: the artifacts that hold text, in the order they first appeared.
func (s *shownText) nonEmpty() []string {
	var out []string
	for _, id := range s.order {
		if strings.TrimSpace(s.text[id]) != "" {
			out = append(out, s.text[id])
		}
	}
	return out
}

func streamShown(t *testing.T, c *a2aclient.Client, question string) *shownText {
	t.Helper()
	shown := &shownText{}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(question))
	for event, err := range c.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: msg}) {
		if err != nil {
			t.Fatalf("A2A turn failed: %v", err)
		}
		if e, ok := event.(*a2a.TaskArtifactUpdateEvent); ok {
			shown.apply(e)
		}
	}
	return shown
}

// seen reports whether any text an artifact ever held, at any moment of the turn, contains want.
func (s *shownText) seen(want string) bool {
	for _, states := range s.history {
		for _, text := range states {
			if strings.Contains(text, want) {
				return true
			}
		}
	}
	return false
}

func call(id, name string, args map[string]any) *genai.FunctionCall {
	return &genai.FunctionCall{ID: id, Name: name, Args: args}
}

// TestOnlyTheFinalResponseIsTheAnswer pins that the text a model writes before and between tool calls ("Let me
// search...") is never shown, not even for a moment, while the response after the last tool call still streams.
func TestOnlyTheFinalResponseIsTheAnswer(t *testing.T) {
	answer := "Connect it with an AgentBackend.\n\n" + strings.Repeat("The backend names the agent's URL. ", 30) + "\n\nSources:\n- how-to/connect-agent.md"
	turn1 := append(pieces("I'll search the documentation.", 8),
		aggregated("I'll search the documentation.", call("c1", "search_docs", map[string]any{"query": "connect agent"})))
	turn2 := append(pieces("Let me read the guide.", 8),
		aggregated("Let me read the guide.", call("c2", "read_page", map[string]any{"path": "how-to/connect-agent.md"})))
	turn3 := append(pieces(answer, 60), aggregated(answer))
	m := &streamingModel{turns: [][]*model.LLMResponse{turn1, turn2, turn3}}
	a := startAgent(t, m)
	shown := streamShown(t, a.client(t), "How do I connect an agent?")

	if got := shown.nonEmpty(); len(got) != 1 || got[0] != answer {
		t.Fatalf("the reader sees %q, want only the answer", got)
	}
	for _, narration := range []string{"I'll search", "Let me read"} {
		if shown.seen(narration) {
			t.Errorf("%q was shown at some moment of the turn", narration)
		}
	}
	var streamed []string
	for _, id := range shown.order {
		if shown.text[id] == answer {
			streamed = shown.history[id]
		}
	}
	// The answer grows piece by piece: a reader watching the thread sees it before the turn ends.
	if len(streamed) < 3 || streamed[0] == answer || !strings.HasPrefix(answer, streamed[0]) {
		t.Errorf("the answer reached the reader in %d states, the first %q; want it to stream in pieces", len(streamed), streamed)
	}
	if m.next.Load() != 3 {
		t.Errorf("model turns = %d, want 3", m.next.Load())
	}
}

// TestLongNarrationIsRetracted covers narration so long that it was let through while it streamed: it is gone when the
// response turns out to carry a tool call.
func TestLongNarrationIsRetracted(t *testing.T) {
	const answer = "Connect it with an AgentBackend."
	narration := strings.Repeat("I will now look through the documentation for pages about connecting an agent. ", 12)
	turn1 := append(pieces(narration, 50), aggregated(narration, call("c1", "search_docs", map[string]any{"query": "connect agent"})))
	m := &streamingModel{turns: [][]*model.LLMResponse{turn1, {aggregated(answer)}}}
	a := startAgent(t, m)
	shown := streamShown(t, a.client(t), "How do I connect an agent?")

	if got := shown.nonEmpty(); len(got) != 1 || got[0] != answer {
		t.Fatalf("the reader sees %q, want only the answer %q", got, answer)
	}
	if !shown.seen("I will now look") {
		t.Error("the narration was never streamed, so this test does not exercise the retraction")
	}
}

// TestNarrationBeforeAToolCallIsRetractedWhateverIsStreamed covers a turn that was never streamed in chunks, and one
// that carries thoughts: neither may leave text behind when the turn calls a tool.
func TestNarrationBeforeAToolCallIsRetractedWhateverIsStreamed(t *testing.T) {
	m := &streamingModel{turns: [][]*model.LLMResponse{
		{{Content: &genai.Content{Role: "model", Parts: []*genai.Part{
			{Text: "Thinking about the question.", Thought: true},
			{Text: "Searching now."},
			{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "search_docs", Args: map[string]any{"query": "limits"}}},
		}}}},
		{{TurnComplete: true, Content: &genai.Content{Role: "model", Parts: []*genai.Part{
			{Text: "The limits are on the limits page."},
		}}}},
	}}
	a := startAgent(t, m)
	shown := streamShown(t, a.client(t), "What are the limits?")
	if got := shown.nonEmpty(); len(got) != 1 || got[0] != "The limits are on the limits page." {
		t.Fatalf("the reader sees %q, want only the final response", got)
	}
}

// TestThoughtsAreNeverTheAnswer pins that a thought is dropped from a final response that has no tool call, in the
// chunks that stream it and in the aggregated response.
func TestThoughtsAreNeverTheAnswer(t *testing.T) {
	const answer = "The limits are on the limits page."
	thought := &genai.Part{Text: "Private reasoning about the question.", Thought: true}
	m := &streamingModel{turns: [][]*model.LLMResponse{{
		{Partial: true, Content: &genai.Content{Role: "model", Parts: []*genai.Part{thought}}},
		chunk(answer),
		{TurnComplete: true, Content: &genai.Content{Role: "model", Parts: []*genai.Part{thought, {Text: answer}}}},
	}}}
	a := startAgent(t, m)
	shown := streamShown(t, a.client(t), "What are the limits?")
	if got := shown.nonEmpty(); len(got) != 1 || got[0] != answer {
		t.Fatalf("the reader sees %q, want only %q", got, answer)
	}
	if shown.seen("Private reasoning") {
		t.Error("a thought was shown at some moment of the turn")
	}
}

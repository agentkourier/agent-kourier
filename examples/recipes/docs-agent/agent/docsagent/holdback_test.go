package docsagent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type replay []*model.LLMResponse

func (replay) Name() string { return "replay" }
func (r replay) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, resp := range r {
			if !yield(resp, nil) {
				return
			}
		}
	}
}

func partial(parts ...*genai.Part) *model.LLMResponse {
	return &model.LLMResponse{Partial: true, Content: &genai.Content{Role: "model", Parts: parts}}
}

func text(s string) *genai.Part { return &genai.Part{Text: s} }

func collect(t *testing.T, m model.LLM) []*model.LLMResponse {
	t.Helper()
	var out []*model.LLMResponse
	for resp, err := range m.GenerateContent(t.Context(), &model.LLMRequest{}, true) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, resp)
	}
	return out
}

func TestHoldBackKeepsTheStartOfATextBack(t *testing.T) {
	long := strings.Repeat("x", holdBackBytes)
	final := &model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{text("short" + long + "tail")}}}
	tests := []struct {
		name string
		in   []*model.LLMResponse
		want []string // the text of each response passed on, "<final>" for the aggregated one
	}{
		{"text under the limit is never passed on", []*model.LLMResponse{partial(text("a")), partial(text("b")), final}, []string{"<final>"}},
		{"text over the limit is passed on in one piece, then as it comes",
			[]*model.LLMResponse{partial(text("short")), partial(text(long)), partial(text("tail")), final},
			[]string{"short" + long, "tail", "<final>"}},
		{"a thought passes at once", []*model.LLMResponse{partial(&genai.Part{Text: "hm", Thought: true}), final}, []string{"hm", "<final>"}},
		{"a tool call passes at once", []*model.LLMResponse{partial(&genai.Part{FunctionCall: &genai.FunctionCall{Name: "f"}}), final}, []string{"", "<final>"}},
		{"a response with no content passes at once", []*model.LLMResponse{{Partial: true}, final}, []string{"", "<final>"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collect(t, holdBack{replay(tt.in)})
			if len(got) != len(tt.want) {
				t.Fatalf("passed on %d responses, want %d", len(got), len(tt.want))
			}
			for i, resp := range got {
				var b strings.Builder
				if resp.Content != nil {
					for _, p := range resp.Content.Parts {
						b.WriteString(p.Text)
					}
				}
				want := tt.want[i]
				if want == "<final>" {
					if resp != final {
						t.Errorf("response %d is not the aggregated one", i)
					}
					continue
				}
				if b.String() != want {
					t.Errorf("response %d text = %q, want %q", i, b.String(), want)
				}
			}
		})
	}
}

type failing struct{ replay }

func (f failing) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for resp, err := range f.replay.GenerateContent(ctx, req, stream) {
			if !yield(resp, err) {
				return
			}
		}
		yield(nil, errors.New("boom"))
	}
}

func TestHoldBackPassesAnErrorOn(t *testing.T) {
	var gotErr error
	for _, err := range (holdBack{failing{replay{partial(text("a"))}}}).GenerateContent(t.Context(), &model.LLMRequest{}, true) {
		gotErr = err
	}
	if gotErr == nil || gotErr.Error() != "boom" {
		t.Fatalf("error = %v, want boom", gotErr)
	}
}

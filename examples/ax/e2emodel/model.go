// Package e2emodel drives the same gated, memory, question and final-output scenarios as kagent's scenario model.
// It is compiled only into the separate E2E agent binary, never the Bedrock agent.
package e2emodel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Model is one scenario's deterministic ADK model and gate control handler.
type Model struct {
	Scenario string
	mu       sync.Mutex
	gates    map[string]chan struct{}
}

// Name identifies the deterministic test model.
func (m *Model) Name() string { return "ax-e2e-" + m.Scenario }

func (m *Model) gate(name string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gates == nil {
		m.gates = make(map[string]chan struct{})
	}
	if m.gates[name] == nil {
		m.gates[name] = make(chan struct{})
	}
	return m.gates[name]
}

// ServeHTTP releases a test gate. Deploy only on the isolated E2E actor's control route.
func (m *Model) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/_control/release/")
	if r.Method != http.MethodPost || name == r.URL.Path || !validGate.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	gate := m.gate(name)
	m.mu.Lock()
	select {
	case <-gate:
	default:
		close(gate)
	}
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

var validGate = regexp.MustCompile(`^[\w-]+$`)
var gateInText = regexp.MustCompile(`gate:([\w-]+)`)

// GenerateContent runs the selected scenario using the actual persisted ADK conversation.
func (m *Model) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if len(req.Contents) == 0 {
			yield(nil, errors.New("missing conversation"))
			return
		}
		last := req.Contents[len(req.Contents)-1]
		var text strings.Builder
		for _, p := range last.Parts {
			if p.FunctionResponse != nil {
				answer, _ := p.FunctionResponse.Response["answer"].(string)
				yield(response("Answers: "+answer+".", false), nil)
				return
			}
			text.WriteString(p.Text)
		}
		switch m.Scenario {
		case "gated":
			match := gateInText.FindStringSubmatch(text.String())
			if len(match) != 2 {
				yield(response("No gate named, not holding.", false), nil)
				return
			}
			first := "Holding on gate " + match[1] + "."
			if !yield(response(first, true), nil) {
				return
			}
			timer := time.NewTimer(10 * time.Minute)
			defer timer.Stop()
			select {
			case <-m.gate(match[1]):
			case <-timer.C:
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
			yield(response(first+" Released.", false), nil)
		case "memory":
			var users []string
			for _, c := range req.Contents {
				if c.Role != "user" {
					continue
				}
				for _, p := range c.Parts {
					if strings.HasPrefix(p.Text, "[Slack: ") {
						users = append(users, p.Text)
					}
				}
			}
			earlier := "none"
			if len(users) > 1 {
				parts := make([]string, len(users)-1)
				for i, s := range users[:len(users)-1] {
					b, _ := json.Marshal(s)
					parts[i] = string(b)
				}
				earlier = strings.Join(parts, ", ")
			}
			yield(response(fmt.Sprintf("Message %d. Earlier: %s.", len(users), earlier), false), nil)
		case "ask":
			yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: uuid.NewString(), Name: "ask_user", Args: map[string]any{"question": "Which service?"}}}}}, TurnComplete: true}, nil)
		case "fails":
			if strings.Contains(text.String(), "provider-error") {
				yield(nil, errors.New("AGENT_SECRET_MARKER upstream provider failed"))
				return
			}
			yield(&model.LLMResponse{Content: &genai.Content{Role: "model"}, TurnComplete: true}, nil)
		default:
			yield(response("The answer is 4.", false), nil)
		}
	}
}

func response(text string, partial bool) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}}, Partial: partial, TurnComplete: !partial}
}

// Package smoke runs the docs agent end to end on loopback: the MCP server over HTTP, the ADK agent calling it, the
// A2A server in front, and an A2A client asking questions the way Agent Kourier does. With the scripted model it
// needs no network and no credentials, so it is repeatable; with a real model it is the owner's live check.
package smoke

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsagent"
)

// What the recipe's corpus is: the repository's documentation as it is published.
const (
	// DocsSiteURL is where the documentation is published.
	DocsSiteURL = "https://agentkourier.dev"
)

// DocsExclude is what the image leaves out of the repository's docs directory: the brand kit and the spike notes
// are not published, so the agent must not answer from them.
var DocsExclude = []string{"brand", "spikes"}

// Case is one question and what its answer must hold.
type Case struct {
	// Question is sent as one user message.
	Question string
	// WantAll lists strings the answer must contain; WantNone lists strings it must not.
	WantAll  []string
	WantNone []string
}

// DefaultCases are the fixed questions of the deterministic smoke, for the repository's documentation. The first
// must be answered from the page that explains it; the second is a follow-up in the same context; the third has no
// searchable match in the documentation and must be said not to be covered.
func DefaultCases() []Case {
	return []Case{
		{
			Question: "How do I connect a generic A2A agent?",
			WantAll:  []string{"how-to/connect-a2a-agent.md", DocsSiteURL + "/how-to/connect-a2a-agent/"},
		},
		{
			Question: "How do I rotate a binding token?",
			WantAll:  []string{"how-to/rotate-binding-token.md"},
		},
		{
			Question: "What is the airspeed velocity of an unladen swallow?",
			WantAll:  []string{"does not cover"},
			WantNone: []string{".md", DocsSiteURL},
		},
	}
}

// Options configures Run.
type Options struct {
	// Docs is the corpus directory; Exclude and SiteURL are as for corpus.Options and docsmcp.Config.
	Docs    string
	Exclude []string
	SiteURL string
	// Model is the model under test. Required.
	Model model.LLM
	// Cases are the questions to ask, in order, in one A2A context. At least one is required.
	Cases []Case
}

// Exchange is a question and the answer to it.
type Exchange struct {
	Question string
	Answer   string
}

// Run starts the agent on loopback, asks each case's question in one conversation, and checks the answer. It
// returns every exchange, and an error naming the first question whose answer fails its case, or a turn that fails.
func Run(ctx context.Context, opts Options) ([]Exchange, error) {
	if opts.Model == nil || len(opts.Cases) == 0 {
		return nil, errors.New("smoke: a model and at least one case are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	// The card names the agent's own address, which the listener knows before the server starts.
	agentHTTP := httptest.NewUnstartedServer(nil)
	handler, _, err := docsagent.Assemble(ctx, docsagent.AssembleOptions{
		Corpus: opts.Docs, Exclude: opts.Exclude, SiteURL: opts.SiteURL, Model: opts.Model,
		PublicURL: "http://" + agentHTTP.Listener.Addr().String() + "/a2a",
	})
	if err != nil {
		_ = agentHTTP.Listener.Close()
		return nil, err
	}
	agentHTTP.Config.Handler = handler
	agentHTTP.Start()
	defer agentHTTP.Close()

	card := &a2a.AgentCard{Name: "smoke", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(agentHTTP.URL+"/a2a", a2a.TransportProtocolJSONRPC)}}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithDefaultsDisabled(), a2aclient.WithJSONRPCTransport(agentHTTP.Client()))
	if err != nil {
		return nil, fmt.Errorf("A2A client: %w", err)
	}
	var out []Exchange
	var contextID string
	for _, c := range opts.Cases {
		task, err := ask(ctx, client, c.Question, contextID)
		if err != nil {
			return out, fmt.Errorf("question %q: %w", c.Question, err)
		}
		contextID = task.ContextID
		answer := answerText(task)
		out = append(out, Exchange{Question: c.Question, Answer: answer})
		if err := check(c, answer); err != nil {
			return out, fmt.Errorf("question %q: %w", c.Question, err)
		}
	}
	return out, nil
}

// ask sends one user message and returns the settled task.
func ask(ctx context.Context, c *a2aclient.Client, text, contextID string) (*a2a.Task, error) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ContextID = contextID
	var id a2a.TaskID
	for event, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg}) {
		if err != nil {
			return nil, fmt.Errorf("A2A turn failed: %w", err)
		}
		switch e := event.(type) {
		case *a2a.Task:
			id = e.ID
		case *a2a.TaskStatusUpdateEvent:
			id = e.TaskID
		case *a2a.TaskArtifactUpdateEvent:
			id = e.TaskID
		}
	}
	task, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
	if err != nil {
		return nil, fmt.Errorf("get the task: %w", err)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		return nil, fmt.Errorf("the task ended %s, not completed", task.Status.State)
	}
	return task, nil
}

// answerText is the text of the task's artifacts, which is where Agent Kourier reads an answer.
func answerText(task *a2a.Task) string {
	var b strings.Builder
	for _, art := range task.Artifacts {
		for _, p := range art.Parts {
			if text, ok := p.Content.(a2a.Text); ok {
				b.WriteString(string(text))
			}
		}
	}
	return b.String()
}

func check(c Case, answer string) error {
	for _, want := range c.WantAll {
		if !strings.Contains(answer, want) {
			return fmt.Errorf("the answer lacks %q", want)
		}
	}
	for _, bad := range c.WantNone {
		if strings.Contains(answer, bad) {
			return fmt.Errorf("the answer holds %q, which it must not", bad)
		}
	}
	return nil
}

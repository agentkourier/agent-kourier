package docsagent_test

import (
	"context"
	"iter"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/corpus"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsagent"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsmcp"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/scriptedmodel"
)

// serveAssembled runs the assembled handler on a server whose address the handler's card already names.
func serveAssembled(t *testing.T, opts docsagent.AssembleOptions) (*agentServer, <-chan error) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	opts.PublicURL = "http://" + ts.Listener.Addr().String() + "/a2a"
	h, done, err := docsagent.Assemble(t.Context(), opts)
	if err != nil {
		_ = ts.Listener.Close()
		t.Fatalf("Assemble: %v", err)
	}
	ts.Config.Handler = h
	ts.Start()
	t.Cleanup(ts.Close)
	return &agentServer{url: ts.URL, ts: ts}, done
}

func TestAssembleEmbedsTheMCPServer(t *testing.T) {
	a, done := serveAssembled(t, docsagent.AssembleOptions{Corpus: fixture, SiteURL: "https://docs.example.com", Model: &scriptedmodel.Model{}})
	if done == nil {
		t.Fatal("an embedded MCP server reports when it stops")
	}
	task := ask(t, a.client(t), "How do I connect an agent?", "")
	if text := answerText(task); !strings.Contains(text, "https://docs.example.com/how-to/connect-agent/") {
		t.Errorf("answer = %q", text)
	}
}

func TestAssembleUsesAnExternalMCPServer(t *testing.T) {
	ix, err := corpus.Load(t.Context(), fixture, corpus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := docsmcp.New(docsmcp.Config{Searcher: ix, Reader: ix, SiteURL: "https://elsewhere.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	mcpHTTP := httptest.NewServer(docsmcp.Handler(srv))
	defer mcpHTTP.Close()
	// No Corpus: the pages live behind the MCP server, not in this process.
	a, done := serveAssembled(t, docsagent.AssembleOptions{MCPURL: mcpHTTP.URL, Model: &scriptedmodel.Model{}})
	if done != nil {
		t.Error("no MCP server runs in this process, so there is nothing to wait for")
	}
	task := ask(t, a.client(t), "How do I connect an agent?", "")
	if text := answerText(task); !strings.Contains(text, "https://elsewhere.example.com/how-to/connect-agent/") {
		t.Errorf("answer = %q, want the external server's URLs", text)
	}
}

func TestAssembleRefusals(t *testing.T) {
	tests := []struct {
		name string
		opts docsagent.AssembleOptions
		want string
	}{
		{"no corpus and no MCP server", docsagent.AssembleOptions{Model: &scriptedmodel.Model{}, PublicURL: "http://x/a2a"}, "Corpus"},
		{"a missing corpus", docsagent.AssembleOptions{Corpus: "testdata/missing", Model: &scriptedmodel.Model{}, PublicURL: "http://x/a2a"}, "corpus"},
		{"a bad MCP URL", docsagent.AssembleOptions{MCPURL: "ftp://x", Model: &scriptedmodel.Model{}, PublicURL: "http://x/a2a"}, "MCPURL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := docsagent.Assemble(t.Context(), tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
}

// recordingModel is the scripted model that also notes the names of the tools each request offers.
type recordingModel struct {
	scriptedmodel.Model
	mu    sync.Mutex
	tools [][]string
}

func (m *recordingModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	var names []string
	if req.Config != nil {
		for _, t := range req.Config.Tools {
			for _, d := range t.FunctionDeclarations {
				names = append(names, d.Name)
			}
		}
	}
	slices.Sort(names)
	m.mu.Lock()
	m.tools = append(m.tools, names)
	m.mu.Unlock()
	return m.Model.GenerateContent(ctx, req, stream)
}

// TestAssembleOffersTheModelOnlyTheTwoTools pins that an external MCP server cannot widen what the model can call.
func TestAssembleOffersTheModelOnlyTheTwoTools(t *testing.T) {
	ix, err := corpus.Load(t.Context(), fixture, corpus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := docsmcp.New(docsmcp.Config{Searcher: ix, Reader: ix})
	if err != nil {
		t.Fatal(err)
	}
	type none struct{}
	mcp.AddTool(srv, &mcp.Tool{Name: "delete_everything", Description: "a third tool the agent must never offer"},
		func(context.Context, *mcp.CallToolRequest, none) (*mcp.CallToolResult, none, error) {
			return nil, none{}, nil
		})
	mcpHTTP := httptest.NewServer(docsmcp.Handler(srv))
	defer mcpHTTP.Close()

	rec := &recordingModel{}
	a, _ := serveAssembled(t, docsagent.AssembleOptions{MCPURL: mcpHTTP.URL, Model: rec})
	ask(t, a.client(t), "How do I connect an agent?", "")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.tools) == 0 {
		t.Fatal("the model was never called")
	}
	want := []string{"read_page", "search_docs"}
	for i, got := range rec.tools {
		if !slices.Equal(got, want) {
			t.Errorf("request %d offered the tools %v, want %v", i, got, want)
		}
	}
}

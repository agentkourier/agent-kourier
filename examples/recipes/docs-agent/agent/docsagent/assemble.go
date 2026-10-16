package docsagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/corpus"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsmcp"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/httpserve"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/urlcheck"
)

// AssembleOptions say where the agent's pages come from and which model answers.
type AssembleOptions struct {
	// Corpus is the directory of Markdown files, with Exclude and SiteURL as for corpus.Options and docsmcp.Config.
	// It is required unless MCPURL is set.
	Corpus  string
	Exclude []string
	SiteURL string
	// MCPURL, if set, is the streamable HTTP endpoint of another MCP server that offers search_docs and read_page.
	// The agent then serves no corpus of its own, and uses only the tools named search_docs and read_page, whatever
	// else the server offers. That is how a different retrieval, such as a vector search, is
	// attached without changing the agent.
	MCPURL string
	// Model, PublicURL and Subject are as for Config.
	Model     model.LLM
	PublicURL string
	Subject   string
}

// Assemble builds the agent's handler as the docs-agent command runs it. Unless MCPURL is set it loads the corpus,
// serves it as an MCP server on a loopback port that only this process reaches, and points the agent's MCP toolset
// at that. The returned channel receives once, when that embedded server stops, and is nil if there is none; ctx
// ends it.
func Assemble(ctx context.Context, o AssembleOptions) (http.Handler, <-chan error, error) {
	endpoint := o.MCPURL
	var done <-chan error
	switch {
	case endpoint != "":
		if _, err := urlcheck.HTTP(endpoint); err != nil {
			return nil, nil, fmt.Errorf("docsagent: MCPURL %w", err)
		}
	case o.Corpus == "":
		return nil, nil, errors.New("docsagent: Corpus is required when MCPURL is not set")
	default:
		ix, err := corpus.Load(ctx, o.Corpus, corpus.Options{Exclude: o.Exclude})
		if err != nil {
			return nil, nil, err
		}
		srv, err := docsmcp.New(docsmcp.Config{Searcher: ix, Reader: ix, SiteURL: o.SiteURL})
		if err != nil {
			return nil, nil, err
		}
		endpoint, done, err = httpserve.Loopback(ctx, docsmcp.Handler(srv), 5*time.Second)
		if err != nil {
			return nil, nil, err
		}
	}
	set, err := mcptoolset.New(mcptoolset.Config{Endpoint: endpoint})
	if err != nil {
		return nil, nil, fmt.Errorf("docsagent: build the MCP toolset: %w", err)
	}
	// Only the two tools the prompt names reach the model, whatever else an external server offers.
	tools := tool.FilterToolset(set, tool.AllowedToolsPredicate([]string{docsmcp.ToolSearch, docsmcp.ToolRead}))
	h, err := New(Config{Model: o.Model, Tools: tools, PublicURL: o.PublicURL, Subject: o.Subject})
	if err != nil {
		return nil, nil, err
	}
	return h, done, nil
}
